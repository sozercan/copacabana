package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RolloutPhase is the phase of a PatchRollout.
type RolloutPhase string

// PatchRollout phases.
const (
	RolloutPending          RolloutPhase = "Pending"
	RolloutAwaitingApproval RolloutPhase = "AwaitingApproval"
	RolloutSelecting        RolloutPhase = "Selecting"
	RolloutRestarting       RolloutPhase = "Restarting"
	RolloutSoaking          RolloutPhase = "Soaking"
	RolloutSucceeded        RolloutPhase = "Succeeded"
	RolloutRollingBack      RolloutPhase = "RollingBack"
	RolloutRolledBack       RolloutPhase = "RolledBack"
	RolloutRecoveryFailed   RolloutPhase = "RecoveryFailed"
	RolloutSuperseded       RolloutPhase = "Superseded"
)

// Terminal reports whether the phase is final. Succeeded is final but can still be failed by a team.
func (p RolloutPhase) Terminal() bool {
	switch p {
	case RolloutSucceeded, RolloutRolledBack, RolloutRecoveryFailed, RolloutSuperseded:
		return true
	}
	return false
}

// WorkloadPhase is the phase of one workload inside a rollout.
type WorkloadPhase string

// Workload phases.
const (
	WorkloadPending    WorkloadPhase = "Pending"
	WorkloadRestarting WorkloadPhase = "Restarting"
	WorkloadHealthy    WorkloadPhase = "Healthy"
	WorkloadFailed     WorkloadPhase = "Failed"
	WorkloadIneligible WorkloadPhase = "Ineligible"
	WorkloadSuperseded WorkloadPhase = "Superseded"
	WorkloadOptedOut   WorkloadPhase = "OptedOut"
)

// RolloutWorkload tracks one Deployment in a rollout.
type RolloutWorkload struct {
	// Kind is Deployment unless set. Only Deployments are restarted; other kinds are listed as Ineligible.
	// +optional
	Kind string `json:"kind,omitempty"`
	Name string `json:"name"`
	UID  string `json:"uid"`
	// Generation is the Deployment generation after the controller's restart. A higher generation means a team deployed.
	// +optional
	Generation int64 `json:"generation,omitempty"`
	// TemplateHash is a hash of the pod template after the controller's restart. A different template means a
	// team deployed; scaling (an HPA changing replicas) changes the generation but not the template.
	// +optional
	TemplateHash string `json:"templateHash,omitempty"`
	// RestartedAt is the value the controller set on the restart annotation.
	// +optional
	RestartedAt string `json:"restartedAt,omitempty"`
	// RestartBaseline is the total container restart count when the pods became ready.
	// +optional
	RestartBaseline *int32 `json:"restartBaseline,omitempty"`
	// ReadySince is when every new pod was ready and running the expected image.
	// +optional
	ReadySince *metav1.Time  `json:"readySince,omitempty"`
	Phase      WorkloadPhase `json:"phase"`
	// +optional
	Message string `json:"message,omitempty"`
}

// PatchRolloutSpec is the candidate for one source in one rollout group. Only the controller writes it.
type PatchRolloutSpec struct {
	// Source is the canonical source reference.
	Source string `json:"source"`
	// SourceImage is the name of the SourceImage the candidate was built from.
	SourceImage string `json:"sourceImage"`
	// SourceDigest the candidate was built from.
	SourceDigest string `json:"sourceDigest"`
	// Candidate is the patch to roll out.
	Candidate PatchRef `json:"candidate"`
}

// PatchRolloutStatus is the observed state of a PatchRollout.
type PatchRolloutStatus struct {
	// +optional
	Phase RolloutPhase `json:"phase,omitempty"`
	// ApprovedBy is the PatchApproval or policy that approved the candidate.
	// +optional
	ApprovedBy string `json:"approvedBy,omitempty"`
	// Restored is what recovery put back: a patch ref or the fallback.
	// +optional
	Restored string `json:"restored,omitempty"`
	// +optional
	Workloads []RolloutWorkload `json:"workloads,omitempty"`
	// PhaseSince is when the current phase started.
	// +optional
	PhaseSince *metav1.Time `json:"phaseSince,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=prollout
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.source`
// +kubebuilder:printcolumn:name="Candidate",type=string,JSONPath=`.spec.candidate.tag`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.message`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PatchRollout rolls one candidate patch out to one rollout group (namespace).
type PatchRollout struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PatchRolloutSpec   `json:"spec,omitempty"`
	Status PatchRolloutStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PatchRolloutList contains a list of PatchRollout.
type PatchRolloutList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PatchRollout `json:"items"`
}

// PatchApprovalSpec approves one candidate of one rollout.
type PatchApprovalSpec struct {
	// Rollout is the PatchRollout name in this namespace.
	Rollout string `json:"rollout"`
	// CandidateDigest must match the rollout's candidate digest, or the approval is ignored.
	CandidateDigest string `json:"candidateDigest"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=papproval
// +kubebuilder:printcolumn:name="Rollout",type=string,JSONPath=`.spec.rollout`
// +kubebuilder:printcolumn:name="Candidate",type=string,JSONPath=`.spec.candidateDigest`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PatchApproval is created by an approver to approve a PatchRollout candidate.
type PatchApproval struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec PatchApprovalSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// PatchApprovalList contains a list of PatchApproval.
type PatchApprovalList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PatchApproval `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PatchRollout{}, &PatchRolloutList{}, &PatchApproval{}, &PatchApprovalList{})
}
