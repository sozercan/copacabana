// Package controller implements the copa-rollout-controller reconcilers.
package controller

import (
	"crypto/ed25519"
	"strings"
	"time"

	"github.com/sozercan/copa-controller/internal/imageref"
)

// Modes the controller runs in.
const (
	ModeReportOnly = "report-only"
	ModeBuilder    = "builder"
	ModeConsumer   = "consumer"
)

// Config is the controller configuration. In a later version this would come from a PatchPolicy resource.
type Config struct {
	Mode string

	// Platforms every patch must cover, such as linux/amd64.
	Platforms []string
	// InsecureRegistries are reached over plain HTTP.
	InsecureRegistries []string
	// PatchedPrefix is where verified patches live, such as myacr.azurecr.io/patched.
	PatchedPrefix string
	// StagingPrefix is where build Jobs push, such as myacr.azurecr.io/staging.
	StagingPrefix string

	SystemNamespace   string
	BuilderNamespace  string
	VerifierNamespace string

	WorkerImage   string
	BuildKitImage string
	TrivyServer   string

	// PublicKey verifies attestations from the verify Job.
	PublicKey ed25519.PublicKey

	ResolveInterval     time.Duration
	RescanInterval      time.Duration
	MaxConcurrentBuilds int
	BuildTimeout        time.Duration

	RolloutDeadline       time.Duration
	HealthyFor            time.Duration
	Soak                  time.Duration
	MaxConcurrentRestarts int
	// RequireSafeStrategy marks Deployments without a surge-only rolling update and readiness probes as ineligible.
	RequireSafeStrategy bool
	// AllowNewVulnerabilities lets verification pass when patching introduces vulnerabilities the source didn't have.
	AllowNewVulnerabilities bool
}

// RepoKey turns a canonical repository into a path that keeps registries apart,
// such as index.docker.io/library/nginx -> docker.io/library/nginx.
func RepoKey(repository string) string {
	host, path, ok := strings.Cut(repository, "/")
	if !ok {
		return repository
	}
	if host == "index.docker.io" {
		host = "docker.io"
	}
	return strings.ReplaceAll(host, ":", "-") + "/" + path
}

// PatchedRepository returns where verified patches of a source repository live.
func (c *Config) PatchedRepository(sourceRepo string) string {
	return c.PatchedPrefix + "/" + RepoKey(sourceRepo)
}

// StagingRepository returns where build Jobs push patched images of a source repository.
func (c *Config) StagingRepository(sourceRepo string) string {
	return c.StagingPrefix + "/" + RepoKey(sourceRepo)
}

// ReportRepository returns where scan Jobs push reports for a source repository.
func (c *Config) ReportRepository(sourceRepo string) string {
	return c.StagingPrefix + "/reports/" + RepoKey(sourceRepo)
}

// TagBase returns the base for patched tags of a canonical reference.
func TagBase(reference string) string {
	p, err := imageref.Parse(reference)
	if err != nil {
		return "unknown"
	}
	return imageref.TagBase(p)
}
