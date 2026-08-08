package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// shortTermFixture builds a brain with one indexed session (full build), then
// appends a new turn to it and adds a brand-new session WITHOUT re-running the
// full build — the exact "in-flight work" state the short-term path serves.
func shortTermFixture(t *testing.T) (brainDir string, changedRel, newRel string) {
	t.Helper()
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	brainDir = t.TempDir()
	changedRel = "sessions/main/20260807T100000Z_first.jsonl"
	base := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Fix the flaky lock test"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Decision: raised the lock timeout on Windows."}]}}
`
	full := filepath.Join(brainDir, filepath.FromSlash(changedRel))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion, GeneratedAt: now, RepoKey: "test/stm", DefaultBranch: "main",
		Sources: &brainSources{Sessions: &sessionSourceManifest{GeneratedAt: now, DefaultBranch: "main", Sessions: []exportSession{
			{SessionID: "sess-live", Branch: "main", Agent: "claude", LatestCheckpoint: "cp1", TranscriptPath: changedRel, CreatedAt: now.Add(-time.Hour)},
		}}},
	}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := writeBrainHistoryIndexAndSource(brainDir, now, nil); err != nil {
		t.Fatal(err)
	}

	// In-flight work AFTER the full build: the live session gains a turn about
	// vector store corruption, and a second terminal starts a new session.
	appended := base + `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Now debug the corrupted vector store checksum"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Decision: the checksum mismatch came from a truncated mmap write; guarding with a full-file digest."}]}}
`
	if err := os.WriteFile(full, []byte(appended), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(full, future, future); err != nil {
		t.Fatal(err)
	}
	newRel = "sessions/main/20260807T113000Z_second.jsonl"
	second := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Parallel terminal: profile the exporter hot loop"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Decision: the exporter hot loop spends 80 percent in gzip; switching to level 1."}]}}
`
	newFull := filepath.Join(brainDir, filepath.FromSlash(newRel))
	if err := os.WriteFile(newFull, []byte(second), 0o600); err != nil {
		t.Fatal(err)
	}
	onDisk, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	onDisk.Sources.Sessions.Sessions = append(onDisk.Sources.Sessions.Sessions, exportSession{
		SessionID: "sess-parallel", Branch: "main", Agent: "claude", LatestCheckpoint: "cp2",
		TranscriptPath: newRel, CreatedAt: now.Add(-10 * time.Minute),
	})
	if err := writeBrainManifestAndReadme(brainDir, *onDisk); err != nil {
		t.Fatal(err)
	}
	return brainDir, changedRel, newRel
}

func buildShortTerm(t *testing.T, brainDir string) shortTermStats {
	t.Helper()
	var stats shortTermStats
	if err := withBrainWriteLock(brainDir, func() error {
		var err error
		stats, err = buildHistoryShortTermLocked(brainDir, time.Date(2026, 8, 7, 13, 0, 0, 0, time.UTC))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return stats
}

func TestShortTermDeltaIndexesOnlyChangedTranscripts(t *testing.T) {
	brainDir, changedRel, newRel := shortTermFixture(t)
	stats := buildShortTerm(t, brainDir)
	if stats.Files != 2 || stats.Scanned != 2 || stats.Reused != 0 {
		t.Fatalf("stats = %+v, want 2 files scanned", stats)
	}
	if stats.Exchanges < 3 {
		t.Fatalf("exchanges = %d, want >= 3 (two from the changed session, one from the new)", stats.Exchanges)
	}
	manifest, _ := loadBrainManifest(brainDir)
	overlay := loadHistoryShortTerm(brainDir, manifest.Sources.History)
	if len(overlay.Files) != 2 {
		t.Fatalf("overlay files = %v", len(overlay.Files))
	}
	if _, ok := overlay.Files[changedRel]; !ok {
		t.Fatalf("changed transcript missing from overlay: %v", overlay.Files)
	}
	if _, ok := overlay.Files[newRel]; !ok {
		t.Fatalf("new transcript missing from overlay: %v", overlay.Files)
	}

	// A second delta with nothing further changed carries everything over
	// without re-scanning.
	again := buildShortTerm(t, brainDir)
	if again.Scanned != 0 || again.Reused != 2 {
		t.Fatalf("second delta = %+v, want pure carry-over", again)
	}
}

func TestShortTermRecordsAreRetrievableAndGettable(t *testing.T) {
	brainDir, changedRel, _ := shortTermFixture(t)

	// Before the delta: the in-flight turn is invisible.
	before, err := retrieveConversation(brainDir, "corrupted vector store checksum", 10, modeLexical, retrievalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range before {
		if strings.Contains(result.Text, "checksum mismatch") {
			t.Fatalf("fresh turn visible before delta: %+v", result)
		}
	}

	buildShortTerm(t, brainDir)

	// Conversation source: the fresh exchange from the changed session and the
	// parallel session's exchange are both recallable.
	after, err := retrieveConversation(brainDir, "corrupted vector store checksum", 10, modeLexical, retrievalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var freshID string
	for _, result := range after {
		if strings.Contains(result.Text, "checksum mismatch came from a truncated mmap write") {
			freshID = result.ID
		}
	}
	if freshID == "" {
		t.Fatalf("fresh exchange not retrievable after delta: %+v", after)
	}
	parallel, err := retrieveConversation(brainDir, "profile the exporter hot loop", 10, modeLexical, retrievalOptions{})
	if err != nil || len(parallel) == 0 {
		t.Fatalf("parallel session not retrievable: %v (%d results)", err, len(parallel))
	}
	if parallel[0].SessionID != "sess-parallel" {
		t.Fatalf("parallel result identity: %+v", parallel[0])
	}

	// General history arm sees the fresh decision too.
	unified, err := retrieveUnifiedWithOptions("", brainDir, "main", "exporter hot loop gzip", 10, modeLexical, retrievalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	foundDecision := false
	for _, result := range unified {
		if result.Source == "history" && strings.Contains(result.Text, "gzip") {
			foundDecision = true
		}
		if result.Source == retrievalSourceConversation {
			t.Fatalf("exchange leaked into default retrieval via the overlay: %+v", result)
		}
	}
	if !foundDecision {
		t.Fatalf("fresh decision not in default history arm: %+v", unified)
	}

	// get: the fresh exchange id expands to the bounded full exchange, with the
	// source digest matching the CHANGED file.
	found, missing, err := getUnifiedBatch("", brainDir, "main", []string{freshID})
	if err != nil || len(missing) != 0 || len(found) != 1 {
		t.Fatalf("get: err=%v missing=%v", err, missing)
	}
	if !strings.Contains(found[0].Text, "guarding with a full-file digest") {
		t.Fatalf("expansion text = %q", found[0].Text)
	}
	if found[0].Path != changedRel {
		t.Fatalf("expansion path = %q", found[0].Path)
	}
}

func TestShortTermSupersedesLongTermRecordsOfChangedFiles(t *testing.T) {
	brainDir, changedRel, _ := shortTermFixture(t)
	buildShortTerm(t, brainDir)
	manifest, _ := loadBrainManifest(brainDir)
	fresh, err := loadFreshHistory(brainDir, manifest.Sources.History)
	if err != nil {
		t.Fatal(err)
	}
	// No record for the changed file may come from the long-term tier: the
	// overlay's re-scan replaces the whole file, exactly as a rebuild would.
	merged := fresh.mergedRecords()
	overlayIDs := map[string]bool{}
	for _, record := range fresh.overlay {
		overlayIDs[record.ID] = true
	}
	for _, record := range merged {
		if record.Path == changedRel && !overlayIDs[record.ID] {
			t.Fatalf("stale long-term record for changed file survived the merge: %+v", record)
		}
	}
	// And ranking must not duplicate: the original decision exists in both
	// tiers (same content), but only the overlay copy may surface.
	scored := rankFreshHistory(fresh, "history", "lock timeout windows", 20, func(longTerm historyIndex) ([]scoredHistoryRecord, bool) {
		return rankHistoryViaFTS(brainDir, longTerm, "history", "lock timeout windows", 20)
	})
	seen := map[string]int{}
	for _, s := range scored {
		seen[s.Record.ID]++
		if s.Record.ID != "" && seen[s.Record.ID] > 1 {
			t.Fatalf("duplicate record across tiers: %s", s.Record.ID)
		}
	}
}

func TestShortTermLexicalOverlayPreservesDirectFTSPayloadPath(t *testing.T) {
	brainDir, _, _ := shortTermFixture(t)
	buildShortTerm(t, brainDir)
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	// The direct FTS payload path must not need index.json even when a short-term
	// overlay is present. Corrupt the truth file after the FTS generation and
	// require both the direct access mode and the fresh overlay result.
	indexPath := filepath.Join(brainDir, filepath.FromSlash(historyIndexPath))
	corrupt, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	corrupt[0] = '!'
	if err := os.WriteFile(indexPath, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	scored, access, _, err := rankFreshHistoryLexicalFromSource(
		brainDir,
		manifest.Sources.History,
		"history",
		"exporter hot loop gzip",
		20,
	)
	if err != nil {
		t.Fatal(err)
	}
	if access != historyIndexAccessFTSPayload {
		t.Fatalf("history access = %q, want %q", access, historyIndexAccessFTSPayload)
	}
	foundFresh := false
	for _, result := range scored {
		if strings.Contains(result.Record.Summary, "gzip") {
			foundFresh = true
			break
		}
	}
	if !foundFresh {
		t.Fatalf("short-term result missing from direct FTS merge: %+v", scored)
	}
}

func TestShortTermConsolidationClearsOverlayAndPreservesRecall(t *testing.T) {
	brainDir, _, _ := shortTermFixture(t)
	buildShortTerm(t, brainDir)
	if _, err := os.Stat(filepath.Join(brainDir, filepath.FromSlash(historyShortTermPath))); err != nil {
		t.Fatalf("overlay must exist before consolidation: %v", err)
	}

	// Consolidation: the full build absorbs the changed transcripts and clears
	// the short-term buffer.
	if _, err := writeBrainHistoryIndexAndSource(brainDir, time.Date(2026, 8, 7, 14, 0, 0, 0, time.UTC), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(brainDir, filepath.FromSlash(historyShortTermPath))); !os.IsNotExist(err) {
		t.Fatalf("overlay must be cleared by consolidation: %v", err)
	}
	// Everything the overlay held is now long-term recallable.
	results, err := retrieveConversation(brainDir, "corrupted vector store checksum", 10, modeLexical, retrievalOptions{})
	if err != nil || len(results) == 0 {
		t.Fatalf("post-consolidation recall failed: %v (%d)", err, len(results))
	}
}

func TestShortTermEmptyOverlayPreservesRankingExactly(t *testing.T) {
	brainDir, _, _ := shortTermFixture(t)
	// NO delta ran: rankFreshHistory must return the long-term ranking
	// bit-for-bit (scores included).
	manifest, _ := loadBrainManifest(brainDir)
	fresh, err := loadFreshHistory(brainDir, manifest.Sources.History)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh.overlay) != 0 {
		t.Fatalf("no delta ran; overlay must be empty: %d", len(fresh.overlay))
	}
	direct, ok := rankHistoryViaFTS(brainDir, fresh.index, "history", "flaky lock test", 10)
	if !ok {
		t.Fatal("fts unavailable")
	}
	wrapped := rankFreshHistory(fresh, "history", "flaky lock test", 10, func(longTerm historyIndex) ([]scoredHistoryRecord, bool) {
		return rankHistoryViaFTS(brainDir, longTerm, "history", "flaky lock test", 10)
	})
	if len(direct) != len(wrapped) {
		t.Fatalf("length drift: %d vs %d", len(direct), len(wrapped))
	}
	for i := range direct {
		if direct[i].Record.ID != wrapped[i].Record.ID || direct[i].Score != wrapped[i].Score {
			t.Fatalf("ranking drift at %d: %+v vs %+v", i, direct[i], wrapped[i])
		}
	}
}

func TestShortTermStaleBasePinIsIgnored(t *testing.T) {
	brainDir, _, _ := shortTermFixture(t)
	buildShortTerm(t, brainDir)
	manifest, _ := loadBrainManifest(brainDir)
	if got := loadHistoryShortTerm(brainDir, manifest.Sources.History); len(got.Files) == 0 {
		t.Fatal("precondition: overlay valid")
	}
	// A different long-term build time voids the overlay (belt-and-braces for
	// a consolidation that somehow failed to clear it).
	stale := *manifest.Sources.History
	stale.GeneratedAt = stale.GeneratedAt.Add(time.Minute)
	if got := loadHistoryShortTerm(brainDir, &stale); len(got.Files) != 0 {
		t.Fatalf("stale-pinned overlay must read as empty: %d files", len(got.Files))
	}
}

func TestShortTermHonorsTombstonesAndBuffersBound(t *testing.T) {
	brainDir, changedRel, newRel := shortTermFixture(t)
	// Tombstone the parallel session before the delta: it must not enter
	// short-term memory.
	stones := loadSessionTombstones(brainDir)
	stones.Excluded["sess-parallel"] = sessionTombstone{At: time.Date(2026, 8, 7, 12, 30, 0, 0, time.UTC)}
	if err := saveSessionTombstones(brainDir, stones); err != nil {
		t.Fatal(err)
	}
	stats := buildShortTerm(t, brainDir)
	if stats.Files != 1 {
		t.Fatalf("tombstoned session entered short-term memory: %+v", stats)
	}
	manifest, _ := loadBrainManifest(brainDir)
	overlay := loadHistoryShortTerm(brainDir, manifest.Sources.History)
	if _, ok := overlay.Files[newRel]; ok {
		t.Fatal("excluded transcript in overlay")
	}
	if _, ok := overlay.Files[changedRel]; !ok {
		t.Fatal("included transcript missing from overlay")
	}

	// Buffer bound: with a cap of 1 record, the oldest file is dropped and the
	// overlay reports truncation.
	old := historyShortTermMaxRecords
	historyShortTermMaxRecords = 1
	t.Cleanup(func() { historyShortTermMaxRecords = old })
	delete(stones.Excluded, "sess-parallel")
	if err := saveSessionTombstones(brainDir, stones); err != nil {
		t.Fatal(err)
	}
	stats = buildShortTerm(t, brainDir)
	if !stats.Truncated {
		t.Fatalf("over-cap overlay must report truncation: %+v", stats)
	}
	overlay = loadHistoryShortTerm(brainDir, manifest.Sources.History)
	if len(overlay.Files) >= 2 {
		t.Fatalf("bound not enforced: %d files", len(overlay.Files))
	}
}

func TestWatchTickRunsShortTermDeltaEveryTick(t *testing.T) {
	var out strings.Builder
	deltas := 0
	steps := watchSteps{
		now:         func() time.Time { return time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC) },
		fingerprint: func(ctxArg context.Context) string { return "same" },
		delta:       func(ctxArg context.Context) error { deltas++; return nil },
		refresh:     func(ctxArg context.Context) error { t.Fatal("refresh must not run without change"); return nil },
		seed:        func(ctxArg context.Context) error { return nil },
		distill:     func(ctxArg context.Context) error { return nil },
	}
	cursorPath := filepath.Join(t.TempDir(), "cursor.json")
	// Seed the cursor so "no change" is reachable (a zero cursor forces refresh).
	if err := saveWatchCursor(cursorPath, watchCursor{LastFingerprint: "same", LastRefreshAt: time.Date(2026, 8, 7, 11, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	watchTick(nil, &out, defaultWatchOptions(), cursorPath, steps, &calls)
	watchTick(nil, &out, defaultWatchOptions(), cursorPath, steps, &calls)
	if deltas != 2 {
		t.Fatalf("delta ran %d times over 2 ticks, want every tick", deltas)
	}
	if !strings.Contains(out.String(), "short-term memory updated") {
		t.Fatalf("tick output missing short-term line: %q", out.String())
	}
}
