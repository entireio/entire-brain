package factmerge

import (
	"errors"
	"fmt"
	"strings"
)

// The ORIGIN set: which kind of producer wrote this record. Origin was a free
// string until RC.M1, with three spellings living in internal/cli and no
// constant here, which meant the wire contract shared with brainstore had no
// agreed vocabulary for it at all.
//
// Measured over the real store on 2026-11-01 (35,410 records across 33
// branches): only "distilled" (34,403) and "authored" (1,007) have ever been
// written. "imported" is defined in internal/cli and has produced zero
// records; it is retained here because the import path still stamps it and a
// validator that rejected it would reject that path's first success.
//
// OriginReview is new and is the reason this set had to exist. A review
// finding written back as a durable record is neither distilled from a
// transcript nor authored by a person: it is derived by a detector from the
// current tree plus an existing record. Without its own origin it is
// indistinguishable from something the user typed, so no consumer can weight
// it differently, re-derive it, or retire it when the review's premise moves.
const (
	OriginDistilled = "distilled"
	OriginAuthored  = "authored"
	OriginImported  = "imported"
	OriginReview    = "review"
)

var validOrigins = map[string]struct{}{
	OriginDistilled: {},
	OriginAuthored:  {},
	OriginImported:  {},
	OriginReview:    {},
}

// ValidOrigin reports whether origin (case/space-insensitively) is one of the
// closed ORIGIN set.
func ValidOrigin(origin string) bool {
	_, ok := validOrigins[strings.ToLower(strings.TrimSpace(origin))]
	return ok
}

// The RELATION set: what one record asserts about another.
//
// Before RC.M1 the only record-to-record edge was RelatedIDs, an untyped bag
// of ids. Every writer of it in this package means exactly one thing —
// "these two facts are in unresolved tension, a human should look" (merge.go
// cross-links a declined merge/supersede, admin.go the same for proposals) —
// but it is read as generic relatedness by the dashboard, the brief compactor
// and viz, and as "verify against these" by the retrieval guard. A provenance
// graph over that bag cannot tell a contradiction from a derivation, which is
// precisely the distinction RC.M3 has to render.
//
// RelationSupersedes is deliberately absent: supersession already has its own
// typed field (Record.SupersededBy) plus a status transition, and duplicating
// it as an untyped edge would create two sources of truth for the one
// relationship the merge engine actually acts on.
const (
	// RelationContradicts marks two records whose claims disagree. This is
	// what every pre-RC.M1 RelatedIDs cross-link meant.
	RelationContradicts = "contradicts"
	// RelationDerivedFrom points at the record this one was derived FROM:
	// a review finding points at the memory whose expectation it checked.
	// Provenance anchors address the transcript stream; this addresses the
	// record stream, which anchors cannot express.
	RelationDerivedFrom = "derived-from"
	// RelationSupports points at a record this one is corroborating
	// evidence for, without either superseding or contradicting it.
	RelationSupports = "supports"
)

var validRelationTypes = map[string]struct{}{
	RelationContradicts: {},
	RelationDerivedFrom: {},
	RelationSupports:    {},
}

// ValidRelationType reports whether typ (case/space-insensitively) is one of
// the closed RELATION set.
func ValidRelationType(typ string) bool {
	_, ok := validRelationTypes[strings.ToLower(strings.TrimSpace(typ))]
	return ok
}

// Relation is one typed edge from the record that carries it to another
// record. Like RelatedIDs and Author it is OUTSIDE the content id: two
// records differing only in their relations are the same fact, so adding an
// edge never re-keys a stored record.
type Relation struct {
	TargetID string `json:"target_id"`
	Type     string `json:"type"`
}

// AppendRelation adds target/typ to relations unless that exact edge is
// already present, returning the possibly-grown slice. Two different types
// may both point at the same target (a record can contradict one claim while
// being derived from it), so uniqueness is on the pair, not the target.
func AppendRelation(relations []Relation, target, typ string) []Relation {
	target = strings.TrimSpace(target)
	typ = strings.ToLower(strings.TrimSpace(typ))
	if target == "" || typ == "" {
		return relations
	}
	for _, existing := range relations {
		if existing.TargetID == target && existing.Type == typ {
			return relations
		}
	}
	return append(relations, Relation{TargetID: target, Type: typ})
}

// RelationTargets returns the ids this record points at with the given
// relation type, in the order they were recorded.
func RelationTargets(r Record, typ string) []string {
	typ = strings.ToLower(strings.TrimSpace(typ))
	var out []string
	for _, rel := range r.Relations {
		if rel.Type == typ {
			out = append(out, rel.TargetID)
		}
	}
	return out
}

// AddConflictLink records that r is in unresolved tension with otherID, in
// BOTH representations: the legacy RelatedIDs entry the sync wire and the
// hosted store read, and the typed RelationContradicts edge the provenance
// graph reads.
//
// It exists as one function because the two writes are one decision. Five
// separate defects in this repository have lived in the ORDER or the PAIRING
// of two calls rather than in either call, and a mirror updated at four call
// sites is that shape exactly: a site that appended the id and forgot the
// edge would leave a conflict visible to review and invisible to the graph,
// with every test still green.
//
// A supersede uses this same edge. The merge engine's own vocabulary calls
// supersede "contradicts/replaces an older fact", and the two cases stay
// distinguishable without a second relation type because the target carries
// Status and SupersededBy: a contradicts edge whose target is superseded is a
// replacement, and one whose target is still active is an open disagreement.
// A RelationSupersedes would duplicate SupersededBy and create a second
// source of truth for the one edge the engine acts on.
func AddConflictLink(r Record, otherID string) Record {
	otherID = strings.TrimSpace(otherID)
	if otherID == "" || otherID == r.ID {
		return r
	}
	r.RelatedIDs = appendUniqueString(r.RelatedIDs, otherID)
	r.Relations = AppendRelation(r.Relations, otherID, RelationContradicts)
	return r
}

// ErrInvalidRecord is the sentinel every Validate failure wraps, so callers
// can distinguish a structurally bad record from an I/O or identity failure.
var ErrInvalidRecord = errors.New("invalid fact record")

// Validate checks the structural invariants of a record: the closed
// vocabularies (kind, origin, status, relation type), the documented
// provenance floor, the path grammar and count, and the self-reference and
// lifecycle rules.
//
// It reports EVERY violation joined, not the first, because the caller is
// usually repairing a record by hand and one error at a time is three more
// round trips.
//
// What it does NOT check: content identity. VerifyIdentity owns the
// text+paths hash, and the two are deliberately separable — a record can be
// structurally sound and forged, or malformed and honestly hashed, and the
// error messages for those two problems have nothing in common.
//
// Every invariant below was measured against the whole store before being
// enforced (35,410 records, 33 branches, 2026-11-01): zero records violate
// any of them. This matters because a validator that rejects data the product
// already wrote is not a validator, it is an outage. The one invariant I did
// NOT add for that reason: "a retracted record names what retracted it",
// because nothing records that today.
func (r Record) Validate() error {
	var problems []error

	if strings.TrimSpace(r.ID) == "" {
		problems = append(problems, errors.New("id is empty"))
	}
	if strings.TrimSpace(r.Text) == "" {
		problems = append(problems, errors.New("text is empty"))
	}

	switch {
	case len(r.Paths) == 0:
		problems = append(problems, errors.New("no taxonomy path: a record needs at least one"))
	case len(r.Paths) > MaxPaths:
		problems = append(problems, fmt.Errorf("%d taxonomy paths exceeds the maximum of %d", len(r.Paths), MaxPaths))
	}
	for _, path := range r.Paths {
		if !ValidPath(path) {
			problems = append(problems, fmt.Errorf("taxonomy path %q is not category.subcategory.type (three dot-separated segments; lowercase letters, digits and underscores only, and hyphens are not legal)", path))
		}
	}

	// Kind is json-omitempty for wire compatibility, but all 35,410 stored
	// records carry one and `recall --kind` is a primary retrieval filter, so
	// a kindless record is unreachable through it rather than merely untidy.
	if strings.TrimSpace(r.Kind) == "" {
		problems = append(problems, errors.New("kind is empty: it is the retrieval filter, so a kindless record is unreachable by --kind"))
	} else if !ValidFactKind(r.Kind) {
		problems = append(problems, fmt.Errorf("kind %q is outside the closed set (%s)", r.Kind, strings.Join(FactKinds(), ", ")))
	}

	if strings.TrimSpace(r.Origin) == "" {
		problems = append(problems, errors.New("origin is empty"))
	} else if !ValidOrigin(r.Origin) {
		problems = append(problems, fmt.Errorf("origin %q is outside the closed set (%s)", r.Origin, strings.Join(Origins(), ", ")))
	}

	if strings.TrimSpace(r.Status) == "" {
		problems = append(problems, errors.New("status is empty"))
	} else if !validStatus(r.Status) {
		problems = append(problems, fmt.Errorf("status %q is outside the closed set (%s)", r.Status, strings.Join(Statuses(), ", ")))
	}

	// The struct comment has said ">=1" since the type was written and
	// nothing enforced it. A record with no anchor cannot be verified, cannot
	// be traced to the reasoning that produced it, and is exactly what
	// reasoning capture is supposed to stop producing.
	if len(r.Provenance) == 0 {
		problems = append(problems, errors.New("no provenance anchor: a durable record must cite at least one source"))
	}

	for _, rel := range r.Relations {
		if strings.TrimSpace(rel.TargetID) == "" {
			problems = append(problems, fmt.Errorf("relation of type %q has an empty target", rel.Type))
			continue
		}
		if !ValidRelationType(rel.Type) {
			problems = append(problems, fmt.Errorf("relation to %s has type %q, outside the closed set (%s)", rel.TargetID, rel.Type, strings.Join(RelationTypes(), ", ")))
		}
		if rel.TargetID == r.ID {
			problems = append(problems, fmt.Errorf("relation of type %q points at the record itself", rel.Type))
		}
	}

	for _, related := range r.RelatedIDs {
		if related == r.ID {
			problems = append(problems, errors.New("related_ids contains the record's own id"))
		}
	}

	if r.SupersededBy != "" {
		if r.SupersededBy == r.ID {
			problems = append(problems, errors.New("superseded_by points at the record itself"))
		}
		// A record that names its replacement while still claiming to be
		// active is served by recall as current while a newer record claims
		// to have replaced it: both answers reach the agent and one is wrong.
		if r.Status == StatusActive {
			problems = append(problems, fmt.Errorf("status is %q but superseded_by names %s: an active record cannot already be replaced", StatusActive, r.SupersededBy))
		}
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%w %s: %w", ErrInvalidRecord, r.ID, errors.Join(problems...))
}

// ValidateRecords validates every record, returning the joined failures. It
// does not stop at the first: a store repair wants the whole list.
func ValidateRecords(records []Record) error {
	var problems []error
	for _, record := range records {
		if err := record.Validate(); err != nil {
			problems = append(problems, err)
		}
	}
	return errors.Join(problems...)
}

var validStatuses = map[string]struct{}{
	StatusActive:     {},
	StatusCandidate:  {},
	StatusSuperseded: {},
	StatusRetracted:  {},
}

func validStatus(status string) bool {
	_, ok := validStatuses[strings.TrimSpace(status)]
	return ok
}

// FactKinds, Origins, RelationTypes and Statuses return the closed sets in a
// stable order, so an error message, a --help string and a test all read the
// same vocabulary from one place. The kind set shipped with six members and
// one of them (closed-negative) was missing from all three places that told a
// user which kinds existed; a single source is the fix for that class.
func FactKinds() []string {
	return []string{KindDecision, KindInvariant, KindGotcha, KindPreference, KindConvention, KindClosedNegative}
}

func Origins() []string {
	return []string{OriginDistilled, OriginAuthored, OriginImported, OriginReview}
}

func RelationTypes() []string {
	return []string{RelationContradicts, RelationDerivedFrom, RelationSupports}
}

func Statuses() []string {
	return []string{StatusActive, StatusCandidate, StatusSuperseded, StatusRetracted}
}
