package cli

import (
	"context"
	"errors"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"github.com/entireio/entire-brain/internal/cli/internal/retrieval"
	patterncore "github.com/entireio/entire-brain/internal/cli/patterns"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
)

// This file locks the defects found reviewing this branch. Every case below
// was an untested path: the full suite passed with each bug present.

// TestInspectRawTextFailsClosedOnUnreadableManifest locks the raw-scan
// exclusion guard. rawGuard.Paths is the ONLY exclusion mechanism in the raw
// file walk (a bare file carries no session id to match), and it is populated
// only from the manifest, so swallowing the manifest load error emptied the
// guard and scanned tombstoned transcripts. `brain brief` merges these raw
// matches into its history matches with no further filtering.
func TestInspectRawTextScansSessions(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	report, err := inspectBrainRawText(brainDir, "all", privacyCanary, 50)
	if err != nil {
		t.Fatalf("raw inspect: %v", err)
	}
	found := false
	for _, match := range report.Matches {
		if strings.Contains(match.Excerpt, privacyCanary) {
			found = true
		}
	}
	if !found {
		t.Fatalf("raw inspect did not reach the session transcript: %+v", report.Matches)
	}
}

// TestConversationQuerySurvivesUnavailableFTS locks that an FTS arm that could
// not run is a fallback, never a "filter scan exceeded" refusal. The complete
// flag is only meaningful when ok is true; honoring it on the unavailable path
// turned every FTS outage into a bogus degraded error on the DEFAULT unfiltered
// conversation query, contradicting retrieve.go's own contract that FTS is an
// optimization and never load-bearing.
func TestConversationQuerySurvivesUnavailableFTS(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	// historyFTSMatchExpr returns "" for a query that reduces to nothing
	// indexable, which is one of rankHistoryViaFTSFiltered's (nil,false,false)
	// unavailability returns. The in-memory scorer must still answer.
	if expr := historyFTSMatchExpr("cursor"); expr == "" {
		t.Fatal("fixture query must be indexable")
	}
	results, err := retrieveConversation(brainDir, "cursor", 5, retrieval.ModeLexical, retrievalOptions{})
	if err != nil {
		t.Fatalf("unfiltered conversation query must not fail: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("unfiltered conversation query returned nothing")
	}
	// The degraded error is reserved for a genuine filtered-scan overflow.
	if _, err := retrieveConversation(brainDir, "a", 5, retrieval.ModeLexical, retrievalOptions{}); err != nil {
		if strings.Contains(err.Error(), "filter scan exceeded") {
			t.Fatalf("stopword-only query reported a filter-scan overflow: %v", err)
		}
	}
}

// TestLocalRepoPhysicalSpellingRecoversOneBrain locks that a repository reached
// through a symlinked ANCESTOR resolves to a single brain. A brain already
// stored under the physical spelling, which is the spelling a relative
// invocation from inside the tree can produce, must be recovered in place
// rather than shadowed by a fresh empty one.
func TestLocalRepoPhysicalSpellingRecoversOneBrain(t *testing.T) {
	parent := t.TempDir()
	realParent := filepath.Join(parent, "real-parent")
	repoDir := filepath.Join(realParent, "repo")
	if err := os.MkdirAll(repoDir, 0o700); err != nil {
		t.Fatal(err)
	}
	aliasParent := filepath.Join(parent, "alias-parent")
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Skipf("directory symlinks are unavailable on this platform: %v", err)
	}
	logicalRepoDir := filepath.Join(aliasParent, "repo")

	env := EntireEnv{
		PluginConfigDir: t.TempDir(),
		PluginDataDir:   t.TempDir(),
		PluginStateDir:  t.TempDir(),
		PluginCacheDir:  t.TempDir(),
	}
	dirs, err := base.ResolvePluginDirs(env)
	if err != nil {
		t.Fatal(err)
	}
	// A brain established by an ordinary in-repo invocation, which is keyed on
	// the fully resolved physical root (what `git rev-parse --show-toplevel`
	// reports, and what filepath.Abs yields on platforms that expand a prefix
	// such as macOS /var -> /private/var).
	physicalKey, err := localRepoStorageKey(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	physical := base.RepoStorageForKey(dirs, physicalKey)
	if err := os.MkdirAll(physical.BrainDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(physical.BrainDir, bmanifest.FileName), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Addressing the same repo through the symlinked ancestor (what the plugin
	// host does with an absolute ENTIRE_REPO_ROOT) must find that brain.
	got, err := repoStoragePaths(context.Background(), nil, env, logicalRepoDir)
	if err != nil {
		t.Fatalf("resolve storage through a symlinked ancestor: %v", err)
	}
	if got.BrainDir != physical.BrainDir {
		t.Fatalf("symlinked-ancestor spelling opened a second brain\n got: %s\nwant: %s", got.BrainDir, physical.BrainDir)
	}

	// Both spellings populated is a refusal, never a silent pick.
	lexicalKey := localRepoStorageKeyForSpelling(logicalRepoDir)
	lexical := base.RepoStorageForKey(dirs, lexicalKey)
	if lexicalKey == physicalKey {
		t.Skip("platform collapsed the two spellings; nothing to disambiguate")
	}
	if err := os.MkdirAll(lexical.BrainDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lexical.BrainDir, bmanifest.FileName), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var conflict *localRepoIdentityConflictError
	if _, err := repoStoragePaths(context.Background(), nil, env, logicalRepoDir); !errors.As(err, &conflict) {
		t.Fatalf("state under both spellings must be a conflict, got %v", err)
	}
}

// TestHistoryInventoryReportsMissingSessionsRootAsEmpty locks the empty-Brain
// branch. collectHistorySessionInventory wraps its ENOENT in a typed error, and
// neither os.IsNotExist (no unwrapping at all) nor the type's custom Is (it
// matches only the degraded sentinel) could see it, so `refresh delta` and every
// `watch` tick hard-failed on a Brain with no sessions/ directory.
func TestHistoryInventoryReportsMissingSessionsRootAsEmpty(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	manifest := exportManifest{SchemaVersion: bmanifest.SchemaVersion, GeneratedAt: now, RepoKey: "test/empty", DefaultBranch: "main"}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}

	_, err := collectHistorySessionInventory(context.Background(), brainDir)
	if !errors.Is(err, errHistorySessionsRootMissing) {
		t.Fatalf("absent sessions/ must report the missing-root sentinel, got %v", err)
	}
	if errors.Is(err, errHistorySessionInventoryDegraded) {
		t.Fatalf("an empty Brain is not a degraded inventory: %v", err)
	}

	// The delta clears the overlay and succeeds instead of failing the tick.
	stats, err := buildHistoryShortTermLocked(brainDir, now)
	if err != nil {
		t.Fatalf("refresh delta on a Brain with no sessions/: %v", err)
	}
	if stats.Records != 0 || stats.Files != 0 {
		t.Fatalf("empty Brain produced records: %+v", stats)
	}

	// The long-term build keeps its actionable guidance.
	if _, _, err := buildBrainHistoryIndex(brainDir, now, nil); err == nil ||
		!strings.Contains(err.Error(), "run `"+base.BrainInvocation(os.LookupEnv, "refresh sessions")+"` first") {
		t.Fatalf("history build guidance = %v", err)
	}
}

// TestPatternRunHistoryRepairsCorruptLog locks the repair path. appendPatternRun
// is the only writer of runs.ndjson and rewrites it wholesale, so refusing to
// write when the existing content is corrupt left `patterns status`/`list`
// failing forever: the refresh meant to repair the file bailed out before
// writing it. Older builds wrote the log non-atomically, so a truncated final
// line is reachable on any existing brain.
func TestPatternRunHistoryRepairsCorruptLog(t *testing.T) {
	brainDir := t.TempDir()
	full := filepath.Join(brainDir, filepath.FromSlash(patterncore.RunsRelPath))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(`{"at":"2026-08-01T00:00:00Z","indexer_version":1}`+"\n"+`{"at":"2026-08-0`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := patterncore.LoadRunsChecked(brainDir); err == nil {
		t.Fatal("a truncated line must still read as corrupt")
	}
	if err := patterncore.AppendRun(brainDir, patterncore.Run{At: "2026-08-09T00:00:00Z", IndexerVersion: patterncore.IndexerVersion}); err != nil {
		t.Fatalf("refresh must be able to repair a corrupt run log: %v", err)
	}
	runs, err := patterncore.LoadRunsChecked(brainDir)
	if err != nil {
		t.Fatalf("run log still unreadable after repair: %v", err)
	}
	if len(runs) != 1 || runs[0].At != "2026-08-09T00:00:00Z" {
		t.Fatalf("repaired log = %+v", runs)
	}
	if _, ok, err := patterncore.LastRunChecked(brainDir); err != nil || !ok {
		t.Fatalf("patterns status must work after repair: ok=%v err=%v", ok, err)
	}
}

// TestJobInventoryScanCompleteFalseWhenDirectoryUnreadable locks the honesty of
// the scan-complete flag. Returning early with the initialised true reported an
// inventory where NOTHING was enumerated as a complete scan, so consumers keyed
// on it concluded the queue was fully observed.
func TestJobInventoryScanCompleteFalseWhenDirectoryUnreadable(t *testing.T) {
	brainDir := t.TempDir()
	jobsDir := filepath.Join(brainDir, filepath.FromSlash(memoryJobsDirRel))
	if err := os.MkdirAll(filepath.Dir(jobsDir), 0o700); err != nil {
		t.Fatal(err)
	}
	// A regular file where the job directory belongs: readMemoryStateDirectory
	// refuses it.
	if err := os.WriteFile(jobsDir, []byte("not a directory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inventory := loadMemoryJobInventory(brainDir)
	if inventory.ScanComplete {
		t.Fatalf("an unreadable job directory must not report a complete scan: %+v", inventory)
	}
	if len(inventory.Issues) == 0 {
		t.Fatal("an unreadable job directory must record an issue")
	}
}

// TestWriteLockCheckIsOkWhenPresentUnproven locks that doctor stops crying wolf.
// The lock leaf is created with O_CREATE and never unlinked on release, so it
// exists on every healthy brain after the first refresh; warning on its presence
// warned forever and carried no information, because a genuinely held lock
// produces the identical read-only state. Liveness belongs to memory_coordinator.
func TestWriteLockCheckIsOkWhenPresentUnproven(t *testing.T) {
	snapshot := memoryReadOnlyHealthSnapshot{Payload: map[string]any{
		"locks": map[string]any{"write": map[string]any{"state": "present_unproven"}},
	}}
	var check doctorCheckResult
	for _, candidate := range memoryDoctorChecks(snapshot) {
		if candidate.Name == "write_lock" {
			check = candidate
		}
	}
	if check.Name == "" {
		t.Fatal("no write_lock check emitted")
	}
	if check.State != "ok" {
		t.Fatalf("a persistent lock leaf must not warn: %+v", check)
	}
	if !strings.Contains(check.Detail, "present_unproven") {
		t.Fatalf("the unproven state must still be named: %+v", check)
	}
	for state, want := range map[string]string{"unsafe": "error", "unavailable": "error", "absent": "ok"} {
		snapshot.Payload["locks"] = map[string]any{"write": map[string]any{"state": state}}
		found := false
		for _, candidate := range memoryDoctorChecks(snapshot) {
			if candidate.Name == "write_lock" {
				found = true
				if candidate.State != want {
					t.Fatalf("write_lock(%s) = %s, want %s", state, candidate.State, want)
				}
			}
		}
		if !found {
			t.Fatalf("write_lock check missing for state %s", state)
		}
	}
}

// A partial bundle write reports missing blobs without deleting existing files.
func TestPartialTranscriptFailureSignalsIncompleteExport(t *testing.T) {
	const (
		badID  = "aaa111aaa111"
		goodID = "bbb222bbb222"
	)
	badPath := snapshotTranscriptPath(badID, 0, v1TranscriptFileName)
	goodPath := snapshotTranscriptPath(goodID, 0, v1TranscriptFileName)
	snapshot := &checkpointSnapshot{
		TranscriptMode:     "raw",
		TranscriptFileName: v1TranscriptFileName,
		Sources: map[string]checkpointSnapshotSource{
			"bad": {
				GitDir: "/repo", Ref: v1MainRef, VirtualRoot: checkpointPath(badID), ActualRoot: checkpointPath(badID),
				TreePaths: map[string]struct{}{badPath: {}},
			},
			"good": {
				GitDir: "/repo", Ref: v1OriginRef, VirtualRoot: checkpointPath(goodID), ActualRoot: checkpointPath(goodID),
				TreePaths: map[string]struct{}{goodPath: {}},
			},
		},
	}
	created := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	sessions := []exportSession{
		{SessionID: "bad-session", Branch: "main", LatestCheckpoint: badID, SourceKey: "bad", SourceTranscriptPath: badPath, CreatedAt: created},
		{SessionID: "good-session", Branch: "main", LatestCheckpoint: goodID, SourceKey: "good", SourceTranscriptPath: goodPath, CreatedAt: created},
	}
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "cat-file", "-p", v1MainRef+":"+badPath):    {err: errors.New("missing object")},
		fakeCommandKey("git", "cat-file", "-p", v1OriginRef+":"+goodPath): {stdout: "good transcript\n"},
	}}
	branchDirs := buildBranchDirectories(sessions, "main")
	outputDir := t.TempDir()
	if err := ensureExportDirectories(outputDir, branchDirs); err != nil {
		t.Fatal(err)
	}
	// A previous export already wrote the now-unreadable session's transcript.
	priorRel := filepath.Join(branchDirs["main"], sessionFileName(created, "", "bad-session", badID, ".jsonl"))
	priorFull := filepath.Join(outputDir, priorRel)
	if err := os.MkdirAll(filepath.Dir(priorFull), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(priorFull, []byte("previously exported transcript\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	written, _, complete, err := writeSnapshotSessionTranscripts(context.Background(), runner, snapshot, outputDir, sessions, branchDirs)
	if err != nil {
		t.Fatalf("partial write: %v", err)
	}
	if len(written) != 1 {
		t.Fatalf("expected the readable session only, got %+v", written)
	}
	if complete {
		t.Fatal("a skipped transcript must report the write as incomplete")
	}
	if _, statErr := os.Stat(priorFull); statErr != nil {
		t.Fatalf("partial bundle write removed retained transcript: %v", statErr)
	}
}

// TestInspectRawScanSkipsDerivedStores locks the raw walk against DERIVED
// state: generation and staging directories hold projection copies of the same
// transcripts, so a walk that reads derived state would duplicate every hit.
func TestInspectRawScanSkipsDerivedStores(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	// Derived stores carry session content under their own filenames.
	derived := map[string]string{
		"history/short-term.json": `{"version":1,"files":{"x":{"records":[{"summary":"` + privacyCanary + `"}]}}}`,
		"facts/main.ndjson":       `{"id":"f1","text":"` + privacyCanary + `"}`,
		"patterns/dossiers.json":  `[{"title":"` + privacyCanary + `"}]`,
		"seed/seed.md":            "seed mentions " + privacyCanary,
		"export/cursor.json":      `{"note":"` + privacyCanary + `"}`,
		"history/index.json":      `{"records":[{"summary":"` + privacyCanary + `"}]}`,
		"semantic/snapshot.jsonl": `{"name":"` + privacyCanary + `"}`,
	}
	for rel, body := range derived {
		full := filepath.Join(brainDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for name, scan := range map[string]func() ([]brainTextMatch, error){
		"inspect": func() ([]brainTextMatch, error) {
			report, err := inspectBrainRawText(brainDir, "all", privacyCanary, 50)
			return report.Matches, err
		},
	} {
		t.Run(name, func(t *testing.T) {
			matches, err := scan()
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			for _, match := range matches {
				if strings.Contains(match.Excerpt, privacyCanary) && !strings.HasPrefix(filepath.ToSlash(match.Path), "sessions/") {
					t.Fatalf("content surfaced from derived store %s: %+v", match.Path, match)
				}
			}
		})
	}
}

// TestDocumentExpansionSeparatesReadFailureFromSizeBound locks the three
// distinct expansion states apart. Wrapping every safeReadAll failure as
// "too large" reported an I/O error or a cancelled context as a size-bound
// violation, which is the one thing this surface promises not to do.
func TestDocumentExpansionSeparatesReadFailureFromSizeBound(t *testing.T) {
	// A genuine bound violation is still reported as one.
	if _, err := base.SafeReadAll(strings.NewReader(strings.Repeat("x", 64)), 8, "probe"); err == nil {
		t.Fatal("oversized read must fail")
	} else {
		var bound *readBoundExceededError
		if !errors.As(err, &bound) {
			t.Fatalf("size overflow = %v, want a typed bound error", err)
		}
		if !strings.Contains(err.Error(), "exceeds maximum size of 8 bytes") {
			t.Fatalf("bound message changed: %v", err)
		}
	}
	// An I/O failure must NOT be classified as a bound violation.
	injected := errors.New("injected device failure")
	_, err := base.SafeReadAll(iotest.ErrReader(injected), 1<<20, "probe")
	if !errors.Is(err, injected) {
		t.Fatalf("read failure = %v, want the underlying error", err)
	}
	var bound *readBoundExceededError
	if errors.As(err, &bound) {
		t.Fatalf("read failure was classified as a size-bound violation: %v", err)
	}
	if errors.Is(err, errConversationSourceTooLarge) {
		t.Fatalf("read failure reported as conversation-source-too-large: %v", err)
	}
}

// TestLineTranscriptExpansionIsNotBoundedByTheDocumentCap locks the streaming
// contract for LINE transcripts. A size gate applied before dialect detection
// rejected any transcript over the document ceiling as "document too large",
// which is precisely the case this surface is supposed to stream: the digest
// covers the whole file, but only the requested range is materialized.
func TestLineTranscriptExpansionIsNotBoundedByTheDocumentCap(t *testing.T) {
	brainDir := t.TempDir()
	rel := "sessions/main/20260808T000000Z_line.jsonl"
	full := filepath.Join(brainDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	var body strings.Builder
	body.WriteString(`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"why did the export fail"}]}}` + "\n")
	body.WriteString(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Decision: the cursor skipped it."}]}}` + "\n")
	// Bulk well past the document ceiling, in lines that are individually small.
	for i := 0; i < 400; i++ {
		body.WriteString(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"` + strings.Repeat("filler ", 40) + `"}]}}` + "\n")
	}
	raw := []byte(body.String())
	if err := os.WriteFile(full, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	record := historyRecord{
		ID: conversationIDPrefix + "line", Kind: conversationKind, Path: rel,
		Line: 1, EndLine: 2, Summary: "stored projection", ContentRole: conversationContentRole,
		SourceDigest: conversationDigest(raw),
	}
	oldMax := base.MaxDocumentTranscriptBytes
	base.MaxDocumentTranscriptBytes = 1024 // far below the transcript size
	t.Cleanup(func() { base.MaxDocumentTranscriptBytes = oldMax })
	if int64(len(raw)) <= base.MaxDocumentTranscriptBytes {
		t.Fatalf("fixture invalid: transcript must exceed the document cap (%d bytes)", len(raw))
	}

	expansion, err := expandConversationExchange(brainDir, record)
	if err != nil {
		t.Fatalf("line transcript over the document cap must still stream: %v", err)
	}
	if !strings.Contains(expansion.Request+expansion.Response, "why did the export fail") {
		t.Fatalf("expansion lost the requested range: %+v", expansion)
	}
}

// TestDeletedTranscriptReportsStaleNotUnreadable locks the classification of a
// vanished transcript. The component walk reports a bare os.ErrNotExist, but a
// delete racing the open surfaces the same ENOENT wrapped in a typed inventory
// error; os.IsNotExist cannot see through that, so the reader was told the
// transcript was unreadable rather than stale.
func TestDeletedTranscriptReportsStaleNotUnreadable(t *testing.T) {
	brainDir := t.TempDir()
	rel := "sessions/main/20260808T000000Z_gone.jsonl"
	full := filepath.Join(brainDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"why did the export fail"}]}}` + "\n")
	if err := os.WriteFile(full, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	record := historyRecord{
		ID: conversationIDPrefix + "gone", Kind: conversationKind, Path: rel,
		Line: 1, EndLine: 1, Summary: "stored projection", ContentRole: conversationContentRole,
		SourceDigest: conversationDigest(raw),
	}
	if err := os.Remove(full); err != nil {
		t.Fatal(err)
	}
	_, err := expandConversationExchange(brainDir, record)
	if !errors.Is(err, errConversationSourceStale) {
		t.Fatalf("deleted transcript = %v, want the stale classification", err)
	}
	result := conversationGetResult(brainDir, record)
	if !hasCaveatKind(result, retrieval.CaveatConversationSourceStale) {
		t.Fatalf("deleted transcript caveats = %+v, want stale", result.Caveats)
	}
	if hasCaveatKind(result, retrieval.CaveatConversationSourceUnreadable) {
		t.Fatalf("deleted transcript reported unreadable: %+v", result.Caveats)
	}
}

// TestRawScansSkipLeftoverSessionFiles pins the catalog bound on every raw
// walk: a sessions-tree file the manifest does not list (a deleted
// checkpoint's cached transcript awaiting the sweep, which daemon ticks skip)
// must not be served, and an unreadable manifest fails the scan instead of
// scanning unbounded.
func TestRawScansSkipLeftoverSessionFiles(t *testing.T) {
	const leftoverCanary = "LEFTOVER-RAW-SCAN-CANARY"
	brainDir := writePrivacyFixture(t)
	leftover := filepath.Join(brainDir, "sessions", "main", "19990101T000000Z_leftover.jsonl")
	if err := os.WriteFile(leftover, []byte(`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"`+leftoverCanary+`"}]}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	scans := map[string]func() ([]brainTextMatch, error){
		"inspect": func() ([]brainTextMatch, error) {
			report, err := inspectBrainRawText(brainDir, "all", leftoverCanary, 50)
			return report.Matches, err
		},
	}
	for name, scan := range scans {
		t.Run(name, func(t *testing.T) {
			matches, err := scan()
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			for _, match := range matches {
				if strings.Contains(match.Excerpt, leftoverCanary) {
					t.Fatalf("unlisted session file served by %s: %+v", name, match)
				}
			}
		})
	}
	// A listed transcript still matches, so the skip is the catalog bound and
	// not an empty scan.
	report, err := inspectBrainRawText(brainDir, "all", privacyCanary, 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, match := range report.Matches {
		if strings.Contains(match.Excerpt, privacyCanary) {
			found = true
		}
	}
	if !found {
		t.Fatal("listed transcript stopped matching; the catalog bound is too wide")
	}
	// The bound is manifest-derived, so an unreadable manifest fails the scan.
	if err := os.WriteFile(filepath.Join(brainDir, bmanifest.FileName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, scan := range scans {
		if matches, err := scan(); err == nil {
			t.Fatalf("%s scanned without a readable catalog: %d matches", name, len(matches))
		}
	}
}
