package cli

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// TestHistoryIndexReusesScanCache verifies the per-file scan cache is used on
// the second build (so unchanged session files are not re-parsed) and is
// invalidated when a file's size/mtime changes.
func TestHistoryIndexReusesScanCache(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	storage, err := repoStoragePaths((&cobra.Command{}).Context(), runner, env, repoDir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	sessionDir := filepath.Join(storage.BrainDir, exportSessionsDirectory, "main")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatalf("mkdir session dir: %v", err)
	}
	sessionPath := filepath.Join(sessionDir, "session.jsonl")
	line := `{"type":"agent_message","message":"Decision: keep the export cursor authoritative for transcript reuse."}` + "\n"
	if err := os.WriteFile(sessionPath, []byte(line), 0o600); err != nil {
		t.Fatalf("write session: %v", err)
	}

	source, err := writeBrainHistoryIndexAndSource(storage.BrainDir, now, nil)
	if err != nil {
		t.Fatalf("first build: %v", err)
	}
	if source.Decisions != 1 {
		t.Fatalf("expected 1 decision, got %+v", source)
	}

	cachePath := filepath.Join(storage.BrainDir, filepath.FromSlash(historyScanCachePath))
	cache := loadHistoryScanCache(storage.BrainDir)
	if len(cache.Files) != 1 {
		t.Fatalf("expected 1 cached file, got %d (%s)", len(cache.Files), cachePath)
	}

	// Replace the cached records for the (unchanged) file with a sentinel. If
	// the second build reuses the cache, the sentinel propagates into the index;
	// if it re-scanned the file, it would not.
	for key, entry := range cache.Files {
		entry.Records = []historyRecord{{
			ID:      "sentinel",
			Kind:    "decision",
			Path:    key,
			Line:    1,
			Summary: "Decision: sentinel cache marker.",
		}}
		cache.Files[key] = entry
	}
	saveHistoryScanCache(storage.BrainDir, cache)

	var lastDone, lastTotal int
	progress := func(done, total int) { lastDone, lastTotal = done, total }
	if _, _, err := buildBrainHistoryIndex(storage.BrainDir, now, progress); err != nil {
		t.Fatalf("second build: %v", err)
	}
	if lastTotal != 1 || lastDone != 1 {
		t.Fatalf("progress = %d/%d, want 1/1", lastDone, lastTotal)
	}
	reused := loadHistoryScanCache(storage.BrainDir)
	if !cacheContainsRecordID(reused, "sentinel") {
		t.Fatalf("second build did not reuse cache: %+v", reused.Files)
	}

	// Changing the file (size + mtime) must invalidate the entry and re-scan.
	if err := os.WriteFile(sessionPath, []byte(line+line), 0o600); err != nil {
		t.Fatalf("rewrite session: %v", err)
	}
	future := now.Add(time.Hour)
	if err := os.Chtimes(sessionPath, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if _, _, err := buildBrainHistoryIndex(storage.BrainDir, now, nil); err != nil {
		t.Fatalf("third build: %v", err)
	}
	rescanned := loadHistoryScanCache(storage.BrainDir)
	if cacheContainsRecordID(rescanned, "sentinel") {
		t.Fatalf("changed file was not re-scanned: stale sentinel survived: %+v", rescanned.Files)
	}
}

func cacheContainsRecordID(cache historyScanCache, id string) bool {
	for _, entry := range cache.Files {
		for _, record := range entry.Records {
			if record.ID == id {
				return true
			}
		}
	}
	return false
}

// TestHistoryScanCacheVersionMismatchIgnored ensures a cache written by a
// different extraction version is discarded rather than trusted.
func TestHistoryScanCacheVersionMismatchIgnored(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, historyDirName), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	stale := historyScanCache{Version: historyScanCacheVersion + 1, Files: map[string]historyScanCacheEntry{
		"sessions/main/old.jsonl": {Records: []historyRecord{{ID: "stale"}}},
	}}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if err := json.NewEncoder(gz).Encode(stale); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(historyScanCachePath)), buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write cache: %v", err)
	}
	loaded := loadHistoryScanCache(dir)
	if len(loaded.Files) != 0 {
		t.Fatalf("expected version mismatch to yield empty cache, got %+v", loaded.Files)
	}
}
