// Package attest builds, signs and verifies the in-toto attestation the verify Job attaches to every promoted patch.
package attest

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
)

// Types used in the statement and envelope.
const (
	StatementType = "https://in-toto.io/Statement/v1"
	PredicateType = "https://copa.sh/attestation/patch/v1"
	PayloadType   = "application/vnd.in-toto+json"
)

// Statement is an in-toto v1 statement with the patch predicate.
type Statement struct {
	Type          string    `json:"_type"`
	Subject       []Subject `json:"subject"`
	PredicateType string    `json:"predicateType"`
	Predicate     Predicate `json:"predicate"`
}

// Subject names the patched index.
type Subject struct {
	Name   string            `json:"name"`
	Digest map[string]string `json:"digest"`
}

// Predicate records how a patch was built and what verification observed.
type Predicate struct {
	Source       Source             `json:"source"`
	Revision     int                `json:"revision"`
	Tag          string             `json:"tag"`
	Platforms    []PlatformRecord   `json:"platforms"`
	Builder      Builder            `json:"builder"`
	Verification VerificationRecord `json:"verification"`
}

// Source is the image the patch was built from.
type Source struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

// PlatformRecord is one platform of the patch.
type PlatformRecord struct {
	Platform      string `json:"platform"`
	SourceDigest  string `json:"sourceDigest"`
	PatchedDigest string `json:"patchedDigest"`
	ReportRef     string `json:"reportRef"`
	VEXRef        string `json:"vexRef,omitempty"`
	// Fixed lists vulnerability/package pairs the build claimed fixed and the rescan confirmed gone.
	Fixed []string `json:"fixed"`
	// RemainingFixable counts fixable vulnerabilities still present after patching.
	RemainingFixable int `json:"remainingFixable"`
}

// Builder records the tools and build that produced the patch.
type Builder struct {
	PatchBuild string `json:"patchBuild"`
	Worker     string `json:"worker"`
}

// VerificationRecord records the checks the verify Job ran.
type VerificationRecord struct {
	LayerCheck    string `json:"layerCheck"`
	ConfigCheck   string `json:"configCheck"`
	PostPatchScan string `json:"postPatchScan"`
}

// Envelope is a DSSE envelope.
type Envelope struct {
	PayloadType string      `json:"payloadType"`
	Payload     string      `json:"payload"`
	Signatures  []Signature `json:"signatures"`
}

// Signature is one DSSE signature.
type Signature struct {
	KeyID string `json:"keyid"`
	Sig   string `json:"sig"`
}

// pae is the DSSE pre-authentication encoding.
func pae(payloadType string, payload []byte) []byte {
	return fmt.Appendf(nil, "DSSEv1 %d %s %d %s", len(payloadType), payloadType, len(payload), payload)
}

// KeyID derives a short key ID from a public key.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// Sign marshals and signs a statement.
func Sign(st *Statement, priv ed25519.PrivateKey) ([]byte, error) {
	payload, err := json.Marshal(st)
	if err != nil {
		return nil, err
	}
	sig := ed25519.Sign(priv, pae(PayloadType, payload))
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("unexpected public key type")
	}
	return json.Marshal(Envelope{
		PayloadType: PayloadType,
		Payload:     base64.StdEncoding.EncodeToString(payload),
		Signatures:  []Signature{{KeyID: KeyID(pub), Sig: base64.StdEncoding.EncodeToString(sig)}},
	})
}

// Verify checks an envelope against a trusted public key and returns the statement.
func Verify(envelope []byte, pub ed25519.PublicKey) (*Statement, error) {
	var env Envelope
	if err := json.Unmarshal(envelope, &env); err != nil {
		return nil, fmt.Errorf("parsing envelope: %w", err)
	}
	if env.PayloadType != PayloadType {
		return nil, fmt.Errorf("unexpected payload type %q", env.PayloadType)
	}
	payload, err := base64.StdEncoding.DecodeString(env.Payload)
	if err != nil {
		return nil, fmt.Errorf("decoding payload: %w", err)
	}
	verified := false
	for _, s := range env.Signatures {
		sig, err := base64.StdEncoding.DecodeString(s.Sig)
		if err != nil {
			continue
		}
		if ed25519.Verify(pub, pae(env.PayloadType, payload), sig) {
			verified = true
			break
		}
	}
	if !verified {
		return nil, errors.New("no valid signature from the trusted key")
	}
	var st Statement
	if err := json.Unmarshal(payload, &st); err != nil {
		return nil, fmt.Errorf("parsing statement: %w", err)
	}
	if st.Type != StatementType || st.PredicateType != PredicateType {
		return nil, fmt.Errorf("unexpected statement type %q / predicate %q", st.Type, st.PredicateType)
	}
	return &st, nil
}

// SubjectDigest returns the sha256 digest of the statement's single subject as sha256:hex.
func (st *Statement) SubjectDigest() (string, error) {
	if len(st.Subject) != 1 {
		return "", fmt.Errorf("expected one subject, got %d", len(st.Subject))
	}
	h, ok := st.Subject[0].Digest["sha256"]
	if !ok {
		return "", errors.New("subject has no sha256 digest")
	}
	return "sha256:" + h, nil
}

// GenerateKey returns a new key pair as PEM.
func GenerateKey() (privPEM, pubPEM []byte, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}),
		pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), nil
}

// ParsePrivateKey parses a PEM ed25519 private key.
func ParsePrivateKey(data []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("no PEM block in private key")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not ed25519")
	}
	return priv, nil
}

// ParsePublicKey parses a PEM ed25519 public key.
func ParsePublicKey(data []byte) (ed25519.PublicKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("no PEM block in public key")
	}
	k, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	pub, ok := k.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("public key is not ed25519")
	}
	return pub, nil
}

// SubjectName strips a tag or digest from a reference for use as a subject name.
func SubjectName(repo string) string {
	if i := strings.Index(repo, "@"); i >= 0 {
		return repo[:i]
	}
	return repo
}
