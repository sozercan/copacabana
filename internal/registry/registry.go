// Package registry wraps go-containerregistry for the operations the controller and worker need:
// resolving tags, listing patched revisions, and pushing and reading OCI artifacts with subjects.
package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/sozercan/copacabana/api/v1alpha1"
)

// Media and artifact types used for the artifacts this project writes.
const (
	EmptyConfigMediaType = "application/vnd.oci.empty.v1+json"

	ArtifactTypeReport      = "application/vnd.copa.trivy-report.v1+json"
	ArtifactTypeVEX         = "application/vnd.openvex.v1+json"
	ArtifactTypeAttestation = "application/vnd.copa.patch-attestation.v1+dsse"

	MediaTypeReport = "application/vnd.aquasec.trivy.report.v1+json"
	MediaTypeVEX    = "application/vnd.openvex.v1+json"
	MediaTypeDSSE   = "application/vnd.dsse.envelope.v1+json"
)

// Client talks to registries. Hosts listed as insecure are reached over plain HTTP.
type Client struct {
	insecure map[string]bool
	keychain authn.Keychain
}

// New returns a client. insecure lists registry hosts (host:port) to reach over plain HTTP.
func New(insecure []string) *Client {
	m := map[string]bool{}
	for _, h := range insecure {
		if h = strings.TrimSpace(h); h != "" {
			m[h] = true
		}
	}
	return &Client{insecure: m, keychain: authn.DefaultKeychain}
}

func (c *Client) opts(ctx context.Context) []remote.Option {
	return []remote.Option{remote.WithContext(ctx), remote.WithAuthFromKeychain(c.keychain)}
}

// Insecure reports whether a registry host is configured for plain HTTP.
func (c *Client) Insecure(host string) bool { return c.insecure[host] }

// ParseRef parses a reference, allowing plain HTTP for configured hosts.
func (c *Client) ParseRef(s string) (name.Reference, error) {
	ref, err := name.ParseReference(s)
	if err != nil {
		return nil, err
	}
	if c.insecure[ref.Context().RegistryStr()] {
		return name.ParseReference(s, name.Insecure)
	}
	return ref, nil
}

// ParseRepo parses a repository, allowing plain HTTP for configured hosts.
func (c *Client) ParseRepo(s string) (name.Repository, error) {
	repo, err := name.NewRepository(s)
	if err != nil {
		return name.Repository{}, err
	}
	if c.insecure[repo.RegistryStr()] {
		return name.NewRepository(s, name.Insecure)
	}
	return repo, nil
}

// IsNotFound reports whether err is a registry 404.
func IsNotFound(err error) bool {
	var terr *transport.Error
	if errors.As(err, &terr) {
		return terr.StatusCode == http.StatusNotFound
	}
	return false
}

// Resolved is a reference resolved to content.
type Resolved struct {
	Digest    string
	MediaType types.MediaType
	// Platforms are the platform manifests that match the wanted platforms.
	Platforms []v1alpha1.PlatformDigest
	// Available lists every real platform in the source, wanted or not.
	Available []string
}

// Resolve resolves a reference to its digest and the manifest digests of the wanted platforms.
func (c *Client) Resolve(ctx context.Context, ref string, want []string) (*Resolved, error) {
	r, err := c.ParseRef(ref)
	if err != nil {
		return nil, err
	}
	desc, err := remote.Get(r, c.opts(ctx)...)
	if err != nil {
		return nil, fmt.Errorf("resolving %s: %w", ref, err)
	}
	out := &Resolved{Digest: desc.Digest.String(), MediaType: desc.MediaType}
	if desc.MediaType.IsIndex() {
		idx, err := desc.ImageIndex()
		if err != nil {
			return nil, err
		}
		im, err := idx.IndexManifest()
		if err != nil {
			return nil, err
		}
		for _, m := range im.Manifests {
			if m.Platform == nil || m.Platform.OS == "unknown" {
				continue // attestation manifests and similar
			}
			p := PlatformString(m.Platform)
			out.Available = append(out.Available, p)
			if slices.Contains(want, p) {
				out.Platforms = append(out.Platforms, v1alpha1.PlatformDigest{Platform: p, Digest: m.Digest.String()})
			}
		}
		return out, nil
	}
	img, err := desc.Image()
	if err != nil {
		return nil, err
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		return nil, err
	}
	p := PlatformString(&v1.Platform{OS: cfg.OS, Architecture: cfg.Architecture, Variant: cfg.Variant})
	out.Available = []string{p}
	if slices.Contains(want, p) {
		out.Platforms = []v1alpha1.PlatformDigest{{Platform: p, Digest: desc.Digest.String()}}
	}
	return out, nil
}

// PlatformString formats a platform as os/arch[/variant].
func PlatformString(p *v1.Platform) string {
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

// ParsePlatform parses os/arch[/variant].
func ParsePlatform(s string) (*v1.Platform, error) {
	parts := strings.Split(s, "/")
	if len(parts) < 2 || len(parts) > 3 {
		return nil, fmt.Errorf("invalid platform %q", s)
	}
	p := &v1.Platform{OS: parts[0], Architecture: parts[1]}
	if len(parts) == 3 {
		p.Variant = parts[2]
	}
	return p, nil
}

// ListTags lists the tags in a repository. A missing repository has no tags.
func (c *Client) ListTags(ctx context.Context, repo string) ([]string, error) {
	r, err := c.ParseRepo(repo)
	if err != nil {
		return nil, err
	}
	tags, err := remote.List(r, c.opts(ctx)...)
	if err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		var terr *transport.Error
		if errors.As(err, &terr) && slices.ContainsFunc(terr.Errors, func(d transport.Diagnostic) bool { return d.Code == transport.NameUnknownErrorCode }) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing tags of %s: %w", repo, err)
	}
	return tags, nil
}

// Get fetches a descriptor.
func (c *Client) Get(ctx context.Context, ref string) (*remote.Descriptor, error) {
	r, err := c.ParseRef(ref)
	if err != nil {
		return nil, err
	}
	return remote.Get(r, c.opts(ctx)...)
}

// Image fetches an image.
func (c *Client) Image(ctx context.Context, ref string) (v1.Image, error) {
	r, err := c.ParseRef(ref)
	if err != nil {
		return nil, err
	}
	return remote.Image(r, c.opts(ctx)...)
}

// WriteImage writes an image to ref, mounting blobs from the same registry where possible.
func (c *Client) WriteImage(ctx context.Context, ref string, img v1.Image) error {
	r, err := c.ParseRef(ref)
	if err != nil {
		return err
	}
	return remote.Write(r, img, c.opts(ctx)...)
}

// WriteIndex writes an index to ref.
func (c *Client) WriteIndex(ctx context.Context, ref string, idx v1.ImageIndex) error {
	r, err := c.ParseRef(ref)
	if err != nil {
		return err
	}
	return remote.WriteIndex(r, idx, c.opts(ctx)...)
}

type artifactManifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     types.MediaType   `json:"mediaType"`
	ArtifactType  string            `json:"artifactType"`
	Config        v1.Descriptor     `json:"config"`
	Layers        []v1.Descriptor   `json:"layers"`
	Subject       *v1.Descriptor    `json:"subject,omitempty"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

type rawManifest struct {
	raw []byte
	mt  types.MediaType
}

func (r rawManifest) RawManifest() ([]byte, error)        { return r.raw, nil }
func (r rawManifest) MediaType() (types.MediaType, error) { return r.mt, nil }

// PushArtifact pushes a single-blob OCI artifact to repo and returns its pinned reference.
// If subject is set, the artifact is a referrer of that manifest. If tag is set, the artifact is also tagged.
func (c *Client) PushArtifact(ctx context.Context, repo, artifactType, mediaType string, data []byte,
	subject *v1.Descriptor, annotations map[string]string, tag string,
) (string, error) {
	r, err := c.ParseRepo(repo)
	if err != nil {
		return "", err
	}
	cfg := static.NewLayer([]byte("{}"), EmptyConfigMediaType)
	blob := static.NewLayer(data, types.MediaType(mediaType))
	for _, l := range []v1.Layer{cfg, blob} {
		if err := remote.WriteLayer(r, l, c.opts(ctx)...); err != nil {
			return "", fmt.Errorf("uploading blob to %s: %w", repo, err)
		}
	}
	cfgDigest, _ := cfg.Digest()
	blobDigest, _ := blob.Digest()
	m := artifactManifest{
		SchemaVersion: 2,
		MediaType:     types.OCIManifestSchema1,
		ArtifactType:  artifactType,
		Config:        v1.Descriptor{MediaType: EmptyConfigMediaType, Digest: cfgDigest, Size: 2},
		Layers:        []v1.Descriptor{{MediaType: types.MediaType(mediaType), Digest: blobDigest, Size: int64(len(data))}},
		Subject:       subject,
		Annotations:   annotations,
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	digest, _, err := v1.SHA256(bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	target := name.Reference(r.Digest(digest.String()))
	if tag != "" {
		target = r.Tag(tag)
	}
	if err := remote.Put(target, rawManifest{raw: raw, mt: types.OCIManifestSchema1}, c.opts(ctx)...); err != nil {
		return "", fmt.Errorf("pushing artifact to %s: %w", repo, err)
	}
	return r.Name() + "@" + digest.String(), nil
}

// FetchArtifact returns the first blob of an artifact.
func (c *Client) FetchArtifact(ctx context.Context, ref string) ([]byte, error) {
	r, err := c.ParseRef(ref)
	if err != nil {
		return nil, err
	}
	desc, err := remote.Get(r, c.opts(ctx)...)
	if err != nil {
		return nil, fmt.Errorf("fetching artifact %s: %w", ref, err)
	}
	var m artifactManifest
	if err := json.Unmarshal(desc.Manifest, &m); err != nil {
		return nil, err
	}
	if len(m.Layers) == 0 {
		return nil, fmt.Errorf("artifact %s has no blobs", ref)
	}
	layer, err := remote.Layer(r.Context().Digest(m.Layers[0].Digest.String()), c.opts(ctx)...)
	if err != nil {
		return nil, err
	}
	rc, err := layer.Compressed()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// Referrers lists referrers of a pinned manifest with the given artifact type.
func (c *Client) Referrers(ctx context.Context, pinned, artifactType string) ([]v1.Descriptor, error) {
	r, err := c.ParseRef(pinned)
	if err != nil {
		return nil, err
	}
	d, ok := r.(name.Digest)
	if !ok {
		return nil, fmt.Errorf("referrers need a digest reference, got %s", pinned)
	}
	idx, err := remote.Referrers(d, c.opts(ctx)...)
	if err != nil {
		return nil, fmt.Errorf("listing referrers of %s: %w", pinned, err)
	}
	im, err := idx.IndexManifest()
	if err != nil {
		return nil, err
	}
	var out []v1.Descriptor
	for _, m := range im.Manifests {
		if artifactType == "" || m.ArtifactType == artifactType {
			out = append(out, m)
		}
	}
	return out, nil
}
