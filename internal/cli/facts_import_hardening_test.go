package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
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

// importTaxonomyPath takes the FIRST usable category and breaks, so a memory
// tagged ["deployment","security"] was filed under project.imported.deployment
// with "security" gone and nothing said -- while factmerge.MaxPaths is 2, so
// the room for a second path was already there.
func TestImportCarriesASecondCategoryAndReportsTheRest(t *testing.T) {
	t.Parallel()

	paths, dropped := importedFactPaths("project.imported", []string{"deployment", "security"})
	if len(paths) != 2 {
		t.Fatalf("two categories must yield two paths, got %v", paths)
	}
	if len(dropped) != 0 {
		t.Errorf("nothing is dropped at the cap, got %v", dropped)
	}
	joined := strings.Join(paths, " ")
	for _, want := range []string{"deployment", "security"} {
		if !strings.Contains(joined, want) {
			t.Errorf("category %q was lost; paths = %v", want, paths)
		}
	}

	// Past the cap the loss is real, so it must be named.
	_, over := importedFactPaths("project.imported", []string{"a", "b", "c", "d"})
	if len(over) != 2 || over[0] != "c" || over[1] != "d" {
		t.Errorf("categories past the cap must be returned for reporting, got %v", over)
	}

	// Duplicates after sanitising are not two categories.
	dedup, _ := importedFactPaths("project.imported", []string{"Deployment", "deployment"})
	if len(dedup) != 1 {
		t.Errorf("one category spelled two ways is one path, got %v", dedup)
	}

	// No categories still yields a usable path; an unpathed fact is unfindable.
	bare, _ := importedFactPaths("project.imported", nil)
	if len(bare) != 1 || !strings.Contains(bare[0], "general") {
		t.Errorf("a memory with no categories must still get a path, got %v", bare)
	}
}

// Superseded was incremented once per MEMORY while Imported counts distinct
// FACTS. Two memories that dedupe into one fact, both superseded at the
// source, reported "2 already superseded" out of "1 imported" -- two numbers in
// different units presented as comparable.
func TestSupersededIsCountedInTheSameUnitAsImported(t *testing.T) {
	t.Parallel()

	// Two records, one id: exactly the collapse case.
	facts := []factRecord{
		{ID: "same", Status: factStatusSuperseded},
		{ID: "same", Status: factStatusSuperseded},
	}
	if got := countSupersededFacts(facts); got != 1 {
		t.Errorf("two memories collapsing into one fact is one superseded fact, got %d", got)
	}
	mixed := []factRecord{
		{ID: "a", Status: factStatusSuperseded},
		{ID: "b", Status: factStatusActive},
		{ID: "c", Status: factStatusSuperseded},
	}
	if got := countSupersededFacts(mixed); got != 2 {
		t.Errorf("two distinct superseded facts must count 2, got %d", got)
	}
	if got := countSupersededFacts(nil); got != 0 {
		t.Errorf("no facts is no superseded facts, got %d", got)
	}
}

// Facts are branch-scoped: recall on another branch does not see them and
// reports an empty corpus, which reads as a failed import rather than a
// misplaced one. The report must say which branch they landed on.
func TestImportReportNamesTheBranch(t *testing.T) {
	t.Parallel()

	_, report := memoriesToFacts(
		[]importedMemory{{ForeignID: "x", Text: "We deploy on Thursdays."}},
		0, "mem0", "project.imported", "feature/x", time.Now().UTC())
	if report.Branch != "feature/x" {
		t.Errorf("the report must name the branch the facts landed on, got %q", report.Branch)
	}
}

// A command that is going to fail must not mutate the brain first.
//
// The all-skipped check sat AFTER the write block, so a doomed import took the
// write lock, rewrote the facts file with an empty set, and bumped the
// manifest's generation and freshness fields -- making the brain look freshly
// imported -- and only then returned the error saying nothing was imported.
//
// Driven through the real command and asserted on the BYTES on disk, because
// the defect was in the ORDER of two calls and no unit test of either one can
// see it.
func TestADoomedImportDoesNotTouchTheBrainFirst(t *testing.T) {
	f := newVerifyFixture(t)
	writeDistillFixtureAt(t, f.brainDir, f.now)

	manifestPath := filepath.Join(f.brainDir, exportManifestFileName)
	before, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	// Non-vacuity: the fixture must have a manifest with content, or comparing
	// it proves nothing.
	if len(before) == 0 {
		t.Fatal("fixture manifest is empty; this guard would pass trivially")
	}

	// An export from another tool: records with no `memory` field at all.
	export := filepath.Join(t.TempDir(), "letta.json")
	if err := os.WriteFile(export, []byte(`[{"id":"a","content":"x"},{"id":"b","content":"y"}]`), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cmd := NewRootCommand(f.opts)
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"facts", "import", "--source", "mem0", "--file", export})
	err = cmd.Execute()
	if err == nil {
		t.Fatalf("an import where no record has memory text must fail:\n%s", out.String())
	}
	// NEGATIVE CONTROL. The first version of this test passed "--from", which
	// does not exist: cobra rejected the flag, the command never ran, and the
	// manifest was trivially unchanged. The guard reported success while
	// measuring nothing. Pin the error to the IMPORT failing, not the parse.
	if strings.Contains(err.Error(), "unknown flag") || strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("the command never ran, so this guard measures nothing: %v", err)
	}
	if !strings.Contains(err.Error(), "has a \"memory\" field") {
		t.Fatalf("expected the all-skipped error, got: %v", err)
	}

	after, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest after: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("a failed import rewrote the manifest:\nbefore: %s\nafter:  %s", before, after)
	}
}

// factmerge.Upsert keys on the content-addressed id and keeps the EXISTING
// record's Origin while unioning provenance. So an imported memory whose text
// and paths normalise onto a pre-existing AUTHORED fact -- which importing
// under an existing taxonomy prefix explicitly permits -- produces a fact with
// Origin != "imported" carrying a foreign anchor.
//
// Neither record-level origin gate fires for that record, so verify tried to
// resolve "mem0:abc" as a local session and reported the fact ORPHANED ("its
// evidence is gone") when its evidence was never here: the same false and
// alarming diagnosis the origin gates exist to prevent, reached another way.
func TestForeignAnchorDetectionIsShapeAndSourceBased(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		anchor factAnchor
		want   bool
	}{
		"import-minted anchor":        {factAnchor{SessionID: "mem0:abc123"}, true},
		"unknown tool is not foreign": {factAnchor{SessionID: "notatool:abc"}, false},
		"local id with no colon":      {factAnchor{SessionID: "20373dcb"}, false},
		"source prefix but no id":     {factAnchor{SessionID: "mem0:"}, false},
		"bare source name":            {factAnchor{SessionID: "mem0"}, false},
		"empty":                       {factAnchor{}, false},
		"foreign prefix + commit":     {factAnchor{SessionID: "mem0:abc", Commit: "deadbeef"}, false},
		"foreign prefix + checkpoint": {factAnchor{SessionID: "mem0:abc", CheckpointID: "cp1"}, false},
		"foreign prefix + transcript": {factAnchor{SessionID: "mem0:abc", Transcript: "s/1.jsonl"}, false},
		"foreign prefix + turn":       {factAnchor{SessionID: "mem0:abc", TurnID: "t1"}, false},
		"foreign prefix + line":       {factAnchor{SessionID: "mem0:abc", Line: 3}, false},
		"foreign prefix + verified":   {factAnchor{SessionID: "mem0:abc", Verified: true}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := anchorNamesAForeignSource(tc.anchor); got != tc.want {
				t.Errorf("anchorNamesAForeignSource(%+v) = %v, want %v", tc.anchor, got, tc.want)
			}
		})
	}
}

// The anchor an import actually writes must be classified as foreign. Without
// this, the table above could pass while the real anchor shape drifted away
// from it -- the two must be checked against each other, not just asserted.
func TestTheAnchorImportWritesIsDetectedAsForeign(t *testing.T) {
	t.Parallel()

	facts, _ := memoriesToFacts(
		[]importedMemory{{ForeignID: "abc123", Text: "We deploy on Thursdays."}},
		0, "mem0", "project.imported", "main", time.Now().UTC())
	if len(facts) != 1 || len(facts[0].Provenance) != 1 {
		t.Fatalf("expected one fact with one anchor, got %d fact(s)", len(facts))
	}
	if !anchorNamesAForeignSource(facts[0].Provenance[0]) {
		t.Errorf("the anchor import writes (%+v) must be detected as foreign, or verify will call it orphaned",
			facts[0].Provenance[0])
	}
	// And every declared source must be detectable, so adding one to
	// factImportSources without teaching verify about it cannot slip through.
	for _, source := range factImportSources {
		if !anchorNamesAForeignSource(factAnchor{SessionID: source + ":x"}) {
			t.Errorf("declared import source %q is not detected as foreign", source)
		}
	}
}

// Foreign ids are untrusted text, exactly like the memory body. ForeignID is
// embedded in the anchor and `facts show` prints anchor.SessionID straight
// through valueOrUnset with %s (facts_read_cmd.go:540), which neither quotes
// nor strips.
func TestForeignIDsAreSanitisedLikeTheMemoryText(t *testing.T) {
	t.Parallel()

	memories, _, err := parseMem0([]byte(`{"results":[
	  {"id":"a\u001b[31mRED","memory":"We deploy on Thursdays.","replaced_by":"b​ZWSP"}
	]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(memories) != 1 {
		t.Fatalf("want 1 memory, got %d", len(memories))
	}
	m := memories[0]
	if strings.ContainsRune(m.ForeignID, '\u001b') {
		t.Errorf("an escape sequence survived in the foreign id: %q", m.ForeignID)
	}
	if strings.ContainsRune(m.ReplacedBy, '​') {
		t.Errorf("a zero-width character survived in replaced_by: %q", m.ReplacedBy)
	}
	// Stripping must be disclosed, like the text case.
	if !strings.Contains(strings.Join(m.Unsupported, " | "), "source id") {
		t.Errorf("stripping a source id must be reported: %v", m.Unsupported)
	}
}

// THE CALL SITE. The detector tests above all stayed green when the gate in
// verifyAnchor was removed -- a perfect detector nothing consults looks
// identical to them. This drives verifyFact on the record the collision
// actually produces.
//
// Shape of that record: Origin "authored" (Upsert keeps the EXISTING origin),
// one local anchor and one foreign anchor unioned in. The local anchor must
// still be checked normally; the foreign one must come back
// unverifiable-here, never orphaned -- "its evidence is gone" about evidence
// that was never here is the false alarm the origin gates exist to prevent.
func TestVerifyDoesNotCallAForeignAnchorOrphaned(t *testing.T) {
	f := newVerifyFixture(t)

	v := &verifyContext{
		ctx:      context.Background(),
		opts:     f.opts,
		repoDir:  f.repoDir,
		brainDir: f.brainDir,
		branch:   "main",
		manifest: &exportManifest{
			DefaultBranch: "main",
			Sources:       &brainSources{Sessions: &sessionSourceManifest{Sessions: nil}},
		},
	}

	// Origin is NOT "imported": that is the whole point -- Upsert kept the
	// pre-existing authored origin while unioning the foreign anchor in.
	collided := factRecord{
		ID:         "fact:collided",
		Text:       "We deploy on Thursdays.",
		Branch:     "main",
		Origin:     factOriginAuthored,
		Status:     factStatusActive,
		Provenance: []factAnchor{{SessionID: "mem0:abc123"}},
	}

	result := v.verifyFact(collided)
	if len(result.Anchors) != 1 {
		t.Fatalf("expected one anchor result, got %d", len(result.Anchors))
	}
	if result.Anchors[0].Verdict == verifyVerdictOrphaned {
		t.Errorf("a foreign anchor was reported orphaned -- 'its evidence is gone' about evidence that was never here: %s",
			result.Anchors[0].Reason)
	}
	if result.Anchors[0].Verdict != verifyVerdictUnverifiableHere {
		t.Errorf("anchor verdict = %q (%s), want %q",
			result.Anchors[0].Verdict, result.Anchors[0].Reason, verifyVerdictUnverifiableHere)
	}
	if !strings.Contains(result.Anchors[0].Reason, "mem0") {
		t.Errorf("the reason must name the tool the anchor points at, got %q", result.Anchors[0].Reason)
	}
	if result.Verdict == verifyVerdictOrphaned {
		t.Errorf("the FACT verdict must not be orphaned either: %s", result.Reason)
	}
}
