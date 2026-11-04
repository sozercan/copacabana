package controller

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"github.com/sozercan/copa-controller/api/v1alpha1"
)

const src = "index.docker.io/library/nginx:1.25.3"

func deployment(mu, ms intstr.IntOrString, probe bool) *appsv1.Deployment {
	c := corev1.Container{Name: "web", Image: "nginx:1.25.3"}
	if probe {
		c.ReadinessProbe = &corev1.Probe{}
	}
	return &appsv1.Deployment{Spec: appsv1.DeploymentSpec{
		Replicas: ptr.To[int32](2),
		Strategy: appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType,
			RollingUpdate: &appsv1.RollingUpdateDeployment{MaxUnavailable: &mu, MaxSurge: &ms}},
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{c}}},
	}}
}

func TestEligibility(t *testing.T) {
	zero, one := intstr.FromInt32(0), intstr.FromInt32(1)
	if r := eligibility(deployment(zero, one, true), src); r != "" {
		t.Errorf("safe deployment rejected: %s", r)
	}
	if r := eligibility(deployment(one, one, true), src); r == "" {
		t.Error("maxUnavailable 1 accepted")
	}
	if r := eligibility(deployment(zero, zero, true), src); r == "" {
		t.Error("maxSurge 0 accepted")
	}
	if r := eligibility(deployment(zero, one, false), src); r == "" {
		t.Error("missing readiness probe accepted")
	}
	d := deployment(zero, one, true)
	d.Spec.Strategy = appsv1.DeploymentStrategy{} // defaults: 25%/25% of 2 -> maxUnavailable rounds down to 0, surge up to 1
	if r := eligibility(d, src); r != "" {
		t.Errorf("default strategy with 2 replicas rejected: %s", r)
	}
}

func TestUsesSource(t *testing.T) {
	spec := &corev1.PodSpec{Containers: []corev1.Container{{Image: "docker.io/library/nginx:1.25.3"}}}
	if !usesSource(spec, src) {
		t.Error("alias not matched")
	}
	if usesSource(spec, "index.docker.io/library/nginx:1.25.4") {
		t.Error("different tag matched")
	}
}

func TestSwapListRebuild(t *testing.T) {
	sw := &v1alpha1.SwapList{Spec: v1alpha1.SwapListSpec{Entries: []v1alpha1.SwapEntry{
		{Source: src, Aliases: []string{"nginx:1.25.3", src}, Fallback: "index.docker.io/library/nginx@sha256:aa",
			Selected: &v1alpha1.PatchRef{Ref: "reg/patched/nginx@sha256:bb"}},
		{Source: "x", Aliases: []string{"x:1"}, Fallback: "x@sha256:cc"},
	}}}
	sw.RebuildImages()
	if sw.Spec.Images["nginx:1.25.3"] != "reg/patched/nginx@sha256:bb" || sw.Spec.Images[src] != "reg/patched/nginx@sha256:bb" {
		t.Errorf("selected not applied: %v", sw.Spec.Images)
	}
	if sw.Spec.Images["x:1"] != "x@sha256:cc" {
		t.Errorf("fallback not applied: %v", sw.Spec.Images)
	}
}

func TestRepoKeyAndBaseline(t *testing.T) {
	if got := RepoKey("index.docker.io/library/nginx"); got != "docker.io/library/nginx" {
		t.Errorf("RepoKey = %s", got)
	}
	if got := RepoKey("kind-registry:5000/library/nginx"); got != "kind-registry-5000/library/nginx" {
		t.Errorf("RepoKey = %s", got)
	}
	pb := &v1alpha1.PatchBuild{
		Spec:   v1alpha1.PatchBuildSpec{Baseline: &v1alpha1.PatchRef{Platforms: []v1alpha1.PlatformPatch{{Platform: "linux/amd64", RemainingFixable: 2}}}},
		Status: v1alpha1.PatchBuildStatus{Platforms: []v1alpha1.PlatformBuild{{Platform: "linux/amd64", BaselineFixable: ptr.To(2)}}},
	}
	if baselineNeedsBuild(pb) {
		t.Error("rebuild requested with no new fixes")
	}
	pb.Status.Platforms[0].BaselineFixable = ptr.To(3)
	if !baselineNeedsBuild(pb) {
		t.Error("no rebuild with a new fix")
	}
}
