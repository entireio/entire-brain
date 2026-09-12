package brainwire

import (
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"
)

const foldTestArtifact = `{"manifest":{"repo_key":"honest","generated_at":"2026-01-02T03:04:05Z","brain_schema_version":"1.0"}}`

// Regression: canonical form used to PASS THROUGH an unknown member that folds
// to a contract field ("manIfest"), because it is not a known key by exact
// match. Go's encoding/json matches struct fields case-insensitively, so the very
// same bytes decoded to two different artifacts depending on which decoder read
// them — and VerifyBytes accepted them, since the unknown key survived
// canonicalization verbatim. One signature, two meanings, chosen by the
// consumer's parser. Found by FuzzUnknownFieldsCannotSmuggle.
func TestCanonicalizeRejectsCaseFoldedContractKeys(t *testing.T) {
	for _, raw := range []string{
		`{"manIfest":{"repo_key":"EVIL","generated_at":"2026-01-02T03:04:05Z","brain_schema_version":"1.0"}}`,
		`{"MANIFEST":{"repo_key":"EVIL","generated_at":"2026-01-02T03:04:05Z","brain_schema_version":"1.0"}}`,
		`{"manifest":{"repo_key":"honest","Repo_Key":"EVIL","generated_at":"2026-01-02T03:04:05Z","brain_schema_version":"1.0"}}`,
		`{"manifest":{"repo_key":"honest","generated_at":"2026-01-02T03:04:05Z","brain_schema_version":"1.0"},"Snapshots":[]}`,
	} {
		if _, err := Canonicalize([]byte(raw)); err == nil {
			t.Errorf("Canonicalize accepted a case-folded contract key: %s", raw)
		} else if !strings.Contains(err.Error(), "case-folds") {
			t.Errorf("error should name the collision, got %v", err)
		}
	}
}

// The smuggling scenario end to end: a signature over the honest bytes must not
// verify bytes whose meaning depends on the decoder.
func TestSignedArtifactCannotBeReinterpretedByFolding(t *testing.T) {
	signer, err := GenerateEd25519Signer(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateEd25519Signer: %v", err)
	}
	canon, err := Canonicalize([]byte(foldTestArtifact))
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	sig, err := SignCanonical(canon, signer)
	if err != nil {
		t.Fatalf("SignCanonical: %v", err)
	}

	smuggled := []byte(`{"manIfest":{"repo_key":"EVIL","generated_at":"2026-01-02T03:04:05Z","brain_schema_version":"1.0"}}`)
	// The plain Go decoder still reads the smuggled key as the manifest — that is
	// encoding/json's documented behavior and is exactly why the canonical layer
	// must refuse the bytes.
	var viaPlainJSON BrainArtifact
	if err := json.Unmarshal(smuggled, &viaPlainJSON); err != nil {
		t.Fatalf("plain decode: %v", err)
	}
	if viaPlainJSON.Manifest.RepoKey != "EVIL" {
		t.Fatalf("precondition changed: encoding/json no longer folds; got %q", viaPlainJSON.Manifest.RepoKey)
	}
	// The signed path must refuse it outright.
	if err := VerifyBytes(smuggled, sig, signer.Public()); err == nil {
		t.Fatal("VerifyBytes accepted bytes whose meaning depends on the decoder")
	}
	var viaCanonical BrainArtifact
	if err := CanonicalUnmarshal(smuggled, &viaCanonical); err == nil {
		t.Fatalf("CanonicalUnmarshal accepted a fold-colliding artifact: %#v", viaCanonical)
	}
}

// Genuinely unknown newer-minor fields must STILL pass through: the fix must not
// cost the additive-only tolerance ADR 0001 requires.
func TestUnknownFieldsStillPassThrough(t *testing.T) {
	raw := `{"manifest":{"repo_key":"r","generated_at":"2026-01-02T03:04:05Z","brain_schema_version":"1.0","future_field":"x"},"future_top":{"b":1,"a":2}}`
	canon, err := Canonicalize([]byte(raw))
	if err != nil {
		t.Fatalf("Canonicalize rejected legitimate unknown fields: %v", err)
	}
	for _, want := range []string{`"future_field":"x"`, `"future_top":{"a":2,"b":1}`} {
		if !strings.Contains(string(canon), want) {
			t.Errorf("canonical output dropped %s: %s", want, canon)
		}
	}
	twice, err := Canonicalize(canon)
	if err != nil || string(twice) != string(canon) {
		t.Fatalf("canonical form is not a fixed point with unknown fields: %v", err)
	}
}
