package brainwire

// Cross-implementation manifest signing (milestone P1.M1.4).
//
// Blobs are already content-addressed: every ContentRef carries a
// `sha256:<hex>` digest of its bytes, so a reader that fetches a blob can check
// it against the digest without any signature at all. What is NOT self-proving
// is the artifact itself — the set of digests, their keys (commit,
// (base,head), branch) and the identity/version manifest. That is what a
// signature has to cover, and it is why this milestone signs the MANIFEST
// (i.e. the whole BrainArtifact envelope, of which BrainManifest is the
// header), never the blob bytes.
//
// Because every referenced digest is inside the signed pre-image, the
// signature transitively covers all blob content: swapping, adding, or
// removing a blob changes a digest, changes the canonical bytes, and
// invalidates the signature. TestSignatureCommitsToEveryBlobDigest asserts
// exactly that.
//
// # Why the canonical encoding is the substrate
//
// A signature that a non-Go implementation must verify cannot be taken over
// "whatever encoding/json emitted", because that byte string depends on Go's
// HTML escaping, struct declaration order, omitempty, and RFC3339Nano time
// formatting (see canonical.go). The signed bytes are therefore always the
// M1.3 canonical form: any implementation that agrees on the artifact's
// logical content reproduces the same pre-image and the same signature.
//
// # The signed pre-image
//
// A signature does not cover the canonical artifact bytes alone; it covers a
// domain-separated pre-image that also binds the envelope's own metadata:
//
//	SigningDomain     "\n"
//	Signature.Alg     "\n"
//	Signature.KeyID   "\n"
//	Signature.CanonicalEncoding "\n"
//	<canonical artifact bytes>
//
// LF is an unambiguous separator here: Alg, KeyID and CanonicalEncoding are
// validated to contain no LF, and canonical artifact bytes never contain a raw
// LF (the canonical encoder emits no whitespace and escapes control characters
// inside strings). Binding the metadata means an attacker cannot take a valid
// signature and re-label it under a different algorithm, key, or canonical
// encoding version: doing so changes the pre-image, so it no longer verifies.
//
// # Rotation
//
// The algorithm identifier lives in the envelope (Signature.Alg), so a future
// milestone can add a second algorithm without changing the wire shape; a
// verifier that does not know an identifier fails closed with
// ErrUnknownAlgorithm rather than skipping the check. Likewise
// Signature.CanonicalEncoding pins the canonicalization the signature was
// produced under, so a future canonical-encoding change cannot silently
// validate against bytes produced by a different set of rules.

import (
	"crypto"
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
	"io"
	"io/fs"
	"os"
	"runtime"
	"strings"
)

const (
	// SigAlgEd25519 is the algorithm identifier for pure Ed25519 (RFC 8032)
	// over the signing pre-image. It is the only algorithm this build
	// produces or accepts; the identifier is carried in the envelope so a
	// later milestone can rotate to another one additively.
	SigAlgEd25519 = "ed25519"

	// CanonicalEncodingVersion identifies the canonicalization rules
	// (canonical.go, milestone M1.3) a signature was produced under. It is
	// recorded in every envelope and checked on verify, so a future change to
	// the canonical rules cannot silently validate.
	CanonicalEncodingVersion = "brainwire-canonical/1"

	// SigningDomain domain-separates this pre-image from any other byte
	// string the same key might ever sign.
	SigningDomain = "entire-brain/artifact-signature/v1"

	// keyIDPrefix prefixes the public-key fingerprint in Signature.KeyID.
	keyIDPrefix = "ed25519:"
	// keyIDBytes is how many bytes of the SHA-256 public-key hash the
	// fingerprint keeps (128 bits, ample for identification; the key id is an
	// identifier, not a security boundary — the signature check is).
	keyIDBytes = 16
)

// Signature verification and key-loading failures. Every one is distinguishable
// with errors.Is so a caller can react to "this is not signed by the key I
// expected" differently from "these bytes were tampered with".
var (
	// ErrBadSignature means the cryptographic check failed: the artifact,
	// the signature, or some bound envelope field was altered after signing.
	ErrBadSignature = errors.New("brainwire: signature does not verify")

	// ErrUnknownAlgorithm means Signature.Alg names an algorithm this build
	// does not implement, or the supplied key is not of the envelope's
	// algorithm type. Verification fails closed.
	ErrUnknownAlgorithm = errors.New("brainwire: unknown signature algorithm")

	// ErrUnsupportedEncoding means the signature was produced under a
	// canonical-encoding version this build does not implement, so its
	// pre-image cannot be reconstructed.
	ErrUnsupportedEncoding = errors.New("brainwire: unsupported canonical encoding version")

	// ErrKeyMismatch means the envelope names a different key than the one
	// offered for verification.
	ErrKeyMismatch = errors.New("brainwire: signature key id does not match the verification key")

	// ErrNotCanonical means the bytes offered for signing or verification are
	// not in canonical form, so signing or verifying them would not be
	// reproducible by another implementation.
	ErrNotCanonical = errors.New("brainwire: input is not in canonical form")

	// ErrMalformedSignature means the envelope is structurally invalid: a
	// missing field, a field containing LF, or a signature that is not
	// base64 of the right length.
	ErrMalformedSignature = errors.New("brainwire: malformed signature envelope")

	// ErrNoKey means no usable key material was found at the given source.
	ErrNoKey = errors.New("brainwire: no key material")

	// ErrInsecureKeyFile means a private-key file is readable by users other
	// than its owner.
	ErrInsecureKeyFile = errors.New("brainwire: private key file is group- or world-accessible")
)

// Signature is the detached signature envelope for a BrainArtifact.
//
// Detached: it is transported alongside the artifact, never inside it, so the
// artifact's canonical bytes are exactly what was signed and adding a signature
// does not change them. Every field is explicit — nothing about the algorithm,
// the key, or the encoding rules is implied by context, and all four are bound
// into the signed pre-image (see SigningInput).
type Signature struct {
	// Alg is the signature algorithm identifier, e.g. SigAlgEd25519.
	Alg string `json:"alg"`
	// KeyID is the deterministic fingerprint of the public key that produced
	// the signature, as returned by KeyID (e.g. "ed25519:9f86d081884c7d65…").
	// It is derived from the public key, so it cannot disagree with the key
	// it names; it is not secret.
	KeyID string `json:"key_id"`
	// CanonicalEncoding is the canonical-encoding version the signed bytes
	// were produced under, e.g. CanonicalEncodingVersion.
	CanonicalEncoding string `json:"canonical_encoding"`
	// Sig is the raw detached signature, standard base64 with padding.
	Sig string `json:"sig"`
}

// Validate checks the envelope's structure — not the cryptography. It reports
// ErrMalformedSignature for a missing field or a field carrying an LF (which
// would make the signing pre-image ambiguous), ErrUnknownAlgorithm for an
// algorithm this build cannot verify, and ErrUnsupportedEncoding for a
// canonical-encoding version it cannot reproduce.
func (s Signature) Validate() error {
	for _, f := range []struct{ name, val string }{
		{"alg", s.Alg},
		{"key_id", s.KeyID},
		{"canonical_encoding", s.CanonicalEncoding},
		{"sig", s.Sig},
	} {
		if f.val == "" {
			return fmt.Errorf("%w: %s is empty", ErrMalformedSignature, f.name)
		}
		if strings.ContainsRune(f.val, '\n') {
			return fmt.Errorf("%w: %s contains a newline", ErrMalformedSignature, f.name)
		}
	}
	if s.Alg != SigAlgEd25519 {
		return fmt.Errorf("%w: %q (this build implements %q)", ErrUnknownAlgorithm, s.Alg, SigAlgEd25519)
	}
	if s.CanonicalEncoding != CanonicalEncodingVersion {
		return fmt.Errorf("%w: %q (this build implements %q)", ErrUnsupportedEncoding, s.CanonicalEncoding, CanonicalEncodingVersion)
	}
	return nil
}

// SigningInput returns the exact bytes a signature covers, given the envelope's
// metadata and the canonical artifact bytes. Signature.Sig is ignored.
//
// It is exported so a non-Go implementation can be checked against a golden
// pre-image, and so callers can prove for themselves what a signature commits
// to. canonical must already be in canonical form; SigningInput does not
// canonicalize it.
func SigningInput(env Signature, canonical []byte) ([]byte, error) {
	for _, f := range []struct{ name, val string }{
		{"alg", env.Alg},
		{"key_id", env.KeyID},
		{"canonical_encoding", env.CanonicalEncoding},
	} {
		if f.val == "" {
			return nil, fmt.Errorf("%w: %s is empty", ErrMalformedSignature, f.name)
		}
		if strings.ContainsRune(f.val, '\n') {
			return nil, fmt.Errorf("%w: %s contains a newline", ErrMalformedSignature, f.name)
		}
	}
	head := SigningDomain + "\n" + env.Alg + "\n" + env.KeyID + "\n" + env.CanonicalEncoding + "\n"
	out := make([]byte, 0, len(head)+len(canonical))
	out = append(out, head...)
	out = append(out, canonical...)
	return out, nil
}

// KeyID returns the deterministic fingerprint of an Ed25519 public key:
// "ed25519:" followed by the first 16 bytes of SHA-256 over the 32 raw public
// key bytes, lowercase hex. Two implementations derive the same id from the
// same key, which is what makes ErrKeyMismatch meaningful across languages.
func KeyID(pub crypto.PublicKey) (string, error) {
	ed, err := ed25519PublicKey(pub)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(ed)
	return keyIDPrefix + hex.EncodeToString(sum[:keyIDBytes]), nil
}

func ed25519PublicKey(pub crypto.PublicKey) (ed25519.PublicKey, error) {
	ed, ok := pub.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("%w: public key of type %T is not %s", ErrUnknownAlgorithm, pub, SigAlgEd25519)
	}
	if len(ed) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: ed25519 public key is %d bytes, want %d", ErrUnknownAlgorithm, len(ed), ed25519.PublicKeySize)
	}
	return ed, nil
}

// Sign produces a detached signature over the canonical encoding of a, using
// signer's private key. The artifact is canonicalized first, so the signature
// is over bytes any implementation can reproduce.
//
// signer is a crypto.Signer rather than raw key bytes so callers can hold keys
// in an HSM, a KMS, or an agent; this build requires signer.Public() to be an
// ed25519.PublicKey.
func Sign(a BrainArtifact, signer crypto.Signer) (Signature, error) {
	canonical, err := CanonicalMarshal(a)
	if err != nil {
		return Signature{}, err
	}
	return SignCanonical(canonical, signer)
}

// SignCanonical signs bytes that are already in canonical form. It rejects
// non-canonical input with ErrNotCanonical rather than silently normalizing it,
// because a caller that hands over non-canonical bytes has almost certainly
// lost track of what will be verified later.
func SignCanonical(canonical []byte, signer crypto.Signer) (Signature, error) {
	if signer == nil {
		return Signature{}, fmt.Errorf("%w: nil signer", ErrNoKey)
	}
	if err := assertCanonical(canonical); err != nil {
		return Signature{}, err
	}

	pub, err := ed25519PublicKey(signer.Public())
	if err != nil {
		return Signature{}, err
	}
	keyID, err := KeyID(pub)
	if err != nil {
		return Signature{}, err
	}

	env := Signature{
		Alg:               SigAlgEd25519,
		KeyID:             keyID,
		CanonicalEncoding: CanonicalEncodingVersion,
	}
	input, err := SigningInput(env, canonical)
	if err != nil {
		return Signature{}, err
	}

	// crypto.Hash(0) selects pure Ed25519 (Ed25519ph is a different scheme
	// and would not interoperate).
	raw, err := signer.Sign(rand.Reader, input, crypto.Hash(0))
	if err != nil {
		return Signature{}, fmt.Errorf("brainwire: sign artifact: %w", err)
	}
	if len(raw) != ed25519.SignatureSize {
		return Signature{}, fmt.Errorf("%w: signer returned %d bytes, want %d", ErrMalformedSignature, len(raw), ed25519.SignatureSize)
	}
	env.Sig = base64.StdEncoding.EncodeToString(raw)
	return env, nil
}

// Verify checks a detached signature over the canonical encoding of a.
//
// It returns nil only when the envelope is well formed, names an algorithm and
// canonical encoding this build implements, names the offered key, and the
// cryptographic check passes. Failures are distinguishable with errors.Is:
// ErrMalformedSignature, ErrUnknownAlgorithm, ErrUnsupportedEncoding,
// ErrKeyMismatch, ErrBadSignature.
func Verify(a BrainArtifact, sig Signature, pub crypto.PublicKey) error {
	canonical, err := CanonicalMarshal(a)
	if err != nil {
		return err
	}
	return VerifyCanonical(canonical, sig, pub)
}

// VerifyBytes verifies a signature against artifact bytes received over a
// transport. The bytes are canonicalized first, so a producer in another
// language whose serializer differs cosmetically (key order, escaping, an
// explicit "size":0) still verifies, as long as the logical content matches.
//
// Unknown fields introduced by a newer additive minor survive canonicalization,
// so an older verifier can still check a newer producer's signature. Malformed
// or non-canonicalizable input is reported as ErrNotCanonical.
func VerifyBytes(raw []byte, sig Signature, pub crypto.PublicKey) error {
	canonical, err := Canonicalize(raw)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNotCanonical, err)
	}
	return VerifyCanonical(canonical, sig, pub)
}

// VerifyCanonical verifies a signature against bytes that are already in
// canonical form, rejecting non-canonical input with ErrNotCanonical. Use
// VerifyBytes when the bytes came off a transport and may merely be
// equivalent rather than canonical.
func VerifyCanonical(canonical []byte, sig Signature, pub crypto.PublicKey) error {
	if err := sig.Validate(); err != nil {
		return err
	}
	if err := assertCanonical(canonical); err != nil {
		return err
	}

	ed, err := ed25519PublicKey(pub)
	if err != nil {
		return err
	}
	keyID, err := KeyID(ed)
	if err != nil {
		return err
	}
	if keyID != sig.KeyID {
		return fmt.Errorf("%w: envelope names %s, verification key is %s", ErrKeyMismatch, sig.KeyID, keyID)
	}

	raw, err := base64.StdEncoding.DecodeString(sig.Sig)
	if err != nil {
		return fmt.Errorf("%w: sig is not standard base64", ErrMalformedSignature)
	}
	if len(raw) != ed25519.SignatureSize {
		return fmt.Errorf("%w: sig is %d bytes, want %d", ErrMalformedSignature, len(raw), ed25519.SignatureSize)
	}

	input, err := SigningInput(sig, canonical)
	if err != nil {
		return err
	}
	if !ed25519.Verify(ed, input, raw) {
		return ErrBadSignature
	}
	return nil
}

// assertCanonical reports ErrNotCanonical unless b is byte-identical to its own
// canonicalization.
func assertCanonical(b []byte) error {
	canon, err := Canonicalize(b)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNotCanonical, err)
	}
	if string(canon) != string(b) {
		return fmt.Errorf("%w: bytes differ from their canonical form", ErrNotCanonical)
	}
	return nil
}

// ReferencedDigests returns every blob digest the artifact references, in
// artifact order: snapshots, then overlays, then facts.
//
// Each of these strings appears verbatim inside the canonical bytes a signature
// covers, which is what makes "the signature commits to every blob" checkable
// rather than merely asserted. See TestSignatureCommitsToEveryBlobDigest.
func ReferencedDigests(a BrainArtifact) []string {
	out := make([]string, 0, len(a.Snapshots)+len(a.Overlays)+len(a.Facts))
	for _, r := range a.Snapshots {
		out = append(out, r.Content.Digest)
	}
	for _, r := range a.Overlays {
		out = append(out, r.Content.Digest)
	}
	for _, r := range a.Facts {
		out = append(out, r.Content.Digest)
	}
	return out
}

// --- keys ---------------------------------------------------------------------

// Ed25519Signer is a crypto.Signer over an in-memory Ed25519 private key that
// refuses to print its key material.
//
// String, GoString, MarshalText and MarshalJSON all render only the public key
// id, so the key cannot leak through a log line, a %v, a %#v, a structured
// logger, or a JSON dump of a config struct that happens to embed it.
type Ed25519Signer struct {
	priv  ed25519.PrivateKey
	keyID string
}

// Compile-time proof of the interfaces we rely on for redaction and signing.
var (
	_ crypto.Signer  = (*Ed25519Signer)(nil)
	_ fmt.Stringer   = (*Ed25519Signer)(nil)
	_ fmt.GoStringer = (*Ed25519Signer)(nil)
	_ json.Marshaler = (*Ed25519Signer)(nil)
)

// NewEd25519Signer wraps an existing private key.
func NewEd25519Signer(priv ed25519.PrivateKey) (*Ed25519Signer, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("%w: ed25519 private key is %d bytes, want %d", ErrNoKey, len(priv), ed25519.PrivateKeySize)
	}
	keyID, err := KeyID(priv.Public())
	if err != nil {
		return nil, err
	}
	return &Ed25519Signer{priv: priv, keyID: keyID}, nil
}

// GenerateEd25519Signer generates a fresh key. Pass nil for crypto/rand.
func GenerateEd25519Signer(entropy io.Reader) (*Ed25519Signer, error) {
	if entropy == nil {
		entropy = rand.Reader
	}
	_, priv, err := ed25519.GenerateKey(entropy)
	if err != nil {
		return nil, fmt.Errorf("brainwire: generate ed25519 key: %w", err)
	}
	return NewEd25519Signer(priv)
}

// Public returns the ed25519.PublicKey.
func (s *Ed25519Signer) Public() crypto.PublicKey { return s.priv.Public() }

// KeyID returns the public-key fingerprint that Sign records in the envelope.
func (s *Ed25519Signer) KeyID() string { return s.keyID }

// Sign implements crypto.Signer with pure Ed25519. opts must select no hash
// (crypto.Hash(0)); Ed25519ph would not interoperate with this contract.
func (s *Ed25519Signer) Sign(_ io.Reader, message []byte, opts crypto.SignerOpts) ([]byte, error) {
	if opts != nil && opts.HashFunc() != crypto.Hash(0) {
		return nil, fmt.Errorf("%w: %s requires pure Ed25519 (no pre-hash), got %v", ErrUnknownAlgorithm, SigAlgEd25519, opts.HashFunc())
	}
	return ed25519.Sign(s.priv, message), nil
}

// String renders the signer without its key material.
func (s *Ed25519Signer) String() string {
	if s == nil {
		return "brainwire.Ed25519Signer(nil)"
	}
	return "brainwire.Ed25519Signer(" + s.keyID + ")"
}

// GoString renders the signer without its key material under %#v.
func (s *Ed25519Signer) GoString() string { return s.String() }

// MarshalText renders the signer without its key material.
func (s *Ed25519Signer) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// MarshalJSON renders the signer without its key material, so a config struct
// embedding it cannot serialize the private key.
func (s *Ed25519Signer) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

// LoadSignerFile reads a private key from path.
//
// Accepted encodings: a PEM "PRIVATE KEY" block (PKCS#8, what `openssl genpkey
// -algorithm ed25519` writes) or bare base64 of a 32-byte seed or a 64-byte
// Ed25519 private key. On non-Windows hosts the file must not be readable by
// group or other, or the load fails with ErrInsecureKeyFile.
//
// No error returned by this function contains key material — only the path,
// the encoding that was attempted, and lengths.
func LoadSignerFile(path string) (*Ed25519Signer, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("%w: stat key file %s: %w", ErrNoKey, path, err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&fs.FileMode(0o077) != 0 {
		return nil, fmt.Errorf("%w: %s has mode %04o, want 0600", ErrInsecureKeyFile, path, info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: read key file %s: %w", ErrNoKey, path, err)
	}
	signer, err := ParseEd25519PrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("key file %s: %w", path, err)
	}
	return signer, nil
}

// LoadSignerEnv reads a private key from the named environment variable, in the
// same encodings as LoadSignerFile (a PEM block with literal or "\n"-escaped
// newlines, or bare base64). An unset or empty variable is ErrNoKey.
//
// No error returned by this function contains key material — only the variable
// name.
func LoadSignerEnv(name string) (*Ed25519Signer, error) {
	val, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(val) == "" {
		return nil, fmt.Errorf("%w: environment variable %s is unset or empty", ErrNoKey, name)
	}
	// Env vars commonly carry PEM with escaped newlines; accept that form.
	val = strings.ReplaceAll(val, `\n`, "\n")
	signer, err := ParseEd25519PrivateKey([]byte(val))
	if err != nil {
		return nil, fmt.Errorf("environment variable %s: %w", name, err)
	}
	return signer, nil
}

// ParseEd25519PrivateKey parses a PEM PKCS#8 "PRIVATE KEY" block, or bare
// base64 (standard or raw, with surrounding whitespace) of a 32-byte seed or a
// 64-byte Ed25519 private key.
//
// Errors never include key material.
func ParseEd25519PrivateKey(data []byte) (*Ed25519Signer, error) {
	if block, _ := pem.Decode(data); block != nil {
		if block.Type != "PRIVATE KEY" {
			return nil, fmt.Errorf("%w: PEM block type %q, want \"PRIVATE KEY\"", ErrNoKey, block.Type)
		}
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%w: PKCS#8 parse failed", ErrNoKey)
		}
		priv, ok := key.(ed25519.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("%w: PKCS#8 key of type %T is not %s", ErrUnknownAlgorithm, key, SigAlgEd25519)
		}
		return NewEd25519Signer(priv)
	}

	raw, err := decodeBareBase64(data)
	if err != nil {
		return nil, err
	}
	switch len(raw) {
	case ed25519.SeedSize:
		return NewEd25519Signer(ed25519.NewKeyFromSeed(raw))
	case ed25519.PrivateKeySize:
		return NewEd25519Signer(ed25519.PrivateKey(raw))
	default:
		return nil, fmt.Errorf("%w: decoded %d bytes, want a %d-byte seed or %d-byte private key",
			ErrNoKey, len(raw), ed25519.SeedSize, ed25519.PrivateKeySize)
	}
}

// LoadPublicKeyFile reads a verification key from path, in the encodings
// ParseEd25519PublicKey accepts. Public keys carry no permission requirement.
func LoadPublicKeyFile(path string) (ed25519.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: read public key file %s: %w", ErrNoKey, path, err)
	}
	pub, err := ParseEd25519PublicKey(data)
	if err != nil {
		return nil, fmt.Errorf("public key file %s: %w", path, err)
	}
	return pub, nil
}

// ParseEd25519PublicKey parses a PEM PKIX "PUBLIC KEY" block or bare base64 of
// the 32 raw public key bytes.
func ParseEd25519PublicKey(data []byte) (ed25519.PublicKey, error) {
	if block, _ := pem.Decode(data); block != nil {
		if block.Type != "PUBLIC KEY" {
			return nil, fmt.Errorf("%w: PEM block type %q, want \"PUBLIC KEY\"", ErrNoKey, block.Type)
		}
		key, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%w: PKIX parse failed: %w", ErrNoKey, err)
		}
		return ed25519PublicKey(key)
	}
	raw, err := decodeBareBase64(data)
	if err != nil {
		return nil, err
	}
	return ed25519PublicKey(ed25519.PublicKey(raw))
}

// MarshalPrivateKeyPEM encodes a signer's key as a PKCS#8 "PRIVATE KEY" PEM
// block — the interoperable on-disk form, readable by openssl and by non-Go
// implementations. Callers must write the result with mode 0600; see
// LoadSignerFile.
func MarshalPrivateKeyPEM(s *Ed25519Signer) ([]byte, error) {
	if s == nil {
		return nil, fmt.Errorf("%w: nil signer", ErrNoKey)
	}
	der, err := x509.MarshalPKCS8PrivateKey(s.priv)
	if err != nil {
		return nil, fmt.Errorf("brainwire: marshal private key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// MarshalPublicKeyPEM encodes a public key as a PKIX "PUBLIC KEY" PEM block.
func MarshalPublicKeyPEM(pub crypto.PublicKey) ([]byte, error) {
	ed, err := ed25519PublicKey(pub)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKIXPublicKey(ed)
	if err != nil {
		return nil, fmt.Errorf("brainwire: marshal public key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}

// decodeBareBase64 decodes standard or raw (unpadded) base64, ignoring
// surrounding and embedded whitespace. Its error never includes the input.
func decodeBareBase64(data []byte) ([]byte, error) {
	s := strings.Join(strings.Fields(string(data)), "")
	if s == "" {
		return nil, fmt.Errorf("%w: empty key material", ErrNoKey)
	}
	if raw, err := base64.StdEncoding.DecodeString(s); err == nil {
		return raw, nil
	}
	raw, err := base64.RawStdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("%w: key material is neither PEM nor base64", ErrNoKey)
	}
	return raw, nil
}
