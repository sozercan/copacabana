package controller

import (
	"encoding/json"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/sozercan/copacabana/api/v1alpha1"
	"github.com/sozercan/copacabana/internal/imageref"
	"github.com/sozercan/copacabana/internal/worker"
)

// Service accounts and objects the Jobs use. Each step has its own identity.
const (
	scanServiceAccount   = "copa-scan"
	buildServiceAccount  = "copa-build"
	verifyServiceAccount = "copa-verify"
	buildkitConfigMap    = "buildkitd-config"
	signingKeySecret     = "copa-signing-key"
	builderPoolLabel     = "copa.sh/pool"
	builderPoolValue     = "builder"
	builderTaint         = "copa.sh/builder"
	workerUID            = 65532
	buildkitUID          = 1000
)

func platformSlug(p string) string { return strings.ReplaceAll(p, "/", "-") }

func jobName(pb *v1alpha1.PatchBuild, stage, platform string) string {
	parts := []string{stage}
	if platform != "" {
		parts = append(parts, platformSlug(platform))
	}
	return imageref.ObjectName(pb.Namespace+"/"+pb.Name+"/"+stage+"/"+platform, parts...)
}

func (r *PatchBuildReconciler) workerContainer(args []string, extraEnv ...corev1.EnvVar) corev1.Container {
	env := []corev1.EnvVar{
		{Name: "COPA_INSECURE_REGISTRIES", Value: strings.Join(r.Config.InsecureRegistries, ",")},
		{Name: "COPA_TRIVY_SERVER", Value: r.Config.TrivyServer},
		{Name: "HOME", Value: "/tmp"},
		{Name: "TRIVY_CACHE_DIR", Value: "/tmp/trivy"},
	}
	return corev1.Container{
		Name:                     "worker",
		Image:                    r.Config.WorkerImage,
		Command:                  []string{"/usr/local/bin/copa-worker"},
		Args:                     args,
		Env:                      append(env, extraEnv...),
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		SecurityContext: &corev1.SecurityContext{
			RunAsNonRoot:             ptr.To(true),
			RunAsUser:                ptr.To[int64](workerUID),
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		VolumeMounts: []corev1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}, {Name: "work", MountPath: "/work"}},
	}
}

func (r *PatchBuildReconciler) baseJob(pb *v1alpha1.PatchBuild, name, namespace, sa, stage string, onBuilderPool bool) *batchv1.Job {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				v1alpha1.LabelPatchBuild: pb.Name,
				"copa.sh/stage":          stage,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To[int32](0),
			ActiveDeadlineSeconds:   ptr.To(int64(r.Config.BuildTimeout.Seconds())),
			TTLSecondsAfterFinished: ptr.To[int32](3600),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{v1alpha1.LabelPatchBuild: pb.Name, "copa.sh/stage": stage}},
				Spec: corev1.PodSpec{
					ServiceAccountName:           sa,
					AutomountServiceAccountToken: ptr.To(false),
					RestartPolicy:                corev1.RestartPolicyNever,
					EnableServiceLinks:           ptr.To(false),
					Volumes: []corev1.Volume{
						{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					},
				},
			},
		},
	}
	if onBuilderPool {
		job.Spec.Template.Spec.NodeSelector = map[string]string{builderPoolLabel: builderPoolValue}
		job.Spec.Template.Spec.Tolerations = []corev1.Toleration{{Key: builderTaint, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}}
	}
	return job
}

func (r *PatchBuildReconciler) scanJob(pb *v1alpha1.PatchBuild, stage, platform, image, reportRepo string) *batchv1.Job {
	job := r.baseJob(pb, jobName(pb, stage, platform), r.Config.BuilderNamespace, scanServiceAccount, stage, true)
	args := []string{"scan", "--image", image}
	if reportRepo != "" {
		args = append(args, "--report-repo", reportRepo)
	}
	job.Spec.Template.Spec.Containers = []corev1.Container{r.workerContainer(args)}
	return job
}

func (r *PatchBuildReconciler) buildJob(pb *v1alpha1.PatchBuild, platform, source, reportRef, target string) *batchv1.Job {
	job := r.baseJob(pb, jobName(pb, "build", platform), r.Config.BuilderNamespace, buildServiceAccount, "build", true)
	spec := &job.Spec.Template.Spec
	spec.NodeSelector["kubernetes.io/arch"] = strings.Split(platform, "/")[1]
	spec.Volumes = append(spec.Volumes,
		corev1.Volume{Name: "buildkit-run", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		corev1.Volume{Name: "buildkit-state", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		corev1.Volume{Name: "buildkit-config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: buildkitConfigMap},
		}}},
	)
	// BuildKit runs as a native sidecar with no identity. Package scripts from the image run inside its sandbox.
	spec.InitContainers = []corev1.Container{{
		Name:          "buildkitd",
		Image:         r.Config.BuildKitImage,
		RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways),
		Args: []string{
			"--addr", "unix:///run/buildkit/buildkitd.sock",
			"--oci-worker-no-process-sandbox",
			"--config", "/etc/buildkit/buildkitd.toml",
		},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:      ptr.To[int64](buildkitUID),
			RunAsGroup:     ptr.To[int64](buildkitUID),
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
			AppArmorProfile: &corev1.AppArmorProfile{
				Type: corev1.AppArmorProfileTypeUnconfined,
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "buildkit-run", MountPath: "/run/buildkit"},
			{Name: "buildkit-state", MountPath: "/home/user/.local/share/buildkit"},
			{Name: "buildkit-config", MountPath: "/etc/buildkit", ReadOnly: true},
		},
	}}
	c := r.workerContainer([]string{
		"build", "--source", source, "--report", reportRef, "--target", target,
		"--buildkit", "unix:///run/buildkit/buildkitd.sock", "--timeout", r.Config.BuildTimeout.String(),
	})
	// Copa talks to BuildKit over the shared socket, so it must run as the same user as rootless BuildKit.
	c.SecurityContext.RunAsUser = ptr.To[int64](buildkitUID)
	c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "buildkit-run", MountPath: "/run/buildkit"})
	spec.Containers = []corev1.Container{c}
	return job
}

func (r *PatchBuildReconciler) verifyJob(pb *v1alpha1.PatchBuild, vs worker.VerifySpec) (*batchv1.Job, error) {
	raw, err := json.Marshal(vs)
	if err != nil {
		return nil, err
	}
	job := r.baseJob(pb, jobName(pb, "verify", ""), r.Config.VerifierNamespace, verifyServiceAccount, "verify", false)
	spec := &job.Spec.Template.Spec
	spec.Volumes = append(spec.Volumes, corev1.Volume{Name: "signing-key", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
		SecretName: signingKeySecret, DefaultMode: ptr.To[int32](0o440),
	}}})
	spec.SecurityContext = &corev1.PodSecurityContext{FSGroup: ptr.To[int64](workerUID)}
	c := r.workerContainer([]string{"verify", "--key", "/keys/signing.key"}, corev1.EnvVar{Name: "COPA_VERIFY_SPEC", Value: string(raw)})
	c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "signing-key", MountPath: "/keys", ReadOnly: true})
	spec.Containers = []corev1.Container{c}
	return job, nil
}
