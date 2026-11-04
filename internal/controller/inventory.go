package controller

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
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
	"github.com/sozercan/copacabana/internal/registry"
)

const gapNoPlatforms = "source has none of the required platforms"

var inventoryKey = reconcile.Request{NamespacedName: types.NamespacedName{Name: "inventory"}}

// InventoryReconciler finds the images opted-in Deployments ask for, resolves them to digests,
// and keeps one SourceImage per digest.
type InventoryReconciler struct {
	client.Client
	Config   *Config
	Registry *registry.Client
	Recorder events.EventRecorder

	mu    sync.Mutex
	cache map[string]cachedResolve
}

type cachedResolve struct {
	at  time.Time
	res *registry.Resolved
	err error
}

// SetupWithManager registers the reconciler. Every Deployment or Namespace change triggers one inventory pass.
func (r *InventoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	toInventory := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{inventoryKey}
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("inventory").
		Watches(&appsv1.Deployment{}, toInventory).
		Watches(&corev1.Namespace{}, toInventory).
		WithOptions(controllerOptions(1)).
		Complete(r)
}

type usage struct {
	parsed    imageref.Parsed
	workloads []v1alpha1.WorkloadRef
}

// Reconcile rebuilds the inventory.
func (r *InventoryReconciler) Reconcile(ctx context.Context, _ reconcile.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var nsList corev1.NamespaceList
	if err := r.List(ctx, &nsList, client.MatchingLabels{v1alpha1.LabelNamespaceOptIn: v1alpha1.OptInValue}); err != nil {
		return ctrl.Result{}, err
	}

	// canonical reference -> usage
	uses := map[string]*usage{}
	for _, ns := range nsList.Items {
		var deps appsv1.DeploymentList
		if err := r.List(ctx, &deps, client.InNamespace(ns.Name)); err != nil {
			return ctrl.Result{}, err
		}
		for i := range deps.Items {
			d := &deps.Items[i]
			if d.Spec.Template.Labels[v1alpha1.LabelWorkloadOptIn] != v1alpha1.OptInValue {
				continue
			}
			for _, img := range templateImages(&d.Spec.Template.Spec) {
				p, err := imageref.Parse(img)
				if err != nil {
					r.Recorder.Eventf(d, nil, corev1.EventTypeWarning, "InvalidImage", "Inventory", "%v", err)
					continue
				}
				key := p.Canonical()
				u, ok := uses[key]
				if !ok {
					u = &usage{parsed: p}
					uses[key] = u
				}
				u.workloads = append(u.workloads, v1alpha1.WorkloadRef{Namespace: d.Namespace, Name: d.Name, Reference: img})
			}
		}
	}

	// Resolve each reference and group by digest.
	type group struct {
		repo      string
		res       *registry.Resolved
		refs      []string
		floating  []string
		workloads []v1alpha1.WorkloadRef
	}
	groups := map[string]*group{}
	for key, u := range uses {
		res, err := r.resolve(ctx, key)
		if err != nil {
			log.Info("can't resolve image", "reference", key, "error", err.Error())
			continue
		}
		gk := u.parsed.Repository + "@" + res.Digest
		g, ok := groups[gk]
		if !ok {
			g = &group{repo: u.parsed.Repository, res: res}
			groups[gk] = g
		}
		if imageref.FloatingTag(u.parsed) {
			g.floating = append(g.floating, key)
		} else {
			g.refs = append(g.refs, key)
		}
		g.workloads = append(g.workloads, u.workloads...)
	}

	// One bad source must not stop the others from being recorded, so errors are collected and returned at the end.
	var errs []error
	now := metav1.Now()
	seen := map[string]bool{}
	for gk, g := range groups {
		name := imageref.ObjectName(gk, path.Base(g.repo))
		seen[name] = true
		var si v1alpha1.SourceImage
		err := r.Get(ctx, client.ObjectKey{Name: name}, &si)
		if apierrors.IsNotFound(err) {
			si = v1alpha1.SourceImage{
				ObjectMeta: metav1.ObjectMeta{Name: name},
				Spec:       v1alpha1.SourceImageSpec{Repository: g.repo, Digest: g.res.Digest, Platforms: g.res.Platforms},
			}
			if err := r.Create(ctx, &si); err != nil {
				errs = append(errs, fmt.Errorf("creating SourceImage %s: %w", name, err))
				continue
			}
			log.Info("new source image", "name", name, "repository", g.repo, "digest", g.res.Digest)
		} else if err != nil {
			errs = append(errs, err)
			continue
		}
		sort.Strings(g.refs)
		sort.Strings(g.floating)
		sort.Slice(g.workloads, func(i, j int) bool {
			a, b := g.workloads[i], g.workloads[j]
			return a.Namespace+"/"+a.Name+a.Reference < b.Namespace+"/"+b.Name+b.Reference
		})
		patch := client.MergeFrom(si.DeepCopy())
		si.Status.References = g.refs
		si.Status.FloatingReferences = g.floating
		si.Status.UsedBy = g.workloads
		si.Status.UsedByCount = len(g.workloads)
		si.Status.LastResolved = &now
		if len(g.res.Platforms) == 0 {
			si.Status.Gap = fmt.Sprintf("%s %v (it has %v)", gapNoPlatforms, r.Config.Platforms, g.res.Available)
		} else if strings.HasPrefix(si.Status.Gap, gapNoPlatforms) {
			si.Status.Gap = ""
		}
		if err := r.Status().Patch(ctx, &si, patch); err != nil {
			errs = append(errs, fmt.Errorf("updating SourceImage %s: %w", name, err))
		}
	}

	// Source images nothing uses anymore keep their record but lose their users.
	var all v1alpha1.SourceImageList
	if err := r.List(ctx, &all); err != nil {
		return ctrl.Result{}, err
	}
	for i := range all.Items {
		si := &all.Items[i]
		if seen[si.Name] || len(si.Status.UsedBy) == 0 && len(si.Status.References) == 0 {
			continue
		}
		patch := client.MergeFrom(si.DeepCopy())
		si.Status.UsedBy = nil
		si.Status.UsedByCount = 0
		si.Status.References = nil
		si.Status.FloatingReferences = nil
		meta.SetStatusCondition(&si.Status.Conditions, metav1.Condition{
			Type: "Ready", Status: metav1.ConditionFalse, Reason: "Unused",
			Message: "no opted-in workload asks for this digest; swap-list entries are kept until removed",
		})
		if err := r.Status().Patch(ctx, si, patch); err != nil {
			errs = append(errs, fmt.Errorf("updating SourceImage %s: %w", si.Name, err))
		}
	}
	if len(errs) > 0 {
		return ctrl.Result{}, errors.Join(errs...)
	}
	return ctrl.Result{RequeueAfter: r.Config.ResolveInterval}, nil
}

// resolve resolves a canonical reference, caching results for ResolveInterval so tag drift is noticed.
func (r *InventoryReconciler) resolve(ctx context.Context, ref string) (*registry.Resolved, error) {
	r.mu.Lock()
	if r.cache == nil {
		r.cache = map[string]cachedResolve{}
	}
	c, ok := r.cache[ref]
	r.mu.Unlock()
	if ok && time.Since(c.at) < r.Config.ResolveInterval {
		return c.res, c.err
	}
	res, err := r.Registry.Resolve(ctx, ref, r.Config.Platforms)
	r.mu.Lock()
	r.cache[ref] = cachedResolve{at: time.Now(), res: res, err: err}
	r.mu.Unlock()
	return res, err
}

// templateImages returns the images of a pod template's containers and init containers.
func templateImages(spec *corev1.PodSpec) []string {
	var out []string
	for _, c := range spec.InitContainers {
		out = append(out, c.Image)
	}
	for _, c := range spec.Containers {
		out = append(out, c.Image)
	}
	slices.Sort(out)
	return slices.Compact(out)
}
