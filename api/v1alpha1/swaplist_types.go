package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SwapEntry records the patch selection for one source reference in a namespace.
type SwapEntry struct {
	// Source is the canonical source reference, such as index.docker.io/library/nginx:1.25.3.
	Source string `json:"source"`
	// Aliases are every image string that means Source. Each one is a key in spec.images.
	Aliases []string `json:"aliases"`
	// SourceDigest is the source digest the selected patch was built from.
	SourceDigest string `json:"sourceDigest"`
	// Fallback is the source pinned by digest. Recovery from a first patch restores this.
	Fallback string `json:"fallback"`
	// Selected is the patch admission applies now. Nil means the fallback is selected.
	// +optional
	Selected *PatchRef `json:"selected,omitempty"`
	// Previous is the patch that was selected before Selected.
	// +optional
	Previous *PatchRef `json:"previous,omitempty"`
	// Rollout is the PatchRollout currently changing this entry.
	// +optional
	Rollout string `json:"rollout,omitempty"`
}

// SwapListSpec is read by admission. Only the controller writes it.
type SwapListSpec struct {
	// Images maps each requested image string to the image to run. Admission reads only this field.
	// +optional
	Images map[string]string `json:"images,omitempty"`
	// Entries explain Images and keep rollback state.
	// +optional
	Entries []SwapEntry `json:"entries,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=swap
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SwapList records which patched image admission applies for each source image in a namespace.
type SwapList struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec SwapListSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// SwapListList contains a list of SwapList.
type SwapListList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SwapList `json:"items"`
}

// Entry returns the entry for a canonical source, or nil.
func (s *SwapList) Entry(source string) *SwapEntry {
	for i := range s.Spec.Entries {
		if s.Spec.Entries[i].Source == source {
			return &s.Spec.Entries[i]
		}
	}
	return nil
}

// RebuildImages recomputes spec.images from the entries.
func (s *SwapList) RebuildImages() {
	images := map[string]string{}
	for _, e := range s.Spec.Entries {
		target := e.Fallback
		if e.Selected != nil {
			target = e.Selected.Ref
		}
		for _, a := range e.Aliases {
			images[a] = target
		}
	}
	s.Spec.Images = images
}

func init() {
	SchemeBuilder.Register(&SwapList{}, &SwapListList{})
}
