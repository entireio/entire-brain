package factmerge

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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

// ErrIdentityMismatch reports a record whose id is not the content-derived id
// for its own text and paths. Match it with errors.Is.
var ErrIdentityMismatch = errors.New("factmerge: record id does not match its content")

// VerifyIdentity re-derives a record's content id and checks the record carries
// it. It is the check that makes "same id" mean "same statement" rather than
// merely "same label", and is applied at the shared fact-set read and publication boundaries. It
// does not authenticate lifecycle fields or provenance; see
// docs/shared-fact-identity.md for the scope and recovery policy.
//
// Nothing about a record is authenticated by its transport: the shared fact-set
// head is written by every member with push access. Without this check a record
// can carry SOMEONE ELSE'S id with attacker-chosen text, and Promote — which
// treats an id match as "this fact is already present, just union the
// provenance" — then discards the victim's real statement and keeps the forged
// one, with the victim's own anchors unioned onto it. The forged text is what
// subsequently feeds every member's agent prompt, cited as the victim's work.
//
// What the check does and does not cover, precisely:
//
//   - Text is covered up to NormalizeText, which lowercases and collapses
//     whitespace runs. Two statements that differ only in case or spacing share
//     an id by construction, so this stops a rewritten STATEMENT, not a
//     re-cased one. A fact whose meaning turns on case (an identifier, a
//     constant) is therefore still substitutable; closing that would require
//     changing RecordID and re-keying every stored fact.
//   - Paths are covered, and are additionally required to be stored in their
//     normalized form. Hashing NormalizePaths(r.Paths) alone would let a record
//     verify while carrying extra or differently-spelled paths — the ones the
//     listing, recall and same-path conflict detection actually read — because
//     normalization drops and truncates them before hashing.
//   - Every field OUTSIDE the id is untouched by this check: Status,
//     SupersededBy, Kind, Locus, Confidence, RelatedIDs, Branch and Provenance.
//     A peer can still republish a byte-identical statement marked superseded,
//     or attach an anchor naming a session it never saw. Those need their own
//     authentication (a signed record), which this milestone does not have.
func VerifyIdentity(r Record) error {
	normalized := NormalizePaths(r.Paths)
	if !equalPathSets(r.Paths, normalized) {
		return fmt.Errorf("%w: record %s stores paths %v, which normalize to %v", ErrIdentityMismatch, r.ID, r.Paths, normalized)
	}
	want := RecordID(r.Text, normalized)
	if r.ID != want {
		return fmt.Errorf("%w: record claims %s but its content hashes to %s", ErrIdentityMismatch, r.ID, want)
	}
	return nil
}

func equalPathSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// VerifyIdentities applies VerifyIdentity to every record, failing on the first
// mismatch. It fails closed: a fact set containing one forged record is not
// partially usable, because the merge that would consume it cannot tell which
// of the remaining records the forger also authored.
func VerifyIdentities(records []Record) error {
	for _, r := range records {
		if err := VerifyIdentity(r); err != nil {
			return err
		}
	}
	return nil
}
