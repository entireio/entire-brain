package brainwire

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// goldenSeedHex is a fixed Ed25519 seed so the golden key id and signature are
// reproducible. Ed25519 is deterministic (RFC 8032): the same key over the same
// message always yields the same 64 signature bytes, which is what lets a
// non-Go implementation be checked against these exact constants.
const goldenSeedHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// Golden values for representativeArtifact signed with goldenSeedHex. Any
// change to the canonical encoding, the pre-image layout, the domain string, or
// the key-id derivation breaks these — which is the point.
//
// Both were reproduced OUTSIDE Go, which is the whole claim of this milestone.
// The key id, from the raw 32-byte public key:
//
//	printf '302e020100300506032b657004220420<seed-hex>' | xxd -r -p > k.der
//	openssl pkey -inform DER -in k.der -out k.pem
//	openssl pkey -in k.pem -pubout -outform DER | tail -c 32 | shasum -a 256 | cut -c1-32
//	# -> bd4e02f43853c45ca08a9ca2cbe39944
//
// The signature, over the pre-image written to msg.bin (SigningDomain, alg,
// key id and canonical encoding version, each LF-terminated, then the canonical
// artifact bytes):
//
//	openssl pkey -in k.pem -pubout -out kpub.pem
//	openssl pkeyutl -verify -rawin -pubin -inkey kpub.pem -sigfile sig.bin -in msg.bin
//	# -> Signature Verified Successfully
const (
	goldenKeyID     = "ed25519:bd4e02f43853c45ca08a9ca2cbe39944"
	goldenSignature = "1xqZbMY9if7m8FR4BLkz8MKsOoKZkdTG8Q1IUDlc8PrFD0RTZzIFqQ+U8DtjxrZKYxcbQKbSfoxIb+xG50AlCw=="
)

func goldenSigner(t *testing.T) *Ed25519Signer {
	t.Helper()
	seed, err := hex.DecodeString(goldenSeedHex)
	if err != nil {
		t.Fatalf("decode golden seed: %v", err)
	}
	s, err := NewEd25519Signer(ed25519.NewKeyFromSeed(seed))
	if err != nil {
		t.Fatalf("NewEd25519Signer: %v", err)
	}
	return s
}

func mustSigner(t *testing.T) *Ed25519Signer {
	t.Helper()
	s, err := GenerateEd25519Signer(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateEd25519Signer: %v", err)
	}
	return s
}

func mustSign(t *testing.T, a BrainArtifact, s crypto.Signer) Signature {
	t.Helper()
	sig, err := Sign(a, s)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return sig
}

// cloneArtifact deep-copies the reference slices so a mutation in one copy
// cannot reach through a shared backing array into the other.
func cloneArtifact(a BrainArtifact) BrainArtifact {
	out := a
	out.Snapshots = append([]SnapshotRef(nil), a.Snapshots...)
	out.Overlays = append([]OverlayRef(nil), a.Overlays...)
	out.Facts = append([]FactsRef(nil), a.Facts...)
	return out
}

// TestSignVerifyRoundTrip is the base case: a signature made over an artifact
// verifies against the signer's public key, through every verify entry point.
func TestSignVerifyRoundTrip(t *testing.T) {
	t.Parallel()
	art := *representativeArtifact(t)
	signer := mustSigner(t)

	sig := mustSign(t, art, signer)

	if sig.Alg != SigAlgEd25519 {
		t.Errorf("alg = %q, want %q", sig.Alg, SigAlgEd25519)
	}
	if sig.CanonicalEncoding != CanonicalEncodingVersion {
		t.Errorf("canonical_encoding = %q, want %q", sig.CanonicalEncoding, CanonicalEncodingVersion)
	}
	if sig.KeyID != signer.KeyID() {
		t.Errorf("key_id = %q, want %q", sig.KeyID, signer.KeyID())
	}
	raw, err := base64.StdEncoding.DecodeString(sig.Sig)
	if err != nil || len(raw) != ed25519.SignatureSize {
		t.Fatalf("sig is not %d base64 bytes: len=%d err=%v", ed25519.SignatureSize, len(raw), err)
	}

	pub := signer.Public()
	if err := Verify(art, sig, pub); err != nil {
		t.Errorf("Verify: %v", err)
	}

	canonical, err := CanonicalMarshal(art)
	if err != nil {
		t.Fatalf("CanonicalMarshal: %v", err)
	}
	if err := VerifyCanonical(canonical, sig, pub); err != nil {
		t.Errorf("VerifyCanonical: %v", err)
	}
	if err := VerifyBytes(canonical, sig, pub); err != nil {
		t.Errorf("VerifyBytes: %v", err)
	}

	// The envelope survives a JSON round trip (it is transported detached).
	encoded, err := json.Marshal(sig)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	var decoded Signature
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if err := Verify(art, decoded, pub); err != nil {
		t.Errorf("Verify after envelope round trip: %v", err)
	}
}

// TestSignatureStableAcrossIndependentCanonicalMarshals proves the signature is
// a function of the artifact's logical content, not of any one serialization:
// two independent canonicalizations (typed value, and a hand-written JSON with
// shuffled keys, explicit omitted defaults and a fractional offset timestamp)
// produce the same bytes and therefore the same signature.
func TestSignatureStableAcrossIndependentCanonicalMarshals(t *testing.T) {
	t.Parallel()
	art := *representativeArtifact(t)
	signer := goldenSigner(t)

	first := mustSign(t, art, signer)
	second := mustSign(t, art, signer)
	if first != second {
		t.Errorf("two signatures over the same artifact differ:\n1=%+v\n2=%+v", first, second)
	}

	// A second, independently produced canonical marshal of the same artifact.
	rebuilt := *representativeArtifact(t)
	third := mustSign(t, rebuilt, signer)
	if third != first {
		t.Errorf("signature over an independently built equal artifact differs:\n1=%+v\n3=%+v", first, third)
	}

	// A foreign producer's non-canonical bytes for the same content: reordered
	// keys, an explicit "size":0-style omitted default, a null, and a
	// sub-second offset timestamp. Canonicalizing yields the same bytes, so
	// the same signature verifies.
	foreign := `{
	  "snapshots": [ { "content": { "path": "semantic/snapshots/abc123/snapshot.json", "size": 2048,
	                                "media_type": "application/json",
	                                "digest": "sha256:1111111111111111111111111111111111111111111111111111111111111111" },
	                   "tree": "tree789", "commit": "abc123" } ],
	  "manifest": { "provider_version": "0.1.0", "repo_key": "gh/org/repo",
	                "generated_at": "2026-06-01T14:00:00.500+02:00",
	                "default_branch": "main", "provider": "entire-graph",
	                "brain_schema_version": "1.0", "provider_schema_version": "1.1" },
	  "facts": [ { "content": { "digest": "sha256:3333333333333333333333333333333333333333333333333333333333333333",
	                            "size": 4096, "media_type": "application/x-ndjson",
	                            "path": "facts/main/facts.ndjson" }, "branch": "main" } ],
	  "overlays": [ { "head_commit": "def456", "base_commit": "abc123", "branch": "feature-x",
	                  "content": { "size": 512, "digest": "sha256:2222222222222222222222222222222222222222222222222222222222222222",
	                               "path": "semantic/overlays/abc123..def456.json",
	                               "media_type": "application/json" } } ]
	}`
	if got := mustCanonicalize(t, foreign); got != goldenCanonicalArtifact {
		t.Fatalf("foreign encoding did not canonicalize to the golden bytes:\n got=%s\nwant=%s", got, goldenCanonicalArtifact)
	}
	if err := VerifyBytes([]byte(foreign), first, signer.Public()); err != nil {
		t.Errorf("VerifyBytes over a foreign encoding of the same content: %v", err)
	}
}

// TestSignatureGolden pins the exact key id, pre-image and signature bytes for
// a fixed key and the representative artifact, so a non-Go implementation has
// something concrete to reproduce.
func TestSignatureGolden(t *testing.T) {
	t.Parallel()
	signer := goldenSigner(t)
	if signer.KeyID() != goldenKeyID {
		t.Errorf("key id drifted: got %q, want %q", signer.KeyID(), goldenKeyID)
	}

	sig := mustSign(t, *representativeArtifact(t), signer)
	if sig.Sig != goldenSignature {
		t.Errorf("signature drifted: got %q, want %q", sig.Sig, goldenSignature)
	}

	wantInput := SigningDomain + "\n" + SigAlgEd25519 + "\n" + goldenKeyID + "\n" +
		CanonicalEncodingVersion + "\n" + goldenCanonicalArtifact
	gotInput, err := SigningInput(sig, []byte(goldenCanonicalArtifact))
	if err != nil {
		t.Fatalf("SigningInput: %v", err)
	}
	if string(gotInput) != wantInput {
		t.Errorf("signing pre-image drifted:\n got=%q\nwant=%q", gotInput, wantInput)
	}
}

// TestTamperedManifestFails: every manifest field is inside the signed
// pre-image, so altering any of them invalidates the signature.
func TestTamperedManifestFails(t *testing.T) {
	t.Parallel()
	art := *representativeArtifact(t)
	signer := mustSigner(t)
	sig := mustSign(t, art, signer)

	tampers := map[string]func(*BrainArtifact){
		"repo_key":                func(a *BrainArtifact) { a.Manifest.RepoKey = "gh/evil/repo" },
		"default_branch":          func(a *BrainArtifact) { a.Manifest.DefaultBranch = "trunk" },
		"generated_at":            func(a *BrainArtifact) { a.Manifest.GeneratedAt = a.Manifest.GeneratedAt.Add(1e9) },
		"brain_schema_version":    func(a *BrainArtifact) { a.Manifest.BrainSchemaVersion = "9.9" },
		"provider":                func(a *BrainArtifact) { a.Manifest.Provider = "not-entire-graph" },
		"provider_version":        func(a *BrainArtifact) { a.Manifest.ProviderVersion = "9.9.9" },
		"provider_schema_version": func(a *BrainArtifact) { a.Manifest.ProviderSchemaVersion = "9.9" },
		"snapshot commit":         func(a *BrainArtifact) { a.Snapshots[0].Commit = "deadbee" },
		"snapshot tree":           func(a *BrainArtifact) { a.Snapshots[0].Tree = "deadbee" },
		"overlay base_commit":     func(a *BrainArtifact) { a.Overlays[0].BaseCommit = "deadbee" },
		"overlay head_commit":     func(a *BrainArtifact) { a.Overlays[0].HeadCommit = "deadbee" },
		"facts branch":            func(a *BrainArtifact) { a.Facts[0].Branch = "attacker" },
		"content size":            func(a *BrainArtifact) { a.Snapshots[0].Content.Size = 1 },
		"content media_type":      func(a *BrainArtifact) { a.Snapshots[0].Content.MediaType = "text/plain" },
	}
	for name, tamper := range tampers {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			bad := cloneArtifact(art)
			tamper(&bad)
			err := Verify(bad, sig, signer.Public())
			if !errors.Is(err, ErrBadSignature) {
				t.Fatalf("tampering with %s: got %v, want ErrBadSignature", name, err)
			}
		})
	}

	// Tampering with the signature bytes themselves also fails.
	raw, err := base64.StdEncoding.DecodeString(sig.Sig)
	if err != nil {
		t.Fatalf("decode sig: %v", err)
	}
	raw[0] ^= 0xff
	flipped := sig
	flipped.Sig = base64.StdEncoding.EncodeToString(raw)
	if err := Verify(art, flipped, signer.Public()); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("flipped signature byte: got %v, want ErrBadSignature", err)
	}
}

// TestSignatureCommitsToEveryBlobDigest is the explicit check that the manifest
// signature covers every blob it references. Blobs are content-addressed, so
// binding their digests is what makes the signature cover their bytes: swap a
// blob and its digest must change, which invalidates the signature.
func TestSignatureCommitsToEveryBlobDigest(t *testing.T) {
	t.Parallel()
	art := *representativeArtifact(t)
	signer := mustSigner(t)
	sig := mustSign(t, art, signer)

	canonical, err := CanonicalMarshal(art)
	if err != nil {
		t.Fatalf("CanonicalMarshal: %v", err)
	}
	input, err := SigningInput(sig, canonical)
	if err != nil {
		t.Fatalf("SigningInput: %v", err)
	}

	digests := ReferencedDigests(art)
	if len(digests) != len(art.Snapshots)+len(art.Overlays)+len(art.Facts) {
		t.Fatalf("ReferencedDigests returned %d digests, want %d", len(digests), len(art.Snapshots)+len(art.Overlays)+len(art.Facts))
	}
	if len(digests) == 0 {
		t.Fatal("representative artifact references no blobs; the test proves nothing")
	}
	// 1. Every digest is literally inside the signed pre-image.
	for _, d := range digests {
		if !strings.Contains(string(input), d) {
			t.Errorf("digest %s is not covered by the signing pre-image", d)
		}
	}

	// 2. Swapping any one blob (i.e. changing its digest) invalidates.
	swaps := []struct {
		name  string
		apply func(*BrainArtifact)
	}{
		{"snapshot blob", func(a *BrainArtifact) { a.Snapshots[0].Content.Digest = "sha256:" + strings.Repeat("9", 64) }},
		{"overlay blob", func(a *BrainArtifact) { a.Overlays[0].Content.Digest = "sha256:" + strings.Repeat("9", 64) }},
		{"facts blob", func(a *BrainArtifact) { a.Facts[0].Content.Digest = "sha256:" + strings.Repeat("9", 64) }},
	}
	for _, sw := range swaps {
		bad := cloneArtifact(art)
		sw.apply(&bad)
		if err := Verify(bad, sig, signer.Public()); !errors.Is(err, ErrBadSignature) {
			t.Errorf("swapped %s: got %v, want ErrBadSignature", sw.name, err)
		}
	}

	// 3. A single flipped hex character in one digest invalidates.
	oneBit := cloneArtifact(art)
	d := []byte(oneBit.Facts[0].Content.Digest)
	if d[len(d)-1] == '0' {
		d[len(d)-1] = '1'
	} else {
		d[len(d)-1] = '0'
	}
	oneBit.Facts[0].Content.Digest = string(d)
	if err := Verify(oneBit, sig, signer.Public()); !errors.Is(err, ErrBadSignature) {
		t.Errorf("one-character digest change: got %v, want ErrBadSignature", err)
	}

	// 4. Adding a blob reference invalidates.
	added := cloneArtifact(art)
	added.AddFacts(FactsRef{
		Branch:  "smuggled",
		Content: ContentRef{Digest: "sha256:" + strings.Repeat("a", 64)},
	})
	if err := Verify(added, sig, signer.Public()); !errors.Is(err, ErrBadSignature) {
		t.Errorf("added blob reference: got %v, want ErrBadSignature", err)
	}

	// 5. Removing a blob reference invalidates.
	removed := cloneArtifact(art)
	removed.Overlays = nil
	if err := Verify(removed, sig, signer.Public()); !errors.Is(err, ErrBadSignature) {
		t.Errorf("removed blob reference: got %v, want ErrBadSignature", err)
	}

	// 6. Reordering blob references invalidates (order is part of the wire
	//    contract, so it is signed).
	art2 := cloneArtifact(art)
	art2.AddSnapshot(SnapshotRef{Commit: "zzz999", Content: ContentRef{Digest: "sha256:" + strings.Repeat("b", 64)}})
	sig2 := mustSign(t, art2, signer)
	reordered := cloneArtifact(art2)
	reordered.Snapshots[0], reordered.Snapshots[1] = reordered.Snapshots[1], reordered.Snapshots[0]
	if err := Verify(reordered, sig2, signer.Public()); !errors.Is(err, ErrBadSignature) {
		t.Errorf("reordered blob references: got %v, want ErrBadSignature", err)
	}
}

// TestWrongKeyFails covers both shapes of "not my key": an honest mismatch
// (caught by the key id) and a forged key id (caught by the signature, because
// the key id is bound into the pre-image).
func TestWrongKeyFails(t *testing.T) {
	t.Parallel()
	art := *representativeArtifact(t)
	alice := mustSigner(t)
	bob := mustSigner(t)
	sig := mustSign(t, art, alice)

	if err := Verify(art, sig, bob.Public()); !errors.Is(err, ErrKeyMismatch) {
		t.Errorf("verifying Alice's signature with Bob's key: got %v, want ErrKeyMismatch", err)
	}

	// Relabel the envelope with Bob's key id. The key id now matches the
	// offered key, so the mismatch check passes — and the signature fails,
	// because the pre-image Alice signed contained her key id.
	relabeled := sig
	relabeled.KeyID = bob.KeyID()
	if err := Verify(art, relabeled, bob.Public()); !errors.Is(err, ErrBadSignature) {
		t.Errorf("relabeled key id: got %v, want ErrBadSignature", err)
	}

	// Bob signing the same artifact produces a different signature.
	bobSig := mustSign(t, art, bob)
	if bobSig.Sig == sig.Sig {
		t.Error("two different keys produced the same signature")
	}
	if err := Verify(art, bobSig, bob.Public()); err != nil {
		t.Errorf("Bob's own signature: %v", err)
	}
}

// TestUnknownAlgorithmAndEncodingFailClosed: an unrecognized algorithm or
// canonical-encoding version must fail, never be skipped.
func TestUnknownAlgorithmAndEncodingFailClosed(t *testing.T) {
	t.Parallel()
	art := *representativeArtifact(t)
	signer := mustSigner(t)
	sig := mustSign(t, art, signer)

	cases := []struct {
		name string
		mut  func(*Signature)
		want error
	}{
		{"unknown alg", func(s *Signature) { s.Alg = "rsa-pss-sha256" }, ErrUnknownAlgorithm},
		{"future alg", func(s *Signature) { s.Alg = "ml-dsa-65" }, ErrUnknownAlgorithm},
		{"empty alg", func(s *Signature) { s.Alg = "" }, ErrMalformedSignature},
		{"future canonical encoding", func(s *Signature) { s.CanonicalEncoding = "brainwire-canonical/2" }, ErrUnsupportedEncoding},
		{"empty canonical encoding", func(s *Signature) { s.CanonicalEncoding = "" }, ErrMalformedSignature},
		{"empty key id", func(s *Signature) { s.KeyID = "" }, ErrMalformedSignature},
		{"empty sig", func(s *Signature) { s.Sig = "" }, ErrMalformedSignature},
		{"newline in key id", func(s *Signature) { s.KeyID += "\nalg=none" }, ErrMalformedSignature},
		{"sig not base64", func(s *Signature) { s.Sig = "!!!not base64!!!" }, ErrMalformedSignature},
		{"sig wrong length", func(s *Signature) { s.Sig = base64.StdEncoding.EncodeToString([]byte("short")) }, ErrMalformedSignature},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bad := sig
			tc.mut(&bad)
			err := Verify(art, bad, signer.Public())
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}

	// A non-ed25519 verification key is an algorithm error, not a silent pass.
	if err := Verify(art, sig, "not-a-key"); !errors.Is(err, ErrUnknownAlgorithm) {
		t.Errorf("string public key: got %v, want ErrUnknownAlgorithm", err)
	}
	if err := Verify(art, sig, ed25519.PublicKey([]byte{1, 2, 3})); !errors.Is(err, ErrUnknownAlgorithm) {
		t.Errorf("short public key: got %v, want ErrUnknownAlgorithm", err)
	}
}

// TestNonCanonicalInputRejected: the signing and verifying entry points that
// promise canonical bytes must refuse anything else, so a caller cannot end up
// signing bytes nobody else can reproduce.
func TestNonCanonicalInputRejected(t *testing.T) {
	t.Parallel()
	art := *representativeArtifact(t)
	signer := mustSigner(t)

	// Go's default encoding is NOT canonical (declaration order, HTML
	// escaping, RFC3339Nano, trailing newline from Encoder).
	goDefault, err := json.Marshal(art)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if _, err := SignCanonical(goDefault, signer); !errors.Is(err, ErrNotCanonical) {
		t.Errorf("SignCanonical over Go-default bytes: got %v, want ErrNotCanonical", err)
	}

	sig := mustSign(t, art, signer)
	if err := VerifyCanonical(goDefault, sig, signer.Public()); !errors.Is(err, ErrNotCanonical) {
		t.Errorf("VerifyCanonical over Go-default bytes: got %v, want ErrNotCanonical", err)
	}
	// ...but VerifyBytes canonicalizes first, so the same bytes verify there.
	if err := VerifyBytes(goDefault, sig, signer.Public()); err != nil {
		t.Errorf("VerifyBytes over Go-default bytes: %v", err)
	}

	for _, bad := range []string{"", "{", `{"manifest":{}} trailing`, `[]`, `{"manifest":{"generated_at":"nope"}}`} {
		if _, err := SignCanonical([]byte(bad), signer); !errors.Is(err, ErrNotCanonical) {
			t.Errorf("SignCanonical(%q): got %v, want ErrNotCanonical", bad, err)
		}
		if err := VerifyBytes([]byte(bad), sig, signer.Public()); !errors.Is(err, ErrNotCanonical) {
			t.Errorf("VerifyBytes(%q): got %v, want ErrNotCanonical", bad, err)
		}
	}

	if _, err := SignCanonical([]byte(goldenCanonicalArtifact), nil); !errors.Is(err, ErrNoKey) {
		t.Errorf("SignCanonical with nil signer: got %v, want ErrNoKey", err)
	}
}

// TestVerifyBytesToleratesUnknownFields: a newer additive minor's artifact
// still verifies on this reader, because canonicalization preserves unknown
// fields. This is the property that keeps signatures usable across a minor bump.
func TestVerifyBytesToleratesUnknownFields(t *testing.T) {
	t.Parallel()
	signer := mustSigner(t)

	newer := `{"manifest":{"repo_key":"gh/org/repo","generated_at":"2026-06-01T12:00:00Z",` +
		`"brain_schema_version":"1.1","future_field":"kept"},` +
		`"snapshots":[{"commit":"abc123","content":{"digest":"sha256:` + strings.Repeat("1", 64) + `"},"future_ref_field":42}]}`

	canonical, err := Canonicalize([]byte(newer))
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if !strings.Contains(string(canonical), "future_field") || !strings.Contains(string(canonical), "future_ref_field") {
		t.Fatalf("unknown fields were dropped by canonicalization: %s", canonical)
	}

	sig, err := SignCanonical(canonical, signer)
	if err != nil {
		t.Fatalf("SignCanonical: %v", err)
	}
	if err := VerifyBytes([]byte(newer), sig, signer.Public()); err != nil {
		t.Errorf("VerifyBytes over the newer producer's own bytes: %v", err)
	}

	// Dropping the unknown field breaks the signature: an older reader must
	// not be able to strip fields it does not understand and still validate.
	stripped := strings.Replace(newer, `,"future_field":"kept"`, "", 1)
	if err := VerifyBytes([]byte(stripped), sig, signer.Public()); !errors.Is(err, ErrBadSignature) {
		t.Errorf("stripped unknown field: got %v, want ErrBadSignature", err)
	}
}

// TestSigningInputBindsEnvelope proves the pre-image really does include the
// envelope metadata, so it cannot be relabeled.
func TestSigningInputBindsEnvelope(t *testing.T) {
	t.Parallel()
	base := Signature{Alg: SigAlgEd25519, KeyID: "ed25519:aa", CanonicalEncoding: CanonicalEncodingVersion}
	canonical := []byte(goldenCanonicalArtifact)

	ref, err := SigningInput(base, canonical)
	if err != nil {
		t.Fatalf("SigningInput: %v", err)
	}
	if !strings.HasPrefix(string(ref), SigningDomain+"\n") {
		t.Errorf("pre-image is not domain-separated: %q", ref[:min(64, len(ref))])
	}
	if !strings.HasSuffix(string(ref), goldenCanonicalArtifact) {
		t.Error("pre-image does not end with the canonical artifact bytes")
	}

	for _, mut := range []func(Signature) Signature{
		func(s Signature) Signature { s.Alg = "other"; return s },
		func(s Signature) Signature { s.KeyID = "ed25519:bb"; return s },
		func(s Signature) Signature { s.CanonicalEncoding = "brainwire-canonical/2"; return s },
	} {
		other, err := SigningInput(mut(base), canonical)
		if err != nil {
			t.Fatalf("SigningInput: %v", err)
		}
		if string(other) == string(ref) {
			t.Error("changing an envelope field did not change the pre-image")
		}
	}

	// Sig is deliberately NOT part of the pre-image (a signature cannot cover
	// itself).
	withSig := base
	withSig.Sig = "AAAA"
	got, err := SigningInput(withSig, canonical)
	if err != nil {
		t.Fatalf("SigningInput: %v", err)
	}
	if string(got) != string(ref) {
		t.Error("Signature.Sig leaked into the pre-image")
	}
}

// TestKeyLoading covers the file and env sources and the interoperable PEM
// encodings. The parent is deliberately NOT parallel: some subtests use
// t.Setenv, which panics if the test or any parent is parallel.
func TestKeyLoading(t *testing.T) {
	signer := goldenSigner(t)
	art := *representativeArtifact(t)
	sig := mustSign(t, art, signer)

	privPEM, err := MarshalPrivateKeyPEM(signer)
	if err != nil {
		t.Fatalf("MarshalPrivateKeyPEM: %v", err)
	}
	pubPEM, err := MarshalPublicKeyPEM(signer.Public())
	if err != nil {
		t.Fatalf("MarshalPublicKeyPEM: %v", err)
	}
	seed, err := hex.DecodeString(goldenSeedHex)
	if err != nil {
		t.Fatalf("decode seed: %v", err)
	}
	seedB64 := base64.StdEncoding.EncodeToString(seed)

	t.Run("file pem", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "key.pem")
		if err := os.WriteFile(path, privPEM, 0o600); err != nil {
			t.Fatalf("write key: %v", err)
		}
		loaded, err := LoadSignerFile(path)
		if err != nil {
			t.Fatalf("LoadSignerFile: %v", err)
		}
		if loaded.KeyID() != goldenKeyID {
			t.Fatalf("key id = %q, want %q", loaded.KeyID(), goldenKeyID)
		}
		if got := mustSign(t, art, loaded); got != sig {
			t.Error("signature from the loaded key differs from the in-memory key")
		}
	})

	t.Run("file bare base64 seed", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "key.b64")
		if err := os.WriteFile(path, []byte(seedB64+"\n"), 0o600); err != nil {
			t.Fatalf("write key: %v", err)
		}
		loaded, err := LoadSignerFile(path)
		if err != nil {
			t.Fatalf("LoadSignerFile: %v", err)
		}
		if loaded.KeyID() != goldenKeyID {
			t.Fatalf("key id = %q, want %q", loaded.KeyID(), goldenKeyID)
		}
	})

	t.Run("file insecure mode", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("POSIX permission bits are not meaningful on Windows")
		}
		t.Parallel()
		path := filepath.Join(t.TempDir(), "key.pem")
		if err := os.WriteFile(path, privPEM, 0o644); err != nil {
			t.Fatalf("write key: %v", err)
		}
		_, err := LoadSignerFile(path)
		if !errors.Is(err, ErrInsecureKeyFile) {
			t.Fatalf("got %v, want ErrInsecureKeyFile", err)
		}
	})

	t.Run("file missing", func(t *testing.T) {
		t.Parallel()
		_, err := LoadSignerFile(filepath.Join(t.TempDir(), "nope.pem"))
		if !errors.Is(err, ErrNoKey) {
			t.Fatalf("got %v, want ErrNoKey", err)
		}
	})

	t.Run("env pem with escaped newlines", func(t *testing.T) {
		// Not parallel: mutates process environment.
		t.Setenv("BRAINWIRE_TEST_KEY", strings.ReplaceAll(string(privPEM), "\n", `\n`))
		loaded, err := LoadSignerEnv("BRAINWIRE_TEST_KEY")
		if err != nil {
			t.Fatalf("LoadSignerEnv: %v", err)
		}
		if loaded.KeyID() != goldenKeyID {
			t.Fatalf("key id = %q, want %q", loaded.KeyID(), goldenKeyID)
		}
	})

	t.Run("env base64", func(t *testing.T) {
		t.Setenv("BRAINWIRE_TEST_KEY", seedB64)
		loaded, err := LoadSignerEnv("BRAINWIRE_TEST_KEY")
		if err != nil {
			t.Fatalf("LoadSignerEnv: %v", err)
		}
		if loaded.KeyID() != goldenKeyID {
			t.Fatalf("key id = %q, want %q", loaded.KeyID(), goldenKeyID)
		}
	})

	t.Run("env unset and empty", func(t *testing.T) {
		t.Setenv("BRAINWIRE_TEST_KEY", "")
		if _, err := LoadSignerEnv("BRAINWIRE_TEST_KEY"); !errors.Is(err, ErrNoKey) {
			t.Fatalf("empty env: got %v, want ErrNoKey", err)
		}
		if _, err := LoadSignerEnv("BRAINWIRE_TEST_KEY_DEFINITELY_UNSET"); !errors.Is(err, ErrNoKey) {
			t.Fatalf("unset env: got %v, want ErrNoKey", err)
		}
	})

	t.Run("public key pem and base64", func(t *testing.T) {
		t.Parallel()
		pub, err := ParseEd25519PublicKey(pubPEM)
		if err != nil {
			t.Fatalf("ParseEd25519PublicKey(PEM): %v", err)
		}
		if err := Verify(art, sig, pub); err != nil {
			t.Errorf("verify with PEM-loaded public key: %v", err)
		}

		path := filepath.Join(t.TempDir(), "key.pub")
		if err := os.WriteFile(path, pubPEM, 0o644); err != nil {
			t.Fatalf("write public key: %v", err)
		}
		fromFile, err := LoadPublicKeyFile(path)
		if err != nil {
			t.Fatalf("LoadPublicKeyFile: %v", err)
		}
		if err := Verify(art, sig, fromFile); err != nil {
			t.Errorf("verify with file-loaded public key: %v", err)
		}

		edPub, ok := signer.Public().(ed25519.PublicKey)
		if !ok {
			t.Fatal("signer public key is not ed25519")
		}
		bare, err := ParseEd25519PublicKey([]byte(base64.StdEncoding.EncodeToString(edPub)))
		if err != nil {
			t.Fatalf("ParseEd25519PublicKey(base64): %v", err)
		}
		if err := Verify(art, sig, bare); err != nil {
			t.Errorf("verify with base64-loaded public key: %v", err)
		}
	})

	t.Run("malformed key material", func(t *testing.T) {
		t.Parallel()
		for _, bad := range []string{
			"",
			"not base64 or pem !!!",
			base64.StdEncoding.EncodeToString([]byte("too short")),
			"-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n",
		} {
			if _, err := ParseEd25519PrivateKey([]byte(bad)); err == nil {
				t.Errorf("ParseEd25519PrivateKey(%q) succeeded, want error", bad)
			}
		}
		if _, err := NewEd25519Signer(ed25519.PrivateKey{1, 2, 3}); !errors.Is(err, ErrNoKey) {
			t.Errorf("short private key: got %v, want ErrNoKey", err)
		}
	})
}

// TestSignerNeverRendersKeyMaterial: the signer must be unable to leak its
// private key through any of the printing / serialization paths a caller might
// reach for, and no error message may contain it either.
//
// Not parallel: it uses t.Setenv.
func TestSignerNeverRendersKeyMaterial(t *testing.T) {
	signer := goldenSigner(t)

	// Two independent spellings of the secret to search for.
	secretHex := goldenSeedHex
	secretB64 := base64.StdEncoding.EncodeToString(signer.priv)

	renders := map[string]string{
		"String":   signer.String(),
		"GoString": signer.GoString(),
		"%v":       fmt.Sprintf("%v", signer),
		"%s":       fmt.Sprintf("%s", signer),
		"%+v":      fmt.Sprintf("%+v", signer),
		"%#v":      fmt.Sprintf("%#v", signer),
		"%q":       fmt.Sprintf("%q", signer),
	}
	text, err := signer.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText: %v", err)
	}
	renders["MarshalText"] = string(text)

	jsonBytes, err := json.Marshal(signer)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	renders["json.Marshal"] = string(jsonBytes)

	// A config struct that embeds the signer must not leak it either.
	wrapped, err := json.Marshal(struct {
		Name   string         `json:"name"`
		Signer *Ed25519Signer `json:"signer"`
	}{Name: "brain", Signer: signer})
	if err != nil {
		t.Fatalf("json.Marshal(wrapper): %v", err)
	}
	renders["json.Marshal(wrapper)"] = string(wrapped)

	for name, out := range renders {
		if strings.Contains(strings.ToLower(out), strings.ToLower(secretHex)) {
			t.Errorf("%s leaked the seed (hex): %s", name, out)
		}
		if strings.Contains(out, secretB64) {
			t.Errorf("%s leaked the private key (base64): %s", name, out)
		}
		// Every render should still identify the key.
		if !strings.Contains(out, goldenKeyID) && name != "json.Marshal(wrapper)" {
			t.Errorf("%s does not identify the key: %s", name, out)
		}
	}

	// Loader errors must not echo key material either. A full 64-byte private
	// key in bare base64 would load successfully, so feed a truncated one to
	// force the error path.
	truncated := secretB64[:20]
	path := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(path, []byte(truncated), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	_, err = LoadSignerFile(path)
	if err == nil {
		t.Fatal("expected an error for truncated key material")
	}
	if strings.Contains(err.Error(), truncated) {
		t.Errorf("loader error echoed key material: %v", err)
	}

	t.Setenv("BRAINWIRE_TEST_BAD_KEY", truncated)
	_, err = LoadSignerEnv("BRAINWIRE_TEST_BAD_KEY")
	if err == nil {
		t.Fatal("expected an error for truncated env key material")
	}
	if strings.Contains(err.Error(), truncated) {
		t.Errorf("env loader error echoed key material: %v", err)
	}
}

// TestSignerRejectsPrehash: the contract is pure Ed25519; Ed25519ph would
// produce signatures no other implementation of this contract could verify.
func TestSignerRejectsPrehash(t *testing.T) {
	t.Parallel()
	signer := mustSigner(t)
	if _, err := signer.Sign(rand.Reader, []byte("msg"), crypto.SHA512); !errors.Is(err, ErrUnknownAlgorithm) {
		t.Fatalf("pre-hashed sign: got %v, want ErrUnknownAlgorithm", err)
	}
	if _, err := signer.Sign(rand.Reader, []byte("msg"), crypto.Hash(0)); err != nil {
		t.Fatalf("pure ed25519 sign: %v", err)
	}
}

// TestSignRejectsNonEd25519Signer: a signer whose public key is not Ed25519 is
// an algorithm error at signing time, not a broken envelope later.
func TestSignRejectsNonEd25519Signer(t *testing.T) {
	t.Parallel()
	if _, err := Sign(*representativeArtifact(t), stubSigner{}); !errors.Is(err, ErrUnknownAlgorithm) {
		t.Fatalf("got %v, want ErrUnknownAlgorithm", err)
	}
}

type stubSigner struct{}

func (stubSigner) Public() crypto.PublicKey { return "not-an-ed25519-key" }
func (stubSigner) Sign(io.Reader, []byte, crypto.SignerOpts) ([]byte, error) {
	return nil, errors.New("should not be reached")
}

// TestGenerateEd25519Signer covers the nil-entropy default and a failing reader.
func TestGenerateEd25519Signer(t *testing.T) {
	t.Parallel()
	s, err := GenerateEd25519Signer(nil)
	if err != nil {
		t.Fatalf("GenerateEd25519Signer(nil): %v", err)
	}
	if !strings.HasPrefix(s.KeyID(), "ed25519:") {
		t.Errorf("key id %q lacks the algorithm prefix", s.KeyID())
	}
	if _, err := GenerateEd25519Signer(failingReader{}); err == nil {
		t.Error("expected an error from a failing entropy source")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("no entropy") }
