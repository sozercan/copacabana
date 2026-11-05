package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/sozercan/copacabana/api/v1alpha1"
)

// podTemplate is an opted-in pod template of any workload kind the inventory understands.
type podTemplate struct {
	kind   string
	obj    client.Object
	labels map[string]string
	spec   *corev1.PodSpec
}

// optedInTemplates lists the pod templates in a namespace that carry the opt-in label. Deployments come first.
// Bare pods aren't included: admission already swapped their images, so their specs don't show the source.
func optedInTemplates(ctx context.Context, c client.Client, ns string) ([]podTemplate, error) {
	var out []podTemplate
	add := func(kind string, obj client.Object, t *corev1.PodTemplateSpec) {
		if t.Labels[v1alpha1.LabelWorkloadOptIn] == v1alpha1.OptInValue {
			out = append(out, podTemplate{kind: kind, obj: obj, labels: t.Labels, spec: &t.Spec})
		}
	}
	var deps appsv1.DeploymentList
	if err := c.List(ctx, &deps, client.InNamespace(ns)); err != nil {
		return nil, err
	}
	for i := range deps.Items {
		add("Deployment", &deps.Items[i], &deps.Items[i].Spec.Template)
	}
	var sts appsv1.StatefulSetList
	if err := c.List(ctx, &sts, client.InNamespace(ns)); err != nil {
		return nil, err
	}
	for i := range sts.Items {
		add("StatefulSet", &sts.Items[i], &sts.Items[i].Spec.Template)
	}
	var ds appsv1.DaemonSetList
	if err := c.List(ctx, &ds, client.InNamespace(ns)); err != nil {
		return nil, err
	}
	for i := range ds.Items {
		add("DaemonSet", &ds.Items[i], &ds.Items[i].Spec.Template)
	}
	var cjs batchv1.CronJobList
	if err := c.List(ctx, &cjs, client.InNamespace(ns)); err != nil {
		return nil, err
	}
	for i := range cjs.Items {
		add("CronJob", &cjs.Items[i], &cjs.Items[i].Spec.JobTemplate.Spec.Template)
	}
	return out, nil
}

// templateHash identifies a Deployment's pod template, so a team deploy can be told apart from scaling.
func templateHash(d *appsv1.Deployment) string {
	raw, _ := json.Marshal(d.Spec.Template)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}
