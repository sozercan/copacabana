package controller

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/sozercan/copa-controller/api/v1alpha1"
	"github.com/sozercan/copa-controller/internal/attest"
	"github.com/sozercan/copa-controller/internal/registry"
)

// Catalog finds verified patches in the patched repository.
type Catalog struct {
	Registry *registry.Client
	Config   *Config
}

// Latest returns the highest-revision patch built from sourceDigest whose attestation verifies
// and covers every required platform. It returns nil when there is none. A non-nil error with a non-nil
// patch means newer revisions failed verification.
func (c *Catalog) Latest(ctx context.Context, sourceRepo, sourceDigest, reference string, sourcePlatforms []v1alpha1.PlatformDigest) (*v1alpha1.PatchRef, error) {
	repo := c.Config.PatchedRepository(sourceRepo)
	tags, err := c.Registry.ListTags(ctx, repo)
	if err != nil {
		return nil, err
	}
	re := regexp.MustCompile("^" + regexp.QuoteMeta(TagBase(reference)) + `-(\d+)$`)
	type cand struct {
		tag string
		rev int
	}
	var cands []cand
	for _, t := range tags {
		if m := re.FindStringSubmatch(t); m != nil {
			n, _ := strconv.Atoi(m[1])
			cands = append(cands, cand{t, n})
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].rev > cands[j].rev })

	// A bad revision mustn't hide an older good one, so verification continues past failures. When an older
	// revision verifies, it's returned along with an error naming the newer revisions that didn't: something
	// pushed unverifiable content to the patched repository, which is worth an alert.
	var rejected []error
	for _, cd := range cands {
		ref, err := c.verify(ctx, repo, cd.tag, sourceDigest, sourcePlatforms)
		if err != nil {
			rejected = append(rejected, err)
			continue
		}
		if ref != nil {
			if len(rejected) > 0 {
				return ref, fmt.Errorf("using %s; ignored newer revisions: %w", cd.tag, errors.Join(rejected...))
			}
			return ref, nil
		}
	}
	return nil, errors.Join(rejected...)
}

// verify checks one patched tag. It returns nil, nil if the tag was built from a different source digest.
func (c *Catalog) verify(ctx context.Context, repo, tag, sourceDigest string, sourcePlatforms []v1alpha1.PlatformDigest) (*v1alpha1.PatchRef, error) {
	desc, err := c.Registry.Get(ctx, repo+":"+tag)
	if err != nil {
		return nil, err
	}
	if !desc.MediaType.IsIndex() {
		return nil, fmt.Errorf("%s:%s is not an index", repo, tag)
	}
	idx, err := desc.ImageIndex()
	if err != nil {
		return nil, err
	}
	im, err := idx.IndexManifest()
	if err != nil {
		return nil, err
	}
	if im.Annotations[v1alpha1.IndexAnnotationSourceDigest] != sourceDigest {
		return nil, nil
	}
	pinned := repo + "@" + desc.Digest.String()

	refs, err := c.Registry.Referrers(ctx, pinned, registry.ArtifactTypeAttestation)
	if err != nil {
		return nil, err
	}
	for _, r := range refs {
		env, err := c.Registry.FetchArtifact(ctx, repo+"@"+r.Digest.String())
		if err != nil {
			continue
		}
		st, err := attest.Verify(env, c.Config.PublicKey)
		if err != nil {
			continue
		}
		if d, err := st.SubjectDigest(); err != nil || d != desc.Digest.String() {
			continue
		}
		if st.Predicate.Source.Digest != sourceDigest {
			continue
		}
		ref, err := patchRefFromStatement(st, pinned, desc.Digest.String(), im.Manifests, sourcePlatforms, c.Config.Platforms)
		if err != nil {
			return nil, err
		}
		return ref, nil
	}
	return nil, fmt.Errorf("%s:%s has no valid attestation from the trusted key", repo, tag)
}

func patchRefFromStatement(st *attest.Statement, pinned, digest string, manifests []v1.Descriptor, sourcePlatforms []v1alpha1.PlatformDigest, required []string) (*v1alpha1.PatchRef, error) {
	inIndex := map[string]bool{}
	for _, m := range manifests {
		inIndex[m.Digest.String()] = true
	}
	srcByPlatform := map[string]string{}
	for _, p := range sourcePlatforms {
		srcByPlatform[p.Platform] = p.Digest
	}
	ref := &v1alpha1.PatchRef{Ref: pinned, Tag: st.Predicate.Tag, Digest: digest, Revision: st.Predicate.Revision}
	covered := map[string]bool{}
	for _, p := range st.Predicate.Platforms {
		if !inIndex[p.PatchedDigest] {
			return nil, fmt.Errorf("attestation names %s, which isn't in the index", p.PatchedDigest)
		}
		if want, ok := srcByPlatform[p.Platform]; ok && want != p.SourceDigest {
			return nil, fmt.Errorf("attestation says %s was built from %s, source has %s", p.Platform, p.SourceDigest, want)
		}
		covered[p.Platform] = true
		ref.Platforms = append(ref.Platforms, v1alpha1.PlatformPatch{
			Platform: p.Platform, SourceDigest: p.SourceDigest, PatchedDigest: p.PatchedDigest,
			RemainingFixable: p.RemainingFixable,
		})
	}
	if len(inIndex) != len(st.Predicate.Platforms) {
		return nil, fmt.Errorf("index has %d manifests but the attestation covers %d", len(inIndex), len(st.Predicate.Platforms))
	}
	for _, p := range required {
		if _, inSource := srcByPlatform[p]; inSource && !covered[p] {
			return nil, fmt.Errorf("patch doesn't cover required platform %s", p)
		}
	}
	slices.SortFunc(ref.Platforms, func(a, b v1alpha1.PlatformPatch) int {
		if a.Platform < b.Platform {
			return -1
		}
		return 1
	})
	return ref, nil
}
