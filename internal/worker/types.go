// Package worker implements the scan, build and verify steps that run inside Jobs.
// Each step prints a small JSON result, which the Job writes to its termination message for the controller.
package worker

import (
	"github.com/sozercan/copa-controller/api/v1alpha1"
)

// TerminationLog is where Kubernetes reads a container's termination message.
const TerminationLog = "/dev/termination-log"

// ScanResult is the output of a scan step.
type ScanResult struct {
	// ReportRef is the pinned report artifact, empty when the report wasn't pushed.
	ReportRef string `json:"reportRef,omitempty"`
	Fixable   int    `json:"fixable"`
	Total     int    `json:"total"`
	// OSFamily is the OS Trivy detected, empty when the image has no OS packages Copa could patch.
	OSFamily string `json:"osFamily"`
}

// BuildResult is the output of a build step.
type BuildResult struct {
	StagedRef string `json:"stagedRef"`
	VEXRef    string `json:"vexRef,omitempty"`
	Fixed     int    `json:"fixed"`
}

// VerifyPlatform is one platform to verify.
type VerifyPlatform struct {
	Platform     string `json:"platform"`
	SourceDigest string `json:"sourceDigest"`
	StagedRef    string `json:"stagedRef"`
	ReportRef    string `json:"reportRef"`
	VEXRef       string `json:"vexRef"`
}

// VerifySpec is the input to the verify step.
type VerifySpec struct {
	PatchBuild        string           `json:"patchBuild"`
	SourceRepository  string           `json:"sourceRepository"`
	SourceDigest      string           `json:"sourceDigest"`
	SourceReference   string           `json:"sourceReference"`
	TagBase           string           `json:"tagBase"`
	PatchedRepository string           `json:"patchedRepository"`
	Platforms         []VerifyPlatform `json:"platforms"`
	// AllowNewVulnerabilities accepts vulnerabilities that appear only after patching.
	AllowNewVulnerabilities bool `json:"allowNewVulnerabilities,omitempty"`
}

// VerifyResult is the output of the verify step.
type VerifyResult struct {
	Ref       string                   `json:"ref"`
	Tag       string                   `json:"tag"`
	Digest    string                   `json:"digest"`
	Revision  int                      `json:"revision"`
	Platforms []v1alpha1.PlatformPatch `json:"platforms"`
}
