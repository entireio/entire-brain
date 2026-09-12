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

package gitmeta

import (
	"fmt"
	"strings"
)

// TreeBasePath builds the tree base path for a target. Schemes:
//
//	commit    -> commit/<first2-of-sha>/<full-sha>
//	path      -> path/<escaped path segments...>/__target__
//	branch    -> branch/<first2-of-sha1(value)>/<value>
//	change-id -> change-id/<first2-of-sha1(value)>/<value>
//	project   -> project
func TreeBasePath(t Target) string {
	switch t.Type {
	case TargetProject:
		return string(TargetProject)
	case TargetCommit:
		// Commit SHAs shard on their own first two hex chars (no rehash).
		// shardPrefix, not Value[:2]: this function returns no error, so a caller
		// that reached it without ValidateTargetValue (which enforces
		// MinTargetValueLen) must get a wrong-but-total path rather than a panic.
		return fmt.Sprintf("%s/%s/%s", t.Type, shardPrefix(t.Value), t.Value)
	case TargetPath:
		return fmt.Sprintf("%s/%s/%s", t.Type, encodePathTargetValue(t.Value), PathTargetSeparator)
	default: // branch, change-id
		return fmt.Sprintf("%s/%s/%s", t.Type, sha1Hex(t.Value)[:2], t.Value)
	}
}

// buildKeyTreePath joins the target base path with the key's colon-separated
// segments turned into nested directories. The target value and key are
// validated first, so a corrupting (e.g. slash-bearing branch) target can never
// reach the tree even if it bypassed ParseTarget.
func buildKeyTreePath(t Target, key string) (string, error) {
	if err := ValidateTargetValue(t); err != nil {
		return "", err
	}
	if err := ValidateKey(key); err != nil {
		return "", err
	}
	return TreeBasePath(t) + "/" + strings.Join(KeyToPathSegments(key), "/"), nil
}

// TreePath returns the full tree path for a string value's terminal blob.
func TreePath(t Target, key string) (string, error) {
	kp, err := buildKeyTreePath(t, key)
	if err != nil {
		return "", err
	}
	return kp + "/" + StringValueBlob, nil
}

// ListDirPath returns the directory holding a list key's entry blobs.
func ListDirPath(t Target, key string) (string, error) {
	kp, err := buildKeyTreePath(t, key)
	if err != nil {
		return "", err
	}
	return kp + "/" + ListValueDir, nil
}

// SetDirPath returns the tree holding a set key's member blobs.
func SetDirPath(t Target, key string) (string, error) {
	kp, err := buildKeyTreePath(t, key)
	if err != nil {
		return "", err
	}
	return kp + "/" + SetValueDir, nil
}

// TombstonePath returns the whole-key tombstone blob path:
// <base>/__tombstones/<key segments>/__deleted.
func TombstonePath(t Target, key string) (string, error) {
	if err := ValidateTargetValue(t); err != nil {
		return "", err
	}
	if err := ValidateKey(key); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/%s/%s/%s",
		TreeBasePath(t), TombstoneRoot, strings.Join(KeyToPathSegments(key), "/"), TombstoneBlob), nil
}

// ListEntryTombstonePath returns the tombstone path for a single list entry:
// <base>/<key>/__list/__tombstones/<entry>.
func ListEntryTombstonePath(t Target, key, entry string) (string, error) {
	kp, err := buildKeyTreePath(t, key)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/%s/%s/%s", kp, ListValueDir, TombstoneRoot, entry), nil
}

// SetMemberTombstonePath returns the tombstone path for a single set member:
// <base>/<key>/__tombstones/<member>.
func SetMemberTombstonePath(t Target, key, member string) (string, error) {
	kp, err := buildKeyTreePath(t, key)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/%s/%s", kp, TombstoneRoot, member), nil
}

// shardPrefix returns the two-character shard directory for a value, or the
// whole value when it is shorter. Values that short never reach a serialized
// tree (ValidateTargetValue rejects them); this only keeps the unvalidated call
// total.
func shardPrefix(value string) string {
	if len(value) < 2 {
		return value
	}
	return value[:2]
}

// encodePathTargetValue escapes reserved path segments for safe tree storage.
// A segment starting with "~" or "__" is prefixed with "~", keeping the
// encoding reversible.
func encodePathTargetValue(value string) string {
	segs := strings.Split(value, "/")
	for i, s := range segs {
		if strings.HasPrefix(s, "~") || strings.HasPrefix(s, "__") {
			segs[i] = "~" + s
		}
	}
	return strings.Join(segs, "/")
}

// decodePathTargetSegments reverses encodePathTargetValue for a slice of
// already-split path-target segments.
func decodePathTargetSegments(segs []string) string {
	out := make([]string, len(segs))
	for i, s := range segs {
		out[i] = strings.TrimPrefix(s, "~")
	}
	return strings.Join(out, "/")
}
