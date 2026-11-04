package worker

import (
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"

	"github.com/sozercan/copacabana/internal/trivy"
)

func report(vulns ...trivy.Vulnerability) *trivy.Report {
	return &trivy.Report{Results: []trivy.Result{{Target: "img", Class: "os-pkgs", Vulnerabilities: vulns}}}
}

func vuln(id, pkg, fixed string) trivy.Vulnerability {
	return trivy.Vulnerability{VulnerabilityID: id, PkgName: pkg, FixedVersion: fixed}
}

func TestCheckScan(t *testing.T) {
	src := report(vuln("CVE-1", "busybox", "1.2"), vuln("CVE-2", "libssl3", "3.1"), vuln("CVE-3", "zlib", ""))

	t.Run("claimed fixes gone", func(t *testing.T) {
		patched := report(vuln("CVE-3", "zlib", ""))
		fixed, err := CheckScan(src, patched, map[string]bool{"CVE-1/busybox": true, "CVE-2/libssl3": true}, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(fixed) != 2 {
			t.Errorf("fixed = %v", fixed)
		}
	})
	t.Run("claimed fix still present", func(t *testing.T) {
		patched := report(vuln("CVE-1", "busybox", "1.2"))
		_, err := CheckScan(src, patched, map[string]bool{"CVE-1/busybox": true}, false)
		if err == nil || !strings.Contains(err.Error(), "still present") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("new vulnerability", func(t *testing.T) {
		patched := report(vuln("CVE-9", "newpkg", ""))
		if _, err := CheckScan(src, patched, nil, false); err == nil {
			t.Fatal("expected failure for introduced vulnerability")
		}
		if _, err := CheckScan(src, patched, nil, true); err != nil {
			t.Fatalf("allowNew: %v", err)
		}
	})
}

func TestVEXFixed(t *testing.T) {
	doc := `{"statements":[
	  {"vulnerability":{"name":"CVE-2023-42363"},"status":"fixed",
	   "products":[{"@id":"pkg:oci/nginx","subcomponents":[{"@id":"pkg:apk/alpine/busybox@1.36.1-r5?arch=x86_64"},{"@id":"pkg:apk/alpine/ssl_client@1.36.1-r5"}]}]},
	  {"vulnerability":{"name":"CVE-2000-1"},"status":"not_affected","products":[{"subcomponents":[{"@id":"pkg:apk/alpine/zlib@1"}]}]}
	]}`
	got, err := VEXFixed([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if !got["CVE-2023-42363/busybox"] || !got["CVE-2023-42363/ssl_client"] || len(got) != 2 {
		t.Errorf("VEXFixed = %v", got)
	}
}

func TestPurlName(t *testing.T) {
	cases := map[string]string{
		"pkg:apk/alpine/busybox@1.36.1-r5?arch=x86_64": "busybox",
		"pkg:deb/debian/libssl3@3.0.11-1?arch=amd64":   "libssl3",
		"pkg:rpm/redhat/openssl-libs":                  "openssl-libs",
	}
	for in, want := range cases {
		if got := PurlName(in); got != want {
			t.Errorf("PurlName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMaxRevision(t *testing.T) {
	tags := []string{"1.25.3-alpine-1", "1.25.3-alpine-3", "1.25.3-alpine", "1.25.3-1", "1.25.3-alpine-x", "sha256-abc"}
	if got := MaxRevision(tags, "1.25.3-alpine"); got != 3 {
		t.Errorf("MaxRevision = %d, want 3", got)
	}
	if got := MaxRevision(tags, "1.25.3"); got != 1 {
		t.Errorf("MaxRevision(1.25.3) = %d, want 1", got)
	}
	if got := MaxRevision(nil, "x"); got != 0 {
		t.Errorf("MaxRevision(nil) = %d", got)
	}
}

func TestCheckLayers(t *testing.T) {
	src, err := random.Image(64, 3)
	if err != nil {
		t.Fatal(err)
	}
	extra, _ := random.Layer(64, "application/vnd.oci.image.layer.v1.tar+gzip")
	patched, err := mutate.AppendLayers(src, extra)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckLayers(src, patched); err != nil {
		t.Errorf("appended layer rejected: %v", err)
	}
	if err := CheckLayers(src, src); err == nil {
		t.Error("image with no appended layer accepted")
	}
	other, _ := random.Image(64, 4)
	if err := CheckLayers(src, other); err == nil {
		t.Error("image with different base layers accepted")
	}
}

func TestCheckConfig(t *testing.T) {
	src, _ := random.Image(64, 1)
	cf, _ := src.ConfigFile()
	cf = cf.DeepCopy()
	cf.Config.Labels = map[string]string{"a": "b"}
	cf.Config.Cmd = []string{"nginx"}
	base, _ := mutate.ConfigFile(src, cf)

	withLabel := setConfig(t, base, func(c *v1.Config) { c.Labels["BaseImage"] = "x" })
	if err := CheckConfig(base, withLabel); err != nil {
		t.Errorf("BaseImage label change rejected: %v", err)
	}
	changedCmd := setConfig(t, base, func(c *v1.Config) { c.Cmd = []string{"sh"} })
	if err := CheckConfig(base, changedCmd); err == nil {
		t.Error("cmd change accepted")
	}
}

func setConfig(t *testing.T, img v1.Image, f func(*v1.Config)) v1.Image {
	t.Helper()
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cf = cf.DeepCopy()
	if cf.Config.Labels == nil {
		cf.Config.Labels = map[string]string{}
	}
	f(&cf.Config)
	out, err := mutate.ConfigFile(img, cf)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
