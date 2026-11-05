package imageref

import (
	"slices"
	"testing"
)

func TestParseAndCanonical(t *testing.T) {
	tests := []struct {
		in        string
		canonical string
		tag       string
	}{
		{"nginx:1.25.3", "index.docker.io/library/nginx:1.25.3", "1.25.3"},
		{"docker.io/library/nginx:1.25.3", "index.docker.io/library/nginx:1.25.3", "1.25.3"},
		{"bitnami/redis:7.2.4", "index.docker.io/bitnami/redis:7.2.4", "7.2.4"},
		{"kind-registry:5000/library/nginx:1.25.3-alpine", "kind-registry:5000/library/nginx:1.25.3-alpine", "1.25.3-alpine"},
		{"nginx", "index.docker.io/library/nginx:latest", "latest"},
		{
			"nginx:1.25.3@sha256:4f7a0000000000000000000000000000000000000000000000000000000000ab",
			"index.docker.io/library/nginx@sha256:4f7a0000000000000000000000000000000000000000000000000000000000ab",
			"1.25.3",
		},
	}
	for _, tc := range tests {
		p, err := Parse(tc.in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.in, err)
		}
		if got := p.Canonical(); got != tc.canonical {
			t.Errorf("Canonical(%q) = %q, want %q", tc.in, got, tc.canonical)
		}
		if p.Tag != tc.tag {
			t.Errorf("Tag(%q) = %q, want %q", tc.in, p.Tag, tc.tag)
		}
	}
}

func TestAliasesDockerHub(t *testing.T) {
	p, _ := Parse("nginx:1.25.3")
	got := Aliases(p)
	for _, want := range []string{
		"nginx:1.25.3",
		"docker.io/nginx:1.25.3",
		"docker.io/library/nginx:1.25.3",
		"index.docker.io/library/nginx:1.25.3",
		"index.docker.io/nginx:1.25.3",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("Aliases missing %q: %v", want, got)
		}
	}
	// Every alias must parse back to the same canonical reference.
	for _, a := range got {
		q, err := Parse(a)
		if err != nil {
			t.Fatalf("alias %q doesn't parse: %v", a, err)
		}
		if q.Canonical() != p.Canonical() {
			t.Errorf("alias %q canonicalizes to %q, want %q", a, q.Canonical(), p.Canonical())
		}
	}
}

func TestAliasesOtherRegistry(t *testing.T) {
	p, _ := Parse("kind-registry:5000/library/nginx:1.25.3-alpine")
	got := Aliases(p)
	if len(got) != 1 || got[0] != "kind-registry:5000/library/nginx:1.25.3-alpine" {
		t.Errorf("Aliases = %v, want only the exact string", got)
	}
}

func TestFloatingTag(t *testing.T) {
	cases := map[string]bool{
		"nginx":                       true,
		"nginx:latest":                true,
		"nginx:1":                     true,
		"nginx:1.25":                  true,
		"nginx:1.25-alpine":           true,
		"nginx:1.25-alpine3.18":       true,
		"python:3.12-slim-bookworm":   true,
		"python:3.12.1-slim-bookworm": false,
		"nginx:alpine":                true,
		"nginx:1.25.3":                false,
		"nginx:1.25.3-alpine":         false,
		"redis:7.2.4-bookworm":        false,
		"myapp:2026.11.03-abc12":      false,
		"nginx@sha256:4f7a0000000000000000000000000000000000000000000000000000000000ab": false,
	}
	for in, want := range cases {
		p, err := Parse(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := FloatingTag(p); got != want {
			t.Errorf("FloatingTag(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestObjectName(t *testing.T) {
	n := ObjectName("index.docker.io/library/nginx@sha256:abc", "nginx", "abc")
	if n != ObjectName("index.docker.io/library/nginx@sha256:abc", "nginx", "abc") {
		t.Error("ObjectName not stable")
	}
	if len(n) > 63 {
		t.Errorf("ObjectName too long: %q", n)
	}
	long := ObjectName("k", "this-is-a-very-long-repository-name-that-goes-on-and-on-and-on")
	if len(long) > 63 {
		t.Errorf("ObjectName too long: %q", long)
	}
}
