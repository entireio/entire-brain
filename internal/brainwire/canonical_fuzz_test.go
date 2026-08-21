package brainwire

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"testing"
	"unicode/utf8"
)

// FuzzCanonicalizeIdempotent asserts the property the whole signing scheme rests on:
// canonical form is a FIXED POINT. If Canonicalize(Canonicalize(x)) can ever differ
// from Canonicalize(x), a signature verified over re-canonicalized bytes is verifying
// something other than what the signer signed.
//
// It also asserts every accepted output is itself acceptable and valid UTF-8, so the
// encoder can never emit bytes its own decoder rejects.
func FuzzCanonicalizeIdempotent(f *testing.F) {
	f.Add([]byte(`{"manifest":{"repo_key":"gh/o/r","generated_at":"2026-01-02T03:04:05Z","brain_schema_version":"1.0"}}`))
	f.Add([]byte(`{"manifest":{"repo_key":"a<b>c&d","generated_at":"2026-01-02T03:04:05.000Z","brain_schema_version":"1.0"}}`))
	f.Add([]byte(`{"manifest":{"brain_schema_version":"1.0","generated_at":"2026-01-02t03:04:05z","repo_key":"r"},"snapshots":[]}`))
	f.Add([]byte(`{"manifest":{"repo_key":"r","generated_at":"2026-01-02T03:04:05Z","brain_schema_version":"1.0"},"facts":[{"branch":"main","content":{"digest":"sha256:aa","size":0}}]}`))

	f.Fuzz(func(t *testing.T, raw []byte) {
		once, err := Canonicalize(raw)
		if err != nil {
			return // rejected input is fine; only accepted input carries obligations
		}
		if !utf8.Valid(once) {
			t.Fatalf("canonical output is not valid UTF-8: %q", once)
		}
		twice, err := Canonicalize(once)
		if err != nil {
			t.Fatalf("canonical output was rejected by its own canonicalizer: %v\ninput=%q\nonce=%q", err, raw, once)
		}
		if !bytes.Equal(once, twice) {
			t.Fatalf("canonical form is not a fixed point:\ninput=%q\nonce =%q\ntwice=%q", raw, once, twice)
		}
	})
}

// FuzzCanonicalMarshalRoundTrip asserts that the struct path and the bytes path
// agree: canonical bytes decode into a BrainArtifact and re-marshal to exactly the
// same bytes. A gap means a signature produced through one path would not verify
// through the other.
//
// Scoped deliberately to artifacts whose keys are ALL known. Unknown newer-minor
// fields pass through canonicalization on purpose (ADR 0001 additive-only: an older
// verifier must still be able to verify a newer producer's artifact), and the struct
// cannot represent them, so re-marshaling necessarily drops them. That is a property
// boundary, not a defect — asserting round-trip equality over such inputs would be
// asserting that tolerance is a bug.
func FuzzCanonicalMarshalRoundTrip(f *testing.F) {
	f.Add([]byte(`{"manifest":{"repo_key":"gh/o/r","generated_at":"2026-01-02T03:04:05Z","brain_schema_version":"1.0"}}`))
	f.Add([]byte(`{"manifest":{"repo_key":"r","generated_at":"2026-01-02T03:04:05Z","brain_schema_version":"1.0"},"snapshots":[{"commit":"abc","content":{"digest":"sha256:ab","size":3}}]}`))

	f.Fuzz(func(t *testing.T, raw []byte) {
		canon, err := Canonicalize(raw)
		if err != nil {
			return
		}
		if canonicalHasUnknownKeys(t, canon) {
			return // tolerated by design; the struct path cannot round-trip it
		}
		var a BrainArtifact
		if err := CanonicalUnmarshal(canon, &a); err != nil {
			t.Fatalf("canonical bytes failed to decode into BrainArtifact: %v\ncanon=%q", err, canon)
		}
		again, err := CanonicalMarshal(a)
		if err != nil {
			t.Fatalf("decoded artifact failed to re-marshal: %v\ncanon=%q", err, canon)
		}
		if !bytes.Equal(canon, again) {
			t.Fatalf("struct path and bytes path disagree:\ncanon=%q\nagain=%q", canon, again)
		}
	})
}

// canonicalHasUnknownKeys reports whether any object in the artifact carries a key
// outside the contract, which the struct path is expected to drop.
func canonicalHasUnknownKeys(t *testing.T, canon []byte) bool {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(canon, &top); err != nil {
		return true // unparseable here means "do not make round-trip claims about it"
	}
	known := map[string][]string{
		"":          {"manifest", "snapshots", "overlays", "facts"},
		"manifest":  {"repo_key", "default_branch", "generated_at", "brain_schema_version", "provider", "provider_version", "provider_schema_version"},
		"content":   {"digest", "size", "media_type", "path"},
		"snapshots": {"commit", "tree", "content"},
		"overlays":  {"base_commit", "head_commit", "branch", "content"},
		"facts":     {"branch", "content"},
	}
	has := func(set []string, k string) bool {
		for _, s := range set {
			if s == k {
				return true
			}
		}
		return false
	}
	// top level
	for k := range top {
		if !has(known[""], k) {
			return true
		}
	}
	// manifest object
	if rawManifest, ok := top["manifest"]; ok {
		var m map[string]json.RawMessage
		if json.Unmarshal(rawManifest, &m) != nil {
			return true
		}
		for k := range m {
			if !has(known["manifest"], k) {
				return true
			}
		}
	}
	// the three ref arrays, plus their nested content objects
	for _, arrKey := range []string{"snapshots", "overlays", "facts"} {
		rawArr, ok := top[arrKey]
		if !ok {
			continue
		}
		var arr []map[string]json.RawMessage
		if json.Unmarshal(rawArr, &arr) != nil {
			return true
		}
		for _, obj := range arr {
			for k := range obj {
				if !has(known[arrKey], k) {
					return true
				}
			}
			if rawContent, ok := obj["content"]; ok {
				var c map[string]json.RawMessage
				if json.Unmarshal(rawContent, &c) != nil {
					return true
				}
				for k := range c {
					if !has(known["content"], k) {
						return true
					}
				}
			}
		}
	}
	return false
}

// FuzzVerifyRejectsTamper is the security property of M1.4: over arbitrary canonical
// bytes, a signature verifies against exactly those bytes and NOTHING else. Any
// mutation — a flipped byte, a stripped field, appended trailing data — must fail
// verification, and must fail by returning an error rather than panicking, since
// VerifyBytes is an entry point for content another member produced.
func FuzzVerifyRejectsTamper(f *testing.F) {
	f.Add([]byte(`{"manifest":{"repo_key":"gh/o/r","generated_at":"2026-01-02T03:04:05Z","brain_schema_version":"1.0"}}`), byte(0), byte(1))
	f.Add([]byte(`{"manifest":{"repo_key":"r","generated_at":"2026-01-02T03:04:05Z","brain_schema_version":"1.0"},"snapshots":[{"commit":"abc","content":{"digest":"sha256:ab","size":3}}]}`), byte(7), byte(200))

	signer := mustFuzzSigner(f)
	f.Fuzz(func(t *testing.T, raw []byte, pos, delta byte) {
		canon, err := Canonicalize(raw)
		if err != nil {
			return
		}
		sig, err := SignCanonical(canon, signer)
		if err != nil {
			t.Fatalf("SignCanonical over accepted canonical bytes: %v", err)
		}
		// The honest case must verify.
		if err := VerifyCanonical(canon, sig, signer.Public()); err != nil {
			t.Fatalf("VerifyCanonical rejected the bytes it just signed: %v\ncanon=%q", err, canon)
		}
		// Any single-byte mutation must be rejected, never accepted, never a panic.
		if len(canon) == 0 || delta == 0 {
			return
		}
		tampered := append([]byte(nil), canon...)
		i := int(pos) % len(tampered)
		tampered[i] += delta
		if bytes.Equal(tampered, canon) {
			return
		}
		if err := VerifyCanonical(tampered, sig, signer.Public()); err == nil {
			t.Fatalf("tampered bytes verified:\norig=%q\ntamp=%q", canon, tampered)
		}
		// Appended trailing data must not verify either.
		if err := VerifyCanonical(append(append([]byte(nil), canon...), ' '), sig, signer.Public()); err == nil {
			t.Fatalf("trailing data verified: %q", canon)
		}
	})
}

// mustFuzzSigner mirrors mustSigner for a *testing.F (fuzz targets need the signer
// built once, outside the per-input function).
func mustFuzzSigner(f *testing.F) *Ed25519Signer {
	f.Helper()
	s, err := GenerateEd25519Signer(rand.Reader)
	if err != nil {
		f.Fatalf("GenerateEd25519Signer: %v", err)
	}
	return s
}
