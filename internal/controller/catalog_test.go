package controller

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/sozercan/copacabana/api/v1alpha1"
	"github.com/sozercan/copacabana/internal/attest"
	"github.com/sozercan/copacabana/internal/registry"
)

const (
	catalogSourceRepo   = "kind-registry:5000/library/nginx"
	catalogSourceDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	catalogPlatformSrc  = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	catalogReference    = catalogSourceRepo + ":1.25.3-alpine"
)

type catalogFixture struct {
	t    *testing.T
	ctx  context.Context
	reg  *registry.Client
	cat  *Catalog
	repo string
	key  ed25519.PrivateKey
}

func newCatalogFixture(t *testing.T) *catalogFixture {
	t.Helper()
	srv := httptest.NewServer(referrersArtifactType(ggcrregistry.New(ggcrregistry.WithReferrersSupport(true))))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{PatchedPrefix: host + "/patched", Platforms: []string{"linux/amd64"}, PublicKey: pub}
	reg := registry.New([]string{host})
	return &catalogFixture{
		t: t, ctx: context.Background(), reg: reg, key: priv,
		cat:  &Catalog{Registry: reg, Config: cfg},
		repo: cfg.PatchedRepository(catalogSourceRepo),
	}
}

// referrersArtifactType works around go-containerregistry's test registry, which reports a referrer's config
// media type as its artifactType instead of the manifest's artifactType (OCI 1.1; Zot gets this right). Every
// referrer in these tests is an attestation.
func referrersArtifactType(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/referrers/") {
			h.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		body := strings.ReplaceAll(rec.Body.String(), `"artifactType":"`+registry.EmptyConfigMediaType+`"`,
			`"artifactType":"`+registry.ArtifactTypeAttestation+`"`)
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.Header().Del("Content-Length")
		w.WriteHeader(rec.Code)
		_, _ = w.Write([]byte(body))
	})
}

// publish pushes a signed patch the way the verify Job does and returns its index digest.
func (f *catalogFixture) publish(rev int) string {
	f.t.Helper()
	img, err := random.Image(64, 2)
	if err != nil {
		f.t.Fatal(err)
	}
	imgDigest, _ := img.Digest()
	if err := f.reg.WriteImage(f.ctx, f.repo+"@"+imgDigest.String(), img); err != nil {
		f.t.Fatal(err)
	}
	idx := mutate.AppendManifests(mutate.IndexMediaType(empty.Index, types.OCIImageIndex), mutate.IndexAddendum{
		Add: img, Descriptor: v1.Descriptor{MediaType: types.OCIManifestSchema1, Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}},
	})
	idx = mutate.Annotations(idx, map[string]string{
		v1alpha1.IndexAnnotationSourceDigest: catalogSourceDigest,
		v1alpha1.IndexAnnotationRevision:     strconv.Itoa(rev),
	}).(v1.ImageIndex)
	digest, _ := idx.Digest()
	size, _ := idx.Size()
	if err := f.reg.WriteIndex(f.ctx, f.repo+"@"+digest.String(), idx); err != nil {
		f.t.Fatal(err)
	}
	tag := fmt.Sprintf("1.25.3-alpine-%d", rev)
	env, err := attest.Sign(&attest.Statement{
		Type:          attest.StatementType,
		Subject:       []attest.Subject{{Name: f.repo, Digest: map[string]string{"sha256": digest.Hex}}},
		PredicateType: attest.PredicateType,
		Predicate: attest.Predicate{
			Source:   attest.Source{Name: catalogReference, Digest: catalogSourceDigest},
			Revision: rev, Tag: tag,
			Platforms: []attest.PlatformRecord{{Platform: "linux/amd64", SourceDigest: catalogPlatformSrc, PatchedDigest: imgDigest.String()}},
		},
	}, f.key)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.reg.PushArtifact(f.ctx, f.repo, registry.ArtifactTypeAttestation, registry.MediaTypeDSSE, env,
		&v1.Descriptor{MediaType: types.OCIImageIndex, Digest: digest, Size: size}, nil, ""); err != nil {
		f.t.Fatal(err)
	}
	f.tag(tag, digest.String())
	return digest.String()
}

func (f *catalogFixture) tag(tag, digest string) {
	f.t.Helper()
	desc, err := f.reg.Get(f.ctx, f.repo+"@"+digest)
	if err != nil {
		f.t.Fatal(err)
	}
	idx, err := desc.ImageIndex()
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.reg.WriteIndex(f.ctx, f.repo+":"+tag, idx); err != nil {
		f.t.Fatal(err)
	}
}

func (f *catalogFixture) latest() (*v1alpha1.PatchRef, error) {
	return f.cat.Latest(f.ctx, catalogSourceRepo, catalogSourceDigest, catalogReference,
		[]v1alpha1.PlatformDigest{{Platform: "linux/amd64", Digest: catalogPlatformSrc}})
}

// An older signed patch re-tagged with a higher revision must not replace the newest signed one.
func TestCatalogIgnoresRetaggedOlderPatch(t *testing.T) {
	f := newCatalogFixture(t)
	first := f.publish(1)
	second := f.publish(2)
	if ref, err := f.latest(); err != nil || ref == nil || ref.Digest != second {
		t.Fatalf("latest = %+v, %v; want revision 2 (%s)", ref, err, second)
	}

	f.tag("1.25.3-alpine-3", first)
	ref, err := f.latest()
	if ref == nil || ref.Digest != second || ref.Revision != 2 {
		t.Fatalf("latest after re-tag = %+v; want revision 2 (%s)", ref, second)
	}
	if err == nil || !strings.Contains(err.Error(), "1.25.3-alpine-3") {
		t.Fatalf("want an error naming the re-tagged revision, got %v", err)
	}
}

// Without a trusted key (report-only mode), patches don't verify; they don't panic.
func TestCatalogWithoutKey(t *testing.T) {
	f := newCatalogFixture(t)
	f.publish(1)
	f.cat.Config.PublicKey = nil
	if ref, err := f.latest(); ref != nil || err == nil {
		t.Fatalf("latest = %+v, %v; want nothing verified", ref, err)
	}
}

// After a signing-key rotation, the persisted current patch no longer verifies. It must not be offered to a
// newly opted-in namespace or used as the baseline of the next build.
func TestRotatedKeyCurrentNotOffered(t *testing.T) {
	f := newCatalogFixture(t)
	digest := f.publish(1)
	old, err := f.latest()
	if err != nil || old == nil || old.Digest != digest {
		t.Fatalf("setup: %+v, %v", old, err)
	}
	rotated, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.cat.Config.PublicKey = rotated
	f.cat.Config.Mode = ModeBuilder
	f.cat.Config.BuilderNamespace = "copa-builder"
	f.cat.Config.MaxConcurrentBuilds = 1
	f.cat.Config.RescanInterval = time.Hour

	si := &v1alpha1.SourceImage{
		ObjectMeta: metav1.ObjectMeta{Name: "nginx-src"},
		Spec: v1alpha1.SourceImageSpec{
			Repository: catalogSourceRepo, Digest: catalogSourceDigest,
			Platforms: []v1alpha1.PlatformDigest{{Platform: "linux/amd64", Digest: catalogPlatformSrc}},
		},
		Status: v1alpha1.SourceImageStatus{
			References: []string{catalogReference},
			UsedBy:     []v1alpha1.WorkloadRef{{Namespace: "new-team", Name: "web", Reference: catalogReference}},
			Current:    old,
		},
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(si).WithStatusSubresource(si).Build()
	r := &SourceImageReconciler{Client: c, Config: f.cat.Config, Catalog: f.cat, Recorder: events.NewFakeRecorder(100)}
	if _, err := r.Reconcile(f.ctx, reconcile.Request{NamespacedName: k8stypes.NamespacedName{Name: si.Name}}); err != nil {
		t.Fatal(err)
	}

	var ros v1alpha1.PatchRolloutList
	if err := c.List(f.ctx, &ros); err != nil {
		t.Fatal(err)
	}
	if len(ros.Items) != 0 {
		t.Fatalf("rollout created for a patch that no longer verifies: %+v", ros.Items[0].Spec.Candidate)
	}
	var builds v1alpha1.PatchBuildList
	if err := c.List(f.ctx, &builds); err != nil {
		t.Fatal(err)
	}
	if len(builds.Items) != 1 || builds.Items[0].Spec.Baseline != nil {
		t.Fatalf("want one build without a baseline, got %+v", builds.Items)
	}
	var got v1alpha1.SourceImage
	if err := c.Get(f.ctx, k8stypes.NamespacedName{Name: si.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if cond := meta.FindStatusCondition(got.Status.Conditions, "Ready"); cond == nil || cond.Reason != "Unverified" {
		t.Fatalf("Ready condition = %+v, want Unverified", cond)
	}
}
