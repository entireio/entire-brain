package brainreview

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/entireio/entire-brain/factmerge"
)

func changedAnomaly() Anomaly {
	return Anomaly{
		File:       "pkg/review_context.go",
		Line:       412,
		Kind:       "changed",
		Identifier: "scopebaseref",
		Symbol:     "ReviewContext",
		Expected:   `scopeBaseRef+"..HEAD"`,
		Current:    `"master..HEAD"`,
		Confidence: 0.8,
		Reason:     "history asserts the range is built from scopeBaseRef",
		Evidence:   "sessions/main/x.jsonl:1",
	}
}

func TestReviewFindingRecordIsAValidDeliverableRecord(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	record, err := ReviewFindingRecord(changedAnomaly(), "main", "deadbeef", "sess-1", now)
	if err != nil {
		t.Fatalf("a well-formed finding must produce a record: %v", err)
	}

	// Validate is the M1 contract; a record this command writes must satisfy
	// it or the write path is producing data the schema forbids.
	if err := record.Validate(); err != nil {
		t.Fatalf("review write-back produced an invalid record: %v", err)
	}
	if err := factmerge.VerifyIdentity(record); err != nil {
		t.Fatalf("the record's id must hash its own content: %v", err)
	}

	if record.Origin != factmerge.OriginReview {
		t.Errorf("origin = %q, want %q: a detector-derived record must be distinguishable from an authored one",
			record.Origin, factmerge.OriginReview)
	}
	if record.Kind != factmerge.KindGotcha {
		t.Errorf("kind = %q, want gotcha: gotcha and closed-negative are the kinds the post-failure hook delivers", record.Kind)
	}
	if len(record.Paths) != 1 || record.Paths[0] != ReviewFactPath {
		t.Errorf("paths = %v, want only %q: review output must not land beside settled engineering decisions", record.Paths, ReviewFactPath)
	}
	if record.Status != factmerge.StatusActive {
		t.Errorf("status = %q: a candidate with no proposal is unreachable — nothing surfaces or promotes it", record.Status)
	}

	// Locus is what makes it deliverable: the pre-edit hook selects memories
	// by the file about to be edited.
	if len(record.Locus) == 0 || record.Locus[0] != "pkg/review_context.go" {
		t.Errorf("locus = %v, want the file first so the pre-edit hook can select it", record.Locus)
	}
	if !containsString(record.Locus, "ReviewContext") || !containsString(record.Locus, "scopebaseref") {
		t.Errorf("locus = %v, want the symbol and identifier too", record.Locus)
	}

	if record.Confidence != "0.80" {
		t.Errorf("confidence = %q, want %q to match the store's existing format", record.Confidence, "0.80")
	}
}

// The commit and session belong in the anchor, where `verify` can re-check
// them, and must NOT be in the text, where they would re-key the record.
func TestReviewFindingProvenanceCarriesTheCommitAndSession(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	record, err := ReviewFindingRecord(changedAnomaly(), "main", "deadbeef", "sess-1", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(record.Provenance) != 1 {
		t.Fatalf("want exactly one anchor, got %d", len(record.Provenance))
	}
	anchor := record.Provenance[0]
	if anchor.Commit != "deadbeef" || anchor.SessionID != "sess-1" {
		t.Errorf("anchor = %+v, want the commit and session", anchor)
	}
	if strings.Contains(record.Text, "deadbeef") || strings.Contains(record.Text, "sess-1") {
		t.Errorf("the citation must not be in the text, which is hashed into the id: %q", record.Text)
	}
}

// This is the anti-duplication guarantee, and the reason the text may carry no
// clock or commit: two reviews of the same unchanged tree must produce ONE
// record, not one per run. Without it the store grows a duplicate on every
// review and the "supersede rather than append" rule is violated by design.
func TestTheSameFindingAlwaysProducesTheSameRecordID(t *testing.T) {
	t.Parallel()

	first, err := ReviewFindingRecord(changedAnomaly(), "main", "commit-one", "sess-1",
		time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	second, err := ReviewFindingRecord(changedAnomaly(), "main", "commit-two", "sess-2",
		time.Date(2026, 12, 25, 9, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("the same finding produced two ids (%s, %s): every review would append a duplicate", first.ID, second.ID)
	}
	if first.Text != second.Text {
		t.Errorf("the text is not a pure function of the finding:\n  %q\n  %q", first.Text, second.Text)
	}
}

func TestDifferentFindingsProduceDifferentRecords(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	a, err := ReviewFindingRecord(changedAnomaly(), "main", "c", "s", now)
	if err != nil {
		t.Fatal(err)
	}
	other := changedAnomaly()
	other.Line = 999
	b, err := ReviewFindingRecord(other, "main", "c", "s", now)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID {
		t.Fatal("two findings at different lines collapsed into one record")
	}
}

// A deleted-kind finding has no current value. Phrasing it like a changed one
// would state that the identifier now reads "", which is a claim the detector
// never made.
func TestDeletedFindingDoesNotClaimACurrentValue(t *testing.T) {
	t.Parallel()

	deleted := changedAnomaly()
	deleted.Kind = "deleted"
	deleted.Current = ""
	record, err := ReviewFindingRecord(deleted, "main", "c", "s", time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(record.Text, "absent from") {
		t.Errorf("a deleted finding must read as absence: %q", record.Text)
	}
	if strings.Contains(record.Text, `reading ""`) {
		t.Errorf("a deleted finding must not claim an empty current value: %q", record.Text)
	}
}

func TestFindingWithoutAFileOrIdentifierIsRefused(t *testing.T) {
	t.Parallel()

	noFile := changedAnomaly()
	noFile.File = "  "
	if _, err := ReviewFindingRecord(noFile, "main", "c", "s", time.Unix(0, 0).UTC()); err == nil {
		t.Error("a finding with no file cannot be located or anchored, so it must be refused")
	}

	noIdentifier := changedAnomaly()
	noIdentifier.Identifier = ""
	if _, err := ReviewFindingRecord(noIdentifier, "main", "c", "s", time.Unix(0, 0).UTC()); err == nil {
		t.Error("a finding with no identifier has no subject, so it must be refused")
	}
}

// Current and Expected are raw source text, so an over-long field has to be
// cut on a rune boundary.
//
// This test originally asserted utf8.ValidString(record.Text) and was
// UNFAILABLE: the quoted fields pass through %q, which escapes invalid tail
// bytes, so a byte-wise cut still yields valid, writable UTF-8. Mutation
// testing caught it. What a byte cut really produces is `\xe2` escape garbage
// delivered into an agent's context, so that is what this asserts — plus the
// property directly, on truncateRunes itself.
func TestLongMultiByteSourceLinesAreCutOnRuneBoundaries(t *testing.T) {
	t.Parallel()

	wide := strings.Repeat("€", maxQuotedField+40)
	if len(wide) == utf8.RuneCountInString(wide) {
		t.Fatal("the fixture must be multi-byte, or this test cannot fail")
	}
	if maxQuotedField%3 == 0 {
		t.Fatalf("maxQuotedField (%d) is a multiple of 3, so a byte cut of a 3-byte rune would land on a boundary and this test could not fail", maxQuotedField)
	}

	// The property, asserted where it lives: the cut value is valid on its own,
	// before any quoting can launder it.
	if cut := truncateRunes(wide, maxQuotedField); !utf8.ValidString(cut) {
		t.Error("truncateRunes produced invalid UTF-8")
	}

	a := changedAnomaly()
	a.Current = wide
	a.Expected = wide
	a.Evidence = strings.Repeat("ß", maxEvidenceField+40)

	record, err := ReviewFindingRecord(a, "main", "c", "s", time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(record.Text, `\x`) {
		t.Errorf("the stored text carries escaped partial-rune bytes, which is what an agent would read: %q", record.Text)
	}
	var buf strings.Builder
	if err := factmerge.WriteNDJSON(&buf, []factmerge.Record{record}); err != nil {
		t.Fatalf("the record must be writable: %v", err)
	}
	if !strings.Contains(record.Text, "…") {
		t.Error("an over-long field must be marked as cut, not silently shortened")
	}
}

// The builder's own Validate call is unreachable given its guards (mutation
// testing confirms deleting it changes nothing), because every remaining
// field is a constant. These are those constants: if one is edited into
// something the schema rejects, this is the test that fails, and it fails
// without needing a review fixture.
func TestReviewFactConstantsSatisfyTheSchema(t *testing.T) {
	t.Parallel()

	if !factmerge.ValidPath(ReviewFactPath) {
		t.Errorf("ReviewFactPath %q is not a valid taxonomy path, so every remembered finding would be refused", ReviewFactPath)
	}
	if !factmerge.ValidFactKind(ReviewFactKind) {
		t.Errorf("ReviewFactKind %q is outside the closed kind set", ReviewFactKind)
	}
	// The path must stay out of the two categories where review suspicions
	// would be returned by engineering queries.
	for _, forbidden := range []string{"architecture.", "constraints."} {
		if strings.HasPrefix(ReviewFactPath, forbidden) {
			t.Errorf("ReviewFactPath %q sits under %s, where it pollutes engineering recall", ReviewFactPath, forbidden)
		}
	}
	// Delivery depends on the kind being one the post-failure hook selects.
	if ReviewFactKind != factmerge.KindGotcha && ReviewFactKind != factmerge.KindClosedNegative {
		t.Errorf("ReviewFactKind %q is not a kind the post-failure hook delivers, so remembered findings would never reach a failing agent", ReviewFactKind)
	}
}

func TestReviewFindingRecordsKeepsGoingPastOneBadFinding(t *testing.T) {
	t.Parallel()

	bad := changedAnomaly()
	bad.File = ""
	records, problems := ReviewFindingRecords(
		[]Anomaly{changedAnomaly(), bad}, "main", "c", "s", time.Unix(0, 0).UTC())

	if len(records) != 1 {
		t.Errorf("want the one good finding recorded, got %d", len(records))
	}
	if len(problems) != 1 {
		t.Errorf("want the one bad finding reported, got %d", len(problems))
	}
}

// --location-only blanks the expected and current values, so the two flags
// together would write near-identical content-free records. The refusal must
// happen before anything is resolved or detected: Config here has NIL Deps,
// so a run that got as far as a dep call would panic rather than pass.
func TestRememberRefusesLocationOnly(t *testing.T) {
	t.Parallel()

	err := RunBrainReview(context.Background(), nil, Config{}, DetectorOptions{Remember: true, LocationOnly: true}, "q")
	if err == nil {
		t.Fatal("--remember with --location-only must be refused")
	}
	for _, want := range []string{"--remember", "--location-only"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must name both flags, missing %q: %v", want, err)
		}
	}
}
