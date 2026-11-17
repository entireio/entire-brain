package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

// historyEvalFixture returns a brain dir with two session transcripts and the
// manifest that names them, plus an in-memory history index whose records
// point at those transcripts (the provenance link eval-gen labels through).
func historyEvalFixture(t *testing.T) (string, *exportManifest, historyIndex) {
	t.Helper()
	brainDir := t.TempDir()
	transcripts := map[string]string{
		"sessions/main/s1.jsonl": `{"type":"user","message":{"content":"add retry backoff to the fetcher"}}` + "\n",
		"sessions/main/s2.jsonl": `{"type":"user","message":{"content":"why does the exporter skip unchanged sessions"}}` + "\n",
	}
	for rel, content := range transcripts {
		p := filepath.Join(brainDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	manifest := &exportManifest{
		SchemaVersion: bmanifest.SchemaVersion,
		GeneratedAt:   now,
		DefaultBranch: "main",
		Sources: &brainSources{Sessions: &sessionSourceManifest{GeneratedAt: now, DefaultBranch: "main", Sessions: []exportSession{
			{SessionID: "session-aaaa-1111", Branch: "main", TranscriptPath: "sessions/main/s1.jsonl", CreatedAt: now.Add(-2 * time.Hour)},
			{SessionID: "session-bbbb-2222", Branch: "main", TranscriptPath: "sessions/main/s2.jsonl", CreatedAt: now.Add(-time.Hour)},
		}}},
	}
	index := historyIndex{GeneratedAt: now, Records: []historyRecord{
		{ID: "r1", Kind: "decision", Path: "sessions/main/s1.jsonl", Line: 3, Summary: "decided to use exponential backoff with jitter for fetch retries"},
		{ID: "r2", Kind: "learning", Path: "sessions/main/s1.jsonl", Line: 9, Summary: "the fetcher must cap retry attempts at five to bound latency"},
		{ID: "r3", Kind: "request", Path: "sessions/main/s1.jsonl", Line: 1, Summary: "add retry backoff to the fetcher"},
		{ID: "r4", Kind: "decision", Path: "sessions/main/s2.jsonl", Line: 4, Summary: "the export cursor reuses unchanged transcripts so refresh stays incremental"},
		{ID: "r5", Kind: "validation", Path: "sessions/main/s2.jsonl", Line: 8, Summary: "verified the exporter cache hit rate on the live brain"},
	}}
	return brainDir, manifest, index
}
