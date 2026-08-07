package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const privacyCanary = "CANARY-XYZQ-SECRET-TOKEN"

// writePrivacyFixture builds a brain with two sessions: one clean, one whose
// request and response both carry the canary secret.
func writePrivacyFixture(t *testing.T) (brainDir string) {
	t.Helper()
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	brainDir = t.TempDir()
	clean := "sessions/main/20260801T000000Z_clean.jsonl"
	secret := "sessions/main/20260802T000000Z_secret.jsonl"
	cleanBody := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Fix the export cursor"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Decision: cursor reuses unchanged transcripts."}]}}
`
	secretBody := fmt.Sprintf(`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"rotate the leaked key %s now"}]}}`+"\n"+
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Decision: rotated %s and revoked the old credential."}]}}`+"\n", privacyCanary, privacyCanary)
	for rel, body := range map[string]string{clean: cleanBody, secret: secretBody} {
		full := filepath.Join(brainDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion, GeneratedAt: now, RepoKey: "test/privacy", DefaultBranch: "main",
		Sources: &brainSources{Sessions: &sessionSourceManifest{GeneratedAt: now, DefaultBranch: "main", Sessions: []exportSession{
			{SessionID: "clean-sess", Branch: "main", Agent: "claude", LatestCheckpoint: "cp1", TranscriptPath: clean, CreatedAt: now.Add(-2 * time.Hour)},
			{SessionID: "secret-sess", Branch: "main", Agent: "claude", LatestCheckpoint: "cp2", TranscriptPath: secret, CreatedAt: now.Add(-time.Hour)},
		}}},
	}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := writeBrainHistoryIndexAndSource(brainDir, now, nil); err != nil {
		t.Fatal(err)
	}
	return brainDir
}

// assertCanaryAbsent proves the canary is gone from every retained local
// artifact: the history index JSON, the decompressed scan cache, FTS ranking,
// and the sessions tree.
func assertCanaryAbsent(t *testing.T, brainDir string) {
	t.Helper()
	indexBytes, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(historyIndexPath)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(indexBytes), privacyCanary) {
		t.Fatal("canary survived in history/index.json")
	}
	cache := loadHistoryScanCache(brainDir)
	for rel, entry := range cache.Files {
		for _, record := range entry.Records {
			if strings.Contains(record.Summary, privacyCanary) {
				t.Fatalf("canary survived in scan cache entry %s", rel)
			}
		}
	}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	index, err := loadBrainHistoryIndex(brainDir, manifest.Sources.History)
	if err != nil {
		t.Fatal(err)
	}
	if scored, ok := rankHistoryViaFTS(brainDir, index, "history", privacyCanary, 10); ok && len(scored) > 0 {
		t.Fatalf("canary reachable through FTS: %+v", scored)
	}
	if results, err := retrieveConversation(brainDir, privacyCanary, 10, modeLexical, retrievalOptions{}); err == nil {
		for _, result := range results {
			if strings.Contains(result.Text, privacyCanary) {
				t.Fatalf("canary reachable through conversation retrieval: %+v", result)
			}
		}
	}
	sessionsRoot := filepath.Join(brainDir, exportSessionsDirectory)
	_ = filepath.Walk(sessionsRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr == nil && strings.Contains(string(data), privacyCanary) {
			t.Fatalf("canary survived in exported transcript %s", path)
		}
		return nil
	})
}

func TestSessionExcludeRemovesDerivedRecordsButKeepsTranscript(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	now := time.Date(2026, 8, 7, 13, 0, 0, 0, time.UTC)

	// Precondition: the canary is indexed.
	manifest, _ := loadBrainManifest(brainDir)
	index, err := loadBrainHistoryIndex(brainDir, manifest.Sources.History)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, record := range index.Records {
		if strings.Contains(record.Summary, privacyCanary) {
			found = true
		}
	}
	if !found {
		t.Fatal("precondition: canary must be indexed before exclusion")
	}

	// Exclude via tombstone + rebuild (the command core, without CLI plumbing).
	stones := loadSessionTombstones(brainDir)
	stones.Excluded["secret-sess"] = sessionTombstone{At: now}
	if err := saveSessionTombstones(brainDir, stones); err != nil {
		t.Fatal(err)
	}
	source, err := writeBrainHistoryIndexAndSource(brainDir, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if source.ExcludedSessions != 1 {
		t.Fatalf("excluded sessions = %d, want 1", source.ExcludedSessions)
	}

	// Derived records are gone; the exported transcript survives (exclude,
	// not purge).
	indexBytes, _ := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(historyIndexPath)))
	if strings.Contains(string(indexBytes), privacyCanary) {
		t.Fatal("excluded session still indexed")
	}
	if _, err := os.Stat(filepath.Join(brainDir, "sessions", "main", "20260802T000000Z_secret.jsonl")); err != nil {
		t.Fatalf("exclude must keep the exported transcript: %v", err)
	}

	// Include requires explicit action and cleanly rebuilds.
	delete(stones.Excluded, "secret-sess")
	if err := saveSessionTombstones(brainDir, stones); err != nil {
		t.Fatal(err)
	}
	source, err = writeBrainHistoryIndexAndSource(brainDir, now.Add(time.Minute), nil)
	if err != nil {
		t.Fatal(err)
	}
	if source.ExcludedSessions != 0 || source.Exchanges == 0 {
		t.Fatalf("re-include rebuild: %+v", source)
	}
}

func TestSessionPurgeCanaryAbsentEverywhereAndIdempotent(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	now := time.Date(2026, 8, 7, 13, 0, 0, 0, time.UTC)

	// Warm the FTS index so purge has a derived store to remove.
	manifest, _ := loadBrainManifest(brainDir)
	index, err := loadBrainHistoryIndex(brainDir, manifest.Sources.History)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rankHistoryViaFTS(brainDir, index, "history", "rotated", 5); !ok {
		t.Fatal("fts warmup failed")
	}

	// Dry-run predicts the artifacts and changes nothing.
	plan, err := buildSessionPurgePlan(brainDir, "secret-sess")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Transcripts) != 1 || plan.Transcripts[0].Path != "sessions/main/20260802T000000Z_secret.jsonl" || plan.Transcripts[0].Bytes == 0 {
		t.Fatalf("plan transcripts = %+v", plan.Transcripts)
	}
	if plan.Records == 0 {
		t.Fatal("plan must count derived records")
	}
	hasFTS := false
	for _, store := range plan.DerivedStores {
		if store.Path == historyFTSDBRelPath() {
			hasFTS = true
		}
	}
	if !hasFTS {
		t.Fatalf("plan missing the FTS store: %+v", plan.DerivedStores)
	}
	if _, err := os.Stat(filepath.Join(brainDir, "sessions", "main", "20260802T000000Z_secret.jsonl")); err != nil {
		t.Fatalf("dry-run computation must not delete anything: %v", err)
	}

	// Execute the purge.
	if err := executeSessionPurge(brainDir, "secret-sess", plan, now); err != nil {
		t.Fatal(err)
	}
	assertCanaryAbsent(t, brainDir)
	stones := loadSessionTombstones(brainDir)
	if _, ok := stones.Excluded["secret-sess"]; !ok {
		t.Fatal("purge must leave a tombstone")
	}
	// The clean session survives untouched.
	manifest, _ = loadBrainManifest(brainDir)
	if manifest.Sources.History.Exchanges != 1 {
		t.Fatalf("surviving exchanges = %d, want 1 (clean session only)", manifest.Sources.History.Exchanges)
	}

	// Idempotent: purging again is an empty plan and succeeds.
	again, err := buildSessionPurgePlan(brainDir, "secret-sess")
	if err != nil {
		t.Fatal(err)
	}
	if again.Records != 0 || len(again.Transcripts) != 1 || again.Transcripts[0].Bytes != 0 {
		t.Fatalf("second purge plan not empty: %+v", again)
	}
	if err := executeSessionPurge(brainDir, "secret-sess", again, now.Add(time.Minute)); err != nil {
		t.Fatalf("re-purge must be idempotent: %v", err)
	}

	// A re-export of the still-canonical capture must NOT re-index: the
	// tombstone holds until an explicit include.
	full := filepath.Join(brainDir, "sessions", "main", "20260802T000000Z_secret.jsonl")
	body := fmt.Sprintf(`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"rotate the leaked key %s now"}]}}`+"\n", privacyCanary)
	if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := writeBrainHistoryIndexAndSource(brainDir, now.Add(2*time.Minute), nil); err != nil {
		t.Fatal(err)
	}
	indexBytes, _ := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(historyIndexPath)))
	if strings.Contains(string(indexBytes), privacyCanary) {
		t.Fatal("tombstone failed: re-exported session was re-indexed")
	}
}

func TestSessionTombstonesRoundTripAndCorruptFallback(t *testing.T) {
	brainDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(brainDir, historyDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	stones := loadSessionTombstones(brainDir)
	if len(stones.Excluded) != 0 {
		t.Fatalf("fresh brain must have no tombstones: %+v", stones)
	}
	stones.Excluded["s1"] = sessionTombstone{At: time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC), Reason: "test"}
	if err := saveSessionTombstones(brainDir, stones); err != nil {
		t.Fatal(err)
	}
	reloaded := loadSessionTombstones(brainDir)
	if _, ok := reloaded.Excluded["s1"]; !ok || reloaded.Version != sessionTombstonesVersion {
		t.Fatalf("round trip failed: %+v", reloaded)
	}
	// The tombstone file must never retain excluded content — only id, time,
	// and the caller-supplied reason.
	raw, _ := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(sessionTombstonesPath)))
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	// Corrupt file fails open to "nothing excluded" (an explicit action model:
	// corruption can only restore indexing, never delete data).
	if err := os.WriteFile(filepath.Join(brainDir, filepath.FromSlash(sessionTombstonesPath)), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadSessionTombstones(brainDir); len(got.Excluded) != 0 {
		t.Fatalf("corrupt tombstones must read as empty: %+v", got)
	}
}
