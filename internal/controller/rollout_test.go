package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/sozercan/copa-controller/api/v1alpha1"
)

const candidateDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

func newRolloutReconciler(t *testing.T, objs ...client.Object) *RolloutReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return &RolloutReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(),
		Config: &Config{RequireSafeStrategy: true},
	}
}

func optedIn(name, uid string) *appsv1.Deployment {
	zero, one := intstr.FromInt32(0), intstr.FromInt32(1)
	d := deployment(zero, one, true)
	d.Name, d.Namespace, d.UID = name, "team", types.UID(uid)
	d.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}}
	d.Spec.Template.Labels = map[string]string{"app": name, v1alpha1.LabelWorkloadOptIn: v1alpha1.OptInValue}
	return d
}

func rollout() *v1alpha1.PatchRollout {
	return &v1alpha1.PatchRollout{
		ObjectMeta: metav1.ObjectMeta{Name: "ro", Namespace: "team"},
		Spec:       v1alpha1.PatchRolloutSpec{Source: src, Candidate: v1alpha1.PatchRef{Digest: candidateDigest}},
	}
}

// Deployments created after the rollout started are added on later passes; tracked ones keep their state.
func TestDiscoverMergesNewDeployments(t *testing.T) {
	ctx := context.Background()
	first := optedIn("first", "u1")
	r := newRolloutReconciler(t, first)
	ro := rollout()
	if err := r.discover(ctx, ro); err != nil {
		t.Fatal(err)
	}
	if len(ro.Status.Workloads) != 1 {
		t.Fatalf("want 1 workload, got %+v", ro.Status.Workloads)
	}
	ro.Status.Workloads[0].Phase = v1alpha1.WorkloadHealthy

	notOptedIn := optedIn("other", "u3")
	delete(notOptedIn.Spec.Template.Labels, v1alpha1.LabelWorkloadOptIn)
	for _, d := range []*appsv1.Deployment{optedIn("late", "u2"), notOptedIn} {
		if err := r.Create(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.discover(ctx, ro); err != nil {
		t.Fatal(err)
	}
	got := map[string]v1alpha1.WorkloadPhase{}
	for _, w := range ro.Status.Workloads {
		got[w.Name] = w.Phase
	}
	want := map[string]v1alpha1.WorkloadPhase{"first": v1alpha1.WorkloadHealthy, "late": v1alpha1.WorkloadPending}
	if len(got) != len(want) || got["first"] != want["first"] || got["late"] != want["late"] {
		t.Fatalf("workloads = %v, want %v", got, want)
	}
}

// An opted-in Deployment the rollout never tracked can still back the patch out.
func TestFailRequestedByUntrackedDeployment(t *testing.T) {
	ctx := context.Background()
	tracked, late := optedIn("tracked", "u1"), optedIn("late", "u2")
	r := newRolloutReconciler(t, tracked, late)
	ro := rollout()
	ro.Status.Workloads = []v1alpha1.RolloutWorkload{{Name: "tracked", UID: "u1", Phase: v1alpha1.WorkloadHealthy}}

	if who, err := r.failRequested(ctx, ro); err != nil || who != "" {
		t.Fatalf("no annotation: got %q, %v", who, err)
	}
	late.Annotations = map[string]string{v1alpha1.AnnotationFailRollout: "some-other-rollout"}
	if err := r.Update(ctx, late); err != nil {
		t.Fatal(err)
	}
	if who, _ := r.failRequested(ctx, ro); who != "" {
		t.Fatalf("annotation for another rollout should be ignored, got %q", who)
	}
	late.Annotations[v1alpha1.AnnotationFailRollout] = "true"
	if err := r.Update(ctx, late); err != nil {
		t.Fatal(err)
	}
	if who, _ := r.failRequested(ctx, ro); who != "deployment/late" {
		t.Fatalf("got %q, want deployment/late", who)
	}
}

// Recovery restarts a Deployment that was never restarted only if admission gave its pods the candidate.
func TestRunsDigest(t *testing.T) {
	ctx := context.Background()
	d := optedIn("web", "u1")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "team", Labels: map[string]string{"app": "web"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "reg/patched/nginx@" + candidateDigest}}},
	}
	r := newRolloutReconciler(t, d, pod)
	if ok, err := r.runsDigest(ctx, "team", "web", candidateDigest); err != nil || !ok {
		t.Fatalf("want true, got %v %v", ok, err)
	}
	if ok, _ := r.runsDigest(ctx, "team", "web", "sha256:2222"); ok {
		t.Fatal("other digest should not match")
	}
	if ok, err := r.runsDigest(ctx, "team", "missing", candidateDigest); err != nil || ok {
		t.Fatalf("missing deployment: got %v %v", ok, err)
	}
}
