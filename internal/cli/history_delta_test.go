package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func replaceDeltaTranscriptSameMetadata(t *testing.T, path, oldText, newText string) {
	t.Helper()
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	replaced := bytes.Replace(body, []byte(oldText), []byte(newText), 1)
	if bytes.Equal(replaced, body) || len(replaced) != len(body) {
		t.Fatalf("same-metadata replacement fixture invalid: old=%q new=%q bytes=%d/%d", oldText, newText, len(body), len(replaced))
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, replaced, before.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) || os.SameFile(before, after) {
		t.Fatalf("replacement did not preserve size+mtime while changing membership: before=%+v after=%+v", before, after)
	}
}

func buildShortTermResult(t *testing.T, brainDir string) (shortTermStats, error) {
	t.Helper()
	var stats shortTermStats
	err := withBrainWriteLock(brainDir, func() error {
		var buildErr error
		stats, buildErr = buildHistoryShortTermLocked(brainDir, time.Now().UTC())
		return buildErr
	})
	return stats, err
}

// shortTermFixture builds a brain with one indexed session (full build), then
// appends a new turn to it and adds a brand-new session WITHOUT re-running the
// full build; the exact "in-flight work" state the short-term path serves.
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

func TestShortTermDeltaRejectsLongTermCacheOnSameMetadataReplacement(t *testing.T) {
	brainDir, rel, _ := historyProjectionFixture(t)
	target := filepath.Join(brainDir, filepath.FromSlash(rel))
	replaceDeltaTranscriptSameMetadata(t, target, "commit the manifest last", "commit the content first")

	stats, err := buildShortTermResult(t, brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Scanned != 1 || stats.Reused != 0 {
		t.Fatalf("same-metadata long-term replacement stats = %+v, want one scan", stats)
	}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	overlay := loadHistoryShortTerm(brainDir, manifest.Sources.History)
	entry, ok := overlay.Files[rel]
	if !ok || entry.ContentDigest == "" {
		t.Fatalf("replacement missing from digest-bound overlay: %+v", overlay.Files)
	}
	joined := ""
	for _, record := range entry.Records {
		joined += record.Summary + "\n"
	}
	if !strings.Contains(joined, "content first") || strings.Contains(joined, "manifest last") {
		t.Fatalf("overlay retained stale long-term records: %q", joined)
	}
}

func TestShortTermDeltaRejectsPriorOverlayOnSameMetadataReplacement(t *testing.T) {
	brainDir, rel, _ := historyProjectionFixture(t)
	target := filepath.Join(brainDir, filepath.FromSlash(rel))
	replaceDeltaTranscriptSameMetadata(t, target, "commit the manifest last", "commit the content first")
	firstStats, err := buildShortTermResult(t, brainDir)
	if err != nil || firstStats.Scanned != 1 {
		t.Fatalf("first delta stats=%+v err=%v", firstStats, err)
	}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	first := loadHistoryShortTerm(brainDir, manifest.Sources.History).Files[rel]
	if first.ContentDigest == "" {
		t.Fatal("first overlay omitted content digest")
	}

	replaceDeltaTranscriptSameMetadata(t, target, "commit the content first", "commit the records first")
	secondStats, err := buildShortTermResult(t, brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if secondStats.Scanned != 1 || secondStats.Reused != 0 {
		t.Fatalf("same-metadata prior-overlay replacement stats = %+v, want rescan", secondStats)
	}
	second := loadHistoryShortTerm(brainDir, manifest.Sources.History).Files[rel]
	if second.ContentDigest == "" || second.ContentDigest == first.ContentDigest {
		t.Fatalf("overlay digest did not change: first=%q second=%q", first.ContentDigest, second.ContentDigest)
	}
	joined := ""
	for _, record := range second.Records {
		joined += record.Summary + "\n"
	}
	if !strings.Contains(joined, "records first") || strings.Contains(joined, "content first") {
		t.Fatalf("prior overlay records were reused after content replacement: %q", joined)
	}
}

func TestShortTermDeltaBoundFailurePreservesPriorOverlay(t *testing.T) {
	brainDir, rel, _ := historyProjectionFixture(t)
	target := filepath.Join(brainDir, filepath.FromSlash(rel))
	replaceDeltaTranscriptSameMetadata(t, target, "commit the manifest last", "commit the content first")
	if _, err := buildShortTermResult(t, brainDir); err != nil {
		t.Fatal(err)
	}
	overlayPath := filepath.Join(brainDir, filepath.FromSlash(historyShortTermPath))
	before, err := os.ReadFile(overlayPath)
	if err != nil {
		t.Fatal(err)
	}
	oldMax := maxDocumentTranscriptBytes
	maxDocumentTranscriptBytes = 8
	t.Cleanup(func() { maxDocumentTranscriptBytes = oldMax })
	if _, err := buildShortTermResult(t, brainDir); err == nil || !strings.Contains(err.Error(), memoryErrInputTooLarge) {
		t.Fatalf("bounded delta error = %v", err)
	}
	after, err := os.ReadFile(overlayPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("bounded delta failure changed the prior overlay")
	}
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
	scored := rankFreshHistory(fresh, "history", "lock timeout windows", 20, nil, func(longTerm historyIndex) ([]scoredHistoryRecord, bool) {
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
	// Warm the derived payload store from the intact truth first: the direct
	// path is an accelerator over an ALREADY-BUILT store, so building it is a
	// precondition of the property under test, not part of it.
	if _, _, _, warmErr := rankFreshHistoryLexicalFromSource(brainDir, manifest.Sources.History, "history", "exporter hot loop gzip", 20); warmErr != nil {
		t.Fatalf("warm derived payload store: %v", warmErr)
	}
	// The direct FTS payload path must not need index.json even when a short-term
	// overlay is present. Corrupt the truth file after the FTS generation and
	// require both the direct access mode and the fresh overlay result.
	// The published index is generation-addressed, so resolve it from the
	// manifest rather than assuming the legacy fixed path.
	indexPath := filepath.Join(brainDir, filepath.FromSlash(manifest.Sources.History.IndexPath))
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
	wrapped := rankFreshHistory(fresh, "history", "flaky lock test", 10, nil, func(longTerm historyIndex) ([]scoredHistoryRecord, bool) {
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
		delta: func(ctxArg context.Context) (shortTermStats, error) {
			deltas++
			return shortTermStats{Records: 1, Files: 1}, nil
		},
		refresh: func(ctxArg context.Context) error { t.Fatal("refresh must not run without change"); return nil },
		seed:    func(ctxArg context.Context) error { return nil },
		distill: func(ctxArg context.Context) error { return nil },
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

// TestWatchTickFailedDeltaRepairIsBounded locks the repair path against
// unbounded thrash. A failing delta must still escalate to the full refresh
// (that is the freshness repair path), but the delta and the full refresh
// contend for the same brain write lock, so a PERSISTENTLY failing delta must
// not fire the heavy refresh on every tick: consecutive repairs back off from
// one tick up to --consolidate-every. The first healthy delta clears the
// backoff.
func TestWatchTickFailedDeltaRepairIsBounded(t *testing.T) {
	start := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	options := defaultWatchOptions() // interval 5m, consolidateEvery 30m
	clock := start
	refreshes := 0
	tick := 0
	deltaFails := true
	steps := watchSteps{
		now:         func() time.Time { return clock },
		fingerprint: func(ctxArg context.Context) string { return fmt.Sprintf("fp-%d", tick) },
		delta: func(ctxArg context.Context) (shortTermStats, error) {
			if deltaFails {
				return shortTermStats{}, errors.New("brain write lock busy")
			}
			return shortTermStats{Records: 1, Files: 1}, nil
		},
		refresh: func(ctxArg context.Context) error { refreshes++; return nil },
		seed:    func(ctxArg context.Context) error { return nil },
		distill: func(ctxArg context.Context) error { return nil },
	}
	cursorPath := filepath.Join(t.TempDir(), "cursor.json")
	if err := saveWatchCursor(cursorPath, watchCursor{LastFingerprint: "seed", LastRefreshAt: start.Add(-6 * time.Minute)}); err != nil {
		t.Fatal(err)
	}

	// 20 ticks of simulated wall clock with the delta failing every time and the
	// fingerprint flipping every tick (an active agent session).
	var out strings.Builder
	calls := 0
	const ticks = 20
	for ; tick < ticks; tick++ {
		watchTick(nil, &out, options, cursorPath, steps, &calls)
		clock = clock.Add(options.interval)
	}
	// Pre-fix this was one full refresh per tick. The backoff must cut it to a
	// small fraction; the exact schedule is 5m/10m/20m/30m/30m over 100 minutes.
	if refreshes >= ticks {
		t.Fatalf("failed delta must not consolidate every tick: refreshes=%d over %d ticks\n%s", refreshes, ticks, out.String())
	}
	if refreshes > 7 {
		t.Fatalf("repair backoff too loose: refreshes=%d over %d ticks\n%s", refreshes, ticks, out.String())
	}
	// It must still repair; deferring forever would leave freshness broken.
	if refreshes == 0 {
		t.Fatalf("failed delta must still escalate to a repair refresh: out=%q", out.String())
	}
	if !strings.Contains(out.String(), "repair refresh backing off") {
		t.Fatalf("deferral must name the repair backoff: %q", out.String())
	}
	cursor := loadWatchCursor(cursorPath)
	if cursor.ConsolidationRepairs == 0 {
		t.Fatalf("consecutive repairs must be recorded: %+v", cursor)
	}

	// A healthy delta clears the backoff so the next failure repairs promptly again.
	deltaFails = false
	out.Reset()
	clock = clock.Add(options.consolidateEvery)
	watchTick(nil, &out, options, cursorPath, steps, &calls)
	if cursor := loadWatchCursor(cursorPath); cursor.ConsolidationRepairs != 0 {
		t.Fatalf("healthy delta must reset the repair backoff: %+v", cursor)
	}
}

// TestWatchTickNilDeltaKeepsNormalCadence locks that a watchSteps without a
// delta step is not treated as a permanently failing delta: with no short-term
// path there is nothing to repair, so --consolidate-every applies as usual.
func TestWatchTickNilDeltaKeepsNormalCadence(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	refreshes := 0
	steps := watchSteps{
		now:         func() time.Time { return now },
		fingerprint: func(ctxArg context.Context) string { return "changed-fp" },
		refresh:     func(ctxArg context.Context) error { refreshes++; return nil },
		seed:        func(ctxArg context.Context) error { return nil },
		distill:     func(ctxArg context.Context) error { return nil },
	}
	cursorPath := filepath.Join(t.TempDir(), "cursor.json")
	if err := saveWatchCursor(cursorPath, watchCursor{LastFingerprint: "old-fp", LastRefreshAt: now.Add(-10 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	calls := 0
	watchTick(nil, &out, defaultWatchOptions(), cursorPath, steps, &calls)
	if refreshes != 0 || !strings.Contains(out.String(), "short-term memory is current") {
		t.Fatalf("nil delta must defer on the normal cadence: refreshes=%d out=%q", refreshes, out.String())
	}
}

// TestWatchTickConsolidationCadence locks the two-tier cadence: within
// --consolidate-every the heavy refresh defers even when the fingerprint
// changed (short-term carries freshness); a full short-term buffer or an
// elapsed interval forces consolidation; 0 restores consolidate-on-every-change.
func TestWatchTickConsolidationCadence(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	newSteps := func(refreshes *int, full bool) watchSteps {
		return watchSteps{
			now:         func() time.Time { return now },
			fingerprint: func(ctxArg context.Context) string { return "changed-fp" },
			delta: func(ctxArg context.Context) (shortTermStats, error) {
				return shortTermStats{Records: 2, Files: 1, Truncated: full}, nil
			},
			refresh: func(ctxArg context.Context) error { *refreshes++; return nil },
			seed:    func(ctxArg context.Context) error { return nil },
			distill: func(ctxArg context.Context) error { return nil },
		}
	}
	cursor := watchCursor{LastFingerprint: "old-fp", LastRefreshAt: now.Add(-10 * time.Minute)}
	options := defaultWatchOptions() // consolidateEvery 30m

	// Changed fingerprint, 10m since last full refresh: deferred.
	var out strings.Builder
	refreshes := 0
	cursorPath := filepath.Join(t.TempDir(), "cursor.json")
	if err := saveWatchCursor(cursorPath, cursor); err != nil {
		t.Fatal(err)
	}
	calls := 0
	watchTick(nil, &out, options, cursorPath, newSteps(&refreshes, false), &calls)
	if refreshes != 0 || !strings.Contains(out.String(), "consolidation deferred") {
		t.Fatalf("expected deferral: refreshes=%d out=%q", refreshes, out.String())
	}

	// Same state but the short-term buffer overflowed: consolidate now.
	out.Reset()
	if err := saveWatchCursor(cursorPath, cursor); err != nil {
		t.Fatal(err)
	}
	watchTick(nil, &out, options, cursorPath, newSteps(&refreshes, true), &calls)
	if refreshes != 1 {
		t.Fatalf("full buffer must force consolidation: refreshes=%d out=%q", refreshes, out.String())
	}

	// Interval elapsed: consolidate.
	out.Reset()
	refreshes = 0
	if err := saveWatchCursor(cursorPath, watchCursor{LastFingerprint: "old-fp", LastRefreshAt: now.Add(-45 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	watchTick(nil, &out, options, cursorPath, newSteps(&refreshes, false), &calls)
	if refreshes != 1 {
		t.Fatalf("elapsed interval must consolidate: refreshes=%d out=%q", refreshes, out.String())
	}

	// consolidate-every 0: pre-short-term behavior (refresh on every change).
	out.Reset()
	refreshes = 0
	options.consolidateEvery = 0
	if err := saveWatchCursor(cursorPath, cursor); err != nil {
		t.Fatal(err)
	}
	watchTick(nil, &out, options, cursorPath, newSteps(&refreshes, false), &calls)
	if refreshes != 1 {
		t.Fatalf("consolidate-every=0 must refresh on change: refreshes=%d out=%q", refreshes, out.String())
	}
}

// TestTwoTierStableIDReconciliation is the R0-4 adversarial fixture: the same
// stable exchange ID exists at an old path in the long-term index and at a
// newer path with newer text in the short-term overlay (and, for a second ID,
// the reverse). Search and get must agree on the newer copy in both
// directions, and a duplicated ID may contribute rank at most once.
func TestTwoTierStableIDReconciliation(t *testing.T) {
	brainDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(brainDir, historyDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	generated := time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC)
	oldPathX := "sessions/main/20260801T000000Z_sess-x.jsonl"
	newPathX := "sessions/main/20260806T000000Z_sess-x.jsonl"
	oldPathY := "sessions/main/20260802T000000Z_sess-y.jsonl"
	newPathY := "sessions/main/20260807T000000Z_sess-y.jsonl"
	idX := conversationIDPrefix + "xdup"
	idY := conversationIDPrefix + "ydup"
	mk := func(id, path, summary, digest, session string) historyRecord {
		return historyRecord{
			ID: id, Kind: conversationKind, Path: path, Line: 1, EndLine: 2, TurnOrdinal: 1,
			SessionID: session, Summary: summary, ContentRole: conversationContentRole,
			SourceDigest: digest,
		}
	}
	// Long-term: X at its OLD export path, Y already at its NEW path.
	index := historyIndex{GeneratedAt: generated, Records: []historyRecord{
		mk(idX, oldPathX, "lock decision stale copy", "sha256:x-old", "sess-x"),
		mk(idY, newPathY, "cache decision current copy", "sha256:y-new", "sess-y"),
	}}
	data, err := json.MarshalIndent(index, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brainDir, filepath.FromSlash(historyIndexPath)), data, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		GeneratedAt:   generated,
		Sources: &brainSources{History: &historySourceManifest{
			GeneratedAt: generated, IndexPath: historyIndexPath, Records: len(index.Records),
		}},
	}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}
	// Overlay: X re-exported at a NEWER path with newer text, Y at an OLDER
	// path with stale text.
	overlay := shortTermIndex{
		Version:           historyShortTermVersion,
		ReconcilerVersion: historyShortTermReconcilerVersion,
		BaseGeneratedAt:   generated,
		GeneratedAt:       generated.Add(time.Hour),
		Files: map[string]shortTermFile{
			newPathX: {Records: []historyRecord{mk(idX, newPathX, "lock decision revised copy", "sha256:x-new", "sess-x")}},
			oldPathY: {Records: []historyRecord{mk(idY, oldPathY, "cache decision stale copy", "sha256:y-old", "sess-y")}},
		},
	}
	overlayData, err := json.MarshalIndent(overlay, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brainDir, filepath.FromSlash(historyShortTermPath)), overlayData, 0o600); err != nil {
		t.Fatal(err)
	}

	fresh, err := loadFreshHistory(brainDir, manifest.Sources.History)
	if err != nil {
		t.Fatal(err)
	}
	// Search: exactly one copy per ID, and it is the newer source both ways.
	scored := rankFreshHistory(fresh, conversationKind, "decision copy", 10, nil, func(longTerm historyIndex) ([]scoredHistoryRecord, bool) {
		return rankHistoryViaFTS(brainDir, longTerm, conversationKind, "decision copy", 10)
	})
	paths := map[string][]string{}
	for _, s := range scored {
		paths[s.Record.ID] = append(paths[s.Record.ID], s.Record.Path)
	}
	if got := paths[idX]; len(got) != 1 || got[0] != newPathX {
		t.Fatalf("search winner for X must be the newer overlay copy exactly once: %v", got)
	}
	if got := paths[idY]; len(got) != 1 || got[0] != newPathY {
		t.Fatalf("search winner for Y must be the newer long-term copy exactly once: %v", got)
	}

	// Get: the same winners, so search and expansion cannot disagree.
	found, missing, err := getUnifiedBatch("", brainDir, "main", []string{idX, idY})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(missing) != 0 || len(found) != 2 {
		t.Fatalf("get: found=%d missing=%v", len(found), missing)
	}
	if found[0].Path != newPathX || !strings.Contains(found[0].Text, "revised") {
		t.Fatalf("get X resolved the stale copy: path=%s text=%q", found[0].Path, found[0].Text)
	}
	if found[1].Path != newPathY || !strings.Contains(found[1].Text, "current") {
		t.Fatalf("get Y resolved the stale copy: path=%s text=%q", found[1].Path, found[1].Text)
	}

	// The semantic view drops only the superseded long-term copy.
	semIndex := fresh.longTermReconciled()
	for _, r := range semIndex.Records {
		if r.ID == idX {
			t.Fatalf("superseded long-term copy of X still in the semantic view: %+v", r)
		}
		if r.ID == idY && r.Path != newPathY {
			t.Fatalf("semantic view holds the wrong Y copy: %+v", r)
		}
	}
}

// TestShortTermOverlayStatesNeverClaimCoverage is the R0-6 acceptance
// fixture: an unreadable transcript, an overflowed buffer, a corrupt overlay,
// and an unknown overlay version are durably distinguishable, and none of
// them lets doctor claim that short-term memory covers the long-term gap.
func TestShortTermOverlayStatesNeverClaimCoverage(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_EMBEDDER", "")
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}
	storage, err := repoStoragePaths((&cobra.Command{}).Context(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	brainDir := storage.BrainDir
	writeTranscript := func(rel, request, response string) {
		t.Helper()
		full := filepath.Join(brainDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		body := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"` + request + `"}]}}` + "\n" +
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"` + response + `"}]}}` + "\n"
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	baseRel := "sessions/main/20260808T100000Z_base.jsonl"
	writeTranscript(baseRel, "fix the exporter", "Decision: pinned the exporter version.")
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion, GeneratedAt: now, RepoKey: storage.Key, DefaultBranch: "main",
		Sources: &brainSources{Sessions: &sessionSourceManifest{GeneratedAt: now, DefaultBranch: "main", Sessions: []exportSession{
			{SessionID: "base-sess", Branch: "main", Agent: "claude", LatestCheckpoint: "cp", TranscriptPath: baseRel, CreatedAt: now.Add(-time.Hour)},
		}}},
	}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := writeBrainHistoryIndexAndSource(brainDir, now, nil); err != nil {
		t.Fatal(err)
	}
	addSession := func(id, rel string) {
		t.Helper()
		onDisk, err := loadBrainManifest(brainDir)
		if err != nil {
			t.Fatal(err)
		}
		onDisk.Sources.Sessions.Sessions = append(onDisk.Sources.Sessions.Sessions, exportSession{
			SessionID: id, Branch: "main", Agent: "claude", LatestCheckpoint: "cp-" + id,
			TranscriptPath: rel, CreatedAt: now.Add(-10 * time.Minute),
		})
		if err := writeBrainManifestAndReadme(brainDir, *onDisk); err != nil {
			t.Fatal(err)
		}
	}
	freshness := func() (string, string, map[string]doctorCheckResult) {
		t.Helper()
		checks := map[string]doctorCheckResult{}
		for _, check := range brainDoctorChecks((&cobra.Command{}).Context(), opts, repoDir) {
			checks[check.Name] = check
		}
		f := checks["history_freshness"]
		return f.State, f.Detail, checks
	}

	// New work after the build: a complete delta covers the gap.
	newRel := "sessions/main/20260808T110000Z_new.jsonl"
	writeTranscript(newRel, "profile the gzip loop", "Decision: gzip level 1.")
	addSession("new-sess", newRel)
	buildShortTerm(t, brainDir)
	if state, detail, _ := freshness(); state != "ok" || !strings.Contains(detail, "covers the gap") {
		t.Fatalf("complete overlay must cover the gap: %s %q", state, detail)
	}

	onDiskManifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}

	// One unreadable transcript: the delta must record the failure durably and
	// doctor must stop claiming coverage. POSIX permission semantics; Windows
	// cannot express an unreadable file through chmod.
	if runtime.GOOS != "windows" {
		badRel := "sessions/main/20260808T113000Z_bad.jsonl"
		writeTranscript(badRel, "unreadable", "unreadable")
		badFull := filepath.Join(brainDir, filepath.FromSlash(badRel))
		if err := os.Chmod(badFull, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(badFull, 0o600) })
		addSession("bad-sess", badRel)
		failedStats := buildShortTerm(t, brainDir)
		if failedStats.Failed != 1 {
			t.Fatalf("stats.Failed = %d, want 1", failedStats.Failed)
		}
		overlay, state := loadHistoryShortTermState(brainDir, onDiskManifest.Sources.History)
		if state != shortTermStateCurrent || len(overlay.FailedFiles) != 1 || overlay.FailedFiles[0] != badRel {
			t.Fatalf("failed file identity not durable: state=%s failed=%v", state, overlay.FailedFiles)
		}
		if fstate, detail, checks := freshness(); fstate != "warn" || strings.Contains(detail, "covers the gap (") {
			t.Fatalf("partial overlay must not claim coverage: %s %q %+v", fstate, detail, checks["short_term_memory"])
		}
		if err := os.Chmod(badFull, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Overflow: the truncated buffer is partial coverage.
	oldMax := historyShortTermMaxRecords
	historyShortTermMaxRecords = 1
	defer func() { historyShortTermMaxRecords = oldMax }()
	overflowStats := buildShortTerm(t, brainDir)
	historyShortTermMaxRecords = oldMax
	if !overflowStats.Truncated {
		t.Fatalf("expected truncation: %+v", overflowStats)
	}
	if fstate, detail, _ := freshness(); fstate != "warn" || !strings.Contains(detail, "partially") {
		t.Fatalf("truncated overlay must be partial: %s %q", fstate, detail)
	}

	// Corrupt overlay JSON.
	overlayPath := filepath.Join(brainDir, filepath.FromSlash(historyShortTermPath))
	if err := os.WriteFile(overlayPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, state := loadHistoryShortTermState(brainDir, onDiskManifest.Sources.History); state != shortTermStateCorrupt {
		t.Fatalf("corrupt overlay state = %s", state)
	}
	if fstate, detail, checks := freshness(); fstate != "warn" || !strings.Contains(checks["short_term_memory"].Detail, "corrupt") {
		t.Fatalf("corrupt overlay must warn: %s %q %+v", fstate, detail, checks["short_term_memory"])
	}

	// Unknown (newer) overlay version.
	if err := os.WriteFile(overlayPath, []byte(`{"version":99,"files":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, state := loadHistoryShortTermState(brainDir, onDiskManifest.Sources.History); state != shortTermStateUnsupported {
		t.Fatalf("unknown version state = %s", state)
	}
	if fstate, _, checks := freshness(); fstate != "warn" || !strings.Contains(checks["short_term_memory"].Detail, "unsupported") {
		t.Fatalf("unsupported overlay must warn: %s %+v", fstate, checks["short_term_memory"])
	}
}

// TestShortTermReconcilerVersionGate proves the R0-6 refinement: an overlay
// built for a different record-reconciliation rule reads as unsupported and
// never joins ranking or coverage claims.
func TestShortTermReconcilerVersionGate(t *testing.T) {
	brainDir, _, _ := shortTermFixture(t)
	buildShortTerm(t, brainDir)
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	overlay, state := loadHistoryShortTermState(brainDir, manifest.Sources.History)
	if state != shortTermStateCurrent {
		t.Fatalf("fresh overlay state = %s", state)
	}
	overlay.ReconcilerVersion = historyShortTermReconcilerVersion - 1
	if err := saveHistoryShortTerm(brainDir, overlay); err != nil {
		t.Fatal(err)
	}
	if _, state := loadHistoryShortTermState(brainDir, manifest.Sources.History); state != shortTermStateUnsupported {
		t.Fatalf("old reconciler version must read unsupported: %s", state)
	}
}

// TestConsolidationKeepsOverlayCoveringNewerSources proves the R0-6
// fingerprint gate: a full build from an older source set must not destroy an
// overlay that covers newer work; a build from the same set clears it.
func TestConsolidationKeepsOverlayCoveringNewerSources(t *testing.T) {
	brainDir, _, _ := shortTermFixture(t)
	buildShortTerm(t, brainDir)
	overlayPath := filepath.Join(brainDir, filepath.FromSlash(historyShortTermPath))
	if _, err := os.Stat(overlayPath); err != nil {
		t.Fatalf("overlay must exist: %v", err)
	}
	// Simulate an overlay built against a NEWER source set than the manifest
	// the full build is about to consume.
	overlay, state := loadHistoryShortTermRaw(brainDir)
	if state != shortTermStateCurrent {
		t.Fatalf("raw overlay state = %s", state)
	}
	overlay.SessionsFingerprint = "sha256:newer-than-this-build"
	if err := saveHistoryShortTerm(brainDir, overlay); err != nil {
		t.Fatal(err)
	}
	if _, err := writeBrainHistoryIndexAndSource(brainDir, time.Date(2026, 8, 8, 15, 0, 0, 0, time.UTC), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(overlayPath); err != nil {
		t.Fatalf("consolidation from an older source set must keep the newer overlay: %v", err)
	}

	// A delta rebuild re-pins the overlay to the current sources; the next
	// consolidation covers it and clears.
	buildShortTerm(t, brainDir)
	if _, err := writeBrainHistoryIndexAndSource(brainDir, time.Date(2026, 8, 8, 16, 0, 0, 0, time.UTC), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(overlayPath); !os.IsNotExist(err) {
		t.Fatalf("covered overlay must be cleared by consolidation: %v", err)
	}
}
