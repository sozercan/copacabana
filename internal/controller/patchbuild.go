package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/sozercan/copacabana/api/v1alpha1"
	"github.com/sozercan/copacabana/internal/worker"
)

// PatchBuildReconciler drives a PatchBuild through scan, build and verify Jobs.
type PatchBuildReconciler struct {
	client.Client
	Config   *Config
	Recorder events.EventRecorder
}

// SetupWithManager registers the reconciler. Jobs live in two namespaces, so they're linked by label, not owner.
func (r *PatchBuildReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.PatchBuild{}).
		Watches(&batchv1.Job{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
			if n := o.GetLabels()[v1alpha1.LabelPatchBuild]; n != "" {
				return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: r.Config.BuilderNamespace, Name: n}}}
			}
			return nil
		})).
		WithOptions(controllerOptions(4)).
		Complete(r)
}

// jobOutcome is the state of a step's Job.
type jobOutcome struct {
	done, failed bool
	message      string
	// waiting explains why a running Job isn't making progress, such as a pod that can't be scheduled.
	waiting string
}

// Reconcile advances one PatchBuild.
func (r *PatchBuildReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	var pb v1alpha1.PatchBuild
	if err := r.Get(ctx, req.NamespacedName, &pb); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if pb.Status.Phase.Terminal() {
		return ctrl.Result{}, nil
	}
	orig := pb.DeepCopy()
	res, err := r.step(ctx, &pb)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pb.Status.Phase != orig.Status.Phase {
		logf.FromContext(ctx).Info("build phase", "build", pb.Name, "phase", pb.Status.Phase, "message", pb.Status.Message)
		if pb.Status.Phase.Terminal() {
			pb.Status.CompletionTime = ptr.To(metav1.Now())
			buildsTotal.WithLabelValues(string(pb.Status.Phase)).Inc()
			evType := corev1.EventTypeNormal
			if pb.Status.Phase == v1alpha1.BuildFailed {
				evType = corev1.EventTypeWarning
			}
			r.Recorder.Eventf(&pb, nil, evType, string(pb.Status.Phase), "Build", "%s", pb.Status.Message)
		}
	}
	if err := r.Status().Patch(ctx, &pb, client.MergeFrom(orig)); err != nil {
		return ctrl.Result{}, err
	}
	return res, nil
}

func (r *PatchBuildReconciler) step(ctx context.Context, pb *v1alpha1.PatchBuild) (ctrl.Result, error) {
	wait := ctrl.Result{RequeueAfter: 15 * time.Second}
	switch pb.Status.Phase {
	case "", v1alpha1.BuildPending:
		pb.Status.Platforms = nil
		for _, p := range pb.Spec.Platforms {
			pb.Status.Platforms = append(pb.Status.Platforms, v1alpha1.PlatformBuild{Platform: p.Platform, SourceDigest: p.Digest})
		}
		pb.Status.Phase = v1alpha1.BuildScanningSource
		pb.Status.Message = "scanning source"
		if pb.Spec.Baseline != nil {
			pb.Status.Phase = v1alpha1.BuildScanningBaseline
			pb.Status.Message = "rescanning current patch " + pb.Spec.Baseline.Tag
		}
		return ctrl.Result{Requeue: true}, nil

	case v1alpha1.BuildScanningBaseline:
		patchedRepo := r.Config.PatchedRepository(pb.Spec.Repository)
		done := true
		for i := range pb.Status.Platforms {
			p := &pb.Status.Platforms[i]
			if p.BaselineFixable != nil {
				continue
			}
			pd := baselineDigest(pb.Spec.Baseline, p.Platform)
			if pd == "" {
				p.BaselineFixable = ptr.To(-1) // current patch doesn't cover this platform
				continue
			}
			out, err := r.runJob(ctx, r.scanJob(pb, "rescan", p.Platform, patchedRepo+"@"+pd, ""))
			if err != nil {
				return ctrl.Result{}, err
			}
			switch {
			case out.failed:
				return r.fail(pb, "scan", out.message)
			case out.done:
				var sr worker.ScanResult
				if err := json.Unmarshal([]byte(out.message), &sr); err != nil {
					return r.fail(pb, "scan", "unreadable scan result: "+out.message)
				}
				p.BaselineFixable = ptr.To(sr.Fixable)
			default:
				done = false
				if out.waiting != "" {
					pb.Status.Message = out.waiting
				}
			}
		}
		if !done {
			return wait, nil
		}
		if !baselineNeedsBuild(pb) {
			pb.Status.Phase = v1alpha1.BuildNothingToFix
			pb.Status.Message = "current patch " + pb.Spec.Baseline.Tag + " has no new fixes"
			return ctrl.Result{}, nil
		}
		pb.Status.Phase = v1alpha1.BuildScanningSource
		pb.Status.Message = "new fixes available; scanning source"
		return ctrl.Result{Requeue: true}, nil

	case v1alpha1.BuildScanningSource:
		reportRepo := r.Config.ReportRepository(pb.Spec.Repository)
		done := true
		for i := range pb.Status.Platforms {
			p := &pb.Status.Platforms[i]
			if p.ReportRef != "" {
				continue
			}
			out, err := r.runJob(ctx, r.scanJob(pb, "scan", p.Platform, pb.Spec.Repository+"@"+p.SourceDigest, reportRepo))
			if err != nil {
				return ctrl.Result{}, err
			}
			switch {
			case out.failed:
				return r.fail(pb, "scan", out.message)
			case out.done:
				var sr worker.ScanResult
				if err := json.Unmarshal([]byte(out.message), &sr); err != nil || sr.ReportRef == "" {
					return r.fail(pb, "scan", "unreadable scan result: "+out.message)
				}
				// Zero findings only means "nothing to fix" when there was an OS to assess.
				if sr.OSFamily == "" {
					return r.fail(pb, "scan", p.Platform+": no OS packages detected; Copa can only patch OS packages")
				}
				p.ReportRef = sr.ReportRef
				p.Fixable = ptr.To(sr.Fixable)
			default:
				done = false
				if out.waiting != "" {
					pb.Status.Message = out.waiting
				}
			}
		}
		if !done {
			return wait, nil
		}
		total := 0
		for _, p := range pb.Status.Platforms {
			total += *p.Fixable
		}
		switch {
		case total == 0:
			pb.Status.Phase = v1alpha1.BuildNothingToFix
			pb.Status.Message = "no fixable OS vulnerabilities"
		case pb.Spec.ScanOnly:
			pb.Status.Phase = v1alpha1.BuildScanned
			pb.Status.Message = fmt.Sprintf("%d fixable OS vulnerabilities (report-only)", total)
		default:
			pb.Status.Phase = v1alpha1.BuildBuilding
			pb.Status.Message = fmt.Sprintf("patching %d fixable OS vulnerabilities", total)
		}
		return ctrl.Result{Requeue: true}, nil

	case v1alpha1.BuildBuilding:
		stagingRepo := r.Config.StagingRepository(pb.Spec.Repository)
		done := true
		for i := range pb.Status.Platforms {
			p := &pb.Status.Platforms[i]
			if p.StagedRef != "" {
				continue
			}
			target := fmt.Sprintf("%s:%s-%s-%s", stagingRepo, TagBase(pb.Spec.Reference), pb.Name, platformSlug(p.Platform))
			out, err := r.runJob(ctx, r.buildJob(pb, p.Platform, pb.Spec.Repository+"@"+p.SourceDigest, p.ReportRef, target))
			if err != nil {
				return ctrl.Result{}, err
			}
			switch {
			case out.failed:
				return r.fail(pb, "build", out.message)
			case out.done:
				var br worker.BuildResult
				if err := json.Unmarshal([]byte(out.message), &br); err != nil || br.StagedRef == "" {
					return r.fail(pb, "build", "unreadable build result: "+out.message)
				}
				p.StagedRef = br.StagedRef
				p.VEXRef = br.VEXRef
			default:
				done = false
				if out.waiting != "" {
					pb.Status.Message = out.waiting
				}
			}
		}
		if !done {
			return wait, nil
		}
		pb.Status.Phase = v1alpha1.BuildVerifying
		pb.Status.Message = "verifying staged images"
		return ctrl.Result{Requeue: true}, nil

	case v1alpha1.BuildVerifying:
		vs := worker.VerifySpec{
			PatchBuild:              pb.Name,
			SourceRepository:        pb.Spec.Repository,
			SourceDigest:            pb.Spec.SourceDigest,
			SourceReference:         pb.Spec.Reference,
			TagBase:                 TagBase(pb.Spec.Reference),
			PatchedRepository:       r.Config.PatchedRepository(pb.Spec.Repository),
			AllowNewVulnerabilities: r.Config.AllowNewVulnerabilities,
		}
		for _, p := range pb.Status.Platforms {
			vs.Platforms = append(vs.Platforms, worker.VerifyPlatform{
				Platform: p.Platform, SourceDigest: p.SourceDigest, StagedRef: p.StagedRef, ReportRef: p.ReportRef, VEXRef: p.VEXRef,
			})
		}
		job, err := r.verifyJob(pb, vs)
		if err != nil {
			return ctrl.Result{}, err
		}
		out, err := r.runJob(ctx, job)
		if err != nil {
			return ctrl.Result{}, err
		}
		switch {
		case out.failed:
			return r.fail(pb, "verify", out.message)
		case out.done:
			var vr worker.VerifyResult
			if err := json.Unmarshal([]byte(out.message), &vr); err != nil || vr.Ref == "" {
				return r.fail(pb, "verify", "unreadable verify result: "+out.message)
			}
			pb.Status.Promoted = &v1alpha1.PatchRef{Ref: vr.Ref, Tag: vr.Tag, Digest: vr.Digest, Revision: vr.Revision, Platforms: vr.Platforms}
			pb.Status.Phase = v1alpha1.BuildPromoted
			pb.Status.Message = "promoted " + vr.Tag
			return ctrl.Result{}, nil
		}
		if out.waiting != "" {
			pb.Status.Message = out.waiting
		}
		return wait, nil
	}
	return ctrl.Result{}, nil
}

func (r *PatchBuildReconciler) fail(pb *v1alpha1.PatchBuild, stage, msg string) (ctrl.Result, error) {
	pb.Status.Phase = v1alpha1.BuildFailed
	pb.Status.FailedStage = stage
	pb.Status.Message = trimMessage(msg)
	return ctrl.Result{}, nil
}

func trimMessage(msg string) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(msg), &e) == nil && e.Error != "" {
		msg = e.Error
	}
	msg = strings.TrimSpace(msg)
	if len(msg) > 600 {
		msg = msg[:600] + "…"
	}
	return msg
}

func baselineDigest(b *v1alpha1.PatchRef, platform string) string {
	if b == nil {
		return ""
	}
	for _, p := range b.Platforms {
		if p.Platform == platform {
			return p.PatchedDigest
		}
	}
	return ""
}

// baselineNeedsBuild reports whether the current patch has more fixable vulnerabilities than it had when verified,
// meaning new fixes were published since. A patch that couldn't fix everything isn't rebuilt every interval.
func baselineNeedsBuild(pb *v1alpha1.PatchBuild) bool {
	for _, p := range pb.Status.Platforms {
		if p.BaselineFixable == nil || *p.BaselineFixable < 0 {
			return true
		}
		remaining := 0
		for _, bp := range pb.Spec.Baseline.Platforms {
			if bp.Platform == p.Platform {
				remaining = bp.RemainingFixable
			}
		}
		if *p.BaselineFixable > remaining {
			return true
		}
	}
	return false
}

// runJob creates the Job if it doesn't exist and reports its outcome, reading the worker's termination message.
// unschedulable reports a Job pod the scheduler can't place, so a missing builder node shows up in the status
// instead of looking like a slow scan until the Job deadline.
func (r *PatchBuildReconciler) unschedulable(ctx context.Context, job *batchv1.Job) string {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(job.Namespace), client.MatchingLabels{"batch.kubernetes.io/job-name": job.Name}); err != nil {
		return ""
	}
	for _, p := range pods.Items {
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
				return fmt.Sprintf("waiting: %s pod can't be scheduled: %s", job.Labels["copa.sh/stage"], c.Message)
			}
		}
	}
	return ""
}

func (r *PatchBuildReconciler) runJob(ctx context.Context, job *batchv1.Job) (jobOutcome, error) {
	var existing batchv1.Job
	err := r.Get(ctx, client.ObjectKeyFromObject(job), &existing)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
			return jobOutcome{}, err
		}
		return jobOutcome{}, nil
	}
	if err != nil {
		return jobOutcome{}, err
	}
	var complete, failed bool
	for _, c := range existing.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			complete = true
		case batchv1.JobFailed:
			failed = true
		}
	}
	if !complete && !failed {
		return jobOutcome{waiting: r.unschedulable(ctx, &existing)}, nil
	}
	msg, err := r.terminationMessage(ctx, &existing)
	if err != nil {
		return jobOutcome{}, err
	}
	if failed && msg == "" {
		for _, c := range existing.Status.Conditions {
			if c.Type == batchv1.JobFailed {
				msg = c.Reason + ": " + c.Message
			}
		}
	}
	return jobOutcome{done: complete, failed: failed, message: msg}, nil
}

func (r *PatchBuildReconciler) terminationMessage(ctx context.Context, job *batchv1.Job) (string, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(job.Namespace), client.MatchingLabels{"batch.kubernetes.io/job-name": job.Name}); err != nil {
		return "", err
	}
	for _, p := range pods.Items {
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Name == "worker" && cs.State.Terminated != nil {
				return strings.TrimSpace(cs.State.Terminated.Message), nil
			}
		}
	}
	return "", nil
}
