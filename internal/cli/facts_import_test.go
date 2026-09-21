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
func TestImportedSupersededFactCarriesItsSuccessor(t *testing.T) {
	facts, report := memoriesToFacts(
		[]importedMemory{{ForeignID: "m1", Text: "Old approach.", ReplacedBy: "m2"}},
		0, "mem0", "", "main", time.Now().UTC())
	if len(facts) != 1 || facts[0].Status != factStatusSuperseded {
		t.Fatalf("got %+v", facts)
	}
	if facts[0].SupersededBy == "" {
		t.Fatal("a superseded fact was imported with no successor; the lineage is gone")
	}
	if !strings.Contains(facts[0].SupersededBy, "m2") {
		t.Fatalf("SupersededBy = %q, which does not name the replacement", facts[0].SupersededBy)
	}
	if report.Superseded != 1 {
		t.Fatalf("report.Superseded = %d", report.Superseded)
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
