package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/sozercan/copacabana/api/v1alpha1"
)

const (
	msTag     = "kind-registry:5000/library/nginx:1.25.3-alpine"
	msPinned  = "kind-registry:5000/library/nginx@sha256:f2802c2a9d09c7aa3ace27445dfc5656ff24355da28e7b958074a0111e3fc076"
	msPatched = "registry.copa-registry.svc.cluster.local:5000/patched/kind-registry-5000/library/nginx@sha256:86ad5d992dc8a72796f3a0856128ed4cc38a8a6661d0676c05431984e778d853"
)

// The tag was recovered to its pinned source, while another workload references the pinned source directly and
// has a patch selected for it.
func recoveredSwapList() *v1alpha1.SwapList {
	return &v1alpha1.SwapList{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.SwapListName, Namespace: "team"},
		Spec: v1alpha1.SwapListSpec{
			Images: map[string]string{msTag: msPinned, msPinned: msPatched},
			Entries: []v1alpha1.SwapEntry{
				{Source: msTag, Aliases: []string{msTag}, Fallback: msPinned},
				{Source: msPinned, Aliases: []string{msPinned}, Fallback: msPinned, Selected: &v1alpha1.PatchRef{Ref: msPatched}},
			},
		},
	}
}

func msPod(name, image, swapped string) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team", UID: types.UID(name)},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: image}}},
	}
	if swapped != "" {
		p.Annotations = map[string]string{v1alpha1.AnnotationSwapped: swapped}
	}
	return p
}

func missedSwapEvents(t *testing.T, c client.Client, pod string) []string {
	t.Helper()
	rec := events.NewFakeRecorder(10)
	r := &MissedSwapReconciler{Client: c, Recorder: rec}
	req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "team", Name: pod}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	close(rec.Events)
	var out []string
	for e := range rec.Events {
		out = append(out, e)
	}
	return out
}

func msScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMissedSwap(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pod    *corev1.Pod
		missed bool
	}{
		{"tag swapped to its recovered pinned source", msPod("recovered", msPinned, "web="+msTag), false},
		{"pinned source not swapped", msPod("direct", msPinned, ""), true},
		{"stale annotation copied from a template", msPod("stale", msPinned, "web="+msPinned), true},
		{"patched", msPod("patched", msPatched, "web="+msPinned), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(msScheme(t)).WithObjects(recoveredSwapList(), tc.pod).Build()
			got := missedSwapEvents(t, c, tc.pod.Name)
			if missed := len(got) > 0; missed != tc.missed {
				t.Fatalf("events = %v, want missed=%v", got, tc.missed)
			}
		})
	}
}

// A transient SwapList read failure is retried instead of marking the pod checked.
func TestMissedSwapRetriesTransientErrors(t *testing.T) {
	failed := false
	c := fake.NewClientBuilder().WithScheme(msScheme(t)).WithObjects(recoveredSwapList(), msPod("direct", msPinned, "")).
		WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*v1alpha1.SwapList); ok && !failed {
				failed = true
				return errors.New("apiserver unavailable")
			}
			return c.Get(ctx, key, obj, opts...)
		}}).Build()
	got := missedSwapEvents(t, c, "direct")
	if !failed || len(got) != 1 || !strings.Contains(got[0], "MissedSwap") {
		t.Fatalf("events after retry = %v", got)
	}
}
