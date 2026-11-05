package worker

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/sozercan/copacabana/api/v1alpha1"
	"github.com/sozercan/copacabana/internal/attest"
	"github.com/sozercan/copacabana/internal/registry"
	"github.com/sozercan/copacabana/internal/trivy"
)

// Version is the worker version recorded in attestations.
var Version = "dev"

// labelsCopaMayChange are config labels Copa sets on patched images.
var labelsCopaMayChange = []string{"BaseImage"}

// Verify checks every staged platform image against its source, assembles an index of the patched
// platforms, pushes it to the patched repository with a signed attestation, and returns the result.
// It never runs the images it checks.
// Signer adds a signature to a pinned patched index before it's tagged, such as a Notation signature for the
// cluster's image integrity policy. It may be nil.
type Signer func(ctx context.Context, pinned string) error

func Verify(ctx context.Context, reg *registry.Client, sc trivy.Scanner, spec VerifySpec, key ed25519.PrivateKey, sign Signer) (*VerifyResult, error) {
	if len(spec.Platforms) == 0 {
		return nil, fmt.Errorf("nothing to verify")
	}
	var (
		records []attest.PlatformRecord
		addends []mutate.IndexAddendum
		result  = &VerifyResult{}
	)
	for _, p := range spec.Platforms {
		rec, img, err := verifyPlatform(ctx, reg, sc, spec, p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p.Platform, err)
		}
		plat, err := registry.ParsePlatform(p.Platform)
		if err != nil {
			return nil, err
		}
		mt, err := img.MediaType()
		if err != nil {
			return nil, err
		}
		addends = append(addends, mutate.IndexAddendum{Add: img, Descriptor: v1.Descriptor{MediaType: mt, Platform: plat}})
		records = append(records, *rec)
		result.Platforms = append(result.Platforms, v1alpha1.PlatformPatch{
			Platform: p.Platform, SourceDigest: p.SourceDigest, PatchedDigest: rec.PatchedDigest,
			RemainingFixable: rec.RemainingFixable,
		})
	}

	rev, err := nextRevision(ctx, reg, spec.PatchedRepository, spec.TagBase)
	if err != nil {
		return nil, err
	}
	tag := fmt.Sprintf("%s-%d", spec.TagBase, rev)

	// Copy each verified platform image into the patched repository, then index only those.
	for i, p := range spec.Platforms {
		if err := reg.WriteImage(ctx, spec.PatchedRepository+"@"+records[i].PatchedDigest, addends[i].Add.(v1.Image)); err != nil {
			return nil, fmt.Errorf("promoting %s: %w", p.Platform, err)
		}
	}
	idx := mutate.IndexMediaType(empty.Index, types.OCIImageIndex)
	idx = mutate.AppendManifests(idx, addends...)
	annotated, ok := mutate.Annotations(idx, map[string]string{
		v1alpha1.IndexAnnotationSourceDigest: spec.SourceDigest,
		v1alpha1.IndexAnnotationSourceRef:    spec.SourceReference,
		v1alpha1.IndexAnnotationRevision:     strconv.Itoa(rev),
	}).(v1.ImageIndex)
	if !ok {
		return nil, fmt.Errorf("annotating index")
	}
	digest, err := annotated.Digest()
	if err != nil {
		return nil, err
	}
	// Publish by digest first. The catalog only looks at tags, so nothing sees this revision until the
	// attestation and signatures are attached and the tag is written last.
	if err := reg.WriteIndex(ctx, spec.PatchedRepository+"@"+digest.String(), annotated); err != nil {
		return nil, fmt.Errorf("writing patched index: %w", err)
	}
	size, err := annotated.Size()
	if err != nil {
		return nil, err
	}

	// Re-attach each platform's VEX in the patched repository; referrers don't cross repositories.
	for i, p := range spec.Platforms {
		if p.VEXRef == "" {
			continue
		}
		vex, err := reg.FetchArtifact(ctx, p.VEXRef)
		if err != nil {
			return nil, err
		}
		pd, err := v1.NewHash(records[i].PatchedDigest)
		if err != nil {
			return nil, err
		}
		mt, _ := addends[i].Add.(v1.Image).MediaType()
		sz, _ := addends[i].Add.(v1.Image).Size()
		ref, err := reg.PushArtifact(ctx, spec.PatchedRepository, registry.ArtifactTypeVEX, registry.MediaTypeVEX, vex,
			&v1.Descriptor{MediaType: mt, Digest: pd, Size: sz}, nil, "")
		if err != nil {
			return nil, err
		}
		records[i].VEXRef = ref
	}

	st := &attest.Statement{
		Type:          attest.StatementType,
		Subject:       []attest.Subject{{Name: spec.PatchedRepository, Digest: map[string]string{"sha256": digest.Hex}}},
		PredicateType: attest.PredicateType,
		Predicate: attest.Predicate{
			Source:    attest.Source{Name: spec.SourceReference, Digest: spec.SourceDigest},
			Revision:  rev,
			Tag:       tag,
			Platforms: records,
			Builder:   attest.Builder{PatchBuild: spec.PatchBuild, Worker: Version},
			Verification: attest.VerificationRecord{
				LayerCheck: "passed", ConfigCheck: "passed", PostPatchScan: "passed",
			},
		},
	}
	env, err := attest.Sign(st, key)
	if err != nil {
		return nil, err
	}
	if _, err := reg.PushArtifact(ctx, spec.PatchedRepository, registry.ArtifactTypeAttestation, registry.MediaTypeDSSE, env,
		&v1.Descriptor{MediaType: types.OCIImageIndex, Digest: digest, Size: size}, nil, ""); err != nil {
		return nil, fmt.Errorf("attaching attestation: %w", err)
	}
	pinned := spec.PatchedRepository + "@" + digest.String()
	if sign != nil {
		if err := sign(ctx, pinned); err != nil {
			return nil, fmt.Errorf("signing %s: %w", pinned, err)
		}
	}
	if err := reg.WriteIndex(ctx, spec.PatchedRepository+":"+tag, annotated); err != nil {
		return nil, fmt.Errorf("tagging patched index: %w", err)
	}

	result.Ref = spec.PatchedRepository + "@" + digest.String()
	result.Tag = tag
	result.Digest = digest.String()
	result.Revision = rev
	return result, nil
}

func verifyPlatform(ctx context.Context, reg *registry.Client, sc trivy.Scanner, spec VerifySpec, p VerifyPlatform) (*attest.PlatformRecord, v1.Image, error) {
	src, err := reg.Image(ctx, spec.SourceRepository+"@"+p.SourceDigest)
	if err != nil {
		return nil, nil, fmt.Errorf("reading source: %w", err)
	}
	staged, err := reg.Image(ctx, p.StagedRef)
	if err != nil {
		return nil, nil, fmt.Errorf("reading staged image: %w", err)
	}
	if err := CheckLayers(src, staged); err != nil {
		return nil, nil, err
	}
	if err := CheckConfig(src, staged); err != nil {
		return nil, nil, err
	}

	srcRaw, err := reg.FetchArtifact(ctx, p.ReportRef)
	if err != nil {
		return nil, nil, err
	}
	srcReport, err := trivy.Parse(srcRaw)
	if err != nil {
		return nil, nil, err
	}
	claimed := map[string]bool{}
	if p.VEXRef != "" {
		vex, err := reg.FetchArtifact(ctx, p.VEXRef)
		if err != nil {
			return nil, nil, err
		}
		if claimed, err = VEXFixed(vex); err != nil {
			return nil, nil, err
		}
	}
	patchedRaw, err := sc.Scan(ctx, p.StagedRef)
	if err != nil {
		return nil, nil, fmt.Errorf("post-patch scan: %w", err)
	}
	patchedReport, err := trivy.Parse(patchedRaw)
	if err != nil {
		return nil, nil, err
	}
	fixed, err := CheckScan(srcReport, patchedReport, claimed, spec.AllowNewVulnerabilities)
	if err != nil {
		return nil, nil, err
	}
	if n := len(srcReport.Fixable()); n > 0 && len(fixed) == 0 {
		return nil, nil, fmt.Errorf("post-patch scan: none of the source's %d fixable vulnerabilities was confirmed fixed", n)
	}
	d, err := staged.Digest()
	if err != nil {
		return nil, nil, err
	}
	return &attest.PlatformRecord{
		Platform:         p.Platform,
		SourceDigest:     p.SourceDigest,
		PatchedDigest:    d.String(),
		ReportRef:        p.ReportRef,
		Fixed:            fixed,
		RemainingFixable: len(patchedReport.Fixable()),
	}, staged, nil
}

// CheckLayers requires the patched image to keep every source layer unchanged and only append layers.
func CheckLayers(src, patched v1.Image) error {
	s, err := layerDigests(src)
	if err != nil {
		return err
	}
	p, err := layerDigests(patched)
	if err != nil {
		return err
	}
	if len(p) <= len(s) {
		return fmt.Errorf("layer check: patched image has %d layers, source has %d; expected appended layers", len(p), len(s))
	}
	for i := range s {
		if s[i] != p[i] {
			return fmt.Errorf("layer check: layer %d differs from source (%s != %s)", i, p[i], s[i])
		}
	}
	return nil
}

// CheckConfig requires the image config to match the source except for labels Copa sets. The whole runtime
// config is compared (entrypoint, env, user, healthcheck, shell, onbuild and the rest), not a list of fields.
func CheckConfig(src, patched v1.Image) error {
	sc, err := src.ConfigFile()
	if err != nil {
		return err
	}
	pc, err := patched.ConfigFile()
	if err != nil {
		return err
	}
	if sc.OS != pc.OS || sc.Architecture != pc.Architecture || sc.Variant != pc.Variant || sc.OSVersion != pc.OSVersion ||
		!slices.Equal(sc.OSFeatures, pc.OSFeatures) {
		return fmt.Errorf("config check: platform changed from %s/%s to %s/%s", sc.OS, sc.Architecture, pc.OS, pc.Architecture)
	}
	a, b := runtimeConfig(sc.Config), runtimeConfig(pc.Config)
	if reflect.DeepEqual(a, b) {
		return nil
	}
	return fmt.Errorf("config check: patched image changed %s", strings.Join(configDiff(a, b), ", "))
}

// runtimeConfig returns c without the labels Copa may change, with empty collections as nil.
func runtimeConfig(c v1.Config) v1.Config {
	c.Labels = maps.Clone(c.Labels)
	for _, k := range labelsCopaMayChange {
		delete(c.Labels, k)
	}
	if len(c.Labels) == 0 {
		c.Labels = nil
	}
	if len(c.ExposedPorts) == 0 {
		c.ExposedPorts = nil
	}
	if len(c.Volumes) == 0 {
		c.Volumes = nil
	}
	return c
}

// configDiff names the top-level config fields that differ, for the error message.
func configDiff(a, b v1.Config) []string {
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	var out []string
	for i := range va.NumField() {
		if !reflect.DeepEqual(va.Field(i).Interface(), vb.Field(i).Interface()) {
			out = append(out, va.Type().Field(i).Name)
		}
	}
	return out
}

// CheckScan compares the source and patched scans. Every vulnerability the build claimed fixed must be gone,
// and, unless allowed, no vulnerability may appear that the source didn't have. It returns the confirmed fixes.
func CheckScan(src, patched *trivy.Report, claimedFixed map[string]bool, allowNew bool) ([]string, error) {
	// A missing finding only means "fixed" if the patched image could be assessed. An appended layer that removes
	// or replaces the package database would otherwise make every vulnerability look fixed.
	if src.OSFamily() != "" && patched.OSFamily() != src.OSFamily() {
		return nil, fmt.Errorf("post-patch scan: source OS is %q but the patched image scans as %q", src.OSFamily(), patched.OSFamily())
	}
	srcKeys := src.Keys()
	patchedKeys := patched.Keys()
	var stillPresent, introduced, fixed []string
	for k := range claimedFixed {
		if patchedKeys[k] {
			stillPresent = append(stillPresent, k)
		} else if srcKeys[k] {
			fixed = append(fixed, k)
		}
	}
	for k := range patchedKeys {
		if !srcKeys[k] {
			introduced = append(introduced, k)
		}
	}
	sort.Strings(stillPresent)
	sort.Strings(introduced)
	sort.Strings(fixed)
	if len(stillPresent) > 0 {
		return nil, fmt.Errorf("post-patch scan: %d vulnerabilities claimed fixed are still present: %s",
			len(stillPresent), strings.Join(first(stillPresent, 5), ", "))
	}
	if len(introduced) > 0 && !allowNew {
		return nil, fmt.Errorf("post-patch scan: %d vulnerabilities not in the source appeared: %s",
			len(introduced), strings.Join(first(introduced, 5), ", "))
	}
	return fixed, nil
}

func first(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// nextRevision returns one more than the highest existing revision for tagBase in repo.
func nextRevision(ctx context.Context, reg *registry.Client, repo, tagBase string) (int, error) {
	tags, err := reg.ListTags(ctx, repo)
	if err != nil {
		return 0, err
	}
	return MaxRevision(tags, tagBase) + 1, nil
}

// MaxRevision returns the highest N among tags of the form tagBase-N, or 0.
func MaxRevision(tags []string, tagBase string) int {
	re := regexp.MustCompile("^" + regexp.QuoteMeta(tagBase) + `-(\d+)$`)
	maxRev := 0
	for _, t := range tags {
		if m := re.FindStringSubmatch(t); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n > maxRev {
				maxRev = n
			}
		}
	}
	return maxRev
}
