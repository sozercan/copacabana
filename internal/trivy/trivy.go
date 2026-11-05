// Package trivy runs Trivy in client mode against a Trivy server and reads its JSON reports.
package trivy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
)

// Report is the subset of the Trivy JSON report this project reads.
type Report struct {
	ArtifactName string   `json:"ArtifactName"`
	Metadata     Metadata `json:"Metadata"`
	Results      []Result `json:"Results"`
}

// Result is one scan target in a report.
type Result struct {
	Target          string          `json:"Target"`
	Class           string          `json:"Class"`
	Vulnerabilities []Vulnerability `json:"Vulnerabilities"`
}

// Vulnerability is one finding.
type Vulnerability struct {
	VulnerabilityID  string `json:"VulnerabilityID"`
	PkgName          string `json:"PkgName"`
	InstalledVersion string `json:"InstalledVersion"`
	FixedVersion     string `json:"FixedVersion"`
	Severity         string `json:"Severity"`
}

// Key identifies a vulnerability in a package.
func (v Vulnerability) Key() string { return v.VulnerabilityID + "/" + v.PkgName }

// Metadata is the part of the report metadata the controller uses.
type Metadata struct {
	// OS is the detected OS. Trivy leaves it out when the image has no OS package database.
	OS *OSInfo `json:"OS,omitempty"`
}

// OSInfo is the detected OS.
type OSInfo struct {
	Family string `json:"Family"`
	Name   string `json:"Name"`
}

// OSFamily returns the detected OS family, or "" when Trivy found no OS packages to assess.
func (r *Report) OSFamily() string {
	if r.Metadata.OS == nil {
		return ""
	}
	return r.Metadata.OS.Family
}

// Parse reads a report.
func Parse(data []byte) (*Report, error) {
	var r Report
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parsing trivy report: %w", err)
	}
	return &r, nil
}

// OS returns OS-package vulnerabilities.
func (r *Report) OS() []Vulnerability {
	var out []Vulnerability
	for _, res := range r.Results {
		if res.Class != "" && res.Class != "os-pkgs" {
			continue
		}
		out = append(out, res.Vulnerabilities...)
	}
	return out
}

// Fixable returns OS vulnerabilities that have a fixed version.
func (r *Report) Fixable() []Vulnerability {
	var out []Vulnerability
	for _, v := range r.OS() {
		if v.FixedVersion != "" {
			out = append(out, v)
		}
	}
	return out
}

// Keys returns the set of vulnerability/package keys for OS vulnerabilities.
func (r *Report) Keys() map[string]bool {
	m := map[string]bool{}
	for _, v := range r.OS() {
		m[v.Key()] = true
	}
	return m
}

// Scanner runs the trivy client.
type Scanner struct {
	// Server is the Trivy server URL. Empty means standalone mode.
	Server string
	// InsecureRegistries are plain-HTTP registry hosts. Trivy's --insecure is set only when the scanned image
	// is on one of them, since it also turns off certificate verification.
	InsecureRegistries []string
	// Binary is the trivy binary path.
	Binary string
}

// Scan scans image for OS vulnerabilities and returns the raw JSON report.
func (s Scanner) Scan(ctx context.Context, image string) ([]byte, error) {
	out, err := os.CreateTemp("", "trivy-*.json")
	if err != nil {
		return nil, err
	}
	out.Close()
	defer os.Remove(out.Name())

	bin := s.Binary
	if bin == "" {
		bin = "trivy"
	}
	args := []string{
		"image", "--quiet",
		"--scanners", "vuln", "--pkg-types", "os",
		"--image-src", "remote",
		"--format", "json", "--output", out.Name(),
	}
	if s.Server != "" {
		args = append(args, "--server", s.Server)
	}
	if s.insecure(image) {
		args = append(args, "--insecure")
	}
	args = append(args, image)
	cmd := exec.CommandContext(ctx, bin, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("trivy %s: %w: %s", image, err, tail(stderr.String(), 1500))
	}
	return os.ReadFile(out.Name())
}

// insecure reports whether image is on a plain-HTTP registry.
func (s Scanner) insecure(image string) bool {
	ref, err := name.ParseReference(image)
	if err != nil {
		return false
	}
	return slices.Contains(s.InsecureRegistries, ref.Context().RegistryStr())
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
