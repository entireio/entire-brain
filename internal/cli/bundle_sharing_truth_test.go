package cli

import (
	"bytes"
	"encoding/json"
	"github.com/spf13/cobra"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Sharing a brain with a teammate is the one path that works today, and it is
// lossy: import with --overwrite replaces the fact layer wholesale. These pin
// the three places the product used to stay quiet about that.

func writeAuditLine(t *testing.T, brainDir, line string) {
	t.Helper()
	dir := filepath.Join(brainDir, semanticDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, semanticAuditLogName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open audit: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatalf("write audit: %v", err)
	}
}

func TestLastBundleImportReadsTheNewestEntry(t *testing.T) {
	t.Parallel()
	brainDir := t.TempDir()

	writeAuditLine(t, brainDir, `{"action":"bundle_export","generated_at":"2026-10-09T10:00:00Z"}`)
	writeAuditLine(t, brainDir, `{"action":"bundle_import","generated_at":"2026-10-10T10:00:00Z","layers":["facts"]}`)
	writeAuditLine(t, brainDir, `{"action":"bundle_import","generated_at":"2026-10-11T10:00:00Z","layers":["seed","docs","facts"]}`)

	at, ok := lastBundleImportAt(brainDir)
	if !ok {
		t.Fatal("a recorded import must be found, or the note can never fire")
	}
	if got := at.Format("2006-01-02"); got != "2026-10-11" {
		t.Fatalf("the newest import must win, got %s", got)
	}
}

// The audit log is local operational state: it may be absent, truncated
// mid-write, or hold lines from a newer build. None of that may turn a working
// recall into an error, so every failure mode degrades to "no note".
func TestLastBundleImportToleratesAnUnusableLog(t *testing.T) {
	t.Parallel()

	if _, ok := lastBundleImportAt(t.TempDir()); ok {
		t.Fatal("a brain with no audit log must report no import")
	}

	brainDir := t.TempDir()
	writeAuditLine(t, brainDir, `{"action":"bundle_impo`) // truncated mid-write
	writeAuditLine(t, brainDir, `{"action":"bundle_import","generated_at":"not-a-time"}`)
	writeAuditLine(t, brainDir, `{"action":"bundle_export","generated_at":"2026-10-11T10:00:00Z"}`)
	if _, ok := lastBundleImportAt(brainDir); ok {
		t.Fatal("no parseable import was recorded, so none may be reported")
	}

	writeAuditLine(t, brainDir, `{"action":"bundle_import","generated_at":"2026-10-11T12:00:00Z","layers":["facts"]}`)
	if _, ok := lastBundleImportAt(brainDir); !ok {
		t.Fatal("a good line after bad ones must still be read, or one truncated write hides every import")
	}
}

// The defect this exists for: after an import replaced Bob's facts, `recall`
// told him "no distillation timestamp is recorded; run refresh, then distill".
// Re-distilling cannot bring back what an import replaced, so the advice was
// not merely unhelpful, it was wrong.
func TestEmptyRecallNamesTheImportThatReplacedTheFacts(t *testing.T) {
	t.Parallel()
	brainDir := t.TempDir()
	now := time.Now().UTC()
	// A real brain, or the "no brain has been built" branch answers first and
	// this guard passes without ever reaching the code it is about.
	// Facts really present on the branch queried: the integrity branch must
	// pass (a store that LOST facts is a more precise answer and rightly wins),
	// and the other-branch branch must not fire either.
	if err := writeFacts(brainDir, "main", []factRecord{
		vitalityTestFact("retries are capped at three", "main", now),
	}); err != nil {
		t.Fatalf("write facts: %v", err)
	}
	writeDistillFixtureAt(t, brainDir, now)
	declareFactsAt(t, brainDir, now, now, 1, "main")
	writeAuditLine(t, brainDir, `{"action":"bundle_import","generated_at":"2026-10-11T10:00:00Z","layers":["seed","docs","facts"]}`)

	note := emptyResultBlindSpotOnBranch(brainDir, "main")
	if note == "" {
		t.Fatal("an empty result after an import must still be explained")
	}
	// The point is not that the word "distillation" is absent -- once work has
	// been distilled SINCE the import, coverage is the more useful lead and the
	// import rides along as a qualifier. The point is that a reader is never
	// sent to re-distill without being told it cannot bring those facts back.
	if strings.Contains(note, "distillation") && !strings.Contains(note, "whatever distillation reports") {
		t.Fatalf("re-distilling cannot restore what an import replaced, and the note does not say so; got: %s", note)
	}
	for _, want := range []string{"bundle import", "2026-10-11", "not evidence of absence"} {
		if !strings.Contains(note, want) {
			t.Errorf("the note must name the import, when it happened, and its limits; missing %q in: %s", want, note)
		}
	}
}

// A brain that never imported anything must keep its old note exactly, or this
// fix has silently replaced every other explanation with its own.
func TestABrainThatNeverImportedKeepsItsUsualNote(t *testing.T) {
	t.Parallel()
	brainDir := t.TempDir()
	now := time.Now().UTC()
	// Facts really present on the branch queried: the integrity branch must
	// pass (a store that LOST facts is a more precise answer and rightly wins),
	// and the other-branch branch must not fire either.
	if err := writeFacts(brainDir, "main", []factRecord{
		vitalityTestFact("retries are capped at three", "main", now),
	}); err != nil {
		t.Fatalf("write facts: %v", err)
	}
	writeDistillFixtureAt(t, brainDir, now)
	declareFactsAt(t, brainDir, now, now, 1, "main")
	writeAuditLine(t, brainDir, `{"action":"bundle_export","generated_at":"2026-10-11T10:00:00Z"}`)

	note := emptyResultBlindSpotOnBranch(brainDir, "main")
	if strings.Contains(note, "bundle import") {
		t.Fatalf("an export is not an import; got: %s", note)
	}
	if !strings.Contains(note, "distillation") {
		t.Fatalf("the ordinary coverage note must survive; got: %s", note)
	}
}

// The refusal used to end "or import into a brain without them" -- a state a
// real receiver is never in, because `setup` always writes seed and docs.
func TestTheImportRefusalNamesOnlyReachableChoices(t *testing.T) {
	t.Parallel()
	brainDir := t.TempDir()
	bundleRoot := t.TempDir()
	_ = brainDir
	_ = bundleRoot

	err := bundleImportConflictError([]string{"seed", "docs", "facts"})
	if err == nil {
		t.Fatal("conflicting layers must be refused")
	}
	msg := err.Error()
	if strings.Contains(msg, "import into a brain without them") {
		t.Fatalf("the refusal names a state a set-up brain can never be in: %s", msg)
	}
	for _, want := range []string{"--overwrite", "facts", "no longer be retrievable"} {
		if !strings.Contains(msg, want) && !strings.Contains(msg, "stops being retrievable") {
			t.Errorf("the refusal must say what --overwrite costs; missing %q in: %s", want, msg)
		}
	}
}

// The audit entry must be written BEFORE the layers are replaced.
//
// Written afterwards, a failed audit write leaves the receiver's facts already
// destroyed with no record that an import did it — and the empty `recall` that
// follows goes back to blaming distillation coverage, which is the exact defect
// the entry exists to prevent. Raised as a HIGH finding on this PR, and correct.
//
// This asserts ORDER, which no fixture can observe after the fact, so it reads
// the source: the audit call must appear before the walk that writes layers.
func TestTheImportIsRecordedBeforeAnythingIsReplaced(t *testing.T) {
	t.Parallel()
	// Reads the source, because ORDER is not observable from the outside once
	// the function has run.
	//
	// This used to claim the windows shards run prebuilt binaries away from the
	// package directory and would therefore skip. They do not: the shard that
	// failed got past this read and panicked further down, so the source was
	// beside it and this guard runs on every platform. The skip is kept for an
	// environment that genuinely has no source, but it is not the windows case
	// and saying so invited the next reader to dismiss a real windows failure.
	raw, err := os.ReadFile("semantic.go")
	if err != nil {
		t.Skipf("package source not available in this environment, so the order cannot be inspected: %v", err)
	}
	// A Windows checkout gets CRLF here: .gitattributes pins LF for templates
	// and golden fixtures but not for .go source. Searching for "\n}\n" then
	// found nothing, and slicing on the -1 panicked the whole package process.
	src := strings.ReplaceAll(string(raw), "\r\n", "\n")

	start := strings.Index(src, "func runSemanticBundleImport(")
	if start < 0 {
		t.Fatal("could not find runSemanticBundleImport, so this guard proves nothing")
	}
	fn := src[start:]
	end := strings.Index(fn, "\n}\n")
	if end < 0 {
		t.Fatal("could not find the end of runSemanticBundleImport, so this guard proves nothing")
	}
	fn = fn[:end]

	audit := strings.Index(fn, `appendSemanticAuditLayers(storage.BrainDir, "bundle_import"`)
	walk := strings.Index(fn, "filepath.WalkDir(tmpDir")
	if audit < 0 || walk < 0 {
		t.Fatalf("expected both the audit write and the layer walk in this function (audit=%d walk=%d)", audit, walk)
	}
	if audit > walk {
		t.Fatal("the import is recorded AFTER layers are replaced: a failed audit write destroys the facts and leaves no trace of why")
	}
	if strings.Count(fn, `appendSemanticAuditLayers(storage.BrainDir, "bundle_import"`) != 1 {
		t.Fatal("the import must be recorded exactly once")
	}
}

// An import that replaced only docs is not an explanation for a missing FACT.
// The audit entry used to carry no layer list, so every bundle_import read as
// proof the facts had been replaced — a note explaining an empty result with
// something that did not happen, which is worse than the distillation advice it
// set out to correct. Raised as a HIGH finding on this PR, and correct.
func TestOnlyAnImportThatTouchedFactsExplainsAMissingFact(t *testing.T) {
	t.Parallel()

	docsOnly := t.TempDir()
	writeAuditLine(t, docsOnly, `{"action":"bundle_import","generated_at":"2026-10-11T10:00:00Z","layers":["docs"]}`)
	if _, ok := lastBundleImportAt(docsOnly); ok {
		t.Error("an import that replaced only docs was reported as having replaced facts")
	}

	withFacts := t.TempDir()
	writeAuditLine(t, withFacts, `{"action":"bundle_import","generated_at":"2026-10-11T10:00:00Z","layers":["seed","facts"]}`)
	if _, ok := lastBundleImportAt(withFacts); !ok {
		t.Error("an import that DID replace facts must still be found")
	}

	// An entry with no layers predates the field. Unknown is not yes: asserting
	// a replacement the record does not describe is the bug being fixed.
	legacy := t.TempDir()
	writeAuditLine(t, legacy, `{"action":"bundle_import","generated_at":"2026-10-11T10:00:00Z"}`)
	if _, ok := lastBundleImportAt(legacy); ok {
		t.Error("an entry that names no layers must not be read as proof the facts were replaced")
	}
}

// The import line is printed to be copy-pasted, and --sha256 is required on
// import, so it is the only record of the digest a reader keeps. An --output
// path with a space in it produced a command that ran against a different path
// than the one just written. Raised as a finding on this PR, and correct.
//
// This drives the real export and reads what it actually printed. A first
// version built the line by hand and passed with the fix reverted, which is
// the whole failure mode this file keeps running into.
func TestThePrintedImportCommandSurvivesASpaceInThePath(t *testing.T) {
	_, opts, cmd, _, _ := bundleTestIndexedBrain(t)

	dir := filepath.Join(t.TempDir(), "with space")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "my bundle.tar")

	var printed bytes.Buffer
	cmd.SetOut(&printed)
	if err := runSemanticBundleExport(cmd.Context(), cmd, opts, output, false); err != nil {
		t.Fatalf("export: %v", err)
	}

	var line string
	for _, l := range strings.Split(printed.String(), "\n") {
		if strings.HasPrefix(l, "import it with:") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatalf("no import command was printed, so this guard proves nothing:\n%s", printed.String())
	}

	// The copy-pasted command must carry the path as ONE argument.
	if strings.Contains(line, "bundle import "+output+" ") {
		t.Errorf("the printed command splits on the space in the path: %s", line)
	}
	if !strings.Contains(line, shellQuotedRepoDir(output)) {
		t.Errorf("the printed command does not carry the quoted path: %s", line)
	}
}

// The import note is true forever -- those facts really are gone -- but it
// stopped being the most useful answer once work had been distilled since, and
// returning it unconditionally masked coverage information permanently.
// Raised as a finding on this PR, and correct.
//
// It still has to be NAMED after that point: re-distilling cannot bring back
// what the import replaced, and a reader sent to distill without being told
// would try it and find nothing.
func TestTheImportNoteStopsLeadingOnceWorkWasDistilledSince(t *testing.T) {
	distilled := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second)

	// distillCoverage reads Sources.Facts.LastDistilledAt, which the fixture
	// helper does not set; without it coverage is "unknown" and the import note
	// leads for the right reason, which would make this test prove nothing.
	withDistillation := func(t *testing.T) string {
		t.Helper()
		dir := writeDistillFixture(t, distilled)
		m, err := loadBrainManifest(dir)
		if err != nil {
			t.Fatal(err)
		}
		if m.Sources == nil {
			t.Fatal("fixture has no sources at all")
		}
		if m.Sources.Facts == nil {
			m.Sources.Facts = &factSourceManifest{}
		}
		m.Sources.Facts.LastDistilledAt = distilled
		if err := writeBrainManifestAndReadme(dir, *m); err != nil {
			t.Fatal(err)
		}
		if _, _, ok := distillCoverage(m); !ok {
			t.Fatal("coverage is still unknown, so this test would pass for the wrong reason")
		}
		return dir
	}

	older := withDistillation(t)
	writeAuditLine(t, older, `{"action":"bundle_import","generated_at":"`+
		distilled.Add(-48*time.Hour).Format(time.RFC3339)+`","layers":["facts"]}`)
	note := emptyResultBlindSpot(older)
	if strings.HasPrefix(note, "note: a bundle import") {
		t.Errorf("an import older than the last distillation still led the note: %s", note)
	}
	if !strings.Contains(note, "bundle import") {
		t.Errorf("the import was not mentioned at all, so a reader is sent to distill for facts it replaced: %s", note)
	}

	newer := withDistillation(t)
	writeAuditLine(t, newer, `{"action":"bundle_import","generated_at":"`+
		distilled.Add(1*time.Hour).Format(time.RFC3339)+`","layers":["facts"]}`)
	if note := emptyResultBlindSpot(newer); !strings.HasPrefix(note, "note: a bundle import") {
		t.Errorf("an import NEWER than the last distillation must still lead: %s", note)
	}
}

// The audit records the layers an import REPLACED, not the layers the bundle
// carried, and the difference is the whole point: the note it feeds claims
// facts recorded here before the import are gone.
//
// Raised twice on this PR as "record what was imported instead". That change
// would fire the note at a brain that never held facts -- telling a reader
// their remembered facts were destroyed when nothing was lost, in preference
// to the coverage advice that would have helped. This pins the behaviour so
// the reasoning does not have to be rediscovered a third time.
func TestAnImportIntoABrainWithNoFactsExplainsNothing(t *testing.T) {
	t.Parallel()

	// Receiver held seed and docs but no facts; the bundle carried facts too,
	// so a "what the bundle contained" record would have listed facts here.
	noFactsBefore := t.TempDir()
	writeAuditLine(t, noFactsBefore, `{"action":"bundle_import","generated_at":"2026-10-11T10:00:00Z","layers":["seed","docs"]}`)
	if _, ok := lastBundleImportAt(noFactsBefore); ok {
		t.Error("an import that replaced no facts was reported as having destroyed some")
	}

	// The same brain, had the audit recorded the bundle's contents instead.
	asImported := t.TempDir()
	writeAuditLine(t, asImported, `{"action":"bundle_import","generated_at":"2026-10-11T10:00:00Z","layers":["seed","docs","facts"]}`)
	if _, ok := lastBundleImportAt(asImported); !ok {
		t.Fatal("the contrast case does not fire, so this test proves nothing about the difference")
	}
}

// The end-to-end half: run a REAL import into a brain that holds no facts and
// read what the audit actually recorded.
//
// The hand-written-audit test above pins how lastBundleImportAt reads the
// entry; it says nothing about what the import WRITES, so adopting the
// repeatedly-proposed "record the layers the bundle carried" change left it
// green. This one does not.
func TestARealImportRecordsOnlyTheLayersItReplaced(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot(semanticSchemaVersion))

	// A bundle that CARRIES facts, into a brain that has none.
	archive := filepath.Join(t.TempDir(), "with-facts.tar")
	writeTestBundle(t, archive, map[string]string{
		exportManifestFileName: `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"schema_version":"` +
			semanticSchemaVersion + `","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson"}}}`,
		"semantic/snapshots/aaa111/snapshot.ndjson": semanticFixtureSnapshot(semanticSchemaVersion),
		"facts/taxonomy.json":                       `{"version":1}`,
		"facts/main-0000/" + factsFileName:          "{}\n",
	})
	if err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"},
		Options{Version: "test", Env: env, Runner: runner, Now: time.Now},
		archive, bundleSHA256(t, archive), true); err != nil {
		t.Fatalf("import: %v", err)
	}

	brainDir := bundleTestBrainDir(env)
	raw, err := os.ReadFile(filepath.Join(brainDir, semanticDirName, semanticAuditLogName))
	if err != nil {
		t.Fatalf("no audit was written at all, so this guard proves nothing: %v", err)
	}
	var entry struct {
		Action string   `json:"action"`
		Layers []string `json:"layers"`
	}
	seen := false
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if err := json.Unmarshal([]byte(line), &entry); err == nil && entry.Action == "bundle_import" {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("no bundle_import entry was recorded: %s", raw)
	}
	for _, layer := range entry.Layers {
		if layer == bundleLayerFacts {
			t.Errorf("the audit claims facts were REPLACED in a brain that held none, which makes the "+
				"empty-recall note assert a loss that did not happen: %v", entry.Layers)
		}
	}
	if _, ok := lastBundleImportAt(brainDir); ok {
		t.Error("an import into a brain with no facts was recorded as having destroyed facts")
	}
}
