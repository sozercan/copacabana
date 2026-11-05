package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BuildPhase is the phase of a PatchBuild.
type BuildPhase string

// PatchBuild phases.
const (
	BuildPending          BuildPhase = "Pending"
	BuildScanningBaseline BuildPhase = "ScanningBaseline"
	BuildScanningSource   BuildPhase = "ScanningSource"
	BuildBuilding         BuildPhase = "Building"
	BuildVerifying        BuildPhase = "Verifying"
	BuildPromoted         BuildPhase = "Promoted"
	BuildNothingToFix     BuildPhase = "NothingToFix"
	BuildScanned          BuildPhase = "Scanned"
	BuildFailed           BuildPhase = "Failed"
)

// Terminal reports whether the phase is final.
func (p BuildPhase) Terminal() bool {
	return p == BuildPromoted || p == BuildNothingToFix || p == BuildFailed || p == BuildScanned
}

// PatchBuildSpec describes one attempt to scan and patch a source digest.
type PatchBuildSpec struct {
	// SourceImage is the name of the SourceImage this build is for.
	SourceImage string `json:"sourceImage"`
	// Repository of the source, canonical.
	Repository string `json:"repository"`
	// SourceDigest of the source index or manifest.
	SourceDigest string `json:"sourceDigest"`
	// Reference is the tag reference used to name the patched image.
	Reference string `json:"reference"`
	// Platforms to patch, with their source manifest digests.
	Platforms []PlatformDigest `json:"platforms"`
	// Baseline is the current patch. If set, the build first rescans it, and only patches if it has new fixes.
	// +optional
	Baseline *PatchRef `json:"baseline,omitempty"`
	// ScanOnly stops after scanning the source. Used in report-only mode.
	// +optional
	ScanOnly bool `json:"scanOnly,omitempty"`
	// Reason the build was started: NewSource, Rescan, Manual, Forced or PlatformsChanged.
	// +optional
	Reason string `json:"reason,omitempty"`
}

// PlatformBuild tracks one platform of a build.
type PlatformBuild struct {
	Platform     string `json:"platform"`
	SourceDigest string `json:"sourceDigest"`
	// BaselineFixable is the fixable count in the baseline patch, if one was scanned.
	// +optional
	BaselineFixable *int `json:"baselineFixable,omitempty"`
	// ReportRef is the pinned reference of the source scan report artifact.
	// +optional
	ReportRef string `json:"reportRef,omitempty"`
	// Fixable is the number of fixable OS vulnerabilities in the source.
	// +optional
	Fixable *int `json:"fixable,omitempty"`
	// StagedRef is the pinned reference of the staged patched image.
	// +optional
	StagedRef string `json:"stagedRef,omitempty"`
	// VEXRef is the pinned reference of the VEX artifact for the staged image.
	// +optional
	VEXRef string `json:"vexRef,omitempty"`
}

// PatchBuildStatus is the observed state of a PatchBuild.
type PatchBuildStatus struct {
	// +optional
	Phase BuildPhase `json:"phase,omitempty"`
	// +optional
	Platforms []PlatformBuild `json:"platforms,omitempty"`
	// +optional
	Promoted *PatchRef `json:"promoted,omitempty"`
	// FailedStage is the stage that failed: scan, build or verify.
	// +optional
	FailedStage string `json:"failedStage,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pbuild
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.reference`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.spec.reason`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Patch",type=string,JSONPath=`.status.promoted.tag`
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.message`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PatchBuild is one attempt to scan, patch, verify and promote a source digest.
type PatchBuild struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PatchBuildSpec   `json:"spec,omitempty"`
	Status PatchBuildStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PatchBuildList contains a list of PatchBuild.
type PatchBuildList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PatchBuild `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PatchBuild{}, &PatchBuildList{})
}
