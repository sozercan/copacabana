package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/sozercan/copa-controller/api/v1alpha1"
	"github.com/sozercan/copa-controller/internal/imageref"
)

// RolloutReconciler rolls one candidate patch out to one namespace: approval, swap-list update,
// bounded rolling restarts, image and health checks, soak, and recovery.
type RolloutReconciler struct {
	client.Client
	Config   *Config
	Recorder events.EventRecorder
}

// SetupWithManager registers the reconciler.
func (r *RolloutReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.PatchRollout{}).
		Watches(&v1alpha1.PatchApproval{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
			a, ok := o.(*v1alpha1.PatchApproval)
			if !ok {
				return nil
			}
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: a.Namespace, Name: a.Spec.Rollout}}}
		})).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(r.rolloutsForDeployment)).
		WithOptions(controllerOptions(2)).
		Complete(r)
}

func (r *RolloutReconciler) rolloutsForDeployment(ctx context.Context, o client.Object) []reconcile.Request {
	var ros v1alpha1.PatchRolloutList
	if err := r.List(ctx, &ros, client.InNamespace(o.GetNamespace())); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, ro := range ros.Items {
		if !ro.Status.Phase.Terminal() || ro.Status.Phase == v1alpha1.RolloutSucceeded {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&ro)})
		}
	}
	return out
}

// Reconcile advances one PatchRollout.
func (r *RolloutReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	var ro v1alpha1.PatchRollout
	if err := r.Get(ctx, req.NamespacedName, &ro); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if ro.Status.Phase.Terminal() && ro.Status.Phase != v1alpha1.RolloutSucceeded {
		return ctrl.Result{}, nil
	}
	orig := ro.DeepCopy()
	res, err := r.step(ctx, &ro)
	if err != nil {
		return ctrl.Result{}, err
	}
	if ro.Status.Phase != orig.Status.Phase {
		now := metav1.Now()
		ro.Status.PhaseSince = &now
		logf.FromContext(ctx).Info("rollout phase", "rollout", req.String(), "phase", ro.Status.Phase, "message", ro.Status.Message)
		evType := corev1.EventTypeNormal
		if ro.Status.Phase == v1alpha1.RolloutRollingBack || ro.Status.Phase == v1alpha1.RolloutRecoveryFailed {
			evType = corev1.EventTypeWarning
		}
		r.Recorder.Eventf(&ro, nil, evType, string(ro.Status.Phase), "Rollout", "%s", ro.Status.Message)
		if ro.Status.Phase.Terminal() {
			rolloutsTotal.WithLabelValues(string(ro.Status.Phase)).Inc()
			if ro.Status.Phase == v1alpha1.RolloutSucceeded {
				rolloutDuration.Observe(time.Since(ro.CreationTimestamp.Time).Seconds())
			}
		}
	}
	if err := r.Status().Patch(ctx, &ro, client.MergeFrom(orig)); err != nil {
		return ctrl.Result{}, err
	}
	return res, nil
}

func (r *RolloutReconciler) step(ctx context.Context, ro *v1alpha1.PatchRollout) (ctrl.Result, error) {
	poll := ctrl.Result{RequeueAfter: 5 * time.Second}

	// A team can fail the rollout for its group during an incident. This also works after success,
	// as long as this rollout's candidate is still the selected patch.
	if ro.Status.Phase != v1alpha1.RolloutRollingBack && ro.Status.Phase != "" {
		if who, err := r.failRequested(ctx, ro); err != nil {
			return ctrl.Result{}, err
		} else if who != "" {
			if ro.Status.Phase == v1alpha1.RolloutSucceeded {
				selected, err := r.candidateSelected(ctx, ro)
				if err != nil || !selected {
					return ctrl.Result{}, err
				}
			}
			if ro.Status.Phase == v1alpha1.RolloutPending || ro.Status.Phase == v1alpha1.RolloutAwaitingApproval {
				ro.Status.Phase = v1alpha1.RolloutRolledBack
				ro.Status.Message = "cancelled before selection by " + who
				return ctrl.Result{}, nil
			}
			return r.startRecovery(ctx, ro, "failed by team request on "+who)
		}
	}

	switch ro.Status.Phase {
	case "", v1alpha1.RolloutPending:
		if err := r.discover(ctx, ro); err != nil {
			return ctrl.Result{}, err
		}
		ro.Status.Phase = v1alpha1.RolloutAwaitingApproval
		ro.Status.Message = "waiting for approval of " + ro.Spec.Candidate.Tag
		return ctrl.Result{Requeue: true}, nil

	case v1alpha1.RolloutAwaitingApproval:
		if err := r.discover(ctx, ro); err != nil {
			return ctrl.Result{}, err
		}
		var ns corev1.Namespace
		if err := r.Get(ctx, client.ObjectKey{Name: ro.Namespace}, &ns); err != nil {
			return ctrl.Result{}, err
		}
		if ns.Labels[v1alpha1.LabelApproval] == "automatic" {
			ro.Status.ApprovedBy = "policy:automatic"
		} else {
			var approvals v1alpha1.PatchApprovalList
			if err := r.List(ctx, &approvals, client.InNamespace(ro.Namespace)); err != nil {
				return ctrl.Result{}, err
			}
			for _, a := range approvals.Items {
				if a.Spec.Rollout == ro.Name && a.Spec.CandidateDigest == ro.Spec.Candidate.Digest {
					ro.Status.ApprovedBy = "patchapproval/" + a.Name
				}
			}
		}
		if ro.Status.ApprovedBy == "" {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		ro.Status.Phase = v1alpha1.RolloutSelecting
		ro.Status.Message = "approved by " + ro.Status.ApprovedBy
		return ctrl.Result{Requeue: true}, nil

	case v1alpha1.RolloutSelecting:
		if err := r.selectCandidate(ctx, ro); err != nil {
			return ctrl.Result{}, err
		}
		ok, got, err := r.dryRun(ctx, ro, ro.Spec.Candidate.Ref)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !ok {
			if ro.Status.PhaseSince != nil && time.Since(ro.Status.PhaseSince.Time) > time.Minute {
				return r.startRecovery(ctx, ro, "admission didn't apply the new mapping in a dry run; got "+got)
			}
			ro.Status.Message = "waiting for admission to see the new mapping"
			return poll, nil
		}
		ro.Status.Phase = v1alpha1.RolloutRestarting
		ro.Status.Message = "admission dry run returned the patched image; restarting workloads"
		return ctrl.Result{Requeue: true}, nil

	case v1alpha1.RolloutRestarting:
		if err := r.discover(ctx, ro); err != nil {
			return ctrl.Result{}, err
		}
		done, failed, err := r.progress(ctx, ro, r.candidateTarget(ro))
		if err != nil {
			return ctrl.Result{}, err
		}
		if failed != "" {
			return r.startRecovery(ctx, ro, failed)
		}
		if !done {
			return poll, nil
		}
		ro.Status.Phase = v1alpha1.RolloutSoaking
		ro.Status.Message = fmt.Sprintf("all workloads healthy; soaking for %s", r.Config.Soak)
		return poll, nil

	case v1alpha1.RolloutSoaking:
		if failed, err := r.soakCheck(ctx, ro); err != nil {
			return ctrl.Result{}, err
		} else if failed != "" {
			return r.startRecovery(ctx, ro, failed)
		}
		if ro.Status.PhaseSince != nil && time.Since(ro.Status.PhaseSince.Time) < r.Config.Soak {
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		if err := r.clearEntryRollout(ctx, ro); err != nil {
			return ctrl.Result{}, err
		}
		ro.Status.Phase = v1alpha1.RolloutSucceeded
		ro.Status.Message = ro.Spec.Candidate.Tag + " is the new known-good patch"
		return ctrl.Result{RequeueAfter: time.Minute}, nil

	case v1alpha1.RolloutSucceeded:
		return ctrl.Result{RequeueAfter: time.Minute}, nil

	case v1alpha1.RolloutRollingBack:
		done, failed, err := r.progress(ctx, ro, r.restoredTarget(ctx, ro))
		if err != nil {
			return ctrl.Result{}, err
		}
		if failed != "" {
			ro.Status.Phase = v1alpha1.RolloutRecoveryFailed
			ro.Status.Message = "recovery failed, operator action needed: " + failed
			return ctrl.Result{}, r.clearEntryRollout(ctx, ro)
		}
		if !done {
			return poll, nil
		}
		ro.Status.Phase = v1alpha1.RolloutRolledBack
		ro.Status.Message = "restored " + ro.Status.Restored + " and workloads recovered; " + ro.Status.Message
		return ctrl.Result{}, r.clearEntryRollout(ctx, ro)
	}
	return ctrl.Result{}, nil
}

// discover adds opted-in Deployments in the namespace that ask for the source and aren't tracked yet.
// It runs again on every pass until the workloads are restarted, so Deployments created while the rollout
// waits for approval are restarted too, and again when recovery starts.
func (r *RolloutReconciler) discover(ctx context.Context, ro *v1alpha1.PatchRollout) error {
	var deps appsv1.DeploymentList
	if err := r.List(ctx, &deps, client.InNamespace(ro.Namespace)); err != nil {
		return err
	}
	tracked := map[string]bool{}
	for _, w := range ro.Status.Workloads {
		tracked[w.UID] = true
	}
	for i := range deps.Items {
		d := &deps.Items[i]
		if tracked[string(d.UID)] || d.DeletionTimestamp != nil {
			continue
		}
		if d.Spec.Template.Labels[v1alpha1.LabelWorkloadOptIn] != v1alpha1.OptInValue || !usesSource(&d.Spec.Template.Spec, ro.Spec.Source) {
			continue
		}
		w := v1alpha1.RolloutWorkload{Name: d.Name, UID: string(d.UID), Phase: v1alpha1.WorkloadPending}
		if reason := eligibility(d, ro.Spec.Source); reason != "" && r.Config.RequireSafeStrategy {
			w.Phase = v1alpha1.WorkloadIneligible
			w.Message = reason + "; admission still applies the patch when this Deployment next creates pods"
		}
		ro.Status.Workloads = append(ro.Status.Workloads, w)
	}
	return nil
}

func usesSource(spec *corev1.PodSpec, source string) bool {
	for _, img := range templateImages(spec) {
		if p, err := imageref.Parse(img); err == nil && p.Canonical() == source {
			return true
		}
	}
	return false
}

// eligibility returns why a Deployment can't be restarted safely, or "".
func eligibility(d *appsv1.Deployment, source string) string {
	if d.Spec.Strategy.Type == appsv1.RecreateDeploymentStrategyType {
		return "uses the Recreate strategy"
	}
	replicas := int(ptr.Deref(d.Spec.Replicas, 1))
	ru := d.Spec.Strategy.RollingUpdate
	maxUnavailable := intstr.FromString("25%")
	maxSurge := intstr.FromString("25%")
	if ru != nil {
		if ru.MaxUnavailable != nil {
			maxUnavailable = *ru.MaxUnavailable
		}
		if ru.MaxSurge != nil {
			maxSurge = *ru.MaxSurge
		}
	}
	if v, _ := intstr.GetScaledValueFromIntOrPercent(&maxUnavailable, replicas, false); v != 0 {
		return "maxUnavailable isn't 0"
	}
	if v, _ := intstr.GetScaledValueFromIntOrPercent(&maxSurge, replicas, true); v == 0 {
		return "maxSurge is 0"
	}
	for _, c := range d.Spec.Template.Spec.Containers {
		if p, err := imageref.Parse(c.Image); err == nil && p.Canonical() == source && c.ReadinessProbe == nil {
			return "container " + c.Name + " has no readiness probe"
		}
	}
	return ""
}

func (r *RolloutReconciler) getSwapList(ctx context.Context, ns string) (*v1alpha1.SwapList, bool, error) {
	sw := &v1alpha1.SwapList{}
	err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: v1alpha1.SwapListName}, sw)
	if apierrors.IsNotFound(err) {
		return &v1alpha1.SwapList{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: v1alpha1.SwapListName}}, false, nil
	}
	return sw, err == nil, err
}

func (r *RolloutReconciler) saveSwapList(ctx context.Context, sw *v1alpha1.SwapList, exists bool) error {
	sw.RebuildImages()
	if exists {
		return r.Update(ctx, sw)
	}
	return r.Create(ctx, sw)
}

// selectCandidate points the swap-list entry at the candidate, keeping the old selection as Previous.
func (r *RolloutReconciler) selectCandidate(ctx context.Context, ro *v1alpha1.PatchRollout) error {
	sw, exists, err := r.getSwapList(ctx, ro.Namespace)
	if err != nil {
		return err
	}
	p, err := imageref.Parse(ro.Spec.Source)
	if err != nil {
		return err
	}
	e := sw.Entry(ro.Spec.Source)
	if e == nil {
		sw.Spec.Entries = append(sw.Spec.Entries, v1alpha1.SwapEntry{Source: ro.Spec.Source})
		e = &sw.Spec.Entries[len(sw.Spec.Entries)-1]
	}
	if e.Selected != nil && e.Selected.Digest == ro.Spec.Candidate.Digest && e.Rollout == ro.Name {
		return nil // already selected by this rollout
	}
	if e.Selected != nil && e.Selected.Digest != ro.Spec.Candidate.Digest {
		e.Previous = e.Selected.DeepCopy()
	}
	cand := ro.Spec.Candidate
	e.Selected = &cand
	e.Aliases = imageref.Aliases(p)
	e.SourceDigest = ro.Spec.SourceDigest
	e.Fallback = p.Repository + "@" + ro.Spec.SourceDigest
	e.Rollout = ro.Name
	return r.saveSwapList(ctx, sw, exists)
}

// startRecovery restores the previous patch, or the pinned source for a first patch, and replaces affected pods.
func (r *RolloutReconciler) startRecovery(ctx context.Context, ro *v1alpha1.PatchRollout, reason string) (ctrl.Result, error) {
	sw, exists, err := r.getSwapList(ctx, ro.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	if e := sw.Entry(ro.Spec.Source); e != nil && e.Selected != nil && e.Selected.Digest == ro.Spec.Candidate.Digest {
		e.Selected = e.Previous
		e.Previous = nil
		e.Rollout = ro.Name
		if err := r.saveSwapList(ctx, sw, exists); err != nil {
			return ctrl.Result{}, err
		}
		ro.Status.Restored = e.Fallback + " (unpatched source, pinned)"
		if e.Selected != nil {
			ro.Status.Restored = e.Selected.Tag
		}
	}
	// Replace every pod that may run the candidate. Workloads the team changed or opted out stay alone.
	// Deployments created since the last pass are picked up first: admission may have given them the candidate.
	if err := r.discover(ctx, ro); err != nil {
		return ctrl.Result{}, err
	}
	for i := range ro.Status.Workloads {
		w := &ro.Status.Workloads[i]
		switch w.Phase {
		case v1alpha1.WorkloadRestarting, v1alpha1.WorkloadHealthy, v1alpha1.WorkloadFailed:
			w.Phase = v1alpha1.WorkloadPending
			w.ReadySince, w.RestartBaseline, w.Message = nil, nil, "recovery"
		case v1alpha1.WorkloadPending:
			runs, err := r.runsDigest(ctx, ro.Namespace, w.Name, ro.Spec.Candidate.Digest)
			if err != nil {
				return ctrl.Result{}, err
			}
			if runs {
				w.Message = "recovery: pods were admitted with the candidate"
			} else {
				w.Phase = v1alpha1.WorkloadSuperseded
				w.Message = "never restarted onto the candidate"
			}
		}
	}
	ro.Status.Phase = v1alpha1.RolloutRollingBack
	ro.Status.Message = reason
	return ctrl.Result{Requeue: true}, nil
}

// runsDigest reports whether any pod of a Deployment was admitted with an image pinned to digest.
func (r *RolloutReconciler) runsDigest(ctx context.Context, ns, name, digest string) (bool, error) {
	var d appsv1.Deployment
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &d); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	sel, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil {
		return false, err
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(ns), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return false, err
	}
	for _, p := range pods.Items {
		for _, c := range append(slices.Clone(p.Spec.InitContainers), p.Spec.Containers...) {
			if strings.HasSuffix(c.Image, "@"+digest) {
				return true, nil
			}
		}
	}
	return false, nil
}

func (r *RolloutReconciler) clearEntryRollout(ctx context.Context, ro *v1alpha1.PatchRollout) error {
	sw, exists, err := r.getSwapList(ctx, ro.Namespace)
	if err != nil || !exists {
		return err
	}
	if e := sw.Entry(ro.Spec.Source); e != nil && e.Rollout == ro.Name {
		e.Rollout = ""
		return r.saveSwapList(ctx, sw, true)
	}
	return nil
}

func (r *RolloutReconciler) candidateSelected(ctx context.Context, ro *v1alpha1.PatchRollout) (bool, error) {
	sw, exists, err := r.getSwapList(ctx, ro.Namespace)
	if err != nil || !exists {
		return false, err
	}
	e := sw.Entry(ro.Spec.Source)
	return e != nil && e.Selected != nil && e.Selected.Digest == ro.Spec.Candidate.Digest, nil
}

// failRequested returns the Deployment that asked to fail this rollout, if any.
// Any Deployment the rollout tracks, or any opted-in Deployment that asks for the source (including ones
// created after the restarts, whose pods admission gave the candidate), can request it.
func (r *RolloutReconciler) failRequested(ctx context.Context, ro *v1alpha1.PatchRollout) (string, error) {
	var deps appsv1.DeploymentList
	if err := r.List(ctx, &deps, client.InNamespace(ro.Namespace)); err != nil {
		return "", err
	}
	tracked := map[string]bool{}
	for _, w := range ro.Status.Workloads {
		tracked[w.UID] = true
	}
	for i := range deps.Items {
		d := &deps.Items[i]
		v := d.Annotations[v1alpha1.AnnotationFailRollout]
		if v != ro.Name && v != "true" {
			continue
		}
		if tracked[string(d.UID)] ||
			d.Spec.Template.Labels[v1alpha1.LabelWorkloadOptIn] == v1alpha1.OptInValue && usesSource(&d.Spec.Template.Spec, ro.Spec.Source) {
			return "deployment/" + d.Name, nil
		}
	}
	return "", nil
}

// target is what pods must run: the image reference and every digest that counts as that image.
type target struct {
	ref     string
	digests []string
}

func (r *RolloutReconciler) candidateTarget(ro *v1alpha1.PatchRollout) target {
	t := target{ref: ro.Spec.Candidate.Ref, digests: []string{ro.Spec.Candidate.Digest}}
	for _, p := range ro.Spec.Candidate.Platforms {
		t.digests = append(t.digests, p.PatchedDigest)
	}
	return t
}

func (r *RolloutReconciler) restoredTarget(ctx context.Context, ro *v1alpha1.PatchRollout) target {
	sw, _, err := r.getSwapList(ctx, ro.Namespace)
	if err != nil {
		return target{}
	}
	e := sw.Entry(ro.Spec.Source)
	if e == nil {
		return target{}
	}
	if e.Selected != nil {
		t := target{ref: e.Selected.Ref, digests: []string{e.Selected.Digest}}
		for _, p := range e.Selected.Platforms {
			t.digests = append(t.digests, p.PatchedDigest)
		}
		return t
	}
	t := target{ref: e.Fallback, digests: []string{e.SourceDigest}}
	var si v1alpha1.SourceImage
	if err := r.Get(ctx, client.ObjectKey{Name: ro.Spec.SourceImage}, &si); err == nil {
		for _, p := range si.Spec.Platforms {
			t.digests = append(t.digests, p.Digest)
		}
	}
	return t
}

// dryRun asks the API server to admit a probe pod and reports whether admission swapped in want.
func (r *RolloutReconciler) dryRun(ctx context.Context, ro *v1alpha1.PatchRollout, want string) (bool, string, error) {
	p, err := imageref.Parse(ro.Spec.Source)
	if err != nil {
		return false, "", err
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "copa-dryrun-",
			Namespace:    ro.Namespace,
			Labels:       map[string]string{v1alpha1.LabelWorkloadOptIn: v1alpha1.OptInValue},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "probe", Image: imageref.Aliases(p)[0]}}},
	}
	if err := r.Create(ctx, pod, client.DryRunAll); err != nil {
		return false, "", fmt.Errorf("dry-run pod create: %w", err)
	}
	got := pod.Spec.Containers[0].Image
	return got == want, got, nil
}

// progress restarts Pending workloads within the concurrency limit and checks the ones restarting.
// It returns done when no workload is pending or restarting, and a failure reason if one failed.
func (r *RolloutReconciler) progress(ctx context.Context, ro *v1alpha1.PatchRollout, want target) (bool, string, error) {
	if want.ref == "" {
		return false, "", fmt.Errorf("no target image for %s", ro.Spec.Source)
	}
	inflight, err := r.inflightRestarts(ctx)
	if err != nil {
		return false, "", err
	}
	done := true
	for i := range ro.Status.Workloads {
		w := &ro.Status.Workloads[i]
		if w.Phase != v1alpha1.WorkloadPending && w.Phase != v1alpha1.WorkloadRestarting {
			continue
		}
		var d appsv1.Deployment
		err := r.Get(ctx, client.ObjectKey{Namespace: ro.Namespace, Name: w.Name}, &d)
		switch {
		case apierrors.IsNotFound(err):
			w.Phase, w.Message = v1alpha1.WorkloadSuperseded, "deleted"
			continue
		case err != nil:
			return false, "", err
		case string(d.UID) != w.UID:
			w.Phase, w.Message = v1alpha1.WorkloadSuperseded, "replaced by a new Deployment"
			continue
		case d.Spec.Template.Labels[v1alpha1.LabelWorkloadOptIn] != v1alpha1.OptInValue:
			w.Phase, w.Message = v1alpha1.WorkloadOptedOut, "opt-in label removed; runs the source image from Git"
			continue
		case !usesSource(&d.Spec.Template.Spec, ro.Spec.Source):
			w.Phase, w.Message = v1alpha1.WorkloadSuperseded, "team changed the image during the rollout"
			continue
		}

		if w.Phase == v1alpha1.WorkloadPending {
			if inflight >= r.Config.MaxConcurrentRestarts {
				done = false
				w.Message = "waiting for a restart slot"
				continue
			}
			restartedAt := time.Now().UTC().Format(time.RFC3339)
			p := client.MergeFrom(d.DeepCopy())
			if d.Spec.Template.Annotations == nil {
				d.Spec.Template.Annotations = map[string]string{}
			}
			d.Spec.Template.Annotations[v1alpha1.AnnotationRestartedAt] = restartedAt
			if err := r.Patch(ctx, &d, p); err != nil {
				return false, "", err
			}
			w.Phase, w.RestartedAt, w.Generation, w.Message = v1alpha1.WorkloadRestarting, restartedAt, d.Generation, "restarting"
			inflight++
			done = false
			continue
		}

		// Restarting: a newer generation than ours means a team deployed. Don't fight it.
		if d.Generation != w.Generation {
			w.Phase, w.Message = v1alpha1.WorkloadSuperseded, "team deployed during the rollout"
			continue
		}
		done = false
		started, _ := time.Parse(time.RFC3339, w.RestartedAt)
		if deadlineExceeded(&d) || time.Since(started) > r.Config.RolloutDeadline {
			w.Phase, w.Message = v1alpha1.WorkloadFailed, "rollout deadline exceeded"
			return false, fmt.Sprintf("%s: rollout deadline exceeded", w.Name), nil
		}
		if !rolledOut(&d) {
			w.Message = fmt.Sprintf("%d/%d updated, %d available", d.Status.UpdatedReplicas, ptr.Deref(d.Spec.Replicas, 1), d.Status.AvailableReplicas)
			continue
		}
		restarts, problem, err := r.checkPods(ctx, &d, w.RestartedAt, ro.Spec.Source, want)
		if err != nil {
			return false, "", err
		}
		if problem != "" {
			w.Phase, w.Message = v1alpha1.WorkloadFailed, problem
			return false, w.Name + ": " + problem, nil
		}
		if w.ReadySince == nil {
			w.ReadySince, w.RestartBaseline = ptr.To(metav1.Now()), ptr.To(restarts)
			w.Message = "running " + shortRef(want.ref) + "; checking it stays healthy"
			continue
		}
		if restarts > *w.RestartBaseline {
			w.Phase, w.Message = v1alpha1.WorkloadFailed, "containers restarted after the patch"
			return false, w.Name + ": containers restarted after the patch", nil
		}
		if time.Since(w.ReadySince.Time) >= r.Config.HealthyFor {
			w.Phase, w.Message = v1alpha1.WorkloadHealthy, "healthy on "+shortRef(want.ref)
		}
	}
	return done, "", nil
}

func shortRef(ref string) string {
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		ref = ref[i+1:]
	}
	if i := strings.Index(ref, "@sha256:"); i >= 0 && len(ref) > i+20 {
		ref = ref[:i+20]
	}
	return ref
}

// inflightRestarts counts workloads restarting across every rollout in the cluster.
func (r *RolloutReconciler) inflightRestarts(ctx context.Context) (int, error) {
	var all v1alpha1.PatchRolloutList
	if err := r.List(ctx, &all); err != nil {
		return 0, err
	}
	n := 0
	for _, ro := range all.Items {
		for _, w := range ro.Status.Workloads {
			if w.Phase == v1alpha1.WorkloadRestarting {
				n++
			}
		}
	}
	return n, nil
}

func deadlineExceeded(d *appsv1.Deployment) bool {
	for _, c := range d.Status.Conditions {
		if c.Type == appsv1.DeploymentProgressing && c.Status == corev1.ConditionFalse && c.Reason == "ProgressDeadlineExceeded" {
			return true
		}
	}
	return false
}

func rolledOut(d *appsv1.Deployment) bool {
	want := ptr.Deref(d.Spec.Replicas, 1)
	return d.Status.ObservedGeneration >= d.Generation &&
		d.Status.UpdatedReplicas == want &&
		d.Status.Replicas == want &&
		d.Status.AvailableReplicas == want
}

// checkPods verifies that the Deployment's new pods run the target image, both in the admitted spec and at runtime.
// It returns the total container restart count of those pods.
func (r *RolloutReconciler) checkPods(ctx context.Context, d *appsv1.Deployment, restartedAt, source string, want target) (int32, string, error) {
	sel, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil {
		return 0, "", err
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(d.Namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return 0, "", err
	}
	var restarts int32
	checked := 0
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp != nil || pod.Annotations[v1alpha1.AnnotationRestartedAt] != restartedAt {
			continue
		}
		checked++
		swapped := pod.Annotations[v1alpha1.AnnotationSwapped]
		names := map[string]bool{}
		for _, c := range append(slices.Clone(pod.Spec.InitContainers), pod.Spec.Containers...) {
			if p, err := imageref.Parse(c.Image); err == nil && p.Canonical() == source {
				return 0, fmt.Sprintf("pod %s container %s runs the unswapped source %s (missed swap)", pod.Name, c.Name, c.Image), nil
			}
			if strings.Contains(swapped, c.Name+"=") {
				if c.Image != want.ref {
					return 0, fmt.Sprintf("pod %s container %s runs %s, want %s", pod.Name, c.Name, c.Image, want.ref), nil
				}
				names[c.Name] = true
			}
		}
		statuses := append(slices.Clone(pod.Status.InitContainerStatuses), pod.Status.ContainerStatuses...)
		for _, cs := range statuses {
			if !names[cs.Name] {
				continue
			}
			if !slices.ContainsFunc(want.digests, func(dg string) bool { return strings.HasSuffix(cs.ImageID, "@"+dg) }) {
				return 0, fmt.Sprintf("pod %s container %s runtime image %s isn't the target", pod.Name, cs.Name, cs.ImageID), nil
			}
			restarts += cs.RestartCount
		}
	}
	if checked == 0 {
		return 0, "no pods from the restart found", nil
	}
	return restarts, "", nil
}

// soakCheck re-checks healthy workloads while the patch soaks.
func (r *RolloutReconciler) soakCheck(ctx context.Context, ro *v1alpha1.PatchRollout) (string, error) {
	want := r.candidateTarget(ro)
	for _, w := range ro.Status.Workloads {
		if w.Phase != v1alpha1.WorkloadHealthy {
			continue
		}
		var d appsv1.Deployment
		if err := r.Get(ctx, client.ObjectKey{Namespace: ro.Namespace, Name: w.Name}, &d); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return "", err
		}
		if d.Generation != w.Generation {
			continue // team deployed; their new pods are checked by admission and the next rollout
		}
		if d.Status.AvailableReplicas < ptr.Deref(d.Spec.Replicas, 1) {
			return fmt.Sprintf("%s: %d/%d available during soak", w.Name, d.Status.AvailableReplicas, ptr.Deref(d.Spec.Replicas, 1)), nil
		}
		restarts, problem, err := r.checkPods(ctx, &d, w.RestartedAt, ro.Spec.Source, want)
		if err != nil {
			return "", err
		}
		if problem != "" {
			return w.Name + ": " + problem, nil
		}
		if w.RestartBaseline != nil && restarts > *w.RestartBaseline {
			return w.Name + ": containers restarted during soak", nil
		}
	}
	return "", nil
}
