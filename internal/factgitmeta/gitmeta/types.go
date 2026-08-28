// Vendored git-meta exchange engine. Originally copied verbatim from
// github.com/entirehq/git-meta-service/internal/gitmeta @
// feat/git-meta-service-integration (f3ec153) — do not edit here.
//
// PROVENANCE HAS MOVED: git-meta-service was a proof of concept, and the shipping
// implementation now lives in github.com/entirehq/entire-api internal/gitmeta. Re-vendor
// from THERE, not from the PoC repo, whose PR was superseded rather than merged.
//
// The two copies have since diverged in both directions — entire-api grew path-target
// support (PathTargetSubtree, ValidatePathTargetValue) and typed accessors, while this
// copy carries helpers of its own and the older ValidateTargetValue name. That is inert
// today because this engine only ever drives the BARE LOCAL store at
// refs/meta/local/main: no remote, no push or fetch, and no ref that entire-api also
// writes, so no record crosses between the two implementations. It stops being inert the
// moment brain exchanges git-meta records with entire-api over a shared ref, which is
// what makes deduplicating this the right follow-up: both are internal packages, so
// neither can import the other and a real fix means extracting the engine into a shared
// module.

// Package gitmeta implements the git-meta exchange format (see
// https://git-meta.com and the upstream git-meta specification). It serializes
// (target, key, value) metadata into deterministic Git trees and materializes
// those trees back into structured rows, matching the upstream Rust reference
// implementation byte-for-byte so the two interoperate over plain Git refs.
package gitmeta

import (
	"crypto/sha1" //nolint:gosec // git object IDs are SHA-1 by definition; this matches the wire format
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Reserved tree components. Any path component beginning with "__" is reserved
// for git-meta structure; user key segments may never start with "__".
const (
	StringValueBlob     = "__value"      // terminal blob holding a string value
	ListValueDir        = "__list"       // directory of list entry blobs
	SetValueDir         = "__set"        // tree of set member blobs (entry name == blob OID)
	TombstoneRoot       = "__tombstones" // whole-key tombstones live here
	TombstoneBlob       = "__deleted"    // terminal blob name for a whole-key tombstone
	PathTargetSeparator = "__target__"   // separates a serialized path target from its key path
)

// GitRefThreshold mirrors the reference implementation's large-value cutoff
// (bytes). Values larger than this are candidates for out-of-line storage; in
// this service we keep full values inline in Postgres but expose the constant
// for parity and guardrails.
const GitRefThreshold = 1024

// TargetType is the kind of object metadata attaches to.
type TargetType string

const (
	TargetCommit   TargetType = "commit"
	TargetChangeID TargetType = "change-id"
	TargetBranch   TargetType = "branch"
	TargetPath     TargetType = "path"
	TargetProject  TargetType = "project"
)

// ParseTargetType parses the wire-format string for a target type.
func ParseTargetType(s string) (TargetType, error) {
	switch TargetType(s) {
	case TargetCommit, TargetChangeID, TargetBranch, TargetPath, TargetProject:
		return TargetType(s), nil
	default:
		return "", fmt.Errorf("unknown target type: %q", s)
	}
}

// ValueType is the storage type of a metadata value.
type ValueType string

const (
	ValueString ValueType = "string"
	ValueList   ValueType = "list"
	ValueSet    ValueType = "set"
)

// Target is a resolved metadata target: a type plus an optional value
// (project targets carry no value).
type Target struct {
	Type  TargetType
	Value string // empty only for project targets
}

// MinTargetValueLen is the fewest characters a non-project target value may
// carry, matching the reference implementation. It is also a hard structural
// requirement for commit targets, whose tree path shards on Value[:2].
const MinTargetValueLen = 3

// ParseTarget parses a target in "type:value" form (e.g. "commit:abc123"), or
// the bare "project". The value must be at least MinTargetValueLen characters
// for non-project targets, matching the reference implementation.
func ParseTarget(s string) (Target, error) {
	if s == string(TargetProject) {
		return Target{Type: TargetProject}, nil
	}
	typeStr, value, ok := strings.Cut(s, ":")
	if !ok {
		return Target{}, errors.New("target must be in type:value format (e.g. commit:abc123)")
	}
	tt, err := ParseTargetType(typeStr)
	if err != nil {
		return Target{}, err
	}
	if tt == TargetProject {
		return Target{Type: TargetProject}, nil
	}
	t := Target{Type: tt, Value: value}
	if err := ValidateTargetValue(t); err != nil {
		return Target{}, err
	}
	return t, nil
}

// ValidateTargetValue enforces that a commit/branch/change-id target value is a
// single, safe tree component. Those values are serialized raw into ONE path
// component (treepath.go's default case: "<type>/<shard>/<value>"), and
// materialize (materialize.go's parseLeafPath default case) reads exactly that
// one component back as the value. A '/' in the value therefore splits it across
// directories and silently corrupts the serialize<->materialize round-trip: e.g.
// branch value "feature/foo" with key "k" round-trips to target "feature", key
// "foo:k" — and can collide in the read-model's (repo_id,target_type,
// target_value,key) primary key, failing the whole COPY transaction. '.', '..',
// and NUL are rejected for the same one-component safety reasons ValidateKey
// enforces on key segments.
//
// Path targets are EXEMPT: their values legitimately contain '/' and are stored
// as nested, escaped segments delimited by the __target__ sentinel
// (encodePathTargetValue), so they round-trip unambiguously. Project targets
// carry no value.
//
// DECISION (H1): reject rather than escape. The upstream `git meta` Rust CLI's
// encoding of a slash-bearing branch/change-id value is unverified here (the
// golden byte-compat test only covers a dash branch), so inventing an escape
// scheme would risk silently diverging from the reference format with no test to
// catch it. Rejecting loudly is strictly safer than the current silent
// corruption and keeps byte-compat intact for the values that do round-trip.
//
// It also enforces MinTargetValueLen, the minimum ParseTarget has always
// documented. That check used to live ONLY in ParseTarget, so a Target built any
// other way — most importantly the one materialize.go reconstructs from a tree
// leaf path, which is bytes another member wrote — skipped it entirely and reached
// TreeBasePath, whose commit-target sharding slices Value[:2] and PANICS on a
// shorter value. Serialize is the only gate between a materialized State and the
// tree, so the check belongs here, where every write path passes through it.
func ValidateTargetValue(t Target) error {
	if t.Type == TargetProject {
		return nil
	}
	if t.Type == TargetPath {
		return ValidatePathTargetValue(t.Value)
	}
	v := t.Value
	switch {
	case v == "":
		return fmt.Errorf("%s target value cannot be empty", t.Type)
	case len(v) < MinTargetValueLen:
		return fmt.Errorf("%s target value %q must be at least %d characters", t.Type, v, MinTargetValueLen)
	case strings.Contains(v, "/"):
		return fmt.Errorf("%s target value %q must not contain '/'", t.Type, v)
	case v == "." || v == "..":
		return fmt.Errorf("%s target value %q is not allowed", t.Type, v)
	case strings.Contains(v, "\x00"):
		return fmt.Errorf("%s target value %q must not contain null byte", t.Type, v)
	}
	return nil
}

// ValidatePathTargetValue checks a path target's value, which is the ONE target
// value that legitimately contains '/': it is stored as nested, escaped segments
// under the __target__ sentinel, so every segment must itself be a legal tree
// component.
//
// Path targets used to be exempt from validation entirely, which left every
// segment rule unenforced: a value with an empty segment ("/", "a//b", a leading
// or trailing slash) encodes to a tree path with an EMPTY component
// ("path///__target__/k/__value"). BuildTree then refuses that path — and it
// refuses the whole Serialize call, not just the offending record, so a single
// such record makes the entire store unwritable for every writer that
// materializes it. '.' and '..' segments are rejected for the same one-component
// safety reasons ValidateKey enforces on key segments, and NUL is never a legal
// tree component.
//
// The entire-api copy of this engine already carries a check of this name; this
// vendored copy had lost it (see the divergence note at the top of the file).
func ValidatePathTargetValue(value string) error {
	if value == "" {
		return fmt.Errorf("%s target value cannot be empty", TargetPath)
	}
	if strings.Contains(value, "\x00") {
		return fmt.Errorf("%s target value %q must not contain null byte", TargetPath, value)
	}
	for _, seg := range strings.Split(value, "/") {
		switch {
		case seg == "":
			return fmt.Errorf("%s target value %q has an empty segment", TargetPath, value)
		case seg == "." || seg == "..":
			return fmt.Errorf("%s target value %q has a %q segment, which is not allowed", TargetPath, value, seg)
		}
	}
	return nil
}

// String renders a target back to its "type:value" form.
func (t Target) String() string {
	if t.Type == TargetProject {
		return string(TargetProject)
	}
	return string(t.Type) + ":" + t.Value
}

// ListEntry is one timestamped entry in a list value. The timestamp drives
// deterministic ordering and the serialized entry name.
type ListEntry struct {
	Value     string `json:"value"`
	Timestamp int64  `json:"timestamp"`
}

// KeyToPathSegments splits a ":"-separated key into path segments.
func KeyToPathSegments(key string) []string {
	return strings.Split(key, ":")
}

// ValidateKey checks that a key can be serialized into the tree layout. Rules
// mirror the reference implementation's validate_key.
func ValidateKey(key string) error {
	if key == "" {
		return errors.New("key cannot be empty")
	}
	for _, seg := range strings.Split(key, ":") {
		if err := validateKeySegment(seg); err != nil {
			return err
		}
	}
	return nil
}

func validateKeySegment(seg string) error {
	switch {
	case seg == "":
		return errors.New("key segments cannot be empty")
	case seg == "." || seg == "..":
		return fmt.Errorf("key segment %q is not allowed", seg)
	case strings.Contains(seg, "/"):
		return fmt.Errorf("key segment %q must not contain '/'", seg)
	case strings.Contains(seg, "\x00"):
		return fmt.Errorf("key segment %q must not contain null byte", seg)
	case strings.HasPrefix(seg, "__"):
		return fmt.Errorf("key segment %q is reserved", seg)
	}
	return nil
}

// SetMemberID returns the deterministic ID of a set member: the Git blob object
// ID of the member value (SHA-1 of "blob <len>\0<value>"). This is used both as
// the tree entry name and is identical to the blob's own OID.
func SetMemberID(value string) string {
	return gitBlobOID(value)
}

// MakeEntryName builds a list entry name "<timestamp>-<sha1(value)[:5]>".
func MakeEntryName(timestamp int64, value string) string {
	sum := sha1.Sum([]byte(value)) //nolint:gosec // matches wire format
	return fmt.Sprintf("%d-%s", timestamp, hex.EncodeToString(sum[:])[:5])
}

// ParseTimestampFromEntryName extracts the leading timestamp from a list entry
// name of the form "<timestamp>-<hash>". Returns ok=false if absent/invalid.
func ParseTimestampFromEntryName(name string) (int64, bool) {
	pre, _, ok := strings.Cut(name, "-")
	if !ok {
		return 0, false
	}
	ts, err := strconv.ParseInt(pre, 10, 64)
	if err != nil {
		return 0, false
	}
	return ts, true
}

// SortListEntries orders entries by their serialized entry name (timestamp then
// content-hash tie-breaker), the deterministic list order from the spec. Entry
// names are computed once (decorate-sort) rather than re-hashed per comparison.
func SortListEntries(entries []ListEntry) {
	type decorated struct {
		name  string
		entry ListEntry
	}
	dec := make([]decorated, len(entries))
	for i, e := range entries {
		dec[i] = decorated{name: MakeEntryName(e.Timestamp, e.Value), entry: e}
	}
	sort.Slice(dec, func(i, j int) bool { return dec[i].name < dec[j].name })
	for i := range dec {
		entries[i] = dec[i].entry
	}
}

// gitBlobOID computes the Git SHA-1 object ID for blob content.
func gitBlobOID(content string) string {
	h := sha1.New() //nolint:gosec // git object IDs are SHA-1
	fmt.Fprintf(h, "blob %d\x00", len(content))
	_, _ = h.Write([]byte(content))
	return hex.EncodeToString(h.Sum(nil))
}

// sha1Hex returns the hex SHA-1 of s (used for target value sharding).
func sha1Hex(s string) string {
	sum := sha1.Sum([]byte(s)) //nolint:gosec // matches wire format
	return hex.EncodeToString(sum[:])
}
