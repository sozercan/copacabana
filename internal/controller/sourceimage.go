package controller

import (
	"context"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/sozercan/copacabana/api/v1alpha1"
	"github.com/sozercan/copacabana/internal/imageref"
)

// AnnotationRebuild on a SourceImage asks for an immediate rescan and rebuild. The value "force" builds a new
// revision even when the current patch has no new fixes (after a Copa upgrade or a signing-key rotation, say).
const AnnotationRebuild = "copa.sh/rebuild"

// promotedVerifyGrace is how long scheduling waits for the catalog to verify a newly promoted patch.
const promotedVerifyGrace = 10 * time.Minute

// LabelSource holds a hash of the canonical source reference on PatchRollouts.
const LabelSource = "copa.sh/source"

// SourceImageReconciler finds verified patches for each source, schedules scans and builds in builder mode,
// and creates a PatchRollout in each namespace that uses the source when a newer patch exists.
type SourceImageReconciler struct {
	client.Client
	Config   *Config
	Catalog  *Catalog
	Recorder events.EventRecorder
}

// SetupWithManager registers the reconciler.
func (r *SourceImageReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.SourceImage{}).
		Watches(&v1alpha1.PatchBuild{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
			if n := o.GetLabels()[v1alpha1.LabelSourceImage]; n != "" {
				return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: n}}}
			}
			return nil
		})).
		Watches(&v1alpha1.PatchRollout{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
			if ro, ok := o.(*v1alpha1.PatchRollout); ok && ro.Status.Phase.Terminal() {
				return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: ro.Spec.SourceImage}}}
			}
			return nil
		})).
		WithOptions(controllerOptions(2))
	return b.Complete(r)
}

// Reconcile handles one SourceImage.
func (r *SourceImageReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	var si v1alpha1.SourceImage
	if err := r.Get(ctx, req.NamespacedName, &si); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	requeue := ctrl.Result{RequeueAfter: time.Minute}
	if len(si.Status.UsedBy) == 0 {
		return ctrl.Result{}, nil
	}
	orig := si.DeepCopy()
	cond := metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse}

	switch {
	case strings.HasPrefix(si.Status.Gap, gapNoPlatforms) || len(si.Spec.Platforms) == 0:
		cond.Reason, cond.Message = "Gap", si.Status.Gap
		if cond.Message == "" {
			cond.Message = gapNoPlatforms
		}
		return requeue, r.patchStatus(ctx, &si, orig, cond)
	case len(si.Status.References) == 0:
		cond.Reason = "NotEligible"
		cond.Message = fmt.Sprintf("only floating tags reference this digest: %s", strings.Join(si.Status.FloatingReferences, ", "))
		return requeue, r.patchStatus(ctx, &si, orig, cond)
	}

	// ref is the newest patch that verifies with the trusted key on this pass. Status.Current is only the last
	// one that did: after a signing-key rotation it no longer verifies, and mustn't be offered or rescanned.
	// Report-only mode may run without a key, and then has nothing to verify.
	var ref *v1alpha1.PatchRef
	var err error
	if len(r.Config.PublicKey) > 0 {
		ref, err = r.Catalog.Latest(ctx, si.Spec.Repository, si.Spec.Digest, si.Status.References[0], si.Spec.Platforms)
	}
	catalogErr := err
	switch {
	case err != nil && ref != nil:
		log.Info("ignored patched revisions that don't verify", "source", si.Name, "error", err.Error())
		r.Recorder.Eventf(&si, nil, corev1.EventTypeWarning, "UnverifiedPatch", "Catalog", "%s", err.Error())
	case err != nil:
		log.Info("patched image found but not verified", "source", si.Name, "error", err.Error())
	}
	if ref != nil && (si.Status.Current == nil || si.Status.Current.Digest != ref.Digest) {
		r.Recorder.Eventf(&si, nil, corev1.EventTypeNormal, "PatchVerified", "Catalog", "verified %s (%s)", ref.Tag, ref.Digest)
	}
	if ref != nil {
		si.Status.Current = ref
	} else if si.Status.Current != nil && !coversPlatforms(si.Status.Current, si.Spec.Platforms) {
		// The current patch no longer covers every platform the cluster runs: its index has no manifest for
		// the new platform, so pods scheduled there couldn't pull it. Stop offering it to new rollouts.
		r.Recorder.Eventf(&si, nil, corev1.EventTypeWarning, "PatchIncomplete", "Catalog",
			"%s doesn't cover %v; rebuilding", si.Status.Current.Tag, platformNames(si.Spec.Platforms))
		si.Status.Current = nil
	}

	building := false
	if r.Config.Mode == ModeBuilder || r.Config.Mode == ModeReportOnly {
		building, err = r.schedule(ctx, &si, ref)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	// New rollouts only for a patch verified on this pass.
	if r.Config.Mode != ModeReportOnly && ref != nil {
		if err := r.ensureRollouts(ctx, &si); err != nil {
			return ctrl.Result{}, err
		}
	}

	switch {
	case si.Status.Gap != "":
		cond.Reason, cond.Message = "Gap", si.Status.Gap
	case si.Status.Current != nil && ref == nil && catalogErr != nil:
		cond.Reason = "Unverified"
		cond.Message = trimMessage(fmt.Sprintf("%s no longer verifies; not offered to new namespaces: %v", si.Status.Current.Tag, catalogErr))
	case si.Status.Current != nil:
		cond.Status, cond.Reason = metav1.ConditionTrue, "Patched"
		cond.Message = si.Status.Current.Tag
		if building {
			cond.Message += "; rescanning"
		}
	case building:
		cond.Reason, cond.Message = "Building", "scan or build in progress"
	case si.Status.Fixable != nil && *si.Status.Fixable == 0:
		cond.Status, cond.Reason, cond.Message = metav1.ConditionTrue, "NoFixes", "no fixable OS vulnerabilities"
	default:
		cond.Reason, cond.Message = "Unpatched", "no verified patch yet"
	}
	return requeue, r.patchStatus(ctx, &si, orig, cond)
}

func (r *SourceImageReconciler) patchStatus(ctx context.Context, si, orig *v1alpha1.SourceImage, cond metav1.Condition) error {
	cond.ObservedGeneration = si.Generation
	meta.SetStatusCondition(&si.Status.Conditions, cond)
	return r.Status().Patch(ctx, si, client.MergeFrom(orig))
}

// schedule starts a PatchBuild when the source is new, the rescan interval passed, or a rebuild was requested.
// It returns whether a build is in progress.
// verified is the current patch if it verified on this pass, and is the baseline a rescan compares against.
func (r *SourceImageReconciler) schedule(ctx context.Context, si *v1alpha1.SourceImage, verified *v1alpha1.PatchRef) (bool, error) {
	var builds v1alpha1.PatchBuildList
	if err := r.List(ctx, &builds, client.InNamespace(r.Config.BuilderNamespace)); err != nil {
		return false, err
	}
	var mine []v1alpha1.PatchBuild
	active := 0
	for _, b := range builds.Items {
		if !b.Status.Phase.Terminal() {
			active++
		}
		if b.Labels[v1alpha1.LabelSourceImage] == si.Name {
			mine = append(mine, b)
		}
	}
	sort.Slice(mine, func(i, j int) bool { return mine[i].CreationTimestamp.After(mine[j].CreationTimestamp.Time) })

	rebuild := si.Annotations[AnnotationRebuild] != ""
	platformsChanged := false
	if len(mine) > 0 {
		last := &mine[0]
		if !last.Status.Phase.Terminal() {
			return true, nil
		}
		si.Status.Fixable = buildFixable(last)
		switch last.Status.Phase {
		case v1alpha1.BuildFailed:
			si.Status.Gap = fmt.Sprintf("%s failed: %s", last.Status.FailedStage, last.Status.Message)
		default:
			if !strings.HasPrefix(si.Status.Gap, gapNoPlatforms) {
				si.Status.Gap = ""
			}
		}
		// The source's platforms changed since the last build (the cluster now requires another platform, say):
		// build now rather than waiting for the rescan interval.
		platformsChanged = !slices.Equal(platformNames(last.Spec.Platforms), platformNames(si.Spec.Platforms))
		if !platformsChanged && last.Status.Phase == v1alpha1.BuildPromoted && last.Status.Promoted != nil &&
			(verified == nil || verified.Digest != last.Status.Promoted.Digest) {
			// Wait for the catalog to verify the promoted patch before deciding anything else, but not forever:
			// a promoted patch that never verifies (its tag overwritten, or signed with a key no longer trusted)
			// would otherwise block every later build.
			if finished := last.Status.CompletionTime; finished == nil || time.Since(finished.Time) < promotedVerifyGrace {
				return false, nil
			}
			si.Status.Gap = fmt.Sprintf("promoted %s doesn't verify", last.Status.Promoted.Tag)
		}
		if !rebuild && !platformsChanged && time.Since(last.CreationTimestamp.Time) < r.Config.RescanInterval {
			return false, nil
		}
	}
	if active >= r.Config.MaxConcurrentBuilds {
		return false, nil
	}

	reason := "NewSource"
	baseline := verified
	switch {
	case si.Annotations[AnnotationRebuild] == "force":
		reason, baseline = "Forced", nil
	case rebuild:
		reason = "Manual"
	case platformsChanged:
		reason = "PlatformsChanged"
	case len(mine) > 0:
		reason = "Rescan"
	}
	pb := &v1alpha1.PatchBuild{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: si.Name + "-",
			Namespace:    r.Config.BuilderNamespace,
			Labels:       map[string]string{v1alpha1.LabelSourceImage: si.Name},
		},
		Spec: v1alpha1.PatchBuildSpec{
			SourceImage:  si.Name,
			Repository:   si.Spec.Repository,
			SourceDigest: si.Spec.Digest,
			Reference:    si.Status.References[0],
			Platforms:    si.Spec.Platforms,
			Baseline:     baseline,
			Reason:       reason,
			ScanOnly:     r.Config.Mode == ModeReportOnly,
		},
	}
	if err := r.Create(ctx, pb); err != nil {
		return false, err
	}
	r.Recorder.Eventf(si, nil, corev1.EventTypeNormal, "BuildStarted", "Schedule", "%s: %s", reason, pb.Name)
	if rebuild {
		p := client.MergeFrom(si.DeepCopy())
		delete(si.Annotations, AnnotationRebuild)
		if err := r.Patch(ctx, si, p); err != nil {
			return true, err
		}
	}
	return true, nil
}

func buildFixable(b *v1alpha1.PatchBuild) *int {
	total, any := 0, false
	for _, p := range b.Status.Platforms {
		switch {
		case p.Fixable != nil:
			total += *p.Fixable
			any = true
		case p.BaselineFixable != nil:
			total += *p.BaselineFixable
			any = true
		}
	}
	if !any {
		return nil
	}
	return &total
}

// ensureRollouts creates a PatchRollout for each namespace and reference that doesn't run the current patch yet.
func (r *SourceImageReconciler) ensureRollouts(ctx context.Context, si *v1alpha1.SourceImage) error {
	eligible := map[string]bool{}
	for _, ref := range si.Status.References {
		eligible[ref] = true
	}
	byNS := map[string]map[string]bool{}
	for _, w := range si.Status.UsedBy {
		p, err := imageref.Parse(w.Reference)
		if err != nil || !eligible[p.Canonical()] {
			continue
		}
		if byNS[w.Namespace] == nil {
			byNS[w.Namespace] = map[string]bool{}
		}
		byNS[w.Namespace][p.Canonical()] = true
	}
	cur := si.Status.Current
	for ns, refs := range byNS {
		var sw v1alpha1.SwapList
		err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: v1alpha1.SwapListName}, &sw)
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		for ref := range refs {
			if e := sw.Entry(ref); e != nil && e.Selected != nil && e.Selected.Digest == cur.Digest {
				continue
			}
			var ros v1alpha1.PatchRolloutList
			if err := r.List(ctx, &ros, client.InNamespace(ns), client.MatchingLabels{LabelSource: imageref.ObjectName(ref)}); err != nil {
				return err
			}
			skip := false
			for _, ro := range ros.Items {
				// One rollout at a time per source, and never retry a candidate that already ran here.
				// Deleting a failed PatchRollout allows a retry.
				if !ro.Status.Phase.Terminal() || ro.Spec.Candidate.Digest == cur.Digest {
					skip = true
					break
				}
			}
			if skip {
				continue
			}
			ro := &v1alpha1.PatchRollout{
				ObjectMeta: metav1.ObjectMeta{
					Name:      imageref.ObjectName(ns+"/"+ref+"@"+cur.Digest, path.Base(si.Spec.Repository), fmt.Sprintf("r%d", cur.Revision)),
					Namespace: ns,
					Labels: map[string]string{
						LabelSource:               imageref.ObjectName(ref),
						v1alpha1.LabelSourceImage: si.Name,
					},
				},
				Spec: v1alpha1.PatchRolloutSpec{
					Source:       ref,
					SourceImage:  si.Name,
					SourceDigest: si.Spec.Digest,
					Candidate:    *cur,
				},
			}
			if err := r.Create(ctx, ro); err != nil && !apierrors.IsAlreadyExists(err) {
				return err
			}
			r.Recorder.Eventf(si, nil, corev1.EventTypeNormal, "RolloutCreated", "Candidate", "%s/%s for %s", ns, ro.Name, cur.Tag)
		}
	}
	return nil
}

func platformNames(ps []v1alpha1.PlatformDigest) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Platform)
	}
	slices.Sort(out)
	return out
}

func coversPlatforms(ref *v1alpha1.PatchRef, want []v1alpha1.PlatformDigest) bool {
	for _, w := range want {
		if !slices.ContainsFunc(ref.Platforms, func(p v1alpha1.PlatformPatch) bool { return p.Platform == w.Platform }) {
			return false
		}
	}
	return true
}
