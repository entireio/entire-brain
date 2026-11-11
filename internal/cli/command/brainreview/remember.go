package brainreview

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/entireio/entire-brain/factmerge"
)

// ReviewFactPath is the only taxonomy path review write-back writes to.
//
// It is deliberately NOT under architecture.* or constraints.*, where a
// review's transient suspicions would sit beside settled engineering
// decisions and be returned by every query about them. The brain store has
// already paid for that once: benchmark-shaped content landed in those two
// categories and now pollutes engineering recall. A dedicated path keeps
// review output filterable, retractable and countable as a group.
const ReviewFactPath = "project.review.regression"

// ReviewFactKind is gotcha, for two reasons that both matter.
//
// Truthfulness: a regression suspicion is not a resolved choice (decision) or
// a must-hold rule (invariant). It is a non-obvious trap — "this line no
// longer says what the record says it said" — which is what gotcha names.
//
// Delivery: gotcha and closed-negative are exactly the two kinds the
// post-failure hook selects from, so a remembered finding reaches an agent at
// the moment a command fails in that area, and the record's locus makes the
// pre-edit hook surface it when that same file is opened. A kind chosen for
// tidiness instead would have written records no surface delivers.
const ReviewFactKind = factmerge.KindGotcha

// Field caps. A fact is read inside an agent's context window, so an
// unbounded source line or history excerpt is a cost paid on every delivery.
// These are rune-safe cuts (see truncateRunes), because Current and Expected
// are raw source text and may be any encoding the repository holds.
const (
	maxQuotedField   = 160
	maxEvidenceField = 240
)

// RememberOutcome reports what a write-back actually persisted. Recorded and
// AlreadyPresent are counted separately because a re-run of the same review
// on an unchanged tree must report zero new records rather than claiming it
// wrote the same findings again.
type RememberOutcome struct {
	Branch         string `json:"branch"`
	Recorded       int    `json:"recorded"`
	AlreadyPresent int    `json:"already_present"`
}

// ReviewFindingRecord builds the durable record for one anomaly.
//
// The record's TEXT is a pure function of the anomaly — no timestamp, no
// commit, no run id. That is a requirement, not a stylistic choice: the record
// id is sha256(normalized text + paths), so a date in the text would mint a
// brand new record on every review of an unchanged tree, and the store would
// grow one duplicate per run. With the text fixed, a re-run produces the same
// id, Upsert unions the provenance anchors, and the "supersede rather than
// append" rule for duplicate findings is satisfied by construction instead of
// by a comparison pass.
//
// The commit and session go in the PROVENANCE anchor, which is where a
// re-checkable citation belongs: `verify` can test an anchor, and it cannot
// test a sentence.
func ReviewFindingRecord(a Anomaly, branch, commit, sessionID string, now time.Time) (factmerge.Record, error) {
	file := strings.TrimSpace(a.File)
	if file == "" {
		return factmerge.Record{}, fmt.Errorf("review finding has no file, so it cannot be anchored or located")
	}
	identifier := strings.TrimSpace(a.Identifier)
	if identifier == "" {
		return factmerge.Record{}, fmt.Errorf("review finding for %s has no identifier, so its claim has no subject", file)
	}

	text := reviewFindingText(a, file, identifier)
	paths := []string{ReviewFactPath}

	anchor := factmerge.Anchor{SessionID: strings.TrimSpace(sessionID), Commit: strings.TrimSpace(commit)}

	record := factmerge.Record{
		ID:         factmerge.RecordID(text, paths),
		Paths:      paths,
		Kind:       ReviewFactKind,
		Locus:      reviewFindingLocus(file, a.Symbol, identifier),
		Text:       text,
		Branch:     strings.TrimSpace(branch),
		Origin:     factmerge.OriginReview,
		Status:     factmerge.StatusActive,
		Confidence: fmt.Sprintf("%.2f", a.Confidence),
		Provenance: []factmerge.Anchor{anchor},
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	// Validate here rather than at the store boundary, so a malformed record
	// is attributed to the finding that produced it instead of surfacing as an
	// anonymous write failure after the lock is taken.
	//
	// Given the guards above this is UNREACHABLE today — every remaining
	// field is a constant or computed — and mutation testing says so: deleting
	// this check leaves the suite green. It is kept as the check that a future
	// field, or a future edit to one of those constants, cannot reach the
	// store unvalidated. The constants themselves are pinned by
	// TestReviewFactConstantsSatisfyTheSchema, which is what would actually
	// catch that today.
	if err := record.Validate(); err != nil {
		return factmerge.Record{}, err
	}
	return record, nil
}

func reviewFindingText(a Anomaly, file, identifier string) string {
	var b strings.Builder
	location := file
	if a.Line > 0 {
		location = fmt.Sprintf("%s:%d", file, a.Line)
	}

	if a.Kind == "deleted" {
		fmt.Fprintf(&b, "A diff-less review found %s absent from %s, where the brain's recorded history asserted %q.",
			identifier, location, truncateRunes(a.Expected, maxQuotedField))
	} else {
		fmt.Fprintf(&b, "A diff-less review found %s reading %q at %s, where the brain's recorded history asserted %q.",
			identifier, truncateRunes(a.Current, maxQuotedField), location, truncateRunes(a.Expected, maxQuotedField))
	}
	if reason := strings.TrimSpace(a.Reason); reason != "" {
		fmt.Fprintf(&b, " %s", strings.TrimSuffix(reason, "."))
		b.WriteString(".")
	}
	if evidence := strings.TrimSpace(a.Evidence); evidence != "" {
		fmt.Fprintf(&b, " Recorded evidence: %q.", truncateRunes(evidence, maxEvidenceField))
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// reviewFindingLocus is what makes a remembered finding deliverable. The
// pre-edit hook selects memories by the file about to be edited, so the file
// path has to be the first locus entry; the symbol and identifier follow so a
// query naming either one reaches the record too.
func reviewFindingLocus(file, symbol, identifier string) []string {
	locus := []string{file}
	for _, extra := range []string{strings.TrimSpace(symbol), identifier} {
		if extra == "" || extra == file {
			continue
		}
		if !containsString(locus, extra) {
			locus = append(locus, extra)
		}
	}
	return locus
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// truncateRunes cuts on a rune boundary and marks the cut.
//
// Measured, because my first comment here claimed the wrong consequence: a
// byte-wise cut does NOT produce a record factmerge refuses. The quoted
// fields go through %q, which escapes the invalid tail bytes, so the stored
// text stays valid UTF-8 and writable. What a byte cut actually produces is a
// record ending in `\xe2` escape garbage, delivered verbatim into an agent's
// context. The cut is rune-safe for legibility, not for writability.
func truncateRunes(value string, limit int) string {
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit]) + "…"
}

// ReviewFindingRecords builds the records for every anomaly, returning the
// records and one error per finding that could not be turned into a record.
// A finding that cannot be recorded does not stop the others: the review
// already ran, and refusing the whole write-back over one unanchored finding
// would discard the rest of a completed analysis.
func ReviewFindingRecords(anomalies []Anomaly, branch, commit, sessionID string, now time.Time) ([]factmerge.Record, []error) {
	var records []factmerge.Record
	var problems []error
	for _, a := range anomalies {
		record, err := ReviewFindingRecord(a, branch, commit, sessionID, now)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		records = append(records, record)
	}
	return records, problems
}
