package attest

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func testStatement() *Statement {
	return &Statement{
		Type:          StatementType,
		Subject:       []Subject{{Name: "kind-registry:5000/patched/library/nginx", Digest: map[string]string{"sha256": "9b1c"}}},
		PredicateType: PredicateType,
		Predicate: Predicate{
			Source:   Source{Name: "kind-registry:5000/library/nginx:1.25.3-alpine", Digest: "sha256:4f7a"},
			Revision: 2,
		},
	}
}

func TestSignVerifyRoundTrip(t *testing.T) {
	privPEM, pubPEM, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	priv, err := ParsePrivateKey(privPEM)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ParsePublicKey(pubPEM)
	if err != nil {
		t.Fatal(err)
	}
	env, err := Sign(testStatement(), priv)
	if err != nil {
		t.Fatal(err)
	}
	st, err := Verify(env, pub)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if d, _ := st.SubjectDigest(); d != "sha256:9b1c" {
		t.Errorf("SubjectDigest = %q", d)
	}
	if st.Predicate.Revision != 2 {
		t.Errorf("Revision = %d", st.Predicate.Revision)
	}
}

func TestVerifyRejectsOtherKey(t *testing.T) {
	privPEM, _, _ := GenerateKey()
	_, otherPub, _ := GenerateKey()
	priv, _ := ParsePrivateKey(privPEM)
	pub, _ := ParsePublicKey(otherPub)
	env, _ := Sign(testStatement(), priv)
	if _, err := Verify(env, pub); err == nil {
		t.Fatal("Verify accepted a signature from an untrusted key")
	}
}

func TestVerifyRejectsTamperedPayload(t *testing.T) {
	privPEM, pubPEM, _ := GenerateKey()
	priv, _ := ParsePrivateKey(privPEM)
	pub, _ := ParsePublicKey(pubPEM)
	raw, _ := Sign(testStatement(), priv)

	var env Envelope
	_ = json.Unmarshal(raw, &env)
	st := testStatement()
	st.Predicate.Source.Digest = "sha256:evil"
	payload, _ := json.Marshal(st)
	env.Payload = base64.StdEncoding.EncodeToString(payload)
	tampered, _ := json.Marshal(env)

	if _, err := Verify(tampered, pub); err == nil {
		t.Fatal("Verify accepted a tampered payload")
	}
}
