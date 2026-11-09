package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	"github.com/spf13/cobra"
)

func TestPrecedentSourcesDegradeAndDedupe(t *testing.T) {
	brainDir := t.TempDir()
	transcript := "sessions/main/present.jsonl"
	if err := os.MkdirAll(filepath.Join(brainDir, "sessions", "main"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brainDir, filepath.FromSlash(transcript)), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	anchors := []factAnchor{
		{SessionID: "sess-a", Commit: "commit-a", CheckpointID: "cp-a"},
		{SessionID: "sess-a", Commit: "commit-a", CheckpointID: "cp-a", Transcript: transcript, Line: 7},
		{SessionID: "sess-a", Commit: "commit-a", CheckpointID: "cp-a", Transcript: transcript, Line: 7},
		{SessionID: "sess-b", Commit: "commit-b", Transcript: "sessions/main/missing.jsonl", Line: 3},
		{SessionID: "sess-c", Transcript: "../outside.jsonl", Line: 1},
		{},
	}
	sources := precedentSources(brainDir, anchors)
	want := []brainBriefPrecedentSource{
		{SessionID: "sess-a", Commit: "commit-a", CheckpointID: "cp-a", Transcript: transcript, Line: 7},
		{SessionID: "sess-b", Commit: "commit-b"},
		{SessionID: "sess-c"},
	}
	if len(sources) != len(want) {
		t.Fatalf("sources = %+v, want %+v", sources, want)
	}
	for i := range want {
		if sources[i] != want[i] {
			t.Fatalf("source[%d] = %+v, want %+v", i, sources[i], want[i])
		}
	}
}

func TestBrainBriefEmitsPrecedentFromFactAnchors(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticBoundaryFixtureSnapshot())
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}

	if err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	storage, err := repoStoragePaths((&cobra.Command{}).Context(), runner, env, repoDir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}

	transcript := "sessions/main/20260605T120000Z_test.jsonl"
	if err := os.MkdirAll(filepath.Join(storage.BrainDir, brainstore.SessionsDirName, "main"), 0o700); err != nil {
		t.Fatalf("mkdir sessions: %v", err)
	}
	if err := os.WriteFile(filepath.Join(storage.BrainDir, filepath.FromSlash(transcript)), []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	paths := normalizeFactPaths([]string{"architecture.boundaries.rationale"})
	withTranscript := factRecord{
		ID:     factRecordID("ValidateToken review default scope is mainline by design.", paths),
		Paths:  paths,
		Text:   "ValidateToken review default scope is mainline by design.",
		Branch: "feature",
		Origin: factstore.OriginDistilled,
		Status: factStatusActive,
		Provenance: []factAnchor{
			{SessionID: "sess-a", Commit: "commit-a", CheckpointID: "cp-a"},
			{SessionID: "sess-a", Commit: "commit-a", CheckpointID: "cp-a", Transcript: transcript, Line: 7},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	missingTranscript := factRecord{
		ID:     factRecordID("ValidateToken callers must pass a scoped token.", paths),
		Paths:  paths,
		Text:   "ValidateToken callers must pass a scoped token.",
		Branch: "feature",
		Origin: factstore.OriginDistilled,
		Status: factStatusActive,
		Provenance: []factAnchor{
			{SessionID: "sess-b", Commit: "commit-b", Transcript: "sessions/main/gone.jsonl", Line: 3},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := factstore.Write(storage.BrainDir, []factRecord{withTranscript, missingTranscript}); err != nil {
		t.Fatalf("write facts: %v", err)
	}
	if err := updateFactSourceManifest(storage.BrainDir, now); err != nil {
		t.Fatalf("update manifest: %v", err)
	}
	runner.responses[fakeCommandKey("git", "diff-index", "-M", "--shortstat", "HEAD")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "diff", "--name-status", "-M", "-C", "HEAD")] = fakeCommandResponse{}

	cmd := NewRootCommand(opts)
	out, err := execute(t, cmd, "brief", "ValidateToken", "--json")
	if err != nil {
		t.Fatalf("brief: %v\n%s", err, out)
	}
	var report brainBriefJSONReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse brief json: %v\n%s", err, out)
	}
	if len(report.Facts) < 2 {
		t.Fatalf("brief should rank both facts, got %+v", report.Facts)
	}
	sources := map[string][]brainBriefPrecedentSource{}
	for _, p := range report.Precedent {
		sources[p.FactID] = p.Sources
	}
	got := sources[withTranscript.ID]
	if len(got) != 1 || got[0] != (brainBriefPrecedentSource{SessionID: "sess-a", Commit: "commit-a", CheckpointID: "cp-a", Transcript: transcript, Line: 7}) {
		t.Fatalf("transcript-backed fact sources = %+v", got)
	}
	got = sources[missingTranscript.ID]
	if len(got) != 1 || got[0] != (brainBriefPrecedentSource{SessionID: "sess-b", Commit: "commit-b"}) {
		t.Fatalf("missing transcript should degrade to session/commit, got %+v", got)
	}
}
