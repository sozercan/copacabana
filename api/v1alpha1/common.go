package v1alpha1

// Well-known labels and annotations.
const (
	// LabelNamespaceOptIn opts a namespace in to patching.
	LabelNamespaceOptIn = "copa.sh/patching"
	// LabelWorkloadOptIn opts a pod template in to image swaps.
	LabelWorkloadOptIn = "copa.sh/rollout"
	// LabelApproval sets the approval mode for a namespace's rollout group: "automatic" or "manual" (default).
	LabelApproval = "copa.sh/approval"
	// LabelSourceImage links a PatchBuild to its SourceImage.
	LabelSourceImage = "copa.sh/source-image"
	// LabelPatchBuild links a Job to its PatchBuild.
	LabelPatchBuild = "copa.sh/patchbuild"

	// AnnotationSwapped is written by admission on every pod it changes.
	AnnotationSwapped = "copa.sh/swapped"
	// AnnotationSwapListGeneration is written by admission with the SwapList generation it used.
	AnnotationSwapListGeneration = "copa.sh/swaplist-generation"
	// AnnotationFailRollout lets a team fail the current rollout for its group during an incident.
	AnnotationFailRollout = "copa.sh/fail-rollout"
	// AnnotationRestartedAt is the annotation kubectl rollout restart uses.
	AnnotationRestartedAt = "kubectl.kubernetes.io/restartedAt"

	// IndexAnnotationSourceDigest records the source digest on a patched index.
	IndexAnnotationSourceDigest = "copa.sh/source-digest"
	// IndexAnnotationSourceRef records the source reference on a patched index.
	IndexAnnotationSourceRef = "copa.sh/source-ref"
	// IndexAnnotationRevision records the patch revision on a patched index.
	IndexAnnotationRevision = "copa.sh/revision"

	// OptInValue is the label value that opts something in.
	OptInValue = "enabled"
	// SwapListName is the fixed name of the per-namespace SwapList.
	SwapListName = "copa"
)

// PlatformDigest pairs a platform with an image manifest digest.
type PlatformDigest struct {
	// Platform such as linux/amd64.
	Platform string `json:"platform"`
	// Digest of the platform-specific image manifest.
	Digest string `json:"digest"`
}

// PlatformPatch records what one platform of a patch was built from.
type PlatformPatch struct {
	Platform      string `json:"platform"`
	SourceDigest  string `json:"sourceDigest"`
	PatchedDigest string `json:"patchedDigest"`
	// RemainingFixable is how many fixable vulnerabilities the post-patch scan still found.
	// +optional
	RemainingFixable int `json:"remainingFixable,omitempty"`
}

// PatchRef identifies a promoted, verified patch.
type PatchRef struct {
	// Ref is the pinned reference, repo@digest. Admission applies this.
	Ref string `json:"ref"`
	// Tag is the readable tag, such as 1.25.3-alpine-2.
	// +optional
	Tag string `json:"tag,omitempty"`
	// Digest of the patched index.
	Digest string `json:"digest"`
	// Revision number of this patch for its source tag.
	// +optional
	Revision int `json:"revision,omitempty"`
	// Platforms that were patched. Every platform in the index was patched.
	// +optional
	Platforms []PlatformPatch `json:"platforms,omitempty"`
}

// WorkloadRef points at a workload that uses a source image.
type WorkloadRef struct {
	// Kind is Deployment, StatefulSet, DaemonSet or CronJob. Empty means Deployment.
	// +optional
	Kind      string `json:"kind,omitempty"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// Reference is the image string as the template writes it.
	Reference string `json:"reference"`
}
