package controller

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/sozercan/copacabana/api/v1alpha1"
	"github.com/sozercan/copacabana/internal/imageref"
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

// Admission matches exact strings, so the spelling a Deployment uses (here tag@digest) must be an alias.
func TestAliasesIncludeWorkloadSpellings(t *testing.T) {
	ctx := context.Background()
	d := optedIn("web", "u1")
	raw := "nginx:1.25.4-alpine@" + candidateDigest
	d.Spec.Template.Spec.Containers[0].Image = raw
	r := newRolloutReconciler(t, d)
	p, err := imageref.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.aliases(ctx, "team", p)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got, raw) || !slices.Contains(got, "nginx@"+candidateDigest) {
		t.Fatalf("aliases %v must include %q and the short digest form", got, raw)
	}
}

// Scaling changes the generation but not the template, so it isn't a team deploy. A template change is.
func TestTeamDeployedIgnoresScaling(t *testing.T) {
	d := optedIn("web", "u1")
	d.Generation = 3
	w := &v1alpha1.RolloutWorkload{Generation: 3, TemplateHash: templateHash(d)}
	d.Generation, d.Spec.Replicas = 4, ptr.To[int32](5)
	if teamDeployed(d, w) {
		t.Fatal("scaling counted as a team deploy")
	}
	d.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "X", Value: "1"}}
	if !teamDeployed(d, w) {
		t.Fatal("template change not detected")
	}
}

// Parallel reconciles must not hand out more restart slots than the limit, even before statuses are written.
func TestReserveRestartLimit(t *testing.T) {
	r := newRolloutReconciler(t)
	r.Config.MaxConcurrentRestarts = 2
	ctx := context.Background()
	got := 0
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, err := r.reserveRestart(ctx, fmt.Sprintf("u%d", i)); err == nil && ok {
				mu.Lock()
				got++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if got != 2 {
		t.Fatalf("granted %d restart slots, want 2", got)
	}
}
