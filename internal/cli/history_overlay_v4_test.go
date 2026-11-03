package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
)

// testdata/history_short_term_v4.json is an overlay written by a pre-release
// build of this change (cab0496c) that stamped historyShortTermVersion 4 and
// had no extractor_version field. Its layout is the v3 layout. A brain that
// ran that build must keep serving the overlay's records after the upgrade,
// and the next delta must re-parse them (their extractor is unknown) and
// rewrite the overlay as v3.
func TestHistoryShortTermReadsLegacyV4Overlay(t *testing.T) {
	brainDir, source := buildConversationBrainFixtureWithTranscript(t, "sess-codex", compactCodexTranscriptFixture)
	const rel = "sessions/main/20260801T120000Z_fix-lock.jsonl"
	data, err := os.ReadFile(filepath.Join("testdata", "history_short_term_v4.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture shortTermIndex
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Version != 4 || fixture.ExtractorVersion != 0 || len(fixture.Files[rel].Records) == 0 {
		t.Fatalf("fixture is not a v4 overlay with records for %s: version %d extractor %d", rel, fixture.Version, fixture.ExtractorVersion)
	}
	// Same state the v4 build left behind: the transcript is short-term
	// material (its scan cache entry no longer loads) and its size, mtime and
	// content digest still match the overlay entry, so only the extractor
	// gate can force the re-parse.
	cache := loadHistoryScanCache(brainDir)
	cache.Version = historyScanCacheVersion - 1
	saveHistoryScanCache(brainDir, cache)
	mtime := time.Unix(0, fixture.Files[rel].ModUnixNano)
	if err := os.Chtimes(filepath.Join(brainDir, filepath.FromSlash(rel)), mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if err := brainstore.WriteBrainRelativeFileAtomic(brainDir, historyShortTermPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	overlay, state := loadHistoryShortTermState(brainDir, source)
	if state != shortTermStateCurrent {
		t.Fatalf("v4 overlay state = %s, want %s (readable)", state, shortTermStateCurrent)
	}
	if got, want := len(overlay.Files[rel].Records), len(fixture.Files[rel].Records); got != want {
		t.Fatalf("v4 overlay records = %d, want %d", got, want)
	}
	if overlay.complete() {
		t.Fatal("a v4 overlay's extractor is unknown; it must not count as complete coverage")
	}
	if health := memoryOverlayHealth(brainDir, source); health["state"] != shortTermStateCurrent {
		t.Fatalf("overlay health = %v, want state %s", health, shortTermStateCurrent)
	}
	for _, finding := range detectMemoryMigrations(brainDir, source) {
		if finding.Path == historyShortTermPath {
			t.Fatalf("v4 overlay reported as %s: %s", finding.State, finding.Action)
		}
	}

	stats, err := buildHistoryShortTermLocked(brainDir, source.GeneratedAt.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("delta: %v", err)
	}
	if stats.Reused != 0 || stats.Scanned != 1 {
		t.Fatalf("delta stats = %d reused / %d scanned, want 0/1", stats.Reused, stats.Scanned)
	}
	overlay, state = loadHistoryShortTermState(brainDir, source)
	if state != shortTermStateCurrent || overlay.Version != historyShortTermVersion || overlay.ExtractorVersion != historyScanCacheVersion {
		t.Fatalf("after delta: state %s version %d extractor %d, want %s %d %d", state, overlay.Version, overlay.ExtractorVersion, shortTermStateCurrent, historyShortTermVersion, historyScanCacheVersion)
	}
}
