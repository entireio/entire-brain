package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestBrainManifestMigratesFlatExportAndPreservesSeed(t *testing.T) {
	outputDir := t.TempDir()
	seed := &seedSourceManifest{
		GeneratedAt:     time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		WorktreeMode:    "tracked",
		FileFingerprint: "sha256:seed",
		SummaryPath:     "seed/repo-overview.md",
	}
	if err := writeBrainSeedSource(outputDir, "gh/example/repo", seed); err != nil {
		t.Fatalf("write seed source: %v", err)
	}

	session := exportSession{
		SessionID:        "session-one",
		LatestCheckpoint: "aaa111aaa111",
		CreatedAt:        time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		TranscriptPath:   "sessions/main/session.jsonl",
	}
	export := exportManifest{
		SchemaVersion:      1,
		GeneratedAt:        time.Date(2026, 1, 2, 1, 0, 0, 0, time.UTC),
		RepoRoot:           "/repo",
		TranscriptMode:     "compact",
		Scope:              exportScopeAll,
		CheckpointLimit:    10,
		CheckpointsScanned: 1,
		Sessions:           []exportSession{session},
		Branches:           []exportBranch{{Branch: "main", Directory: "sessions/main", SessionCount: 1, Default: true}},
	}
	if err := writeBrainSessionSource(outputDir, "gh/example/repo", export); err != nil {
		t.Fatalf("write session source: %v", err)
	}

	manifest, err := loadBrainManifest(outputDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if manifest.SchemaVersion != brainManifestSchemaVersion {
		t.Fatalf("schema = %d, want %d", manifest.SchemaVersion, brainManifestSchemaVersion)
	}
	if manifest.Sources == nil || manifest.Sources.Seed == nil || manifest.Sources.Sessions == nil {
		t.Fatalf("manifest did not preserve both sources: %+v", manifest.Sources)
	}
	if len(manifest.Sessions) != 1 {
		t.Fatalf("legacy sessions alias missing: %+v", manifest.Sessions)
	}
	readme, err := os.ReadFile(filepath.Join(outputDir, exportReadmeFileName))
	if err != nil {
		t.Fatalf("read readme: %v", err)
	}
	if !strings.Contains(string(readme), "Seeded Baseline") || !strings.Contains(string(readme), "Session History") {
		t.Fatalf("combined readme missing sections:\n%s", readme)
	}
}

func TestBrainBriefJSONUsesSemanticContextAndLiveOverlay(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticBoundaryFixtureSnapshot())
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}

	if err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	storage, err := repoStoragePaths((&cobra.Command{}).Context(), runner, env, repoDir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	sessionDir := filepath.Join(storage.BrainDir, exportSessionsDirectory, "main")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatalf("mkdir session dir: %v", err)
	}
	sessionLine := `{"type":"agent_message","message":"Decision: ValidateToken review default scope is mainline; --base <ref> is an explicit override and uncommitted changes are included in the prompt."}` + "\n"
	if err := os.WriteFile(filepath.Join(sessionDir, "session.jsonl"), []byte(sessionLine), 0o600); err != nil {
		t.Fatalf("write session: %v", err)
	}
	if _, err := writeBrainHistoryIndexAndSource(storage.BrainDir, now, nil); err != nil {
		t.Fatalf("write history index: %v", err)
	}
	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{stdout: " M internal/auth/token.go\n?? notes.md\n"}
	runner.responses[fakeCommandKey("git", "status", "--porcelain", "--untracked-files=all")] = fakeCommandResponse{stdout: " M internal/auth/token.go\n?? notes.md\n"}
	runner.responses[fakeCommandKey("git", "diff", "--shortstat", "HEAD")] = fakeCommandResponse{stdout: " 1 file changed, 2 insertions(+)\n"}
	runner.responses[fakeCommandKey("git", "diff", "--name-status", "-M", "-C", "HEAD")] = fakeCommandResponse{stdout: "M\tinternal/auth/token.go\n"}

	cmd := NewRootCommand(opts)
	task := "ValidateToken"
	out, err := execute(t, cmd, "brief", task, "--json")
	if err != nil {
		t.Fatalf("brief: %v\n%s", err, out)
	}
	var report brainBriefReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse brief json: %v\n%s", err, out)
	}
	if report.Task != task {
		t.Fatalf("task = %q", report.Task)
	}
	if !report.Status.Sources.Semantic || !report.Status.Sources.History || !report.Status.Live.Dirty {
		t.Fatalf("brief did not report semantic source and dirty live state: %+v", report.Status)
	}
	if got := report.Status.Live.Unstaged; len(got) != 1 || got[0] != "internal/auth/token.go" {
		t.Fatalf("unstaged = %+v", got)
	}
	if got := report.Status.Live.Untracked; len(got) != 1 || got[0] != "notes.md" {
		t.Fatalf("untracked = %+v", got)
	}
	if len(report.Semantic.Context.Symbols) == 0 || report.Semantic.Context.Symbols[0].Name != "ValidateToken" {
		t.Fatalf("brief missing semantic context: %+v", report.Semantic.Context.Symbols)
	}
	if len(report.History.Matches) == 0 || !strings.Contains(report.History.Matches[0].Excerpt, "mainline") {
		t.Fatalf("brief missing ranked history match: %+v", report.History.Matches)
	}
	if len(report.Guidance) == 0 || !strings.Contains(strings.Join(report.Guidance, "\n"), "indexed snapshot") {
		t.Fatalf("brief missing snapshot guidance: %+v", report.Guidance)
	}
}

func TestBrainSearchShowAndInspectAliases(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	opts := Options{Version: "test", Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	runner.responses[fakeCommandKey("git", "diff", "--shortstat", "HEAD")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "diff", "--name-status", "-M", "-C", "HEAD")] = fakeCommandResponse{}

	cmd := NewRootCommand(opts)
	out, err := execute(t, cmd, "search", "ValidateToken", "--json")
	if err != nil {
		t.Fatalf("search: %v\n%s", err, out)
	}
	if !strings.Contains(out, `"name": "ValidateToken"`) {
		t.Fatalf("search output missing symbol:\n%s", out)
	}

	cmd = NewRootCommand(opts)
	out, err = execute(t, cmd, "show", "auth.ValidateToken", "--json")
	if err != nil {
		t.Fatalf("show: %v\n%s", err, out)
	}
	if !strings.Contains(out, `"qualified_name": "auth.ValidateToken"`) {
		t.Fatalf("show output missing record:\n%s", out)
	}

	cmd = NewRootCommand(opts)
	out, err = execute(t, cmd, "inspect", "code", "ValidateToken", "--json")
	if err != nil {
		t.Fatalf("inspect code: %v\n%s", err, out)
	}
	if !strings.Contains(out, `"name": "ValidateToken"`) {
		t.Fatalf("inspect code output missing symbol:\n%s", out)
	}
}

func TestBrainInspectDecisionsSearchesExportedText(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	opts := Options{Version: "test", Env: env, Runner: runner, Now: time.Now}
	storage, err := repoStoragePaths((&cobra.Command{}).Context(), runner, env, repoDir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(storage.BrainDir, exportSessionsDirectory, "main"), 0o700); err != nil {
		t.Fatalf("mkdir brain: %v", err)
	}
	if err := os.WriteFile(filepath.Join(storage.BrainDir, exportSessionsDirectory, "main", "session.jsonl"), []byte(`{"type":"agent_message","message":"Decision: keep prompt plus local validation instead of output schema."}`+"\n"), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	if _, err := writeBrainHistoryIndexAndSource(storage.BrainDir, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), nil); err != nil {
		t.Fatalf("write history index: %v", err)
	}
	manifest, err := loadBrainManifest(storage.BrainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if manifest.Sources == nil || manifest.Sources.History == nil || manifest.Sources.History.Decisions != 1 {
		t.Fatalf("history source missing decision count: %+v", manifest.Sources)
	}

	cmd := NewRootCommand(opts)
	out, err := execute(t, cmd, "inspect", "decisions", "output schema", "--json")
	if err != nil {
		t.Fatalf("inspect decisions: %v\n%s", err, out)
	}
	var report brainHistoryInspectReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse report: %v\n%s", err, out)
	}
	if len(report.Matches) != 1 || !strings.Contains(report.Matches[0].Excerpt, "Decision:") {
		t.Fatalf("unexpected matches: %+v", report.Matches)
	}
}

func TestBrainInspectParsesStructuredSessionHistory(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	opts := Options{Version: "test", Env: env, Runner: runner, Now: time.Now}
	storage, err := repoStoragePaths((&cobra.Command{}).Context(), runner, env, repoDir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	sessionDir := filepath.Join(storage.BrainDir, exportSessionsDirectory, "main")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatalf("mkdir brain: %v", err)
	}
	events := []map[string]any{
		{
			"type": "session_meta",
			"payload": map[string]any{
				"base_instructions": "Decision: fake metadata should not be indexed.",
			},
		},
		{
			"type": "response_item",
			"payload": map[string]any{
				"type": "message",
				"role": "assistant",
				"content": []map[string]any{{
					"type": "text",
					"text": "Implemented the manual_commit AttributionBaseCommit invariant because condensation must keep attribution stable.",
				}},
			},
		},
		{
			"type": "response_item",
			"payload": map[string]any{
				"type": "message",
				"role": "assistant",
				"content": []map[string]any{{
					"type": "text",
					"text": "Implemented the manual_commit AttributionBaseCommit invariant because condensation must keep attribution stable.",
				}},
			},
		},
		{
			"type": "response_item",
			"payload": map[string]any{
				"type": "message",
				"role": "assistant",
				"content": []map[string]any{{
					"type": "text",
					"text": "I'll treat the exported logs as the source of truth before reading current code.",
				}},
			},
		},
		{
			"type": "response_item",
			"payload": map[string]any{
				"type": "message",
				"role": "assistant",
				"content": []map[string]any{{
					"type": "text",
					"text": "Updated the parser because the old output was noisy.",
				}},
			},
		},
		{
			"type": "user",
			"message": map[string]any{
				"role": "user",
				"content": []map[string]any{{
					"type": "tool_result",
					"content": strings.Join([]string{
						"state.BaseCommit = newHead",
						"// Keep AttributionBaseCommit in sync to prevent stale base drift.",
						"// Without this, a subsequent condensation would diff from the old base,",
						"// inflating human_added with lines from unrelated prior commits.",
						"state.RealignAttributionBase(newHead)",
					}, "\n"),
				}},
			},
		},
		{
			"type": "user",
			"message": map[string]any{
				"role": "user",
				"content": []map[string]any{{
					"type": "tool_result",
					"content": strings.Join([]string{
						"func resolveTranscriptPath(state *SessionState) (string, error) {",
						"\t// Update state so subsequent reads use the correct path.",
						"\tstate.TranscriptPath = resolved",
						"\treturn resolved, nil",
						"}",
					}, "\n"),
				}},
			},
		},
		{
			"type": "response_item",
			"payload": map[string]any{
				"type":      "function_call",
				"name":      "exec_command",
				"arguments": map[string]any{"cmd": "go test ./cmd/entire/cli/strategy"},
			},
		},
		{
			"type": "response_item",
			"payload": map[string]any{
				"type":  "custom_tool_call",
				"name":  "apply_patch",
				"input": "*** Begin Patch\n*** Update File: README.md\n+go test ./...\n",
			},
		},
	}
	var transcript strings.Builder
	for _, event := range events {
		data, err := json.Marshal(event)
		if err != nil {
			t.Fatalf("marshal event: %v", err)
		}
		transcript.Write(data)
		transcript.WriteByte('\n')
	}
	transcript.WriteString(`  "output": "Decision: fake pretty JSON output should not be indexed."` + "\n")
	if err := os.WriteFile(filepath.Join(sessionDir, "session.jsonl"), []byte(transcript.String()), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	source, err := writeBrainHistoryIndexAndSource(storage.BrainDir, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("write history index: %v", err)
	}
	if source.Decisions != 1 || source.Validations != 1 || source.ToolCalls != 2 {
		t.Fatalf("unexpected history source counts: %+v", source)
	}
	if source.CodeFacts != 2 {
		t.Fatalf("unexpected code fact count: %+v", source)
	}

	for _, tc := range []struct {
		args    []string
		want    string
		wantLen int
	}{
		{args: []string{"inspect", "decisions", "manual commit", "--json"}, want: "AttributionBaseCommit", wantLen: 1},
		{args: []string{"inspect", "decisions", "manual stable", "--json"}, wantLen: 0},
		{args: []string{"inspect", "decisions", "fake metadata", "--json"}, wantLen: 0},
		{args: []string{"inspect", "validation", "go test", "--json"}, want: "go test", wantLen: 1},
		{args: []string{"inspect", "tool-paths", "apply_patch", "--json"}, want: "apply_patch", wantLen: 1},
		{args: []string{"inspect", "architecture", "AttributionBaseCommit", "--json"}, want: "invariant", wantLen: 2},
		{args: []string{"inspect", "history", "restore manual commit attribution base drift behavior", "--json"}, want: "AttributionBaseCommit", wantLen: 2},
		{args: []string{"inspect", "history", "transcript re-resolution updates state for subsequent reads", "--json"}, want: "state.TranscriptPath = resolved", wantLen: 1},
	} {
		cmd := NewRootCommand(opts)
		out, err := execute(t, cmd, tc.args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", tc.args, err, out)
		}
		var report brainHistoryInspectReport
		if err := json.Unmarshal([]byte(out), &report); err != nil {
			t.Fatalf("parse report for %v: %v\n%s", tc.args, err, out)
		}
		if len(report.Matches) != tc.wantLen {
			t.Fatalf("%v returned %d matches, want %d: %+v", tc.args, len(report.Matches), tc.wantLen, report.Matches)
		}
		if tc.want != "" && !strings.Contains(report.Matches[0].Excerpt, tc.want) {
			t.Fatalf("%v first match missing %q: %+v", tc.args, tc.want, report.Matches[0])
		}
	}
}

func TestRankHistoryRecordsUsesIdentifierTerms(t *testing.T) {
	index := historyIndex{Records: []historyRecord{
		{
			Kind:    "decision",
			Path:    "sessions/main/generic.jsonl",
			Line:    10,
			Summary: "Final state: drop stale Strip references in review scanner tests.",
		},
		{
			Kind:    "tool_call",
			Path:    "sessions/main/review-env.jsonl",
			Line:    20,
			Summary: "AppendReviewEnv strips stale ENTIRE_REVIEW_* and ENTIRE_INVESTIGATE_* entries before appending fresh values.",
		},
	}}

	records := rankHistoryRecords(index, "history", "Strip stale Entire review provenance before setting fresh ENTIRE_REVIEW_* values.", 2)
	if len(records) == 0 {
		t.Fatal("expected ranked history records")
	}
	if records[0].Path != "sessions/main/review-env.jsonl" {
		t.Fatalf("identifier-specific record did not rank first: %+v", records)
	}

	records = rankHistoryRecords(index, "history", "ENTIRE_REVIEW_*", 2)
	if len(records) == 0 || records[0].Path != "sessions/main/review-env.jsonl" {
		t.Fatalf("identifier-only query did not rank env record first: %+v", records)
	}
}

func TestLoadBrainManifestMigratesLegacyFlatManifest(t *testing.T) {
	outputDir := t.TempDir()
	legacy := exportManifest{
		SchemaVersion:      1,
		GeneratedAt:        time.Date(2026, 1, 2, 1, 0, 0, 0, time.UTC),
		TranscriptMode:     "raw",
		Scope:              exportScopeBranch,
		CheckpointLimit:    5,
		CheckpointsScanned: 1,
		Sessions: []exportSession{{
			SessionID:        "session-one",
			LatestCheckpoint: "aaa111aaa111",
			CreatedAt:        time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
			TranscriptPath:   "sessions/main/session.jsonl",
		}},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal legacy: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outputDir, exportManifestFileName), data, 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}

	manifest, err := loadBrainManifest(outputDir)
	if err != nil {
		t.Fatalf("load legacy: %v", err)
	}
	if manifest.Sources == nil || manifest.Sources.Sessions == nil {
		t.Fatalf("legacy manifest did not get session source: %+v", manifest)
	}
	if manifest.Sources.Sessions.TranscriptMode != "raw" {
		t.Fatalf("transcript mode = %q, want raw", manifest.Sources.Sessions.TranscriptMode)
	}
}
