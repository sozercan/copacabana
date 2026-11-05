package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SourceImageSpec identifies one source digest that opted-in workloads use.
type SourceImageSpec struct {
	// Repository is the canonical repository, such as index.docker.io/library/nginx.
	Repository string `json:"repository"`
	// Digest of the source index or manifest.
	Digest string `json:"digest"`
	// Platforms of the source that the cluster runs, with their manifest digests. Empty when the source has
	// none of the cluster's platforms; the SourceImage then records a gap instead of building.
	// +optional
	Platforms []PlatformDigest `json:"platforms,omitempty"`
}

// SourceImageStatus records who uses the source and what patch exists for it.
type SourceImageStatus struct {
	// References are the canonical tag or digest references that resolve to this digest.
	// +optional
	References []string `json:"references,omitempty"`
	// FloatingReferences resolve to this digest but use tags upstream moves, such as latest or 1.25. They aren't swapped.
	// +optional
	FloatingReferences []string `json:"floatingReferences,omitempty"`
	// UsedBy lists opted-in workloads whose templates ask for this source.
	// +optional
	UsedBy []WorkloadRef `json:"usedBy,omitempty"`
	// UsedByCount is len(UsedBy), for printing.
	// +optional
	UsedByCount int `json:"usedByCount,omitempty"`
	// Current is the latest verified patch for this source.
	// +optional
	Current *PatchRef `json:"current,omitempty"`
	// Fixable is the number of fixable OS vulnerabilities in the latest scan of the source or current patch.
	// +optional
	Fixable *int `json:"fixable,omitempty"`
	// Gap explains why this source can't be patched, if it can't.
	// +optional
	Gap string `json:"gap,omitempty"`
	// LastResolved is when the controller last saw a template resolve to this digest.
	// +optional
	LastResolved *metav1.Time `json:"lastResolved,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=srcimg
// +kubebuilder:printcolumn:name="Repository",type=string,JSONPath=`.spec.repository`
// +kubebuilder:printcolumn:name="Reference",type=string,JSONPath=`.status.references[0]`,priority=1
// +kubebuilder:printcolumn:name="Fixable",type=integer,JSONPath=`.status.fixable`
// +kubebuilder:printcolumn:name="Patch",type=string,JSONPath=`.status.current.tag`
// +kubebuilder:printcolumn:name="Users",type=integer,JSONPath=`.status.usedByCount`,priority=1
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SourceImage is one source digest in use by opted-in workloads.
type SourceImage struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SourceImageSpec   `json:"spec,omitempty"`
	Status SourceImageStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SourceImageList contains a list of SourceImage.
type SourceImageList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SourceImage `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SourceImage{}, &SourceImageList{})
}
