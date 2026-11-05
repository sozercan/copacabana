package controller

import (
	"context"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/sozercan/copacabana/api/v1alpha1"
	"github.com/sozercan/copacabana/internal/imageref"
)

// MissedSwapReconciler flags opted-in pods that ask for a swappable image but weren't swapped.
// Admission fails open, so this is how missed swaps become visible.
type MissedSwapReconciler struct {
	client.Client
	Recorder events.EventRecorder

	mu   sync.Mutex
	seen map[types.UID]bool
}

// SetupWithManager registers the reconciler for opted-in pods only.
func (r *MissedSwapReconciler) SetupWithManager(mgr ctrl.Manager) error {
	optedIn := predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetLabels()[v1alpha1.LabelWorkloadOptIn] == v1alpha1.OptInValue
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("missedswap").
		For(&corev1.Pod{}, builder.WithPredicates(optedIn, predicate.Funcs{
			UpdateFunc: func(event.UpdateEvent) bool { return false },
		})).
		WithOptions(controllerOptions(2)).
		Complete(r)
}

// Reconcile checks one newly created opted-in pod.
func (r *MissedSwapReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	var pod corev1.Pod
	if err := r.Get(ctx, req.NamespacedName, &pod); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	r.mu.Lock()
	if r.seen == nil {
		r.seen = map[types.UID]bool{}
	}
	if r.seen[pod.UID] {
		r.mu.Unlock()
		return ctrl.Result{}, nil
	}
	r.seen[pod.UID] = true
	if len(r.seen) > 50000 {
		r.seen = map[types.UID]bool{}
	}
	r.mu.Unlock()

	var sw v1alpha1.SwapList
	if err := r.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: v1alpha1.SwapListName}, &sw); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// A container still runs a source that has a selected patch, under any spelling (tag@digest, short names),
	// whatever the pod's annotations claim. Containers running a fallback don't count: their entry has no
	// selected patch, or the fallback pins a digest that isn't the entry's source reference.
	patched := map[string]bool{}
	for _, e := range sw.Spec.Entries {
		if e.Selected != nil {
			patched[e.Source] = true
		}
	}
	for _, c := range append(append([]corev1.Container{}, pod.Spec.InitContainers...), pod.Spec.Containers...) {
		p, err := imageref.Parse(c.Image)
		if err != nil {
			continue
		}
		if _, isKey := sw.Spec.Images[c.Image]; patched[p.Canonical()] || isKey && sw.Spec.Images[c.Image] != c.Image {
			missedSwaps.WithLabelValues(pod.Namespace).Inc()
			r.Recorder.Eventf(&pod, nil, corev1.EventTypeWarning, "MissedSwap", "Admission",
				"container %s runs %s, which has an approved patch, but admission didn't swap it", c.Name, c.Image)
		}
	}
	return ctrl.Result{}, nil
}
