package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/historystore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"github.com/spf13/cobra"
)

func TestPrintDistillDryRunReportHumanOutputDistinguishesCacheAndSchedule(t *testing.T) {
	report := distillDryRunReport{
		Sessions: 3, CachedSessions: 1, SessionsToDistill: 2, Chunks: 5, ChunksIfUncached: 7,
		ExtractionAgentCalls: 2, ReconcileAgentCallsUpperBound: 3, EstimatedAgentCallsUpperBound: 5,
		RawBytes: 1000, PreprocessedBytes: 800,
		Branches: []distillDryRunBranch{
			{Branch: "main", Sessions: 2, CachedSessions: 1, Chunks: 3, ChunksIfUncached: 4},
			{Branch: "release", Sessions: 1, Chunks: 2, ChunksIfUncached: 3},
		},
		LargestSessions: []distillDryRunSession{
			{Branch: "main", Transcript: "sessions/a.jsonl", Chunks: 2, ChunksIfUncached: 3, PreprocessedBytes: 500, RawBytes: 600, Cached: true},
			{Branch: "release", Transcript: "sessions/b.jsonl", Chunks: 1, ChunksIfUncached: 1, PreprocessedBytes: 300, RawBytes: 400},
		},
		Warnings: []string{"one transcript was unreadable"},
	}
	cmd := &cobra.Command{Use: "distill"}
	var out bytes.Buffer
	cmd.SetOut(&out)
	printDistillDryRunReport(cmd, report)
	got := out.String()
	for _, expected := range []string{
		"distill dry-run: 3 sessions, 1 cached, 2 to distill, 5 chunks\n",
		"chunks if uncached: 7\n",
		"estimated agent calls: 2 extraction + up to 3 reconcile (batched by similarity cluster; typically far fewer) = up to 5 total\n",
		"branch main: 2 sessions, 1 cached, 3 chunks\n",
		"branch main chunks if uncached: 4\n",
		"- main sessions/a.jsonl: 2 chunks scheduled, 3 if uncached cached, 500 preprocessed bytes, 600 raw bytes\n",
		"- release sessions/b.jsonl: 1 chunks, 300 preprocessed bytes, 400 raw bytes\n",
		"warning: one transcript was unreadable\n",
	} {
		if !strings.Contains(got, expected) {
			t.Fatalf("dry-run output missing %q:\n%s", expected, got)
		}
	}
}

func TestRefreshHistoryRootCommandIndexesIDsWithoutVectorOptIn(t *testing.T) {
	repo := t.TempDir()
	env := semanticTestEnv(t, repo)
	runner := semanticFixtureRunner(repo, "")
	opts := Options{Env: env, Runner: runner, Now: func() time.Time { return time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC) }}
	brainDir := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo")
	transcriptRel := "sessions/main/session.jsonl"
	if err := os.MkdirAll(filepath.Join(brainDir, filepath.Dir(transcriptRel)), 0o700); err != nil {
		t.Fatal(err)
	}
	transcript := []byte(`{"type":"agent_message","message":"Decision: keep the indexed cache."}` + "\n")
	if err := os.WriteFile(filepath.Join(brainDir, filepath.FromSlash(transcriptRel)), transcript, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeBrainManifestAndReadme(brainDir, exportManifest{
		SchemaVersion: bmanifest.SchemaVersion,
		RepoKey:       "gh/example/repo",
		Sources: &brainSources{Sessions: &sessionSourceManifest{Sessions: []exportSession{{
			SessionID: "history-session", Branch: "main", TranscriptPath: transcriptRel,
		}}}},
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENTIRE_BRAIN_EMBEDDER", "")
	out, err := execute(t, NewRootCommand(opts), "refresh", "history")
	if err != nil {
		t.Fatalf("refresh history: %v\n%s", err, out)
	}
	manifest, err := bmanifest.Load(brainDir)
	if err != nil || manifest.Sources == nil || manifest.Sources.History == nil {
		t.Fatalf("history source missing: %v %+v", err, manifest)
	}
	index, err := historystore.LoadIndex(brainDir, manifest.Sources.History)
	if err != nil {
		t.Fatalf("load indexed history: %v", err)
	}
	if len(index.Records) != 1 || index.Records[0].ID == "" || !strings.Contains(index.Records[0].Summary, "keep the indexed cache") {
		t.Fatalf("unexpected indexed records: %+v", index.Records)
	}
	statsText, err := execute(t, NewRootCommand(opts), "stats")
	if err != nil {
		t.Fatalf("stats text: %v\n%s", err, statsText)
	}
	for _, want := range []string{"repo: gh/example/repo", "sessions: 1", "history: 1 records", "scan cache v"} {
		if !strings.Contains(statsText, want) {
			t.Fatalf("stats text missing %q:\n%s", want, statsText)
		}
	}
	statsJSON, err := execute(t, NewRootCommand(opts), "stats", "--json")
	if err != nil {
		t.Fatalf("stats JSON: %v\n%s", err, statsJSON)
	}
	var payload struct {
		RepoKey     string `json:"repo_key"`
		GeneratedAt string `json:"generated_at"`
		Sessions    struct {
			Count int `json:"count"`
		} `json:"sessions"`
		History struct {
			Records int `json:"records"`
		} `json:"history"`
	}
	if err := json.Unmarshal([]byte(statsJSON), &payload); err != nil {
		t.Fatalf("decode stats JSON: %v\n%s", err, statsJSON)
	}
	if payload.RepoKey != "gh/example/repo" || payload.Sessions.Count != 1 || payload.History.Records != 1 || payload.GeneratedAt != "2026-07-01T12:00:00Z" {
		t.Fatalf("unexpected stats payload: %+v", payload)
	}
	if strings.Contains(out, "vectors:") || strings.Contains(out, "embedded") {
		t.Fatalf("history vectors ran without opt-in: %q", out)
	}
	for _, call := range runner.calls {
		if call.name == "entire" && strings.Contains(strings.Join(call.args, " "), "embed") {
			t.Fatalf("unexpected vector/provider call: %+v", call)
		}
	}
	if err := os.WriteFile(filepath.Join(brainDir, bmanifest.FileName), []byte("{malformed"), 0o600); err != nil {
		t.Fatal(err)
	}
	malformedOut, err := execute(t, NewRootCommand(opts), "refresh", "history")
	if err == nil {
		t.Fatal("malformed manifest accepted")
	}
	if malformedOut != "" {
		t.Fatalf("malformed history refresh produced output: %q", malformedOut)
	}
	if got, err := os.ReadFile(filepath.Join(brainDir, bmanifest.FileName)); err != nil || string(got) != "{malformed" {
		t.Fatalf("failed history refresh rewrote malformed manifest: %q, %v", got, err)
	}
}
