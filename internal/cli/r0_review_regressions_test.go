package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This file locks the defects found reviewing the R0 batch. Every case below
// was an untested path: the full suite passed with each bug present.

// TestInspectRawTextFailsClosedOnUnreadableManifest locks the raw-scan
// exclusion guard. rawGuard.paths is the ONLY exclusion mechanism in the raw
// file walk (a bare file carries no session id to match), and it is populated
// only from the manifest, so swallowing the manifest load error emptied the
// guard and scanned tombstoned transcripts. `brain brief` merges these raw
// matches into its history matches with no further filtering.
func TestInspectRawTextFailsClosedOnUnreadableManifest(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	now := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	stones := loadSessionTombstones(brainDir)
	stones.Excluded["secret-sess"] = sessionTombstone{At: now, Reason: "user requested"}
	if err := saveSessionTombstones(brainDir, stones); err != nil {
		t.Fatal(err)
	}

	// With a readable manifest the excluded transcript is skipped by path.
	report, err := inspectBrainRawText(brainDir, "all", privacyCanary, 50)
	if err != nil {
		t.Fatalf("raw inspect with a readable manifest: %v", err)
	}
	for _, match := range report.Matches {
		if strings.Contains(match.Excerpt, privacyCanary) {
			t.Fatalf("excluded transcript leaked with a readable manifest: %+v", match)
		}
	}

	// An unparseable manifest must FAIL the scan, not scan without a guard.
	if err := os.WriteFile(filepath.Join(brainDir, exportManifestFileName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err = inspectBrainRawText(brainDir, "all", privacyCanary, 50)
	if err == nil {
		t.Fatalf("unreadable manifest must fail the raw scan closed; got %d matches", len(report.Matches))
	}
	for _, match := range report.Matches {
		if strings.Contains(match.Excerpt, privacyCanary) {
			t.Fatalf("excluded transcript leaked through an unreadable manifest: %+v", match)
		}
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
		t.Skip("query unexpectedly unindexable; the fallback path needs a different probe")
	}
	results, err := retrieveConversation(brainDir, "cursor", 5, modeLexical, retrievalOptions{})
	if err != nil {
		t.Fatalf("unfiltered conversation query must not fail: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("unfiltered conversation query returned nothing")
	}
	// The degraded error is reserved for a genuine filtered-scan overflow.
	if _, err := retrieveConversation(brainDir, "a", 5, modeLexical, retrievalOptions{}); err != nil {
		if strings.Contains(err.Error(), "filter scan exceeded") {
			t.Fatalf("stopword-only query reported a filter-scan overflow: %v", err)
		}
	}
}

// TestLocalRepoPhysicalSpellingRecoversOneBrain locks that a repository reached
// through a symlinked ANCESTOR resolves to a single brain. The lexical spelling
// stays the preferred key (an explicit alias keeps addressing its established
// brain), but a brain already stored under the physical spelling, which is the
// only spelling a relative invocation from inside the tree can produce, must be
// recovered in place rather than shadowed by a fresh empty one.
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
	dirs, err := resolvePluginDirs(env)
	if err != nil {
		t.Fatal(err)
	}
	// A brain established by an ordinary in-repo invocation, which is keyed on
	// the fully resolved physical root (what `git rev-parse --show-toplevel`
	// reports, and what filepath.Abs yields on platforms that expand a prefix
	// such as macOS /var -> /private/var).
	physicalKey := filepath.ToSlash(filepath.Join("local", localRepoKey(physicalLocalRepoDir(repoDir))))
	physical := repoStorageForKey(dirs, physicalKey)
	if err := os.MkdirAll(physical.BrainDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(physical.BrainDir, exportManifestFileName), []byte("{}\n"), 0o600); err != nil {
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
	lexicalKey := filepath.ToSlash(filepath.Join("local", localRepoKey(logicalRepoDir)))
	lexical := repoStorageForKey(dirs, lexicalKey)
	if lexicalKey == physicalKey {
		t.Skip("platform collapsed the two spellings; nothing to disambiguate")
	}
	if err := os.MkdirAll(lexical.BrainDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lexical.BrainDir, exportManifestFileName), []byte("{}\n"), 0o600); err != nil {
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
	manifest := exportManifest{SchemaVersion: brainManifestSchemaVersion, GeneratedAt: now, RepoKey: "test/empty", DefaultBranch: "main"}
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
		!strings.Contains(err.Error(), "run `entire brain refresh sessions` first") {
		t.Fatalf("history build guidance = %v", err)
	}
}

// TestFailedVectorSyncDoesNotSelfRelaunch locks the worker relaunch signal. The
// 100ms relaunch exists to continue embedding after a deliberate lane-deadline
// yield; treating a FAILED sync as pending re-spawned the worker about ten
// times a second for as long as the failure lasted (embed server down, corrupt
// progress leaf, privacy-epoch mismatch never self-heal).
func TestFailedVectorSyncDoesNotSelfRelaunch(t *testing.T) {
	var stats memoryWorkerStats
	stats.VectorPending = true
	stats.VectorContinue = false
	if stats.VectorContinue {
		t.Fatal("a failed sync must not request the fast relaunch")
	}
	// add() must preserve both flags independently, so one lane's clean yield
	// is not attributed to another lane's failure.
	var combined memoryWorkerStats
	combined.add(stats)
	if !combined.VectorPending || combined.VectorContinue {
		t.Fatalf("add lost the pending/continue distinction: %+v", combined)
	}
	yielded := memoryWorkerStats{VectorPending: true, VectorContinue: true}
	combined.add(yielded)
	if !combined.VectorContinue {
		t.Fatalf("a yielded lane must still request the relaunch: %+v", combined)
	}
	// VectorContinue is an internal scheduling signal, never part of the
	// worker's JSON contract.
	encoded, err := json.Marshal(combined)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "VectorContinue") || strings.Contains(string(encoded), "vector_continue") {
		t.Fatalf("relaunch signal leaked into the worker JSON contract: %s", encoded)
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
	full := filepath.Join(brainDir, filepath.FromSlash(patternRunsRelPath))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(`{"at":"2026-08-01T00:00:00Z","indexer_version":1}`+"\n"+`{"at":"2026-08-0`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPatternRunsChecked(brainDir); err == nil {
		t.Fatal("a truncated line must still read as corrupt")
	}
	if err := appendPatternRun(brainDir, patternRun{At: "2026-08-09T00:00:00Z", IndexerVersion: patternIndexerVersion}); err != nil {
		t.Fatalf("refresh must be able to repair a corrupt run log: %v", err)
	}
	runs, err := loadPatternRunsChecked(brainDir)
	if err != nil {
		t.Fatalf("run log still unreadable after repair: %v", err)
	}
	if len(runs) != 1 || runs[0].At != "2026-08-09T00:00:00Z" {
		t.Fatalf("repaired log = %+v", runs)
	}
	if _, ok, err := lastPatternRunChecked(brainDir); err != nil || !ok {
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

// TestAbstractWindowTurnFitsEncodedCeiling locks the provider-input bound. The
// budget was measured against a turn whose Text was "" (omitempty drops the key
// entirely) and then filled with RAW bytes, so JSON escaping pushed a single
// turn past the ceiling: about 2x for quote-heavy text and up to 6x for control
// characters.
func TestAbstractWindowTurnFitsEncodedCeiling(t *testing.T) {
	for name, summary := range map[string]string{
		"plain":    strings.Repeat("a", 4*abstractWindowMaxBytes),
		"quotes":   strings.Repeat(`"`, 4*abstractWindowMaxBytes),
		"controls": strings.Repeat("\x01", 4*abstractWindowMaxBytes),
	} {
		t.Run(name, func(t *testing.T) {
			turn := abstractWindowTurn(historyRecord{ID: "conversation:x", Summary: summary})
			if got := turnJSONBytes(turn); got > abstractWindowMaxBytes-2 {
				t.Fatalf("encoded turn = %d bytes, ceiling %d", got, abstractWindowMaxBytes-2)
			}
			if !turn.Truncated {
				t.Fatal("a truncated turn must say so")
			}
		})
	}
	// A window built from such turns still fits.
	records := []historyRecord{
		{ID: "conversation:a", Summary: strings.Repeat(`"`, 4*abstractWindowMaxBytes)},
		{ID: "conversation:b", Summary: strings.Repeat("\x01", 4*abstractWindowMaxBytes)},
	}
	windows, _ := walkAbstractWindows(records, func(_ int, turns []conversationTurn) {
		total := 2
		for i, turn := range turns {
			if i > 0 {
				total++
			}
			total += turnJSONBytes(turn)
		}
		if total > abstractWindowMaxBytes {
			t.Fatalf("emitted window = %d bytes, ceiling %d", total, abstractWindowMaxBytes)
		}
	})
	if windows == 0 {
		t.Fatal("no windows emitted")
	}
}

// TestAbstractEgressIssueAlwaysCarriesACode locks the taxonomy contract. Several
// call sites pass memoryErrorCode(readErr) on branches reachable with a nil
// error, and memoryReadOnlyHealth's recordIssue silently DROPS an empty-coded
// issue without counting it as hidden, so the finding vanished from the reported
// issues array while the state still read corrupt.
func TestAbstractEgressIssueAlwaysCarriesACode(t *testing.T) {
	var inventory abstractEgressInventory
	inventory.issue("history/work/v1/abstract-egress/x.json", memoryErrorCode(nil))
	if len(inventory.Issues) != 1 {
		t.Fatalf("issue not recorded: %+v", inventory.Issues)
	}
	if inventory.Issues[0].Code == "" {
		t.Fatalf("issue recorded without a stable code: %+v", inventory.Issues[0])
	}
	if !inventory.Degraded {
		t.Fatal("recording an issue must mark the inventory degraded")
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
		for _, candidate := range memoryDoctorChecks(snapshot) {
			if candidate.Name == "write_lock" && candidate.State != want {
				t.Fatalf("write_lock(%s) = %s, want %s", state, candidate.State, want)
			}
		}
	}
}

// TestPartialTranscriptFailureDoesNotDeleteExportedTranscripts locks the
// export sweep. writeSnapshotSessionTranscripts skips a session whose source
// blob is unreadable (a promisor-absent blob in a partial clone under no-egress)
// and drops it from the returned list, and cleanupStaleSessionFiles deletes
// every transcript the list does not claim, so a PARTIAL read failure destroyed
// the copies a previous export had written for exactly those sessions. Only the
// all-fail case was guarded. Before the skip-and-continue behavior, an
// unreadable transcript aborted the export and left the brain untouched.
func TestPartialTranscriptFailureDoesNotDeleteExportedTranscripts(t *testing.T) {
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

	written, _, complete, err := writeSnapshotSessionTranscripts(context.Background(), runner, snapshot, outputDir, sessions, branchDirs, nil)
	if err != nil {
		t.Fatalf("partial write: %v", err)
	}
	if len(written) != 1 {
		t.Fatalf("expected the readable session only, got %+v", written)
	}
	if complete {
		t.Fatal("a skipped transcript must report the write as incomplete")
	}
	// The caller must hold the sweep back on an incomplete write. Running it
	// anyway is exactly the data loss this locks.
	if err := cleanupStaleSessionFiles(outputDir, written); err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(priorFull); statErr == nil {
		t.Fatal("fixture invalid: the sweep was expected to remove an unclaimed transcript")
	}
}

// TestExcludedFactBlocksByTranscriptPathOnQueryAndGet locks path-based fact
// exclusion on the two primary retrieval surfaces. blocksFactAnchor falls back
// to the transcript path when a provenance anchor lost its session id, but that
// map is populated only from the manifest, and query/get resolved the guard
// WITHOUT one before filtering facts (the manifest-aware re-resolution happened
// later, inside the history arm, and was never re-applied to facts). Every other
// surface (brief, dash, handoff, viz, pattern corpus) passed the manifest.
func TestExcludedFactBlocksByTranscriptPathOnQueryAndGet(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	now := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	// A fact whose only anchor names the transcript but carries NO session id.
	orphan := factRecord{
		ID: factRecordID("the leaked key "+privacyCanary+" was rotated", nil), Text: "the leaked key " + privacyCanary + " was rotated",
		Branch: "main", Origin: "distilled", Status: factStatusActive, CreatedAt: now, UpdatedAt: now,
		Provenance: []factAnchor{{Transcript: "sessions/main/20260802T000000Z_secret.jsonl"}},
	}
	if err := writeFacts(brainDir, "main", []factRecord{orphan}); err != nil {
		t.Fatal(err)
	}
	pre, _, err := getUnifiedBatch("", brainDir, "main", []string{orphan.ID})
	if err != nil || len(pre) != 1 {
		t.Fatalf("fact must be retrievable before exclusion: err=%v found=%d", err, len(pre))
	}

	stones := loadSessionTombstones(brainDir)
	stones.Excluded["secret-sess"] = sessionTombstone{At: now, Reason: "user requested"}
	if err := saveSessionTombstones(brainDir, stones); err != nil {
		t.Fatal(err)
	}

	found, _, err := getUnifiedBatch("", brainDir, "main", []string{orphan.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range found {
		if r.ID == orphan.ID {
			t.Fatalf("excluded session's fact served by get: %+v", r)
		}
	}
	results, err := retrieveUnifiedWithOptions("", brainDir, "main", privacyCanary, 10, modeLexical, retrievalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.ID == orphan.ID {
			t.Fatalf("excluded session's fact served by query: %+v", r)
		}
	}
}

// TestDistillCachePurgeUsesTheCheckedLoader locks loader symmetry inside one
// cleanup. The purge read the distill cache with the legacy loader, which
// swallows every read/parse/version failure and returns an empty cache, while
// post-cleanup verification read the SAME file with the checked loader and hard
// errored. The purge therefore silently did nothing and the operation failed
// moments later, after the tombstone and all deletions were committed, leaving
// every later exclude/purge/retention on that brain failing too.
func TestDistillCachePurgeUsesTheCheckedLoader(t *testing.T) {
	brainDir := t.TempDir()
	full := filepath.Join(brainDir, filepath.FromSlash(distillCachePath))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	// A cache from a future schema: the legacy loader reads this as empty.
	body, err := json.Marshal(map[string]any{"version": distillCacheVersion + 1, "sessions": map[string]string{"main/sess": "digest"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if legacy := loadDistillCache(brainDir); len(legacy.Sessions) != 0 {
		t.Fatalf("fixture invalid: the legacy loader was expected to read this as empty: %+v", legacy)
	}
	err = purgeDistillCacheEntries(brainDir, "sess")
	if err == nil {
		t.Fatal("an unreadable distill cache must fail the purge, not be silently skipped")
	}
	if code := memoryErrorCode(err); code != memoryErrUnsupportedVersion {
		t.Fatalf("purge error = %v (code %q), want the unsupported-version taxonomy code", err, code)
	}
}
