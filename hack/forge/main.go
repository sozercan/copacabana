// Command forge plants untrusted patches in the patched repository to check that the controller ignores them.
// It is a test tool for the edge-case run, not part of the product.
//
//	-3  modified index, genuine attestation of the trusted revision replayed onto it (subject mismatch)
//	-4  modified index, attestation signed by a key the controller doesn't trust
//	-5  the trusted index re-tagged (same digest; must not start a new rollout)
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"

	"github.com/sozercan/copa-controller/internal/attest"
	"github.com/sozercan/copa-controller/internal/registry"
)

func main() {
	repo := flag.String("repo", "localhost:5001/patched/kind-registry-5000/library/nginx", "patched repository")
	trusted := flag.String("trusted", "1.25.3-alpine-2", "tag of the genuine, trusted revision")
	base := flag.String("base", "1.25.3-alpine", "tag base for the forged revisions")
	flag.Parse()
	ctx := context.Background()
	host := strings.SplitN(*repo, "/", 2)[0]
	reg := registry.New([]string{host})

	desc, err := reg.Get(ctx, *repo+":"+*trusted)
	must(err)
	idx, err := desc.ImageIndex()
	must(err)
	pinned := *repo + "@" + desc.Digest.String()
	refs, err := reg.Referrers(ctx, pinned, registry.ArtifactTypeAttestation)
	must(err)
	if len(refs) == 0 {
		log.Fatalf("%s has no attestation", pinned)
	}
	env, err := reg.FetchArtifact(ctx, *repo+"@"+refs[0].Digest.String())
	must(err)

	// -3: replay the genuine envelope onto a different index.
	forged := forge(ctx, reg, idx, *repo+":"+*base+"-3", "replay")
	_, err = reg.PushArtifact(ctx, *repo, registry.ArtifactTypeAttestation, registry.MediaTypeDSSE, env, forged, nil, "")
	must(err)
	fmt.Println("-3 forged index", forged.Digest, "with replayed genuine attestation")

	// -4: sign a correct-looking statement with an attacker key.
	forged = forge(ctx, reg, idx, *repo+":"+*base+"-4", "attacker-key")
	privPEM, _, err := attest.GenerateKey()
	must(err)
	priv, err := attest.ParsePrivateKey(privPEM)
	must(err)
	st := decodeUnverified(env)
	st.Subject[0].Digest = map[string]string{"sha256": forged.Digest.Hex}
	st.Predicate.Revision, st.Predicate.Tag = 4, *base+"-4"
	signed, err := attest.Sign(st, priv)
	must(err)
	_, err = reg.PushArtifact(ctx, *repo, registry.ArtifactTypeAttestation, registry.MediaTypeDSSE, signed, forged, nil, "")
	must(err)
	fmt.Println("-4 forged index", forged.Digest, "signed by an untrusted key")

	// -5: same content as the trusted revision under a newer tag.
	must(reg.WriteIndex(ctx, *repo+":"+*base+"-5", idx))
	fmt.Println("-5 re-tagged", desc.Digest)
}

func forge(ctx context.Context, reg *registry.Client, idx v1.ImageIndex, ref, why string) *v1.Descriptor {
	f := mutate.Annotations(idx, map[string]string{"forged": why}).(v1.ImageIndex)
	must(reg.WriteIndex(ctx, ref, f))
	d, err := f.Digest()
	must(err)
	mt, err := f.MediaType()
	must(err)
	raw, err := f.RawManifest()
	must(err)
	return &v1.Descriptor{MediaType: mt, Digest: d, Size: int64(len(raw))}
}

// decodeUnverified reads the statement out of an envelope without checking its signature.
func decodeUnverified(env []byte) *attest.Statement {
	var e attest.Envelope
	must(json.Unmarshal(env, &e))
	payload, err := base64.StdEncoding.DecodeString(e.Payload)
	must(err)
	var st attest.Statement
	must(json.Unmarshal(payload, &st))
	return &st
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
