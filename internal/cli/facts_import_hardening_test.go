package cli

import (
	"os"
	"strings"
	"testing"
	"time"
)

// Imported text is UNTRUSTED CONTENT, not merely untrusted size.
//
// The import path bounded how much of the file it would read, but stored
// m.Memory verbatim. printFactLine and the compact brief print fact text
// unescaped, so ANSI escapes, bidi overrides and zero-width runes from another
// tool's export reached the terminal and the agent's prompt.
//
// sanitizeDistilledFactText already guarded agent-produced text. Text from a
// third-party tool has strictly weaker provenance, so it cannot be held to a
// weaker standard.
func TestImportedMemoryTextIsSanitised(t *testing.T) {
	t.Parallel()

	raw := "deploy on \u001b[31mFriday\u001b[0m ‮neven‬​"
	payload := `[{"id":"m1","memory":` + jsonQuoteForTest(raw) + `}]`

	memories, skipped, err := parseMem0([]byte(payload))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if skipped != 0 || len(memories) != 1 {
		t.Fatalf("want 1 memory, got %d (skipped %d)", len(memories), skipped)
	}
	got := memories[0].Text
	for name, bad := range map[string]string{
		"ANSI escape":   "\u001b",
		"bidi override": "‮",
		"zero width":    "​",
	} {
		if strings.Contains(got, bad) {
			t.Errorf("%s survived import into fact text: %q", name, got)
		}
	}
	if !strings.Contains(got, "deploy on") {
		t.Fatalf("sanitising destroyed the legible text: %q", got)
	}
	// Changing a fact's text on the way in must be visible, not silent.
	joined := strings.Join(memories[0].Unsupported, " | ")
	if !strings.Contains(joined, "stripped on import") {
		t.Errorf("the text was altered but the report does not say so; Unsupported = %v", memories[0].Unsupported)
	}
}

// Clean text must pass through untouched and raise no note, or the report
// becomes noise everyone learns to ignore.
func TestCleanImportedTextIsUnchangedAndUnflagged(t *testing.T) {
	t.Parallel()

	memories, _, err := parseMem0([]byte(`[{"id":"m1","memory":"retries are capped at three"}]`))
	if err != nil || len(memories) != 1 {
		t.Fatalf("parse: %v (n=%d)", err, len(memories))
	}
	if memories[0].Text != "retries are capped at three" {
		t.Fatalf("clean text was altered: %q", memories[0].Text)
	}
	for _, u := range memories[0].Unsupported {
		if strings.Contains(u, "stripped on import") {
			t.Errorf("clean text raised a stripping note: %v", memories[0].Unsupported)
		}
	}
}

// `facts add --path` refuses a path outside the taxonomy because it would
// "mint immediately-orphaned facts the rest of the system treats as invalid".
// --path-prefix had only a shape check, so it could land a whole import under
// a category GC reports as orphaned while printing "imported N of N".
func TestImportPrefixMustBeUnderAKnownTaxonomyCategory(t *testing.T) {
	t.Parallel()

	f := newVerifyFixture(t)
	now := time.Now().UTC()

	if err := validateImportPrefixAgainstTaxonomy(f.brainDir, "zzznotacategory.inbox", now); err == nil {
		t.Error("an unknown top-level category must be refused, as it is for `facts add --path`")
	}
	// The default (empty) prefix is valid by construction and must not error.
	if err := validateImportPrefixAgainstTaxonomy(f.brainDir, "", now); err != nil {
		t.Errorf("the default prefix must be accepted: %v", err)
	}
	// A known category must pass.
	if err := validateImportPrefixAgainstTaxonomy(f.brainDir, "architecture.imported", now); err != nil {
		t.Errorf("a known category must be accepted: %v", err)
	}
}

func jsonQuoteForTest(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			if r < 0x20 || r > 0x7e {
				b.WriteString(`\u`)
				const hex = "0123456789abcdef"
				b.WriteByte(hex[(r>>12)&0xf])
				b.WriteByte(hex[(r>>8)&0xf])
				b.WriteByte(hex[(r>>4)&0xf])
				b.WriteByte(hex[r&0xf])
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// A real export carrying one bad field was reported as the wrong KIND of file,
// and the decoder's own message — which already named the field — was thrown
// away. `{"results":[{"id":1}]}` is unmistakably a mem0 export.
func TestTypeErrorNamesTheFieldInsteadOfDenyingTheFormat(t *testing.T) {
	t.Parallel()

	_, _, err := parseMem0([]byte(`{"results":[{"id":1,"memory":"x"}]}`))
	if err == nil {
		t.Fatal("a numeric id must be an error")
	}
	if strings.Contains(err.Error(), "not a mem0 export") {
		t.Errorf("a real export with one bad field was reported as the wrong format: %v", err)
	}
	if !strings.Contains(err.Error(), "unexpected type") {
		t.Errorf("the error must say a type was wrong: %v", err)
	}
	// The decoder names the field; discarding that leaves the user nothing to
	// look for.
	if !strings.Contains(err.Error(), "id") {
		t.Errorf("the error must name the offending field: %v", err)
	}
}

// A file that is genuinely not an export must still say so, or the fix above
// would turn every shape problem into a type problem.
func TestNonExportStillReportsTheFormat(t *testing.T) {
	t.Parallel()

	_, _, err := parseMem0([]byte(`{"nope":true}`))
	if err == nil {
		t.Fatal("an object with no results array must be an error")
	}
	if !strings.Contains(err.Error(), "not a mem0 export") {
		t.Errorf("a non-export must be reported as such: %v", err)
	}
}

// The origin field is SELF-DECLARED. factmerge/identity.go is explicit that
// records are not authenticated by transport, and a fact can reach this store
// from a shared fact-set head or a hand-written file as easily as from
// `facts import`.
//
// verify skipped its anchor checks for anything claiming origin "imported",
// on the premise that an imported fact's evidence was never here. That premise
// fails when the anchor itself names repository evidence: `facts import`
// writes one anchor carrying only SessionID "<tool>:<foreign id>", never a
// commit. So a record claiming imported WITH a commit anchor was waved through
// as benign.
func TestImportedOriginWithRepositoryEvidenceIsNotWavedThrough(t *testing.T) {
	t.Parallel()

	legit := factRecord{
		Origin:     factOriginImported,
		Provenance: []factAnchor{{SessionID: "mem0:abc123"}},
	}
	if importedAnchorClaimsThisRepo(legit) {
		t.Error("a genuine import carries only its source tool and foreign id; it must not be flagged")
	}

	for name, anchor := range map[string]factAnchor{
		"commit":     {SessionID: "mem0:abc", Commit: "deadbeef"},
		"checkpoint": {SessionID: "mem0:abc", CheckpointID: "cp1"},
		"transcript": {SessionID: "mem0:abc", Transcript: "sessions/main/s1.jsonl"},
		"verified":   {SessionID: "mem0:abc", Verified: true},
	} {
		t.Run(name, func(t *testing.T) {
			fact := factRecord{Origin: factOriginImported, Provenance: []factAnchor{anchor}}
			if !importedAnchorClaimsThisRepo(fact) {
				t.Errorf("an imported fact naming %s evidence must be flagged; the origin and anchor disagree", name)
			}
		})
	}
}

// Upsert unions anchors and never moves Status or Text, which is right for a
// re-distill: a fact retracted here must not spring back to life because it
// was distilled again. The consequence for an import is that text matching a
// locally RETRACTED fact attaches an anchor to the retracted record, lands
// nothing retrievable, and still counted toward "imported N of N".
//
// countKeptActiveFacts already covered the opposite direction
// (superseded-at-source meeting active-here). This is the missing mirror.
func TestImportOntoARetractedFactIsCountedAndReported(t *testing.T) {
	t.Parallel()

	incoming := []factRecord{{ID: "f1", Status: factStatusActive}}
	existing := []factRecord{{ID: "f1", Status: factStatusRetracted}}

	if got := countImportsOntoInactiveFacts(incoming, existing); got != 1 {
		t.Fatalf("an import landing on a retracted fact must be counted, got %d", got)
	}
	// Superseded counts too: equally not retrievable.
	if got := countImportsOntoInactiveFacts(incoming, []factRecord{{ID: "f1", Status: factStatusSuperseded}}); got != 1 {
		t.Errorf("an import landing on a superseded fact must be counted, got %d", got)
	}
	// The ordinary case must stay silent, or the note becomes noise.
	if got := countImportsOntoInactiveFacts(incoming, []factRecord{{ID: "f1", Status: factStatusActive}}); got != 0 {
		t.Errorf("an import onto an active fact is the normal case and must not be flagged, got %d", got)
	}
	if got := countImportsOntoInactiveFacts(incoming, nil); got != 0 {
		t.Errorf("a fact with no local counterpart must not be flagged, got %d", got)
	}
	// Each id counted once, however many memories mapped to it.
	dupes := []factRecord{{ID: "f1", Status: factStatusActive}, {ID: "f1", Status: factStatusActive}}
	if got := countImportsOntoInactiveFacts(dupes, existing); got != 1 {
		t.Errorf("one id must count once, got %d", got)
	}
}

// A manifest refresh that fails AFTER writeFacts succeeded must not present as
// a failed import: the facts are on disk, and telling the user it failed
// invites a re-run of something that already happened.
//
// This is a SOURCE-LEVEL guard, and weaker than the others here. Forcing
// updateFactSourceManifestLocked to fail would need write-failure injection
// the import path does not expose, so this asserts the shape instead: the error
// is captured and reported, not returned. Stated rather than dressed up as a
// behavioural test.
func TestManifestRefreshFailureIsReportedNotReturned(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("facts_import.go")
	if err != nil {
		t.Fatalf("read facts_import.go: %v", err)
	}
	src := string(data)
	if !strings.Contains(src, "func newFactsImportCommand") {
		t.Fatal("read the wrong file; this guard would be vacuous")
	}
	if strings.Contains(src, "return updateFactSourceManifestLocked(") {
		t.Error("a manifest-refresh error is returned from inside the write lock, which aborts before any " +
			"report: the facts are already on disk, so the user is told the import failed when it succeeded")
	}
	if !strings.Contains(src, "manifestErr = updateFactSourceManifestLocked(") {
		t.Error("the manifest error is no longer captured for reporting")
	}
	if !strings.Contains(src, "the facts were imported, but the source manifest could not be refreshed") {
		t.Error("the warning that distinguishes a stale manifest from a failed import is gone")
	}
}

// parseImportTime returned only a zero time, which made "the field was absent"
// and "the field was present and unreadable" indistinguishable. The caller
// substitutes `now` for a zero, so an export whose timestamps are epoch
// seconds, or use a space instead of a T, had EVERY fact silently re-dated to
// the import date while the report said "imported N of N".
//
// Recency ordering is what these fields are for. A date that is wrong but
// looks real is worse than no date, because nothing downstream can tell.
func TestTimestampParsingDistinguishesAbsentFromUnreadable(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, in string
		wantYear int
		wantBad  bool
	}{
		{"absent", "", 0, false},
		{"whitespace only", "   ", 0, false},
		{"rfc3339", "2024-07-01T12:00:00Z", 2024, false},
		{"date only", "2024-07-01", 2024, false},
		{"space separator", "2024-07-01 12:00:00", 2024, false},
		{"space and minutes", "2024-07-01 12:00", 2024, false},
		{"no zone", "2024-07-01T12:00:00", 2024, false},
		{"python isoformat", "2024-07-01T12:00:00.123456", 2024, false},
		{"epoch seconds", "1719835200", 2024, false},
		{"epoch millis", "1719835200000", 2024, false},
		{"genuinely unreadable", "last Tuesday", 0, true},
		{"a bare year is not a date", "2024", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, bad := parseImportTime(tc.in)
			if bad != tc.wantBad {
				t.Fatalf("parseImportTime(%q) unreadable=%v, want %v", tc.in, bad, tc.wantBad)
			}
			if tc.wantYear == 0 {
				if !got.IsZero() {
					t.Errorf("parseImportTime(%q) = %v, want the zero time", tc.in, got)
				}
				return
			}
			if got.Year() != tc.wantYear {
				t.Errorf("parseImportTime(%q) = %v, want year %d", tc.in, got, tc.wantYear)
			}
		})
	}
}

// An unreadable timestamp must reach the report, naming the field and quoting
// the value -- the fact is dated at import time, and nothing downstream can
// tell that from a real date.
func TestUnreadableTimestampIsReportedPerMemory(t *testing.T) {
	t.Parallel()

	memories, _, err := parseMem0([]byte(`{"results":[
	  {"id":"a","memory":"We deploy on Thursdays.","created_at":"last Tuesday"},
	  {"id":"b","memory":"We use Postgres.","created_at":"2024-07-01T12:00:00Z"}
	]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(memories) != 2 {
		t.Fatalf("want 2 memories, got %d", len(memories))
	}

	var flagged, clean []string
	for _, m := range memories {
		joined := strings.Join(m.Unsupported, " | ")
		if strings.Contains(joined, "could not read") {
			flagged = append(flagged, m.ForeignID)
		} else {
			clean = append(clean, m.ForeignID)
		}
	}
	if len(flagged) != 1 || flagged[0] != "a" {
		t.Errorf("the unreadable timestamp must be reported on memory a alone, flagged=%v", flagged)
	}
	if len(clean) != 1 || clean[0] != "b" {
		t.Errorf("a readable timestamp must not be flagged, clean=%v", clean)
	}
	// The note must name the field and quote the value, or it cannot be acted on.
	note := strings.Join(memories[0].Unsupported, " | ")
	if !strings.Contains(note, "created_at") || !strings.Contains(note, "last Tuesday") {
		t.Errorf("the note must name the field and quote the value: %q", note)
	}
}
