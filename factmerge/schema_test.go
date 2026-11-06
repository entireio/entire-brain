package factmerge

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// validRecord is the shape every case below mutates one field of, so a test
// that fails names exactly one broken invariant.
func validRecord() Record {
	return Record{
		ID:         RecordID("the daemon reads the manifest before the lock", []string{"architecture.daemon.flow"}),
		Paths:      []string{"architecture.daemon.flow"},
		Kind:       KindInvariant,
		Text:       "the daemon reads the manifest before the lock",
		Branch:     "main",
		Origin:     OriginAuthored,
		Status:     StatusActive,
		Provenance: []Anchor{{SessionID: "s1"}},
		CreatedAt:  time.Unix(0, 0).UTC(),
		UpdatedAt:  time.Unix(0, 0).UTC(),
	}
}

func TestValidRecordValidates(t *testing.T) {
	t.Parallel()
	if err := validRecord().Validate(); err != nil {
		t.Fatalf("the baseline record must be valid, else every case below tests the wrong thing: %v", err)
	}
}

// Each case breaks exactly one invariant. The `want` substring is asserted so
// a case cannot pass on some unrelated complaint.
func TestValidateCatchesEachInvariant(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		edit func(*Record)
		want string
	}{
		{"empty id", func(r *Record) { r.ID = "" }, "id is empty"},
		{"empty text", func(r *Record) { r.Text = "" }, "text is empty"},
		{"no path", func(r *Record) { r.Paths = nil }, "at least one"},
		{"too many paths", func(r *Record) {
			r.Paths = []string{"a.b.c", "d.e.f", "g.h.i"}
		}, "exceeds the maximum"},
		{"hyphenated path", func(r *Record) {
			r.Paths = []string{"architecture.agent-hooks.invariant"}
		}, "hyphens are not legal"},
		{"two-level path", func(r *Record) { r.Paths = []string{"architecture.daemon"} }, "three dot-separated"},
		{"empty kind", func(r *Record) { r.Kind = "" }, "kind is empty"},
		{"unknown kind", func(r *Record) { r.Kind = "footgun" }, "outside the closed set"},
		{"empty origin", func(r *Record) { r.Origin = "" }, "origin is empty"},
		{"unknown origin", func(r *Record) { r.Origin = "synthesised" }, "outside the closed set"},
		{"empty status", func(r *Record) { r.Status = "" }, "status is empty"},
		{"unknown status", func(r *Record) { r.Status = "pending" }, "outside the closed set"},
		{"no provenance", func(r *Record) { r.Provenance = nil }, "at least one source"},
		{"relation with empty target", func(r *Record) {
			r.Relations = []Relation{{Type: RelationSupports}}
		}, "empty target"},
		{"relation with unknown type", func(r *Record) {
			r.Relations = []Relation{{TargetID: "fact:aaaa", Type: "inspired-by"}}
		}, "outside the closed set"},
		{"self relation", func(r *Record) {
			r.Relations = []Relation{{TargetID: r.ID, Type: RelationSupports}}
		}, "points at the record itself"},
		{"self related id", func(r *Record) { r.RelatedIDs = []string{r.ID} }, "related_ids contains the record's own id"},
		{"self supersede", func(r *Record) { r.SupersededBy = r.ID }, "superseded_by points at the record itself"},
		{"active yet superseded", func(r *Record) { r.SupersededBy = "fact:bbbb" }, "cannot already be replaced"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			record := validRecord()
			tc.edit(&record)
			err := record.Validate()
			if err == nil {
				t.Fatalf("%s must be rejected", tc.name)
			}
			if !errors.Is(err, ErrInvalidRecord) {
				t.Errorf("error must wrap ErrInvalidRecord so callers can classify it: %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("message must name the problem %q, got: %v", tc.want, err)
			}
		})
	}
}

// A hand-repairing caller gets the whole list or pays a round trip per field.
func TestValidateReportsEveryProblemNotJustTheFirst(t *testing.T) {
	t.Parallel()

	record := validRecord()
	record.Kind = "footgun"
	record.Origin = "synthesised"
	record.Provenance = nil

	err := record.Validate()
	if err == nil {
		t.Fatal("three broken invariants must be rejected")
	}
	for _, want := range []string{"footgun", "synthesised", "at least one source"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message omits %q, so a caller fixing one problem rediscovers the next: %v", want, err)
		}
	}
}

// A superseded record naming its replacement is the normal, valid shape; only
// the ACTIVE-and-replaced combination is contradictory. Without this case the
// lifecycle rule could be written as "SupersededBy is never set" and pass.
func TestSupersededRecordMayNameItsReplacement(t *testing.T) {
	t.Parallel()

	record := validRecord()
	record.Status = StatusSuperseded
	record.SupersededBy = "fact:bbbb"
	if err := record.Validate(); err != nil {
		t.Fatalf("a superseded record pointing at its replacement is the normal shape: %v", err)
	}
}

func TestClosedSetsAgreeWithTheirValidators(t *testing.T) {
	t.Parallel()

	for _, kind := range FactKinds() {
		if !ValidFactKind(kind) {
			t.Errorf("FactKinds advertises %q but ValidFactKind rejects it", kind)
		}
	}
	if len(FactKinds()) != len(validFactKinds) {
		t.Errorf("FactKinds lists %d kinds, the validator knows %d: one of them is lying to the reader", len(FactKinds()), len(validFactKinds))
	}

	for _, origin := range Origins() {
		if !ValidOrigin(origin) {
			t.Errorf("Origins advertises %q but ValidOrigin rejects it", origin)
		}
	}
	if len(Origins()) != len(validOrigins) {
		t.Errorf("Origins lists %d, the validator knows %d", len(Origins()), len(validOrigins))
	}

	for _, typ := range RelationTypes() {
		if !ValidRelationType(typ) {
			t.Errorf("RelationTypes advertises %q but ValidRelationType rejects it", typ)
		}
	}
	if len(RelationTypes()) != len(validRelationTypes) {
		t.Errorf("RelationTypes lists %d, the validator knows %d", len(RelationTypes()), len(validRelationTypes))
	}

	for _, status := range Statuses() {
		if !validStatus(status) {
			t.Errorf("Statuses advertises %q but validStatus rejects it", status)
		}
	}
	if len(Statuses()) != len(validStatuses) {
		t.Errorf("Statuses lists %d, the validator knows %d", len(Statuses()), len(validStatuses))
	}

	// OriginReview is the whole reason the origin set exists: review
	// write-back has to be distinguishable from a hand-authored fact.
	if !ValidOrigin(OriginReview) {
		t.Error("review write-back cannot stamp an origin the validator rejects")
	}
}

func TestOriginAndRelationValidationIgnoreCaseAndSpace(t *testing.T) {
	t.Parallel()

	if !ValidOrigin("  Distilled ") {
		t.Error("origins arrive from JSON and flags; surrounding space must not change the verdict")
	}
	if !ValidRelationType("DERIVED-FROM") {
		t.Error("relation types must compare case-insensitively like kinds do")
	}
	if ValidOrigin("") || ValidRelationType("") {
		t.Error("the empty string is not a member of either set")
	}
}

// The two representations are one decision, so no caller can write one
// without the other.
func TestAddConflictLinkWritesBothRepresentations(t *testing.T) {
	t.Parallel()

	record := AddConflictLink(validRecord(), "fact:bbbb")

	if got := record.RelatedIDs; len(got) != 1 || got[0] != "fact:bbbb" {
		t.Fatalf("legacy RelatedIDs mirror not written: %v", got)
	}
	if got := RelationTargets(record, RelationContradicts); len(got) != 1 || got[0] != "fact:bbbb" {
		t.Fatalf("typed contradicts edge not written: %v", record.Relations)
	}

	// Idempotent: replaying a rebuild must not grow either side.
	again := AddConflictLink(record, "fact:bbbb")
	if len(again.RelatedIDs) != 1 || len(again.Relations) != 1 {
		t.Fatalf("relinking the same pair duplicated an edge: ids=%v relations=%v", again.RelatedIDs, again.Relations)
	}
}

func TestAddConflictLinkRefusesSelfAndEmpty(t *testing.T) {
	t.Parallel()

	record := validRecord()
	if got := AddConflictLink(record, record.ID); len(got.Relations) != 0 || len(got.RelatedIDs) != 0 {
		t.Error("a record cannot contradict itself; Validate rejects that shape, so the writer must not create it")
	}
	if got := AddConflictLink(record, "   "); len(got.Relations) != 0 || len(got.RelatedIDs) != 0 {
		t.Error("an empty target must not become an edge")
	}
}

// Uniqueness is on the (target, type) pair: one record may both contradict a
// claim and be derived from it, and collapsing those loses an edge.
func TestAppendRelationIsUniqueOnThePairNotTheTarget(t *testing.T) {
	t.Parallel()

	var relations []Relation
	relations = AppendRelation(relations, "fact:bbbb", RelationContradicts)
	relations = AppendRelation(relations, "fact:bbbb", RelationDerivedFrom)
	relations = AppendRelation(relations, "fact:bbbb", RelationContradicts)

	if len(relations) != 2 {
		t.Fatalf("want 2 distinct edges to one target, got %d: %v", len(relations), relations)
	}
	if got := RelationTargets(Record{Relations: relations}, RelationDerivedFrom); len(got) != 1 {
		t.Errorf("derived-from edge lost to the contradicts edge on the same target: %v", relations)
	}
}

// The conflict path must produce the typed edge through the real engine, not
// just through the helper: deleting the AddConflictLink call in merge.go has
// to turn a test red.
func TestDeclinedSupersedeCrossLinksWithATypedContradictsEdge(t *testing.T) {
	t.Parallel()

	now := time.Unix(100, 0).UTC()
	target := validRecord()
	candidate := validRecord()
	candidate.Text = "the daemon takes the lock before reading the manifest"
	candidate.ID = RecordID(candidate.Text, candidate.Paths)

	active := ApplyActions([]Record{target}, []Action{{
		Kind:       ActionSupersede,
		TargetID:   target.ID,
		Confidence: 0.1,
		Candidate:  candidate,
	}}, 0.8, now)

	// Both stay active and cross-linked. This used to also queue a proposal;
	// main removed the proposal framework, so the cross-link IS the record of
	// the unsettled tension and the compaction loop settles it later.
	for _, want := range []string{target.ID, candidate.ID} {
		i := IndexOf(active, want)
		if i < 0 {
			t.Fatalf("both facts stay active; %s is missing", want)
		}
		if got := RelationTargets(active[i], RelationContradicts); len(got) != 1 {
			t.Errorf("%s carries no typed contradicts edge: %v", want, active[i].Relations)
		}
	}
}

// A HIGH-confidence supersede cross-links too, and this is the case the
// design leans on: there is no RelationSupersedes, so "replaced" and "still
// disagreeing" are told apart by the TARGET's status behind a contradicts
// edge. Assert both halves, or that claim is only a comment. Found by
// mutation: reverting this call site to the untyped append left every test
// green.
func TestAppliedSupersedeLeavesAContradictsEdgeAtASupersededTarget(t *testing.T) {
	t.Parallel()

	now := time.Unix(100, 0).UTC()
	target := validRecord()
	candidate := validRecord()
	candidate.Text = "the daemon takes the lock before reading the manifest"
	candidate.ID = RecordID(candidate.Text, candidate.Paths)

	active := ApplyActions([]Record{target}, []Action{{
		Kind:       ActionSupersede,
		TargetID:   target.ID,
		Confidence: 0.95,
		Candidate:  candidate,
	}}, 0.8, now)

	ci := IndexOf(active, candidate.ID)
	if ci < 0 {
		t.Fatal("the candidate must be active after an applied supersede")
	}
	if got := RelationTargets(active[ci], RelationContradicts); len(got) != 1 || got[0] != target.ID {
		t.Fatalf("the replacement carries no typed edge back to what it replaced: %v", active[ci].Relations)
	}

	ti := IndexOf(active, target.ID)
	if ti < 0 {
		t.Fatal("a superseded record is retained, never deleted")
	}
	if active[ti].Status != StatusSuperseded || active[ti].SupersededBy != candidate.ID {
		t.Fatalf("the edge is only readable as a replacement if the target says so: status=%q superseded_by=%q",
			active[ti].Status, active[ti].SupersededBy)
	}
}

func TestRelationsSurviveNDJSONRoundTrip(t *testing.T) {
	t.Parallel()

	record := AddConflictLink(validRecord(), "fact:bbbb")
	record.Relations = AppendRelation(record.Relations, "fact:cccc", RelationDerivedFrom)

	var buf strings.Builder
	if err := WriteNDJSON(&buf, []Record{record}); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ParseNDJSON(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 record, got %d", len(got))
	}
	if len(got[0].Relations) != 2 {
		t.Fatalf("relations did not survive the round trip: %v", got[0].Relations)
	}
	if got[0].Relations[1].Type != RelationDerivedFrom || got[0].Relations[1].TargetID != "fact:cccc" {
		t.Errorf("edge order or content changed: %v", got[0].Relations)
	}
}

// Relations are outside the content id, so attaching one must not re-key a
// stored record. If it did, every conflict cross-link would orphan the fact it
// was attached to.
func TestRelationsAreOutsideTheContentIdentity(t *testing.T) {
	t.Parallel()

	record := validRecord()
	linked := AddConflictLink(record, "fact:bbbb")

	if linked.ID != record.ID {
		t.Fatal("adding an edge changed the id")
	}
	if err := VerifyIdentity(linked); err != nil {
		t.Errorf("a record with relations must still verify against its content hash: %v", err)
	}
}

// A build that predates the field must still read a record that carries it.
// ParseNDJSON uses plain json.Unmarshal with no DisallowUnknownFields, which
// is the property this asserts; if a future change tightens that, a record
// written by a newer peer would become a hard parse error on the sync wire.
func TestOlderReadersIgnoreTheRelationsField(t *testing.T) {
	t.Parallel()

	record := AddConflictLink(validRecord(), "fact:bbbb")
	line, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(line), `"relations"`) {
		t.Fatal("the fixture must actually carry the new field")
	}

	// A reader whose Record type has no Relations field at all.
	var old struct {
		ID         string   `json:"id"`
		RelatedIDs []string `json:"related_ids,omitempty"`
	}
	if err := json.Unmarshal(line, &old); err != nil {
		t.Fatalf("a reader without the field must not fail: %v", err)
	}
	if old.ID != record.ID {
		t.Errorf("id lost: %q", old.ID)
	}
	if len(old.RelatedIDs) != 1 {
		t.Error("the RelatedIDs mirror is what an older reader has instead of the typed edge; without it the conflict is invisible there")
	}
}

func TestValidateRecordsReportsEveryBadRecord(t *testing.T) {
	t.Parallel()

	good := validRecord()
	bad1 := validRecord()
	bad1.Kind = "footgun"
	bad2 := validRecord()
	bad2.Origin = "synthesised"

	if err := ValidateRecords([]Record{good}); err != nil {
		t.Fatalf("a valid set must pass: %v", err)
	}
	err := ValidateRecords([]Record{good, bad1, bad2})
	if err == nil {
		t.Fatal("two bad records must be reported")
	}
	for _, want := range []string{"footgun", "synthesised"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ValidateRecords stopped early, omitting %q: %v", want, err)
		}
	}
}
