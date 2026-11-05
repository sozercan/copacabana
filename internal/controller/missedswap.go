package controller

import (
	"context"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	seen := r.seen[pod.UID]
	r.mu.Unlock()
	if seen {
		return ctrl.Result{}, nil
	}

	var sw v1alpha1.SwapList
	if err := r.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: v1alpha1.SwapListName}, &sw); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err // retried; the pod isn't marked seen yet
		}
	}
	r.mu.Lock()
	if r.seen == nil || len(r.seen) > 50000 {
		r.seen = map[types.UID]bool{}
	}
	r.seen[pod.UID] = true
	r.mu.Unlock()
	if sw.Name == "" {
		return ctrl.Result{}, nil
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
	swapped := swappedImages(pod.Annotations[v1alpha1.AnnotationSwapped])
	for _, c := range append(append([]corev1.Container{}, pod.Spec.InitContainers...), pod.Spec.Containers...) {
		// Admission swapped this container to what the SwapList maps its original image to. That target can
		// itself be a key with a patch selected (a tag recovered to its pinned source digest, which another
		// workload has a patch for), and admission rightly doesn't swap twice. An annotation copied from a stale
		// template doesn't pass this check: its recorded original doesn't map to the container's image.
		if orig, ok := swapped[c.Name]; ok && orig != c.Image && sw.Spec.Images[orig] == c.Image {
			continue
		}
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

// swappedImages parses the copa.sh/swapped annotation (name=original,...) into container name -> original image.
func swappedImages(v string) map[string]string {
	out := map[string]string{}
	for _, item := range strings.Split(v, ",") {
		if name, image, ok := strings.Cut(item, "="); ok {
			out[name] = image
		}
	}
	return out
}
