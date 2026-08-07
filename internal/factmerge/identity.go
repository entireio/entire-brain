package factmerge

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
)

// MaxPaths is the cap on taxonomy paths per fact: a fact may live under at most
// two taxonomy paths.
const MaxPaths = 2

// factPathPattern matches a three-level taxonomy path
// (category.subcategory.type), each segment lowercase letters, digits, and
// underscores, the first segment starting with a letter. e.g.
// preferences.coding.style or architecture.boundaries.rationale.
var factPathPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z0-9_]+){2}$`)

// NormalizeText collapses a fact statement to a stable form for content
// hashing: lowercased, with internal whitespace runs reduced to single spaces.
// Two statements that differ only in casing or spacing share an id.
func NormalizeText(text string) string {
	return strings.Join(strings.Fields(strings.ToLower(text)), " ")
}

// RecordID is sha256(normalize(text) + "\x00" + join(sortedPaths, ",")),
// hex-truncated like the other brain ids. Paths must already be normalized
// (see NormalizePaths) so the same statement under the same paths always
// hashes identically regardless of the order the agent emitted them.
func RecordID(text string, sortedPaths []string) string {
	sum := sha256.Sum256([]byte(NormalizeText(text) + "\x00" + strings.Join(sortedPaths, ",")))
	return "fact:" + hex.EncodeToString(sum[:12])
}

// ValidPath reports whether path is a syntactically valid three-level
// taxonomy path. It does not check the path against the active taxonomy.
func ValidPath(path string) bool {
	return factPathPattern.MatchString(path)
}

// NormalizePaths trims, lowercases, drops syntactically invalid paths,
// deduplicates, sorts, and caps the result at MaxPaths. The returned slice
// is the canonical path set used both for the record id and on disk.
func NormalizePaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	cleaned := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.ToLower(strings.TrimSpace(path))
		if path == "" || !ValidPath(path) {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		cleaned = append(cleaned, path)
	}
	sort.Strings(cleaned)
	if len(cleaned) > MaxPaths {
		cleaned = cleaned[:MaxPaths]
	}
	return cleaned
}
