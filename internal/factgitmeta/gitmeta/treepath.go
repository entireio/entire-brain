// Copied verbatim from github.com/entirehq/git-meta-service/internal/gitmeta @
// feat/git-meta-service-integration (f3ec153) — do not edit; de-internalize
// upstream to dedupe (follow-up).

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
		return fmt.Sprintf("%s/%s/%s", t.Type, t.Value[:2], t.Value)
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
