package cli

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testdata/history_brain_rev12_null_message is a brain built by a pre-release
// build of this change (c823593f, extraction revision 12) from a transcript
// whose records carry "message": null or {} next to top-level content. That
// reader returned nothing for those records, so the scan cache holds the
// transcript with no records and the index has no exchanges. The reader now
// falls back to the top-level content, so the extraction revision moved and a
// refresh must rescan the transcript instead of reusing the revision 12 entry.
func TestHistoryRefreshRescansRevision12NullMessageCache(t *testing.T) {
	brainDir := t.TempDir()
	if err := os.CopyFS(brainDir, os.DirFS(filepath.Join("testdata", "history_brain_rev12_null_message"))); err != nil {
		t.Fatal(err)
	}
	const rel = "sessions/main/20260801T120000Z_fix-lock.jsonl"
	f, err := os.Open(filepath.Join(brainDir, filepath.FromSlash(historyScanCachePath)))
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var cache historyScanCache
	if err := json.NewDecoder(gz).Decode(&cache); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	entry, ok := cache.Files[rel]
	if cache.ExtractorVersion != 12 || !ok || len(entry.Records) != 0 {
		t.Fatalf("fixture is not a revision 12 cache with an empty entry for %s: %+v", rel, cache)
	}
	// The copy has a new mtime; restore the one the cache recorded so size,
	// mtime and content digest all match and only the revision decides reuse.
	mtime := time.Unix(0, entry.ModUnixNano)
	if err := os.Chtimes(filepath.Join(brainDir, filepath.FromSlash(rel)), mtime, mtime); err != nil {
		t.Fatal(err)
	}

	source, err := writeBrainHistoryIndexAndSource(brainDir, time.Date(2026, 8, 1, 13, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if source.Exchanges != 2 {
		t.Fatalf("exchanges after refresh = %d, want 2: the revision 12 scan cache entry was reused", source.Exchanges)
	}
	if source.ExtractionVersion != historyScanCacheVersion {
		t.Fatalf("extraction version = %d, want %d", source.ExtractionVersion, historyScanCacheVersion)
	}
}

// doctor must not call an index from an older extractor current just because
// the sessions are unchanged; a full refresh clears the warning.
func TestDoctorHistoryFreshnessFlagsOlderExtractor(t *testing.T) {
	opts, repoDir, brainDir := docsAvailabilityFixture(t)
	if err := os.RemoveAll(brainDir); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(brainDir, os.DirFS(filepath.Join("testdata", "history_brain_rev12_null_message"))); err != nil {
		t.Fatal(err)
	}
	freshness := func() (string, string) {
		checks, _ := brainDoctorReadOnlyReport(context.Background(), opts, repoDir)
		for _, check := range checks {
			if check.Name == "history_freshness" {
				return check.State, check.Detail
			}
		}
		t.Fatal("doctor has no history_freshness check")
		return "", ""
	}
	if state, detail := freshness(); state != "warn" || !strings.Contains(detail, "older extractor") {
		t.Fatalf("history_freshness for a revision 12 index = %s: %s", state, detail)
	}
	if _, err := writeBrainHistoryIndexAndSource(brainDir, time.Date(2026, 8, 1, 13, 0, 0, 0, time.UTC), nil); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if state, detail := freshness(); state != "ok" {
		t.Fatalf("history_freshness after refresh = %s: %s", state, detail)
	}
}
