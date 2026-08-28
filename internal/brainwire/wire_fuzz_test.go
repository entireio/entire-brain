package brainwire

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"testing"
)

// A second wave of brainwire fuzzing, aimed at the surfaces the first wave
// (canonical_fuzz_test.go: Canonicalize idempotence, struct/bytes agreement,
// single-byte tamper on already-canonical bytes) did NOT reach:
//
//   - VerifyBytes, the TRANSPORT entry point, whose input is arbitrary
//     non-canonical bytes rather than canonical ones;
//   - the detached Signature envelope as it arrives over JSON, including
//     attacker-chosen alg / key_id / canonical_encoding / sig;
//   - the unknown-field pass-through, which ADR 0001 requires and which must
//     therefore be proven UNABUSABLE rather than removed;
//   - CheckCompatibility, the version string a hosted brain hands the client
//     before any fact is read.
//
//	go test ./internal/brainwire -run xxx -fuzz FuzzName

// FuzzVerifyBytesRejectsTamper is FuzzVerifyRejectsTamper's transport twin: the
// signature is taken over canonical bytes, but verification is offered the RAW
// (possibly non-canonical) bytes the peer sent. Cosmetically different encodings
// of the same content must still verify; any change to the content must not.
func FuzzVerifyBytesRejectsTamper(f *testing.F) {
	f.Add([]byte(`{"manifest":{"repo_key":"gh/o/r","generated_at":"2026-01-02T03:04:05Z","brain_schema_version":"1.0"}}`), byte(0), byte(1))
	f.Add([]byte(`{"manifest":{"generated_at":"2026-01-02t03:04:05z","brain_schema_version":"1.0","repo_key":"r"},"snapshots":[]}`), byte(3), byte(9))
	f.Add([]byte(`{"manifest":{"repo_key":"r","generated_at":"2026-01-02T03:04:05Z","brain_schema_version":"1.0"},"x_new":{"b":1,"a":2}}`), byte(5), byte(17))

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
		// The raw bytes are equivalent to what was signed, so the transport
		// entry point must accept them even though they are not canonical.
		if err := VerifyBytes(raw, sig, signer.Public()); err != nil {
			t.Fatalf("VerifyBytes rejected bytes equivalent to what was signed: %v\nraw=%q\ncanon=%q", err, raw, canon)
		}
		// Any mutation of the RAW bytes either fails to canonicalize, or
		// canonicalizes to something different, which must not verify.
		if len(raw) == 0 || delta == 0 {
			return
		}
		tampered := append([]byte(nil), raw...)
		tampered[int(pos)%len(tampered)] += delta
		mutCanon, err := Canonicalize(tampered)
		if err != nil {
			return // unparseable after mutation: rejected, which is correct
		}
		if bytes.Equal(mutCanon, canon) {
			return // the mutation was purely cosmetic; verifying is correct
		}
		if err := VerifyBytes(tampered, sig, signer.Public()); err == nil {
			t.Fatalf("mutated transport bytes verified:\norig =%q\ntamp =%q\ncanon=%q\nmut  =%q", raw, tampered, canon, mutCanon)
		}
	})
}

// FuzzUnknownFieldsCannotSmuggle is the ADR 0001 obligation stated as a testable
// property. Unknown fields pass through canonicalization ON PURPOSE so an older
// verifier can check a newer producer's artifact. That tolerance is only safe if
// it cannot change what the KNOWN fields mean: for any pair of byte strings that
// verify under the same signature, their decoded BrainArtifact — the entire
// surface every consumer in this repo reads — must be identical.
func FuzzUnknownFieldsCannotSmuggle(f *testing.F) {
	base := `{"manifest":{"repo_key":"gh/o/r","generated_at":"2026-01-02T03:04:05Z","brain_schema_version":"1.0"}}`
	f.Add([]byte(base), []byte(base))
	f.Add([]byte(base), []byte(`{"manifest":{"repo_key":"gh/o/r","generated_at":"2026-01-02T03:04:05Z","brain_schema_version":"1.0"},"future":1}`))
	f.Add([]byte(`{"manifest":{"repo_key":"a","generated_at":"2026-01-02T03:04:05Z","brain_schema_version":"1.0","future":"x"}}`),
		[]byte(`{"manifest":{"repo_key":"b","generated_at":"2026-01-02T03:04:05Z","brain_schema_version":"1.0","future":"x"}}`))

	signer := mustFuzzSigner(f)
	f.Fuzz(func(t *testing.T, rawA, rawB []byte) {
		canonA, err := Canonicalize(rawA)
		if err != nil {
			return
		}
		sig, err := SignCanonical(canonA, signer)
		if err != nil {
			t.Fatalf("SignCanonical: %v", err)
		}
		if err := VerifyBytes(rawB, sig, signer.Public()); err != nil {
			return // B is not covered by the signature: nothing to prove
		}
		// B verified under a signature made over A. Every consumer-visible
		// field must therefore agree.
		var a, b BrainArtifact
		if err := CanonicalUnmarshal(rawA, &a); err != nil {
			t.Fatalf("signed bytes failed to decode: %v", err)
		}
		if err := CanonicalUnmarshal(rawB, &b); err != nil {
			t.Fatalf("verified bytes failed to decode: %v", err)
		}
		jsonA, _ := json.Marshal(a)
		jsonB, _ := json.Marshal(b)
		if !bytes.Equal(jsonA, jsonB) {
			t.Fatalf("two byte strings verified under one signature decode to different artifacts:\nA=%q -> %s\nB=%q -> %s", rawA, jsonA, rawB, jsonB)
		}
	})
}

// FuzzSignatureEnvelope drives the detached envelope as it arrives over the
// wire: a peer chooses every field. Validate and SigningInput must agree about
// what is well formed, VerifyCanonical must never panic, and — the property that
// matters — an envelope whose bound metadata was altered must never verify, since
// alg/key_id/canonical_encoding are part of the signed pre-image.
func FuzzSignatureEnvelope(f *testing.F) {
	f.Add([]byte(`{"alg":"ed25519","key_id":"ed25519:00","canonical_encoding":"brainwire-canonical/1","sig":"AA=="}`))
	f.Add([]byte(`{"alg":"","key_id":"","canonical_encoding":"","sig":""}`))
	f.Add([]byte(`{"alg":"ed25519\n","key_id":"x","canonical_encoding":"brainwire-canonical/1","sig":"!!!"}`))
	f.Add([]byte(`{"alg":"ed25519","key_id":"ed25519:00","canonical_encoding":"brainwire-canonical/2","sig":"AA=="}`))

	signer := mustFuzzSigner(f)
	canon, err := Canonicalize([]byte(`{"manifest":{"repo_key":"r","generated_at":"2026-01-02T03:04:05Z","brain_schema_version":"1.0"}}`))
	if err != nil {
		f.Fatalf("seed canonicalize: %v", err)
	}
	good, err := SignCanonical(canon, signer)
	if err != nil {
		f.Fatalf("seed sign: %v", err)
	}

	f.Fuzz(func(t *testing.T, envJSON []byte) {
		var env Signature
		if err := json.Unmarshal(envJSON, &env); err != nil {
			return
		}
		// Must never panic, whatever the peer sent.
		validateErr := env.Validate()
		_, inputErr := SigningInput(env, canon)
		if validateErr == nil && inputErr != nil {
			t.Fatalf("Validate accepted an envelope SigningInput rejected: %#v (%v)", env, inputErr)
		}
		if err := VerifyCanonical(canon, env, signer.Public()); err == nil {
			// The only envelope that may verify is the honest one.
			if env.Alg != good.Alg || env.KeyID != good.KeyID ||
				env.CanonicalEncoding != good.CanonicalEncoding || env.Sig != good.Sig {
				t.Fatalf("a forged envelope verified: %#v (honest: %#v)", env, good)
			}
		}
		// Raw-signature-shaped inputs must be rejected without panicking.
		if raw, derr := base64.StdEncoding.DecodeString(env.Sig); derr == nil && len(raw) == ed25519.SignatureSize {
			_ = VerifyCanonical(canon, env, signer.Public())
		}
	})
}

// FuzzCheckCompatibility drives the version string a hosted brain hands the
// client on initialize, before any fact is read. It must classify or error, never
// panic, and never report a DIFFERENT major as compatible.
func FuzzCheckCompatibility(f *testing.F) {
	f.Add("1.0")
	f.Add("1.99")
	f.Add("2.0")
	f.Add("")
	f.Add("1")
	f.Add("1.0.0")
	f.Add("999999999999999999999.0")
	f.Add("-1.-1")
	f.Add("01.02")
	f.Fuzz(func(t *testing.T, v string) {
		ok, _, err := CheckCompatibility(v)
		if err != nil {
			if ok {
				t.Fatalf("CheckCompatibility(%q) reported ok with an error: %v", v, err)
			}
			return
		}
		if !ok {
			return
		}
		major, _, perr := parseSchemaVersion(v)
		if perr != nil {
			t.Fatalf("CheckCompatibility(%q) accepted a version parseSchemaVersion rejects: %v", v, perr)
		}
		localMajor, _, lerr := parseSchemaVersion(BrainSchemaVersion)
		if lerr != nil {
			t.Fatalf("local schema version %q does not parse: %v", BrainSchemaVersion, lerr)
		}
		if major != localMajor {
			t.Fatalf("CheckCompatibility(%q) accepted major %d against local major %d", v, major, localMajor)
		}
	})
}
