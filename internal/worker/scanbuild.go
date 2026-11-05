package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/sozercan/copacabana/internal/registry"
	"github.com/sozercan/copacabana/internal/trivy"
)

// Scan scans image and, if reportRepo is set, pushes the report there as an artifact.
func Scan(ctx context.Context, reg *registry.Client, sc trivy.Scanner, image, reportRepo string) (*ScanResult, error) {
	raw, err := sc.Scan(ctx, image)
	if err != nil {
		return nil, err
	}
	rep, err := trivy.Parse(raw)
	if err != nil {
		return nil, err
	}
	res := &ScanResult{Fixable: len(rep.Fixable()), Total: len(rep.OS()), OSFamily: rep.OSFamily()}
	if reportRepo != "" {
		ref, err := reg.PushArtifact(ctx, reportRepo, registry.ArtifactTypeReport, registry.MediaTypeReport, raw, nil,
			map[string]string{"copa.sh/scanned-image": image}, "")
		if err != nil {
			return nil, err
		}
		res.ReportRef = ref
	}
	return res, nil
}

// BuildOptions configures a build step.
type BuildOptions struct {
	Source    string
	ReportRef string
	Target    string
	BuildKit  string
	CopaBin   string
	WorkDir   string
	Timeout   time.Duration
}

// Build runs Copa against one platform image and pushes the result to the staging target.
func Build(ctx context.Context, reg *registry.Client, o BuildOptions) (*BuildResult, error) {
	if o.CopaBin == "" {
		o.CopaBin = "copa"
	}
	if o.WorkDir == "" {
		o.WorkDir = os.TempDir()
	}
	report, err := reg.FetchArtifact(ctx, o.ReportRef)
	if err != nil {
		return nil, err
	}
	reportPath := filepath.Join(o.WorkDir, "report.json")
	vexPath := filepath.Join(o.WorkDir, "vex.json")
	if err := os.WriteFile(reportPath, report, 0o600); err != nil {
		return nil, err
	}
	if err := waitForSocket(ctx, o.BuildKit, 2*time.Minute); err != nil {
		return nil, err
	}

	args := []string{
		"patch", "-i", o.Source, "-r", reportPath, "-t", o.Target,
		"-a", o.BuildKit, "--push", "-f", "openvex", "-o", vexPath,
	}
	if o.Timeout > 0 {
		args = append(args, "--timeout", o.Timeout.String())
	}
	cmd := exec.CommandContext(ctx, o.CopaBin, args...)
	var out tailBuffer
	cmd.Stdout = multi{os.Stdout, &out}
	cmd.Stderr = multi{os.Stderr, &out}
	fmt.Fprintf(os.Stderr, "running: %s %s\n", o.CopaBin, strings.Join(args, " "))
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("copa patch failed: %w: %s", err, out.String())
	}

	desc, err := reg.Get(ctx, o.Target)
	if err != nil {
		return nil, fmt.Errorf("resolving staged image: %w", err)
	}
	targetRef, err := reg.ParseRef(o.Target)
	if err != nil {
		return nil, err
	}
	stagedRepo := targetRef.Context().Name()
	res := &BuildResult{StagedRef: stagedRepo + "@" + desc.Digest.String()}

	// Copa writes the VEX document only when it updated at least one package. A platform with fixable
	// findings and no VEX was not patched, whatever image Copa pushed.
	rep, err := trivy.Parse(report)
	if err != nil {
		return nil, err
	}
	vex, err := os.ReadFile(vexPath)
	switch {
	case err != nil && !os.IsNotExist(err):
		return nil, fmt.Errorf("reading VEX output: %w", err)
	case len(vex) == 0 && len(rep.Fixable()) > 0:
		return nil, fmt.Errorf("copa updated no packages: no VEX output for %d fixable vulnerabilities", len(rep.Fixable()))
	case len(vex) == 0:
		return res, nil
	}
	fixed, err := VEXFixed(vex)
	if err != nil {
		return nil, err
	}
	if len(fixed) == 0 && len(rep.Fixable()) > 0 {
		return nil, fmt.Errorf("copa's VEX output marks nothing fixed")
	}
	res.Fixed = len(fixed)
	subject := desc.Descriptor
	subject.Platform = nil
	ref, err := reg.PushArtifact(ctx, stagedRepo, registry.ArtifactTypeVEX, registry.MediaTypeVEX, vex, &subject, nil, "")
	if err != nil {
		return nil, err
	}
	res.VEXRef = ref
	return res, nil
}

// VEXFixed returns vulnerability/package keys an OpenVEX document marks as fixed.
func VEXFixed(doc []byte) (map[string]bool, error) {
	var v struct {
		Statements []struct {
			Vulnerability struct {
				Name string `json:"name"`
			} `json:"vulnerability"`
			Status   string `json:"status"`
			Products []struct {
				Subcomponents []struct {
					ID string `json:"@id"`
				} `json:"subcomponents"`
			} `json:"products"`
		} `json:"statements"`
	}
	if err := json.Unmarshal(doc, &v); err != nil {
		return nil, fmt.Errorf("parsing VEX: %w", err)
	}
	out := map[string]bool{}
	for _, s := range v.Statements {
		if s.Status != "fixed" {
			continue
		}
		for _, p := range s.Products {
			for _, sc := range p.Subcomponents {
				out[s.Vulnerability.Name+"/"+PurlName(sc.ID)] = true
			}
		}
	}
	return out, nil
}

// PurlName returns the package name from a package URL such as pkg:apk/alpine/busybox@1.36.1-r5?arch=x86_64.
func PurlName(purl string) string {
	s := purl
	if i := strings.IndexAny(s, "@?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	return s
}

func waitForSocket(ctx context.Context, addr string, timeout time.Duration) error {
	path, ok := strings.CutPrefix(addr, "unix://")
	if !ok {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("buildkit socket %s not ready after %s", path, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// layerDigests returns an image's layer digests.
func layerDigests(img v1.Image) ([]string, error) {
	m, err := img.Manifest()
	if err != nil {
		return nil, err
	}
	out := make([]string, len(m.Layers))
	for i, l := range m.Layers {
		out[i] = l.Digest.String()
	}
	return out, nil
}

// tailBuffer keeps the end of a command's output. os/exec copies stdout and stderr from separate goroutines
// when they're different writers, so writes are serialized.
type tailBuffer struct {
	mu sync.Mutex
	b  []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > 8192 {
		t.b = t.b[len(t.b)-8192:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	s := string(t.b)
	t.mu.Unlock()
	if len(s) > 1500 {
		s = s[len(s)-1500:]
	}
	return s
}

type multi []interface{ Write([]byte) (int, error) }

func (m multi) Write(p []byte) (int, error) {
	for _, w := range m {
		_, _ = w.Write(p)
	}
	return len(p), nil
}
