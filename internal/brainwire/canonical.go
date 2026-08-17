package brainwire

// Canonical encoding for the brain wire artifact (milestone P1.M1.3).
//
// The default Go encoding/json round trip is byte-stable only within Go. It
// relies on four implementation details that no other language shares: HTML
// escaping, struct declaration order, `omitempty`, and RFC3339Nano time
// formatting. Anything that must be signed, content-hashed, or byte-compared
// ACROSS implementations goes through CanonicalMarshal / Canonicalize instead.
//
// The canonical form is a profile of RFC 8785 (JSON Canonicalization Scheme)
// restricted to this schema:
//
//  1. No HTML escaping. `<`, `>`, `&` and every other non-control code point is
//     emitted as raw UTF-8. (Affects repo_key, default_branch, branch, path,
//     media_type, provider.)
//  2. Object keys are sorted LEXICOGRAPHICALLY by UTF-16 code unit, never in Go
//     struct declaration order. So "brain_schema_version" precedes "repo_key"
//     even though the struct declares RepoKey first.
//  3. Optional fields are omitted when — and only when — they equal their
//     omitted default (see canonicalArtifact's field table, which mirrors the
//     `omitempty` tags in contract.go). An explicit `"size":0` or `"tree":""`
//     is NON-canonical input and is normalized to omission. A JSON null and an
//     empty array are both treated as absent. Required fields (no `omitempty`)
//     are ALWAYS emitted and are zero-filled when the input omits them.
//  4. Time: generated_at is converted to UTC and TRUNCATED to whole seconds,
//     then written with a literal "Z" suffix — never "+00:00", never a
//     fractional part. Sub-second input TRUNCATES (it does not error), so
//     "...T12:00:00Z" and "...T12:00:00.123456789Z" canonicalize identically.
//     Callers that need sub-second fidelity must not use this contract.
//  5. Numbers: decimal integers only, within ±2^53 (the range every IEEE-754
//     double-based parser represents exactly). No exponent, no fraction, no
//     leading zeros, no leading `+`, no `-0` (normalized to `0`). A fractional
//     or exponent literal, or a magnitude beyond 2^53, is rejected rather than
//     rounded.
//  6. Strings: valid UTF-8, minimally escaped. Only `"`, `\` and C0 controls
//     are escaped; C0 controls use the short forms \b \f \n \r \t where they
//     exist and lowercase \u00xx otherwise.
//
// No JSON tag in contract.go changes: the tags are the contract, and a tag
// change would be a major schema bump. This file only constrains how the
// existing tags are serialized.
//
// Unknown fields (those a newer additive minor introduced, per the
// tolerant-reader rule) are PRESERVED and canonically re-emitted. Their omitted
// defaults are unknowable to this reader, so their values pass through
// unchanged apart from key ordering, string escaping, and number syntax.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

// CanonicalTimeLayout is the only timestamp form a canonical artifact may
// contain: RFC 3339, UTC, whole seconds, literal "Z".
const CanonicalTimeLayout = "2006-01-02T15:04:05Z"

// CanonicalMarshal encodes a BrainArtifact in the canonical form described at
// the top of this file. Two implementations that agree on the artifact's
// logical content MUST produce identical bytes, which makes the output safe to
// sign or content-hash across implementations.
func CanonicalMarshal(a BrainArtifact) ([]byte, error) {
	// Serialize with Go's encoder purely as a structural bridge (HTML escaping
	// off per rule 1), then run the same normalizer foreign bytes take. Having
	// exactly one normalization path is what guarantees
	// CanonicalMarshal(a) == Canonicalize(anyEncodingOf(a)).
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(a); err != nil {
		return nil, fmt.Errorf("brainwire: marshal artifact: %w", err)
	}
	return Canonicalize(buf.Bytes())
}

// Canonicalize normalizes arbitrary (including non-Go-produced) artifact bytes
// into the canonical form. It is idempotent:
// Canonicalize(Canonicalize(b)) == Canonicalize(b).
//
// It fails on input that cannot be expressed canonically without changing
// meaning: malformed JSON, a non-object artifact, a known field with the wrong
// JSON type, a non-integer number, or an unparseable generated_at.
func Canonicalize(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	// Token-level decode rather than json.Decoder.Decode: Go's decoder keeps
	// the LAST occurrence of a duplicated object member, so a signature checked
	// over re-canonicalized bytes would validate transported bytes whose
	// first-occurrence values a first-wins parser (common elsewhere) reads
	// instead — a cross-parser differential in the exact layer built to prevent
	// one. Canonical form therefore rejects duplicate members outright, as
	// RFC 8785 pipelines conventionally do.
	v, err := decodeCanonicalValue(dec, 0)
	if err != nil {
		return nil, fmt.Errorf("brainwire: decode artifact: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("brainwire: trailing data after artifact")
	}

	obj, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("brainwire: artifact must be a JSON object, got %s", jsonKindOf(v))
	}
	norm, err := normalizeObject(canonicalArtifact, obj)
	if err != nil {
		return nil, err
	}
	return encodeCanonical(norm)
}

// CanonicalUnmarshal canonicalizes raw and decodes it into out with the
// tolerant-reader rules (unknown fields ignored, see contract.go). The decoded
// artifact's GeneratedAt is already UTC-truncated, so CanonicalMarshal(*out)
// reproduces Canonicalize(raw) exactly — except when raw carried unknown
// fields, which the typed decode necessarily drops. Use Canonicalize, not this
// function, when the bytes must survive for a signature check.
func CanonicalUnmarshal(raw []byte, out *BrainArtifact) error {
	if out == nil {
		return errors.New("brainwire: CanonicalUnmarshal into nil artifact")
	}
	canon, err := Canonicalize(raw)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(canon, out); err != nil {
		return fmt.Errorf("brainwire: decode canonical artifact: %w", err)
	}
	return nil
}

// canonicalTime applies rule 4: UTC, truncated to whole seconds, literal "Z".
func canonicalTime(t time.Time) string {
	return t.UTC().Format(CanonicalTimeLayout)
}

// maxCanonicalDepth bounds container nesting in artifact bytes. The decoder
// (and the encoder that mirrors its shape) recurses per level, and Canonicalize
// is an entry point for untrusted bytes: without a bound, input like a few
// megabytes of '[' is an unrecoverable runtime stack overflow, not an error.
// Real artifacts nest a handful of levels; 512 is beyond any legitimate use.
const maxCanonicalDepth = 512

// decodeCanonicalValue decodes one JSON value token-by-token, mirroring
// json.Decoder.Decode's shapes (map[string]any, []any, string, bool,
// json.Number, nil) but rejecting duplicate object members, which Decode
// silently resolves last-wins, and bounding nesting depth.
func decodeCanonicalValue(dec *json.Decoder, depth int) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	return decodeCanonicalToken(dec, tok, depth)
}

func decodeCanonicalToken(dec *json.Decoder, tok json.Token, depth int) (any, error) {
	if depth > maxCanonicalDepth {
		return nil, fmt.Errorf("nesting deeper than %d levels is not canonical", maxCanonicalDepth)
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		// string, bool, json.Number, or nil — already in decoded shape.
		return tok, nil
	}
	switch delim {
	case '{':
		obj := map[string]any{}
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyTok.(string)
			if !ok {
				return nil, fmt.Errorf("object member name must be a string, got %v", keyTok)
			}
			if _, dup := obj[key]; dup {
				return nil, fmt.Errorf("duplicate object member %q (canonical form forbids duplicate keys)", key)
			}
			val, err := decodeCanonicalValue(dec, depth+1)
			if err != nil {
				return nil, err
			}
			obj[key] = val
		}
		if _, err := dec.Token(); err != nil { // consume '}'
			return nil, err
		}
		return obj, nil
	case '[':
		arr := []any{}
		for dec.More() {
			el, err := decodeCanonicalValue(dec, depth+1)
			if err != nil {
				return nil, err
			}
			arr = append(arr, el)
		}
		if _, err := dec.Token(); err != nil { // consume ']'
			return nil, err
		}
		return arr, nil
	default:
		return nil, fmt.Errorf("unexpected delimiter %v", delim)
	}
}

// --- field table -------------------------------------------------------------
//
// The table below is the NORMATIVE, exhaustive statement of rule 3: for every
// field of every wire type, whether it is required and, if not, the exact value
// that is omitted. It mirrors the JSON tags in contract.go one-for-one;
// TestCanonicalFieldTableMatchesStructTags enforces that with reflection.

type canonicalKind int

const (
	kindString canonicalKind = iota
	kindInt
	kindTime
	kindObject
	kindObjectArray
)

// omittedDefault names the value that is dropped from the canonical output.
type omittedDefault int

const (
	// omitNever marks a required field: always emitted, zero-filled if the
	// input omits it (a JSON null counts as omitted).
	omitNever omittedDefault = iota
	omitEmptyString
	omitZeroInt
	omitEmptySlice
)

type canonicalField struct {
	// Key is the JSON tag from contract.go. It is the contract; never change
	// it here without a major schema bump.
	Key     string
	Kind    canonicalKind
	Omitted omittedDefault
	// Object is the nested type for kindObject and kindObjectArray.
	Object *canonicalType
}

type canonicalType struct {
	Name   string
	Fields []canonicalField
}

var canonicalContentRef = &canonicalType{
	Name: "ContentRef",
	Fields: []canonicalField{
		{Key: "digest", Kind: kindString, Omitted: omitNever},
		{Key: "size", Kind: kindInt, Omitted: omitZeroInt},
		{Key: "media_type", Kind: kindString, Omitted: omitEmptyString},
		{Key: "path", Kind: kindString, Omitted: omitEmptyString},
	},
}

var canonicalSnapshotRef = &canonicalType{
	Name: "SnapshotRef",
	Fields: []canonicalField{
		{Key: "commit", Kind: kindString, Omitted: omitNever},
		{Key: "tree", Kind: kindString, Omitted: omitEmptyString},
		{Key: "content", Kind: kindObject, Omitted: omitNever, Object: canonicalContentRef},
	},
}

var canonicalOverlayRef = &canonicalType{
	Name: "OverlayRef",
	Fields: []canonicalField{
		{Key: "base_commit", Kind: kindString, Omitted: omitNever},
		{Key: "head_commit", Kind: kindString, Omitted: omitNever},
		{Key: "branch", Kind: kindString, Omitted: omitEmptyString},
		{Key: "content", Kind: kindObject, Omitted: omitNever, Object: canonicalContentRef},
	},
}

var canonicalFactsRef = &canonicalType{
	Name: "FactsRef",
	Fields: []canonicalField{
		{Key: "branch", Kind: kindString, Omitted: omitNever},
		{Key: "content", Kind: kindObject, Omitted: omitNever, Object: canonicalContentRef},
	},
}

var canonicalManifest = &canonicalType{
	Name: "BrainManifest",
	Fields: []canonicalField{
		{Key: "repo_key", Kind: kindString, Omitted: omitNever},
		{Key: "default_branch", Kind: kindString, Omitted: omitEmptyString},
		{Key: "generated_at", Kind: kindTime, Omitted: omitNever},
		{Key: "brain_schema_version", Kind: kindString, Omitted: omitNever},
		{Key: "provider", Kind: kindString, Omitted: omitEmptyString},
		{Key: "provider_version", Kind: kindString, Omitted: omitEmptyString},
		{Key: "provider_schema_version", Kind: kindString, Omitted: omitEmptyString},
	},
}

var canonicalArtifact = &canonicalType{
	Name: "BrainArtifact",
	Fields: []canonicalField{
		{Key: "manifest", Kind: kindObject, Omitted: omitNever, Object: canonicalManifest},
		{Key: "snapshots", Kind: kindObjectArray, Omitted: omitEmptySlice, Object: canonicalSnapshotRef},
		{Key: "overlays", Kind: kindObjectArray, Omitted: omitEmptySlice, Object: canonicalOverlayRef},
		{Key: "facts", Kind: kindObjectArray, Omitted: omitEmptySlice, Object: canonicalFactsRef},
	},
}

// --- normalization -----------------------------------------------------------

// normalizeObject applies the field table to one decoded object. A nil result
// value means "omit this key".
func normalizeObject(t *canonicalType, in map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(in))
	known := make(map[string]struct{}, len(t.Fields))

	for _, f := range t.Fields {
		known[f.Key] = struct{}{}
		v, err := normalizeField(t, f, in[f.Key])
		if err != nil {
			return nil, err
		}
		if v != nil {
			out[f.Key] = v
		}
	}
	// Unknown (newer-minor) fields pass through; only their encoding is
	// canonicalized, by encodeValue.
	for k, v := range in {
		if _, ok := known[k]; ok {
			continue
		}
		out[k] = v
	}
	return out, nil
}

func normalizeField(t *canonicalType, f canonicalField, raw any) (any, error) {
	where := t.Name + "." + f.Key
	absent := raw == nil // covers both a missing key and an explicit null

	switch f.Kind {
	case kindString:
		s := ""
		if !absent {
			v, ok := raw.(string)
			if !ok {
				return nil, fmt.Errorf("brainwire: %s must be a string, got %s", where, jsonKindOf(raw))
			}
			s = v
		}
		if s == "" && f.Omitted == omitEmptyString {
			return nil, nil
		}
		return s, nil

	case kindInt:
		var n int64
		if !absent {
			num, ok := raw.(json.Number)
			if !ok {
				return nil, fmt.Errorf("brainwire: %s must be a number, got %s", where, jsonKindOf(raw))
			}
			v, err := canonicalInt(num.String())
			if err != nil {
				return nil, fmt.Errorf("brainwire: %s: %w", where, err)
			}
			n = v
		}
		if n == 0 && f.Omitted == omitZeroInt {
			return nil, nil
		}
		return n, nil

	case kindTime:
		if absent {
			// Required field, zero-filled: same bytes a zero time.Time yields.
			return canonicalTime(time.Time{}), nil
		}
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("brainwire: %s must be an RFC 3339 string, got %s", where, jsonKindOf(raw))
		}
		// RFC 3339 section 5.6 permits lowercase 't' and 'z'; Go's time.Parse
		// does not. Normalize exactly those two before parsing so a
		// spec-compliant non-Go producer's timestamps verify.
		normalized := s
		if len(normalized) >= 11 && normalized[10] == 't' {
			normalized = normalized[:10] + "T" + normalized[11:]
		}
		if strings.HasSuffix(normalized, "z") {
			normalized = normalized[:len(normalized)-1] + "Z"
		}
		ts, err := time.Parse(time.RFC3339, normalized)
		if err != nil {
			return nil, fmt.Errorf("brainwire: %s is not RFC 3339: %w", where, err)
		}
		return canonicalTime(ts), nil

	case kindObject:
		obj := map[string]any{}
		if !absent {
			v, ok := raw.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("brainwire: %s must be an object, got %s", where, jsonKindOf(raw))
			}
			obj = v
		}
		return normalizeObject(f.Object, obj)

	case kindObjectArray:
		if absent {
			return nil, nil
		}
		arr, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("brainwire: %s must be an array, got %s", where, jsonKindOf(raw))
		}
		if len(arr) == 0 {
			return nil, nil
		}
		out := make([]any, 0, len(arr))
		for i, el := range arr {
			obj, ok := el.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("brainwire: %s[%d] must be an object, got %s", where, i, jsonKindOf(el))
			}
			norm, err := normalizeObject(f.Object, obj)
			if err != nil {
				return nil, err
			}
			out = append(out, norm)
		}
		return out, nil
	}
	return nil, fmt.Errorf("brainwire: %s has unknown canonical kind %d", where, f.Kind)
}

// maxCanonicalInt bounds canonical integers to the range every IEEE-754
// double-based JSON parser represents exactly (±2^53). A larger int64 signs
// fine in Go but a conforming JCS/JavaScript verifier re-serializes the
// nearest double and computes different canonical bytes — a silent
// cross-implementation signature failure. Rejecting the value here turns that
// into a loud error at the producer.
const maxCanonicalInt = int64(1) << 53

// canonicalInt applies rule 5 to a JSON number literal. The canonical profile
// is deliberately integers-only: non-integer numbers are rejected rather than
// canonicalized, and a future minor that needs them must ship a new
// Signature.CanonicalEncoding identifier (the envelope pins the encoding for
// exactly this kind of rotation) rather than widen this rule in place.
func canonicalInt(lit string) (int64, error) {
	if lit == "-0" {
		return 0, nil
	}
	if strings.ContainsAny(lit, ".eE") {
		return 0, fmt.Errorf("number %q is not a decimal integer (no fractions or exponents in canonical form)", lit)
	}
	n, err := strconv.ParseInt(lit, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("number %q is not a 64-bit decimal integer", lit)
	}
	if n > maxCanonicalInt || n < -maxCanonicalInt {
		return 0, fmt.Errorf("number %q is outside the interoperable ±2^53 canonical range", lit)
	}
	return n, nil
}

// --- encoding ----------------------------------------------------------------

func encodeCanonical(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := encodeValue(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func encodeValue(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
		return nil
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
		return nil
	case string:
		return encodeString(buf, t)
	case int64:
		buf.WriteString(strconv.FormatInt(t, 10))
		return nil
	case json.Number:
		n, err := canonicalInt(t.String())
		if err != nil {
			return fmt.Errorf("brainwire: %w", err)
		}
		buf.WriteString(strconv.FormatInt(n, 10))
		return nil
	case float64:
		return fmt.Errorf("brainwire: floating-point number %v is not canonical (integers only)", t)
	case map[string]any:
		return encodeObject(buf, t)
	case []any:
		buf.WriteByte('[')
		for i, el := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := encodeValue(buf, el); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
		return nil
	default:
		return fmt.Errorf("brainwire: value of type %T is not encodable in canonical form", v)
	}
}

func encodeObject(buf *bytes.Buffer, m map[string]any) error {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Rule 2: lexicographic by UTF-16 code unit (RFC 8785), not declaration
	// order and not Go's byte order for astral code points.
	sort.Slice(keys, func(i, j int) bool { return lessUTF16(keys[i], keys[j]) })

	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		if err := encodeString(buf, k); err != nil {
			return err
		}
		buf.WriteByte(':')
		if err := encodeValue(buf, m[k]); err != nil {
			return err
		}
	}
	buf.WriteByte('}')
	return nil
}

// encodeString applies rules 1 and 6: raw UTF-8, minimal escaping.
func encodeString(buf *bytes.Buffer, s string) error {
	if !utf8.ValidString(s) {
		return errors.New("brainwire: string is not valid UTF-8")
	}
	buf.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			buf.WriteString(`\"`)
		case c == '\\':
			buf.WriteString(`\\`)
		case c == '\b':
			buf.WriteString(`\b`)
		case c == '\f':
			buf.WriteString(`\f`)
		case c == '\n':
			buf.WriteString(`\n`)
		case c == '\r':
			buf.WriteString(`\r`)
		case c == '\t':
			buf.WriteString(`\t`)
		case c < 0x20:
			buf.WriteString(fmt.Sprintf(`\u%04x`, c))
		default:
			// Everything else, including <, >, &, DEL, U+2028/U+2029 and all
			// multi-byte UTF-8, is written raw.
			buf.WriteByte(c)
		}
	}
	buf.WriteByte('"')
	return nil
}

// lessUTF16 orders two strings by their UTF-16 code units, per RFC 8785.
func lessUTF16(a, b string) bool {
	if a == b {
		return false
	}
	// Fast path: both pure ASCII, where byte order and UTF-16 order agree.
	if isASCII(a) && isASCII(b) {
		return a < b
	}
	au := utf16.Encode([]rune(a))
	bu := utf16.Encode([]rune(b))
	for i := 0; i < len(au) && i < len(bu); i++ {
		if au[i] != bu[i] {
			return au[i] < bu[i]
		}
	}
	return len(au) < len(bu)
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

func jsonKindOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case json.Number, float64, int64:
		return "number"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	default:
		return fmt.Sprintf("%T", v)
	}
}
