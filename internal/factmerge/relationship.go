package factmerge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const (
	// RelationshipKindPossibleSameSubject is deliberately advisory. It is not
	// an Action, is never consumed by ApplyActions, and cannot change a fact's
	// state.
	RelationshipKindPossibleSameSubject = "possible_same_subject"
	// RelationshipMaxOwners bounds compact provenance on one local advisory
	// observation. Runtime discovery skips the complete subject block when a
	// pair exceeds it; persisted state must never truncate owners silently.
	RelationshipMaxOwners = 128
)

// RelationshipSubject is the small, evidence-bearing block that made two
// different facts eligible for a neutral relationship. It intentionally has no
// fact text, anchor, confidence, or decision fields.
type RelationshipSubject struct {
	Kind        string `json:"kind"`
	TopLevel    string `json:"top_level"`
	StrongLocus string `json:"strong_locus"`
}

// RelationshipOwner identifies the successful candidate application that
// supplied evidence for an advisory relationship. It permits deterministic
// replacement/removal when a candidate is re-run or pruned.
type RelationshipOwner struct {
	CandidateID     string `json:"candidate_id"`
	SourceSessionID string `json:"source_session_id"`
}

// RelationshipProposal is a neutral, local-only observation. It is purposefully
// distinct from Proposal: neither facts review nor factsync can execute it.
type RelationshipProposal struct {
	ID      string              `json:"id"`
	Branch  string              `json:"branch"`
	Kind    string              `json:"kind"`
	FactIDs []string            `json:"fact_ids"`
	Subject RelationshipSubject `json:"subject"`
	Owners  []RelationshipOwner `json:"owners"`
}

// RelationshipProposalID returns the content-derived ID for one neutral
// relationship. Owners are deliberately excluded: they are evidence for an
// existing relationship, not its identity.
func RelationshipProposalID(branch string, factIDs []string, subject RelationshipSubject) (string, error) {
	if err := validateRelationshipSubject(subject); err != nil {
		return "", err
	}
	ids := append([]string(nil), factIDs...)
	if err := validateRelationshipFactIDs(ids); err != nil {
		return "", err
	}
	if err := validateRelationshipField("branch", branch); err != nil {
		return "", err
	}
	canonical, err := json.Marshal(struct {
		Branch  string              `json:"branch"`
		Kind    string              `json:"kind"`
		FactIDs []string            `json:"fact_ids"`
		Subject RelationshipSubject `json:"subject"`
	}{branch, RelationshipKindPossibleSameSubject, ids, subject})
	if err != nil {
		return "", fmt.Errorf("marshal relationship identity: %w", err)
	}
	sum := sha256.Sum256(append([]byte("fact-relationship-v1\x00"), canonical...))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// BuildPossibleSameSubject constructs an advisory relationship only if the
// records pass the deliberately narrow subject block. The input records are
// never modified.
func BuildPossibleSameSubject(left, right Record, owners []RelationshipOwner) (RelationshipProposal, bool, error) {
	if left.Status != StatusActive || right.Status != StatusActive || left.ID == right.ID || left.Branch == "" || left.Branch != right.Branch {
		return RelationshipProposal{}, false, nil
	}
	leftKind := strings.ToLower(strings.TrimSpace(left.Kind))
	rightKind := strings.ToLower(strings.TrimSpace(right.Kind))
	if !ValidFactKind(leftKind) || leftKind != rightKind {
		return RelationshipProposal{}, false, nil
	}
	top, ok := sharedTopLevel(left.Paths, right.Paths)
	if !ok {
		return RelationshipProposal{}, false, nil
	}
	locus, ok := sharedStrongLocus(left, right)
	if !ok {
		return RelationshipProposal{}, false, nil
	}
	subject := RelationshipSubject{Kind: leftKind, TopLevel: top, StrongLocus: locus}
	ids := []string{left.ID, right.ID}
	sort.Strings(ids)
	id, err := RelationshipProposalID(left.Branch, ids, subject)
	if err != nil {
		return RelationshipProposal{}, false, err
	}
	proposal := RelationshipProposal{ID: id, Branch: left.Branch, Kind: RelationshipKindPossibleSameSubject, FactIDs: ids, Subject: subject, Owners: UnionRelationshipOwners(owners)}
	if err := ValidateRelationshipProposal(proposal); err != nil {
		return RelationshipProposal{}, false, err
	}
	return proposal, true, nil
}

// ValidateRelationshipProposal rejects malformed or non-canonical advisory
// state before it enters a local store.
func ValidateRelationshipProposal(proposal RelationshipProposal) error {
	if proposal.Kind != RelationshipKindPossibleSameSubject {
		return fmt.Errorf("relationship kind is invalid")
	}
	if err := validateRelationshipField("branch", proposal.Branch); err != nil {
		return err
	}
	if err := validateRelationshipFactIDs(proposal.FactIDs); err != nil {
		return err
	}
	if err := validateRelationshipSubject(proposal.Subject); err != nil {
		return err
	}
	if len(proposal.Owners) == 0 {
		return fmt.Errorf("relationship has no owners")
	}
	if len(proposal.Owners) > RelationshipMaxOwners {
		return fmt.Errorf("relationship has too many owners")
	}
	for i, owner := range proposal.Owners {
		if err := validateRelationshipOwner(owner); err != nil {
			return err
		}
		if i > 0 && relationshipOwnerKey(proposal.Owners[i-1]) >= relationshipOwnerKey(owner) {
			return fmt.Errorf("relationship owners are not canonical")
		}
	}
	expected, err := RelationshipProposalID(proposal.Branch, proposal.FactIDs, proposal.Subject)
	if err != nil || proposal.ID != expected {
		return fmt.Errorf("relationship ID does not match identity")
	}
	return nil
}

// UnionRelationshipOwners returns canonical, deduplicated ownership evidence.
func UnionRelationshipOwners(groups ...[]RelationshipOwner) []RelationshipOwner {
	seen := make(map[string]RelationshipOwner)
	for _, group := range groups {
		for _, owner := range group {
			if validateRelationshipOwner(owner) != nil {
				continue
			}
			seen[relationshipOwnerKey(owner)] = owner
		}
	}
	out := make([]RelationshipOwner, 0, len(seen))
	for _, owner := range seen {
		out = append(out, owner)
	}
	sort.Slice(out, func(i, j int) bool { return relationshipOwnerKey(out[i]) < relationshipOwnerKey(out[j]) })
	return out
}

// RemoveRelationshipOwner returns a canonical copy without one candidate
// ownership slot. The boolean reports whether any evidence was removed.
func RemoveRelationshipOwner(owners []RelationshipOwner, owner RelationshipOwner) ([]RelationshipOwner, bool) {
	out := make([]RelationshipOwner, 0, len(owners))
	removed := false
	for _, existing := range owners {
		if existing == owner {
			removed = true
			continue
		}
		out = append(out, existing)
	}
	return UnionRelationshipOwners(out), removed
}

func sharedTopLevel(left, right []string) (string, bool) {
	leftSet := make(map[string]struct{}, len(left))
	for _, path := range left {
		if top := relationshipTopLevel(path); top != "" {
			leftSet[top] = struct{}{}
		}
	}
	best := ""
	for _, path := range right {
		top := relationshipTopLevel(path)
		if _, ok := leftSet[top]; ok && (best == "" || top < best) {
			best = top
		}
	}
	return best, best != ""
}

func relationshipTopLevel(path string) string {
	path = strings.TrimSpace(path)
	if !ValidPath(path) {
		return ""
	}
	if i := strings.IndexByte(path, '.'); i >= 0 {
		path = path[:i]
	}
	if validateRelationshipField("top-level taxonomy", path) != nil {
		return ""
	}
	return path
}

func sharedStrongLocus(left, right Record) (string, bool) {
	leftSet := make(map[string]struct{}, len(left.Locus))
	for _, locus := range left.Locus {
		if normalized, ok := StrongRelationshipLocusForRecord(left, locus); ok {
			leftSet[normalized] = struct{}{}
		}
	}
	best := ""
	for _, locus := range right.Locus {
		normalized, ok := StrongRelationshipLocusForRecord(right, locus)
		if _, shared := leftSet[normalized]; ok && shared && (best == "" || normalized < best) {
			best = normalized
		}
	}
	return best, best != ""
}

var (
	strongPathLocusPattern       = regexp.MustCompile(`^[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)+$`)
	strongQualifiedLocusPattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(?:(?:::|#)[A-Za-z_][A-Za-z0-9_]*|\.[A-Za-z_][A-Za-z0-9_]*)(?:\([^\r\n]*\))?$`)
	strongUnderscoreLocusPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*_[A-Za-z0-9_]+$`)
	strongBareLocusPattern       = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)
	strongQuotedIDPattern        = regexp.MustCompile("^`[A-Za-z_][A-Za-z0-9_]*`$")
	strongRecordTokenPattern     = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*(?:[./][A-Za-z0-9_]+)+|[A-Za-z_]*[a-z][A-Za-z0-9]*[A-Z][A-Za-z0-9_]*|[A-Za-z][A-Za-z0-9]*_[A-Za-z0-9_]+`)
	strongRecordBacktickPattern  = regexp.MustCompile("`([^`]+)`")
)

var genericRelationshipLocus = map[string]struct{}{
	"architecture": {}, "cache": {}, "config": {}, "constraint": {}, "convention": {}, "data": {}, "decision": {}, "entire": {}, "flow": {}, "handler": {}, "migration": {}, "network": {}, "preference": {}, "repo": {}, "repository": {}, "taxonomy": {},
}

// StrongRelationshipLocus recognizes code-address-like and canonicalized
// identifiers. Bare normalized identifiers are retained here because a
// persisted proposal no longer has its source Record; proposal validation can
// therefore enforce canonical shape, while proposal construction must use
// StrongRelationshipLocusForRecord to require source-text evidence.
func StrongRelationshipLocus(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "\r\n\x00") {
		return "", false
	}
	quoted := strongQuotedIDPattern.MatchString(value)
	if quoted {
		value = strings.Trim(value, "`")
	}
	if !quoted && !strongPathLocusPattern.MatchString(value) && !strongQualifiedLocusPattern.MatchString(value) && !strongUnderscoreLocusPattern.MatchString(value) && !strongBareLocusPattern.MatchString(value) {
		return "", false
	}
	normalized := strings.ToLower(value)
	if len(normalized) > 512 {
		return "", false
	}
	if _, generic := genericRelationshipLocus[normalized]; generic {
		return "", false
	}
	if i := strings.IndexAny(normalized, ".#:/"); i > 0 {
		if _, generic := genericRelationshipLocus[normalized[:i]]; generic {
			return "", false
		}
	}
	return normalized, true
}

// StrongRelationshipLocusForRecord admits a normalized bare identifier only
// when the fact text itself proves why the locus extractor considered it code:
// an exact backtick token or a path/qualified/CamelCase/snake_case token lowers
// to the stored locus. Persisted Locus alone is not trusted as that proof.
func StrongRelationshipLocusForRecord(record Record, value string) (string, bool) {
	normalized, ok := StrongRelationshipLocus(value)
	if !ok {
		return "", false
	}
	// Paths, qualified identifiers, snake_case, and quoted identifiers carry
	// their own code-like shape. A normalized bare locus has lost that shape
	// during factLocus extraction, so it must be reproven by the source text.
	trimmed := strings.TrimSpace(value)
	quoted := strongQuotedIDPattern.MatchString(trimmed)
	if quoted || strongPathLocusPattern.MatchString(trimmed) || strongQualifiedLocusPattern.MatchString(trimmed) || strongUnderscoreLocusPattern.MatchString(trimmed) {
		return normalized, true
	}
	want := normalized
	for _, span := range strongRecordBacktickPattern.FindAllStringSubmatch(record.Text, -1) {
		for _, token := range strings.Fields(span[1]) {
			if strings.ToLower(strings.Trim(token, "`.,;:()[]{}'\"")) == want {
				return want, true
			}
		}
	}
	for _, token := range strongRecordTokenPattern.FindAllString(record.Text, -1) {
		if strings.ToLower(strings.Trim(token, "`.,;:()[]{}'\"")) == want {
			return want, true
		}
	}
	return "", false
}

func validateRelationshipSubject(subject RelationshipSubject) error {
	for name, value := range map[string]string{"subject kind": subject.Kind, "subject top-level": subject.TopLevel, "subject strong locus": subject.StrongLocus} {
		if err := validateRelationshipField(name, value); err != nil {
			return err
		}
	}
	if !ValidFactKind(subject.Kind) || subject.Kind != strings.ToLower(strings.TrimSpace(subject.Kind)) {
		return fmt.Errorf("subject kind is invalid")
	}
	if normalized, ok := StrongRelationshipLocus(subject.StrongLocus); !ok || normalized != subject.StrongLocus {
		return fmt.Errorf("subject strong locus is invalid")
	}
	return nil
}

func validateRelationshipFactIDs(ids []string) error {
	if len(ids) != 2 || ids[0] >= ids[1] {
		return fmt.Errorf("relationship fact IDs are not canonical distinct pair")
	}
	for _, id := range ids {
		if err := validateRelationshipField("fact ID", id); err != nil {
			return err
		}
	}
	return nil
}

func validateRelationshipOwner(owner RelationshipOwner) error {
	if err := validateRelationshipField("owner candidate ID", owner.CandidateID); err != nil {
		return err
	}
	return validateRelationshipField("owner source session ID", owner.SourceSessionID)
}

func relationshipOwnerKey(owner RelationshipOwner) string {
	return owner.CandidateID + "\x00" + owner.SourceSessionID
}

func validateRelationshipField(name, value string) error {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 4096 || strings.ContainsAny(value, "\x00\r\n") {
		return fmt.Errorf("%s is invalid", name)
	}
	return nil
}
