package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Brain could export and could not import, which is the friction a switcher
// hits: everything they already recorded stays where it is. These tests hold
// the importer to the part that is easy to get wrong — being honest about what
// crossed, what did not, and what was refused.

const mem0Export = `{"results":[
 {"id":"f4cbdb08","memory":"Alex is planning a trip to San Francisco","created_at":"2024-07-01T12:00:00Z","updated_at":"2024-07-01T12:00:00Z","categories":["travel"]},
 {"id":"aaa-111","memory":"The team deploys on Thursdays, never Fridays.","created_at":"2025-03-04T09:30:00Z","categories":["Team Process"],"user_id":"alex"},
 {"id":"bbb-222","memory":"Old rule, replaced.","created_at":"2024-01-01T00:00:00Z","replaced_by":"aaa-111","expiration_date":"2026-12-01"},
 {"id":"ccc-333","memory":"   "}
]}`

func TestMem0ImportCountsWhatItDropped(t *testing.T) {
	memories, skipped, err := parseMem0([]byte(mem0Export))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(memories) != 3 || skipped != 1 {
		t.Fatalf("got %d memories and %d skipped, want 3 and 1", len(memories), skipped)
	}
	// The export held four. Reporting "3 of 3" would hide the drop, which is
	// the bug this count exists to prevent.
	_, report := memoriesToFacts(memories, skipped, "mem0", "project.imported", "main", time.Now())
	if report.Read != 4 {
		t.Fatalf("read = %d, want 4 — the file's count, not the survivors'", report.Read)
	}
	if report.Imported != 3 || report.SkippedText != 1 {
		t.Fatalf("imported/skipped = %d/%d, want 3/1", report.Imported, report.SkippedText)
	}
}

func TestMem0ImportRefusesAFileThatIsNotAnExport(t *testing.T) {
	// Unmarshalling into a struct succeeds for ANY JSON object, leaving the
	// field nil — so decoding alone cannot tell "an export with no memories"
	// from "not an export". Without an explicit presence check this returned
	// success and imported nothing, which is worse than failing.
	for _, notAnExport := range []string{`{"nope":true}`, `{}`, `"a string"`, `42`} {
		if _, _, err := parseMem0([]byte(notAnExport)); err == nil {
			t.Fatalf("%s was accepted as a mem0 export", notAnExport)
		}
	}
	// A genuinely empty export is valid and must still parse.
	memories, skipped, err := parseMem0([]byte(`{"results":[]}`))
	if err != nil {
		t.Fatalf("an empty export is still an export: %v", err)
	}
	if len(memories) != 0 || skipped != 0 {
		t.Fatalf("empty export produced %d memories, %d skipped", len(memories), skipped)
	}
	// And a bare array, which is what the list endpoint returns.
	if _, _, err := parseMem0([]byte(`[{"id":"z","memory":"bare"}]`)); err != nil {
		t.Fatalf("bare array rejected: %v", err)
	}
}

func TestImportedFactsDoNotFabricateLocalProvenance(t *testing.T) {
	memories, skipped, err := parseMem0([]byte(mem0Export))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	facts, _ := memoriesToFacts(memories, skipped, "mem0", "project.imported", "main", time.Now())

	for _, fact := range facts {
		// An imported memory was recorded against another tool, not against
		// this repository. Giving it a commit anchor would launder a foreign
		// assertion into local evidence.
		if fact.Origin != factOriginImported {
			t.Fatalf("%s: origin = %q, want %q", fact.ID, fact.Origin, factOriginImported)
		}
		if len(fact.Provenance) != 1 {
			t.Fatalf("%s: want exactly one anchor naming the source", fact.ID)
		}
		anchor := fact.Provenance[0]
		if anchor.Commit != "" || anchor.CheckpointID != "" {
			t.Fatalf("%s: imported fact claims local evidence: commit=%q checkpoint=%q",
				fact.ID, anchor.Commit, anchor.CheckpointID)
		}
		if !strings.HasPrefix(anchor.SessionID, "mem0:") {
			t.Fatalf("%s: anchor %q should name the source tool and foreign id", fact.ID, anchor.SessionID)
		}
	}
}

func TestImportPreservesSupersessionAndReportsWhatCannotCross(t *testing.T) {
	memories, skipped, err := parseMem0([]byte(mem0Export))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	facts, report := memoriesToFacts(memories, skipped, "mem0", "project.imported", "main", time.Now())

	// The source had already replaced one memory. Importing it as active would
	// resurrect something its owner retired.
	superseded := 0
	for _, fact := range facts {
		if fact.Status == "superseded" {
			superseded++
		}
	}
	if superseded != 1 || report.Superseded != 1 {
		t.Fatalf("superseded = %d (report %d), want 1", superseded, report.Superseded)
	}

	// Brain has no TTL and does not scope by user. Both must be named, not
	// quietly discarded: an expiry that silently vanishes turns a memory its
	// owner scheduled to disappear into one that never does.
	joined := strings.Join(report.Unsupported, " | ")
	for _, want := range []string{"expiration_date", "user_id"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("report does not mention %q: %v", want, report.Unsupported)
		}
	}
}

func TestImportedFactsAreFiledSomewhereObvious(t *testing.T) {
	memories, skipped, err := parseMem0([]byte(mem0Export))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	facts, _ := memoriesToFacts(memories, skipped, "mem0", "project.imported", "main", time.Now())
	for _, fact := range facts {
		// Foreign categories are free text and cannot be trusted to land under
		// a known top level, so everything stays under the prefix until a human
		// reclassifies it.
		if !strings.HasPrefix(fact.Paths[0], "project.imported.") {
			t.Fatalf("%s filed at %q, outside the import prefix", fact.ID, fact.Paths[0])
		}
	}
	// "Team Process" has to survive as a usable segment. Underscore, not
	// hyphen: the taxonomy pattern admits only [a-z0-9_], so the hyphenated
	// form this test originally expected was a path normalisation would drop,
	// leaving the record with no paths at all.
	found := false
	for _, fact := range facts {
		if fact.Paths[0] == "project.imported.team_process" {
			found = true
		}
	}
	if !found {
		t.Fatalf(`category "Team Process" did not become project.imported.team_process: %q`, facts[0].Paths)
	}
}

// A record whose paths do not equal their own normalisation violates the
// identity invariant VerifyIdentity enforces and `facts sync` relies on, so it
// would be rejected downstream by the check meant to protect it. Categories
// arrive from a foreign system and can contain anything.
func TestImportedPathsSurviveNormalisation(t *testing.T) {
	for _, category := range []string{
		"Team Process", "work-notes", "UPPER", "spaces and more", "emoji 🎉 here",
		"punctuation!@#", "  padded  ", "", "---", "_",
	} {
		facts, _ := memoriesToFacts(
			[]importedMemory{{ForeignID: "m1", Text: "A fact.", Categories: []string{category}}},
			0, "mem0", "", "main", time.Now().UTC())
		if len(facts) != 1 {
			t.Fatalf("category %q produced %d facts", category, len(facts))
		}
		paths := facts[0].Paths
		if len(paths) == 0 {
			t.Fatalf("category %q left the fact with no paths; it would be unfindable", category)
		}
		normalized := normalizeFactPaths(paths)
		if len(normalized) != len(paths) {
			t.Fatalf("category %q produced %q, which normalisation drops", category, paths)
		}
		for i := range paths {
			if paths[i] != normalized[i] {
				t.Fatalf("category %q produced %q, normal form is %q", category, paths[i], normalized[i])
			}
		}
	}
}

// The id is derived from the paths, so it has to be derived from the paths that
// are actually stored. Normalising after the id is computed would produce a
// record whose id does not match its own content.
func TestImportedFactIDMatchesItsStoredPaths(t *testing.T) {
	facts, _ := memoriesToFacts(
		[]importedMemory{{ForeignID: "m1", Text: "A fact.", Categories: []string{"Team Process"}}},
		0, "mem0", "", "main", time.Now().UTC())
	if len(facts) != 1 {
		t.Fatalf("got %d facts", len(facts))
	}
	if want := factRecordID(facts[0].Text, facts[0].Paths); facts[0].ID != want {
		t.Fatalf("id %s does not match its stored text and paths (want %s)", facts[0].ID, want)
	}
}

// A superseded fact with no successor drops the lineage the import otherwise
// goes out of its way to preserve, and the reconciliation and display paths
// read that field.
func TestImportedSupersededFactPointsAtARealFact(t *testing.T) {
	// SupersededBy has to name a fact in THIS store. A foreign id would be
	// looked up by reconciliation and display and never found — a dangling
	// pointer dressed as lineage, which is worse than none because it reads as
	// though it resolves.
	facts, report := memoriesToFacts([]importedMemory{
		{ForeignID: "m1", Text: "Old approach.", ReplacedBy: "m2"},
		{ForeignID: "m2", Text: "New approach."},
	}, 0, "mem0", "", "main", time.Now().UTC())
	if len(facts) != 2 {
		t.Fatalf("got %d facts", len(facts))
	}
	old, replacement := facts[0], facts[1]
	if old.Status != factStatusSuperseded {
		t.Fatalf("the replaced memory is %q, want superseded", old.Status)
	}
	if old.SupersededBy != replacement.ID {
		t.Fatalf("SupersededBy = %q, want the imported successor's id %q", old.SupersededBy, replacement.ID)
	}
	if strings.Contains(old.SupersededBy, "m2") || strings.Contains(old.SupersededBy, "mem0:") {
		t.Fatalf("SupersededBy carries a foreign reference rather than a fact id: %q", old.SupersededBy)
	}
	if report.Superseded != 1 {
		t.Fatalf("report.Superseded = %d", report.Superseded)
	}
}

// A replacement the export does not contain cannot be represented. Reporting
// it beats inventing a reference that resolves to nothing.
func TestImportReportsASupersessionItCannotResolve(t *testing.T) {
	facts, report := memoriesToFacts([]importedMemory{
		{ForeignID: "m1", Text: "Old approach.", ReplacedBy: "m99-not-in-this-export"},
	}, 0, "mem0", "", "main", time.Now().UTC())
	if len(facts) != 1 {
		t.Fatalf("got %d facts", len(facts))
	}
	if facts[0].Status != factStatusSuperseded {
		t.Fatalf("status = %q, want superseded", facts[0].Status)
	}
	if facts[0].SupersededBy != "" {
		t.Fatalf("SupersededBy = %q, but the successor is not in this export", facts[0].SupersededBy)
	}
	if !strings.Contains(strings.Join(report.Unsupported, " | "), "supersession") {
		t.Fatalf("the unresolvable supersession was not reported: %v", report.Unsupported)
	}
}

// A prefix that cannot survive normalisation is rejected by name, rather than
// importing a thousand memories under a path the rest of the system refuses.
func TestImportRejectsAnUnusablePathPrefix(t *testing.T) {
	for _, bad := range []string{"Mixed.Case", "one", "a.b.c.d", "has-hyphen.here"} {
		if err := validateImportPrefix(bad); err == nil {
			t.Fatalf("--path-prefix %q was accepted", bad)
		}
	}
	for _, good := range []string{"", "project.imported", "architecture.storage"} {
		if err := validateImportPrefix(good); err != nil {
			t.Fatalf("--path-prefix %q was rejected: %v", good, err)
		}
	}
}

func TestImportTimestampsFallBackToImportTime(t *testing.T) {
	// A memory with no usable timestamp must be dated at import, not at the
	// zero value — a year-1 fact would sort last forever and read as ancient.
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	facts, _ := memoriesToFacts([]importedMemory{{ForeignID: "x", Text: "undated"}}, 0, "mem0", "project.imported", "main", now)
	if !facts[0].CreatedAt.Equal(now) || !facts[0].UpdatedAt.Equal(now) {
		t.Fatalf("undated memory got %v/%v, want both %v", facts[0].CreatedAt, facts[0].UpdatedAt, now)
	}
	if got := parseImportTime("2024-07-01T12:00:00Z"); got.IsZero() {
		t.Fatal("RFC3339 timestamp failed to parse")
	}
	if got := parseImportTime("not a time"); !got.IsZero() {
		t.Fatalf("unparseable timestamp became %v, want the zero value", got)
	}
}

// Every fact mutator refreshes the source manifest under the same write lock.
// `facts status`, the integrity checks and the freshness report read their
// counts and generation time from it, so an import that skipped the refresh
// left all three describing the brain as it was before the import — and said
// nothing about it.
func TestImportRefreshesTheFactSourceManifest(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	opts, brainDir, branch := rememberReviveEnv(t, now)

	// A brain with a manifest that predates the import.
	if err := os.MkdirAll(brainDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := withBrainWriteLock(brainDir, func() error {
		return updateFactSourceManifestLocked(brainDir, now.Add(-72*time.Hour))
	}); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}
	before, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}

	export := filepath.Join(t.TempDir(), "mem0.json")
	payload := `{"results":[{"id":"m1","memory":"Retries stop after three attempts.","created_at":"2026-09-01T00:00:00Z"}]}`
	if err := os.WriteFile(export, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newFactsImportCommand(opts)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetContext(context.Background())
	if err := cmd.Flags().Set("file", export); err != nil {
		t.Fatal(err)
	}
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("import: %v", err)
	}

	facts, err := loadFacts(brainDir, branch)
	if err != nil || len(facts) == 0 {
		t.Fatalf("the import wrote no facts: %v %+v", err, facts)
	}
	after, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if after == nil || after.Sources == nil || after.Sources.Facts == nil {
		t.Fatal("the manifest has no fact source after an import that wrote facts")
	}
	if before != nil && before.Sources != nil && before.Sources.Facts != nil {
		if !after.Sources.Facts.GeneratedAt.After(before.Sources.Facts.GeneratedAt) {
			t.Fatalf("the fact source was not refreshed: still generated at %s",
				after.Sources.Facts.GeneratedAt)
		}
	}
}

// A status value written as a bare string diverges silently the day the
// constant changes, and a fact stuck on a status nothing matches is invisible
// to every filter that uses the constant.
func TestImportUsesTheSupersededConstant(t *testing.T) {
	memories := []importedMemory{{
		ForeignID:  "m1",
		Text:       "The queue abstraction was removed.",
		ReplacedBy: "m2",
	}}
	facts, report := memoriesToFacts(memories, 0, "mem0", "", "main", time.Now().UTC())
	if len(facts) != 1 {
		t.Fatalf("got %d facts", len(facts))
	}
	if facts[0].Status != factStatusSuperseded {
		t.Fatalf("status = %q, want the factStatusSuperseded constant (%q)", facts[0].Status, factStatusSuperseded)
	}
	if report.Superseded != 1 {
		t.Fatalf("report.Superseded = %d", report.Superseded)
	}
}

// The command must reject an unusable prefix before it writes anything.
// Validating the function is not the same as calling it: an earlier version of
// this change had the check written and never invoked.
func TestImportCommandRejectsAnUnusablePathPrefixBeforeWriting(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	opts, brainDir, branch := rememberReviveEnv(t, now)

	export := filepath.Join(t.TempDir(), "mem0.json")
	payload := `{"results":[{"id":"m1","memory":"Retries stop after three attempts."}]}`
	if err := os.WriteFile(export, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newFactsImportCommand(opts)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetContext(context.Background())
	if err := cmd.Flags().Set("file", export); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("path-prefix", "Mixed.Case"); err != nil {
		t.Fatal(err)
	}
	err := cmd.RunE(cmd, nil)
	if err == nil {
		t.Fatal("the command accepted a prefix that is not in normal form")
	}
	if !strings.Contains(err.Error(), "path-prefix") {
		t.Fatalf("the error does not name the flag: %v", err)
	}
	// And nothing may have been written.
	facts, loadErr := loadFacts(brainDir, branch)
	if loadErr == nil && len(facts) > 0 {
		t.Fatalf("the rejected import still wrote %d fact(s)", len(facts))
	}
}

// This file's contract is that anything the source expresses and Brain cannot
// represent is named in the report rather than dropped — it is the reason
// somebody would trust an import enough to switch. metadata and
// structured_attributes were decoded and then silently discarded.
func TestImportNamesEveryFieldItCannotRepresent(t *testing.T) {
	payload := `{"results":[{
		"id":"m1",
		"memory":"Retries stop after three attempts.",
		"metadata":{"team":"platform","ticket":"ENG-412"},
		"structured_attributes":{"severity":"high"},
		"expiration_date":"2027-01-01",
		"user_id":"u-1"
	}]}`
	memories, skipped, err := parseMem0([]byte(payload))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, report := memoriesToFacts(memories, skipped, "mem0", "", "main", time.Now().UTC())

	joined := strings.Join(report.Unsupported, " | ")
	for _, field := range []string{"metadata", "structured_attributes", "expiration_date", "user_id"} {
		if !strings.Contains(joined, field) {
			t.Fatalf("%s was dropped without being named: %q", field, joined)
		}
	}
}

// And a memory carrying none of them must not be reported as losing something.
// A report that names fields the export did not use is noise, and noise is how
// a report stops being read.
func TestImportDoesNotInventLosses(t *testing.T) {
	payload := `{"results":[{"id":"m1","memory":"Retries stop after three attempts."}]}`
	memories, skipped, err := parseMem0([]byte(payload))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, report := memoriesToFacts(memories, skipped, "mem0", "", "main", time.Now().UTC())
	if len(report.Unsupported) != 0 {
		t.Fatalf("a plain memory reported losses: %v", report.Unsupported)
	}
}

// An export is bounded input like any other. Unbounded reads are a
// memory-exhaustion problem whether the size was chosen deliberately or by
// mistake, and a FIFO at the path blocks forever rather than failing — which is
// why every other untrusted-input surface here uses the safe helpers.
func TestImportRefusesAnOversizedExport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "huge.json")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	// Sparse: the size is what matters, not the bytes.
	if err := f.Truncate(maxManifestBytes + 1); err != nil {
		f.Close()
		t.Skipf("cannot create a sparse file: %v", err)
	}
	f.Close()

	if _, err := readImportFile(path); err == nil {
		t.Fatalf("an export over the %d-byte limit was read in full", int64(maxManifestBytes))
	}
}

func TestImportRefusesAFileThatIsNotARegularFile(t *testing.T) {
	// A FIFO blocks forever on read. Failing is the only terminating outcome.
	dir := t.TempDir()
	fifo := filepath.Join(dir, "export.json")
	if err := makeTestFIFO(fifo); err != nil {
		t.Skipf("cannot create a FIFO: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := readImportFile(fifo)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a FIFO was accepted as an export")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reading a FIFO did not terminate")
	}
}

// `--file -` reads stdin, which is the least bounded input of all: a pipe can
// produce data forever. This drives the real stdin path, because the
// oversized-file test covers only the path branch.
func TestImportRefusesAnOversizedStdinExport(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = original
		r.Close()
	})

	// Write past the limit. The reader stops early, so the writer will block on
	// a full pipe and be abandoned — closing the read end in cleanup releases
	// it, and the write errors are expected rather than checked.
	go func() {
		defer w.Close()
		chunk := make([]byte, 1<<20)
		for i := 0; i < (maxManifestBytes>>20)+2; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}()

	done := make(chan error, 1)
	go func() {
		_, readErr := readImportFile("-")
		done <- readErr
	}()
	select {
	case readErr := <-done:
		if readErr == nil {
			t.Fatalf("stdin past the %d-byte limit was read in full", int64(maxManifestBytes))
		}
	case <-time.After(30 * time.Second):
		t.Fatal("reading oversized stdin did not terminate")
	}
}

// upsertFact unions provenance and never moves a status — right for a
// re-distill, wrong for an import. A memory the source retired that is already
// present locally as active would be imported and stay active, with the report
// claiming it was superseded.
//
// The local status is kept: a fact asserted in this repository is not retired
// by a foreign export. What must not happen is the divergence being invisible.
func TestImportDoesNotSilentlyFailToSupersedeAnExistingFact(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	opts, brainDir, branch := rememberReviveEnv(t, now)

	text := "The queue abstraction was removed."
	// The same statement already active in this brain.
	local := factRecord{
		ID: factRecordID(text, []string{"project.imported.general"}), Paths: []string{"project.imported.general"},
		Text: text, Branch: branch, Status: factStatusActive, Origin: factOriginAuthored,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := withBrainWriteLock(brainDir, func() error {
		return writeFacts(brainDir, branch, []factRecord{local})
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	export := filepath.Join(t.TempDir(), "mem0.json")
	payload := `{"results":[{"id":"m1","memory":"` + text + `","replaced_by":"m2"}]}`
	if err := os.WriteFile(export, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newFactsImportCommand(opts)
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetContext(context.Background())
	if err := cmd.Flags().Set("file", export); err != nil {
		t.Fatal(err)
	}
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("import: %v", err)
	}

	stored, err := loadFacts(brainDir, branch)
	if err != nil {
		t.Fatal(err)
	}
	var found *factRecord
	for i := range stored {
		if stored[i].Text == text {
			found = &stored[i]
		}
	}
	if found == nil {
		t.Fatalf("the fact is gone: %+v", stored)
	}
	if found.Status != factStatusActive {
		t.Fatalf("a local assertion was retired by an export: status = %q", found.Status)
	}
	// And the divergence has to be reported, or the import claims a
	// supersession that did not happen.
	if !strings.Contains(out.String(), "supersession not applied") {
		t.Fatalf("the unapplied supersession was not reported: %q", out.String())
	}
}

// A source id appearing twice cannot identify anything. Resolving a
// replaced_by through it picks whichever memory happened to be last in the
// file — a supersession pointed at an arbitrary fact, which is worse than none
// because it looks deliberate.
func TestImportRefusesToResolveSupersessionThroughADuplicateID(t *testing.T) {
	facts, report := memoriesToFacts([]importedMemory{
		{ForeignID: "m1", Text: "Old approach.", ReplacedBy: "dup"},
		{ForeignID: "dup", Text: "First candidate."},
		{ForeignID: "dup", Text: "Second candidate."},
	}, 0, "mem0", "", "main", time.Now().UTC())
	if len(facts) != 3 {
		t.Fatalf("got %d facts", len(facts))
	}
	if facts[0].SupersededBy != "" {
		t.Fatalf("supersession resolved through a duplicated id to %q", facts[0].SupersededBy)
	}
	joined := strings.Join(report.Unsupported, " | ")
	if !strings.Contains(joined, "duplicate source ids") {
		t.Fatalf("the duplicated ids were not reported: %q", joined)
	}
	if !strings.Contains(joined, "ambiguous") {
		t.Fatalf("the unresolvable supersession was not reported: %q", joined)
	}
}

// And a unique id must still resolve — the duplicate check must scope the
// resolution rather than disable it.
func TestImportStillResolvesUnambiguousSupersessionAlongsideDuplicates(t *testing.T) {
	facts, _ := memoriesToFacts([]importedMemory{
		{ForeignID: "m1", Text: "Old approach.", ReplacedBy: "m2"},
		{ForeignID: "m2", Text: "New approach."},
		{ForeignID: "dup", Text: "First."},
		{ForeignID: "dup", Text: "Second."},
	}, 0, "mem0", "", "main", time.Now().UTC())
	if len(facts) != 4 {
		t.Fatalf("got %d facts", len(facts))
	}
	if facts[0].SupersededBy != facts[1].ID {
		t.Fatalf("an unambiguous supersession was not resolved: %q", facts[0].SupersededBy)
	}
}

// Two memories whose text and category normalise to the same thing are the same
// fact under a content-addressed id, and upsert correctly folds them into one.
// The count must agree: reporting two imported when the store holds one makes
// the numbers disagree with reality and explains nothing.
func TestImportCountsMergedDuplicatesOnce(t *testing.T) {
	facts, report := memoriesToFacts([]importedMemory{
		{ForeignID: "m1", Text: "Retries stop after three attempts."},
		{ForeignID: "m2", Text: "Retries stop after three attempts."},
		{ForeignID: "m3", Text: "A different fact entirely."},
	}, 0, "mem0", "", "main", time.Now().UTC())

	distinct := map[string]bool{}
	for _, fact := range facts {
		distinct[fact.ID] = true
	}
	if len(distinct) != 2 {
		t.Fatalf("expected 2 distinct facts, got %d", len(distinct))
	}
	if report.Imported != 2 {
		t.Fatalf("report.Imported = %d, but only %d distinct facts exist", report.Imported, len(distinct))
	}
	if !strings.Contains(strings.Join(report.Unsupported, " | "), "merged into one fact") {
		t.Fatalf("the merge was not reported: %v", report.Unsupported)
	}
}

// `facts status` shows origins as line items under a total. An origin with no
// bucket makes the parts visibly fail to sum to the whole, with nothing
// accounting for the difference.
func TestImportedFactsAppearInTheStatusOriginTally(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	opts, brainDir, branch := rememberReviveEnv(t, now)

	facts := []factRecord{
		{ID: "a", Paths: []string{"architecture.api.ports"}, Text: "Distilled.", Branch: branch, Origin: factOriginDistilled, Status: factStatusActive, CreatedAt: now, UpdatedAt: now},
		{ID: "b", Paths: []string{"architecture.api.ports"}, Text: "Authored.", Branch: branch, Origin: factOriginAuthored, Status: factStatusActive, CreatedAt: now, UpdatedAt: now},
		{ID: "c", Paths: []string{"architecture.api.ports"}, Text: "Imported.", Branch: branch, Origin: factOriginImported, Status: factStatusActive, CreatedAt: now, UpdatedAt: now},
	}
	if err := withBrainWriteLock(brainDir, func() error {
		return writeFacts(brainDir, branch, facts)
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	counts, countsErr := factsStatusCountsForBranch(brainDir, branch, facts)
	if countsErr != nil {
		t.Fatalf("counts: %v", countsErr)
	}
	if counts.Imported != 1 {
		t.Fatalf("Imported = %d, want 1", counts.Imported)
	}
	if got := counts.Distilled + counts.Authored + counts.Imported; got != 3 {
		t.Fatalf("the origin buckets sum to %d of 3 facts; the difference is unaccounted for", got)
	}
	_ = opts
}

// A memory naming itself as its own replacement resolves through byForeignID to
// itself, which would write SupersededBy == its own ID: a fact retired with
// nowhere to go, and a lineage that can never be followed. The dangling and
// ambiguous cases were both reported; this one was written silently.
func TestImportRefusesSelfReferentialSupersession(t *testing.T) {
	facts, report := memoriesToFacts([]importedMemory{
		{ForeignID: "m1", Text: "It replaces itself.", ReplacedBy: "m1"},
	}, 0, "mem0", "", "main", time.Now().UTC())
	if len(facts) != 1 {
		t.Fatalf("got %d facts", len(facts))
	}
	if facts[0].SupersededBy == facts[0].ID {
		t.Fatalf("fact is superseded by itself: %s", facts[0].SupersededBy)
	}
	if facts[0].SupersededBy != "" {
		t.Fatalf("a self-reference invented a successor: %q", facts[0].SupersededBy)
	}
	joined := strings.Join(report.Unsupported, " | ")
	if !strings.Contains(joined, "the memory itself") {
		t.Fatalf("the self-reference was dropped silently: %q", joined)
	}
}

// Two memories that normalise to one fact but disagree on status used to be
// resolved by export order: Upsert keeps the first copy's Status, so a memory
// the source had retired could land second and lose its retirement, surfacing
// here as Active. Losing a retirement is the worse direction.
func TestImportKeepsARetirementWhenADuplicateDisagrees(t *testing.T) {
	// The retired copy second — the order that used to drop it.
	facts, report := memoriesToFacts([]importedMemory{
		{ForeignID: "a", Text: "We deploy on Thursdays."},
		{ForeignID: "b", Text: "We deploy on Thursdays.", ReplacedBy: "c"},
		{ForeignID: "c", Text: "We deploy on Fridays."},
	}, 0, "mem0", "", "main", time.Now().UTC())

	for _, fact := range facts {
		if fact.Text != "We deploy on Thursdays." {
			continue
		}
		if fact.SupersededBy == "" {
			t.Fatalf("the retirement was dropped on export order: %+v", fact)
		}
		if fact.Status != factStatusSuperseded {
			t.Fatalf("status = %q, want superseded: %+v", fact.Status, fact)
		}
	}
	joined := strings.Join(report.Unsupported, " | ")
	if !strings.Contains(joined, "duplicate a retired memory") {
		t.Fatalf("the status conflict was not reported: %q", joined)
	}
}

// Two memories that normalise to one fact but name DIFFERENT replacements are
// ambiguous in the same way a duplicated source id is: there is no right
// answer, and taking whichever came first would invent a lineage out of export
// order. The retirement-conflict resolution kept the first and overwrote the
// second silently.
func TestImportRefusesConflictingSupersessionTargets(t *testing.T) {
	facts, report := memoriesToFacts([]importedMemory{
		{ForeignID: "a", Text: "We deploy on Thursdays.", ReplacedBy: "x"},
		{ForeignID: "b", Text: "We deploy on Thursdays.", ReplacedBy: "y"},
		{ForeignID: "x", Text: "We deploy on Fridays."},
		{ForeignID: "y", Text: "We deploy on Mondays."},
	}, 0, "mem0", "", "main", time.Now().UTC())

	for _, fact := range facts {
		if fact.Text != "We deploy on Thursdays." {
			continue
		}
		if fact.SupersededBy != "" {
			t.Fatalf("a successor was invented from export order: %q", fact.SupersededBy)
		}
	}
	joined := strings.Join(report.Unsupported, " | ")
	if !strings.Contains(joined, "disagrees between duplicates") {
		t.Fatalf("the conflicting targets were not reported: %q", joined)
	}
}

// A conflicting retirement leaves a fact superseded with no successor, and that
// is deliberate. Both halves are true: the source did retire the memory, and
// which memory replaced it cannot be determined. Resetting the status to active
// would be the harmful repair -- memoriesToFacts marks these superseded because
// "importing it as active would resurrect something its owner retired", and an
// ambiguous successor is not evidence that the retirement did not happen.
//
// Nothing enforces "superseded implies a successor": printFactLine renders the
// marker, the counters count it, and facts_sync_cmd simply matches no proposal,
// which is correct when no successor is known.
func TestConflictingRetirementStaysRetiredWithoutASuccessor(t *testing.T) {
	facts, report := memoriesToFacts([]importedMemory{
		{ForeignID: "a", Text: "We deploy on Thursdays.", ReplacedBy: "x"},
		{ForeignID: "b", Text: "We deploy on Thursdays.", ReplacedBy: "y"},
		{ForeignID: "x", Text: "We deploy on Fridays."},
		{ForeignID: "y", Text: "We deploy on Mondays."},
	}, 0, "mem0", "", "main", time.Now().UTC())

	var seen bool
	for _, fact := range facts {
		if fact.Text != "We deploy on Thursdays." {
			continue
		}
		seen = true
		if fact.SupersededBy != "" {
			t.Fatalf("a successor was invented from export order: %q", fact.SupersededBy)
		}
		if fact.Status != factStatusSuperseded {
			t.Fatalf("status = %q: a memory the source retired came back as %q",
				fact.Status, fact.Status)
		}
	}
	if !seen {
		t.Fatal("the duplicated memory vanished entirely")
	}
	if joined := strings.Join(report.Unsupported, " | "); !strings.Contains(joined, "disagrees between duplicates") {
		t.Fatalf("the ambiguity was not reported to the caller: %q", joined)
	}
}

// An imported fact's provenance anchor names a session in the system it came
// from. Run through the exported-session checks it comes back "orphaned" --
// which reads as "this fact's evidence is gone" -- when the truth is that its
// evidence was never in this repository. After importing a few hundred
// memories that is a screen of alarming verdicts describing nothing wrong.
func TestVerifyReportsAnImportedFactAsUnverifiableRatherThanOrphaned(t *testing.T) {
	facts, _ := memoriesToFacts([]importedMemory{
		{ForeignID: "abc123", Text: "The staging cluster is in eu-west-1."},
	}, 0, "mem0", "", "main", time.Now().UTC())
	if len(facts) != 1 {
		t.Fatalf("fixture produced %d facts", len(facts))
	}
	if facts[0].Origin != factOriginImported {
		t.Fatalf("fixture is not marked imported: %q", facts[0].Origin)
	}
	if len(facts[0].Provenance) == 0 || facts[0].Provenance[0].SessionID == "" {
		t.Fatal("fixture has no session anchor, so the orphaned path is not reached")
	}

	// A context with no exported sessions: exactly what a repository looks like
	// after importing from elsewhere.
	vctx := &verifyContext{branch: "main"}
	result := vctx.verifyFact(facts[0])

	if result.Verdict == verifyVerdictOrphaned {
		t.Fatalf("an imported fact was reported orphaned: %s", result.Reason)
	}
	if result.Verdict != verifyVerdictUnverifiableHere {
		t.Fatalf("verdict = %q, want %q (%s)", result.Verdict, verifyVerdictUnverifiableHere, result.Reason)
	}
	if !strings.Contains(result.Reason, "imported") {
		t.Fatalf("the reason does not say why it cannot be verified: %q", result.Reason)
	}
}

// factsStatusCounts.add rolls per-branch counts into the --all-branches totals,
// field by field. A field added to the struct and forgotten here does not fail
// anywhere: the totals just report zero for it, which reads as "there are none"
// rather than "nobody counted". Imported was added and missed exactly that way.
//
// Reflection rather than a list, so the next field is covered the day it is
// added rather than the day someone notices the total is wrong.
func TestFactsStatusCountsRollUpEveryField(t *testing.T) {
	var one factsStatusCounts
	v := reflect.ValueOf(&one).Elem()
	for i := 0; i < v.NumField(); i++ {
		if field := v.Field(i); field.CanSet() && field.Kind() == reflect.Int {
			field.SetInt(int64(i + 1))
		}
	}

	var total factsStatusCounts
	total.add(one)

	sum := reflect.ValueOf(total)
	for i := 0; i < sum.NumField(); i++ {
		field := sum.Type().Field(i)
		if sum.Field(i).Kind() != reflect.Int {
			continue
		}
		if sum.Field(i).Int() == 0 {
			t.Errorf("%s is not rolled into the --all-branches totals; it will always report zero", field.Name)
		}
	}
}
