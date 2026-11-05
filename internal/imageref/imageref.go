// Package imageref normalizes image references, computes the aliases admission must match,
// and derives names for patched images.
package imageref

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
)

const dockerHubRegistry = "index.docker.io"

// Parsed is a normalized image reference.
type Parsed struct {
	// Original is the string exactly as written.
	Original string
	// Repository is the canonical repository, such as index.docker.io/library/nginx.
	Repository string
	// Tag, if the reference has one.
	Tag string
	// Digest, if the reference is pinned.
	Digest string
}

// Canonical returns the canonical form: repo:tag, or repo@digest when there's no tag.
// A reference with both tag and digest is canonicalized to repo@digest, since the digest is what runs.
func (p Parsed) Canonical() string {
	if p.Digest != "" {
		return p.Repository + "@" + p.Digest
	}
	return p.Repository + ":" + p.Tag
}

// Parse normalizes an image string. Insecure registries must be listed so that
// go-containerregistry doesn't reject plain-HTTP hosts later, but parsing itself is the same.
func Parse(s string) (Parsed, error) {
	ref, err := name.ParseReference(s, name.WithDefaultRegistry(dockerHubRegistry))
	if err != nil {
		return Parsed{}, fmt.Errorf("parsing image reference %q: %w", s, err)
	}
	p := Parsed{Original: s, Repository: ref.Context().Name()}
	switch r := ref.(type) {
	case name.Tag:
		p.Tag = r.TagStr()
	case name.Digest:
		p.Digest = r.DigestStr()
		// name.Digest drops the tag; recover it for naming patched images.
		if before, _, ok := strings.Cut(s, "@"); ok {
			if t, err := name.NewTag(before, name.WithDefaultRegistry(dockerHubRegistry)); err == nil && strings.Contains(lastSegment(before), ":") {
				p.Tag = t.TagStr()
			}
		}
	}
	return p, nil
}

func lastSegment(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// Aliases returns every image string a pod spec might use to mean the same reference.
// For Docker Hub that covers the short forms; other registries only have one spelling.
func Aliases(p Parsed) []string {
	suffix := ":" + p.Tag
	if p.Digest != "" {
		suffix = "@" + p.Digest
	}
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	add(p.Repository + suffix)
	if strings.HasPrefix(p.Repository, dockerHubRegistry+"/") {
		path := strings.TrimPrefix(p.Repository, dockerHubRegistry+"/")
		add("docker.io/" + path + suffix)
		if short, ok := strings.CutPrefix(path, "library/"); ok {
			add(short + suffix)
			add("docker.io/" + short + suffix)
			add(dockerHubRegistry + "/" + short + suffix)
		} else {
			add(path + suffix)
		}
	}
	if p.Original != "" {
		add(p.Original)
	}
	return out
}

var (
	// Tags with fewer than three numeric components (1, 1.25, v2) usually float to newer releases, whatever
	// variant follows them (1.25-alpine3.18, 3.12-slim-bookworm).
	shortVersion = regexp.MustCompile(`^v?\d+(\.\d+)?(-[A-Za-z0-9._-]+)?$`)
	floatingName = map[string]bool{
		"latest": true, "stable": true, "mainline": true, "edge": true, "lts": true,
		"alpine": true, "slim": true, "bookworm": true, "bullseye": true, "jammy": true, "noble": true,
	}
)

// FloatingTag reports whether a reference uses a tag that upstream is expected to move to new releases.
// Swapping those would pin a team to old content, so they aren't eligible.
func FloatingTag(p Parsed) bool {
	if p.Digest != "" {
		return false
	}
	return floatingName[p.Tag] || shortVersion.MatchString(p.Tag)
}

// RepoPath returns the repository path without its registry, such as library/nginx.
func RepoPath(repository string) string {
	if i := strings.Index(repository, "/"); i >= 0 {
		return repository[i+1:]
	}
	return repository
}

// TagBase returns the base used for patched tags: the source tag, or a digest-derived name.
func TagBase(p Parsed) string {
	if p.Tag != "" {
		return p.Tag
	}
	return "sha256-" + strings.TrimPrefix(p.Digest, "sha256:")[:12]
}

var dnsUnsafe = regexp.MustCompile(`[^a-z0-9-]+`)

// ObjectName returns a stable, DNS-safe Kubernetes object name built from parts and a hashed key.
func ObjectName(key string, parts ...string) string {
	sum := sha256.Sum256([]byte(key))
	suffix := hex.EncodeToString(sum[:])[:10]
	base := strings.Trim(dnsUnsafe.ReplaceAllString(strings.ToLower(strings.Join(parts, "-")), "-"), "-")
	if len(base) > 40 {
		base = strings.Trim(base[:40], "-")
	}
	if base == "" {
		return suffix
	}
	return base + "-" + suffix
}

// ShortDigest returns the first 12 hex characters of a digest.
func ShortDigest(d string) string {
	h := strings.TrimPrefix(d, "sha256:")
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
