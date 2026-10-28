package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/entireio/entire-brain/internal/cli/internal/historystore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

// The default refresh exports the CLI's compact transcript.jsonl (Entire
// Transcript Format v1, see entireio/cli transcript/README.md): every agent's
// turns become {"v":1,"type":"user"|"assistant","content":[...]} with the
// content blocks at the record's top level and tool output inlined as
// tool_use.result. The history and conversation readers only read the nested
// claude-code "message" field, so these sessions indexed to zero exchanges and
// almost no history records. distillConversationText already reads both
// shapes; these fixtures pin the same dialect for the history side, for each
// agent whose converter writes it (claude-code, codex, pi). They are built
// from the format spec, not captured from live agent sessions.

const compactClaudeTranscriptFixture = `{"v":1,"agent":"claude-code","cli_version":"0.12.2","type":"user","ts":"2026-10-01T10:00:00Z","content":[{"text":"<system-reminder>wrapper that must not open an exchange</system-reminder>"}]}
{"v":1,"agent":"claude-code","cli_version":"0.12.2","type":"user","ts":"2026-10-01T10:00:01Z","content":[{"id":"u1","text":"Fix   the flaky lock test\non Windows"}]}
{"v":1,"agent":"claude-code","cli_version":"0.12.2","type":"assistant","ts":"2026-10-01T10:00:05Z","id":"msg_1","model":"claude-sonnet-4","input_tokens":10,"output_tokens":20,"content":[{"type":"text","text":"Looking at the lock timeout now."},{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"go test ./..."},"result":{"output":"state.transcriptPath = \"sessions/main\"\nFAIL TestLock SQLITE_BUSY raw tool output must never leak","status":"error"}}]}
{"v":1,"agent":"claude-code","cli_version":"0.12.2","type":"assistant","ts":"2026-10-01T10:00:09Z","id":"msg_2","content":[{"type":"text","text":"Root cause: the write lock timeout is too short on Windows; raising it."}]}
{"v":1,"agent":"claude-code","cli_version":"0.12.2","type":"user","ts":"2026-10-01T10:01:00Z","content":[{"text":"Now update the docs"}]}
{"v":1,"agent":"claude-code","cli_version":"0.12.2","type":"assistant","ts":"2026-10-01T10:01:05Z","id":"msg_3","content":"Docs updated."}
`

const compactCodexTranscriptFixture = `{"v":1,"agent":"codex","cli_version":"0.12.2","type":"user","ts":"2026-10-01T10:00:00Z","content":[{"text":"Why does the export cursor skip sessions?"}]}
{"v":1,"agent":"codex","cli_version":"0.12.2","type":"assistant","ts":"2026-10-01T10:00:05Z","model":"gpt-5-codex","input_tokens":10,"output_tokens":20,"content":[{"type":"tool_use","id":"call_1","name":"exec_command","input":{"cmd":"grep -rn cursor internal/cli"},"result":{"output":"state.transcriptPath = \"sessions/main\"","status":"success"}},{"type":"text","text":"The cursor reuses unchanged transcripts; only new checkpoints are exported."}]}
{"v":1,"agent":"codex","cli_version":"0.12.2","type":"assistant","ts":"2026-10-01T10:00:06Z","input_tokens":5,"output_tokens":7,"content":[]}
{"v":1,"agent":"codex","cli_version":"0.12.2","type":"user","ts":"2026-10-01T10:01:00Z","content":[{"text":"Now patch it"}]}
{"v":1,"agent":"codex","cli_version":"0.12.2","type":"assistant","ts":"2026-10-01T10:01:05Z","content":[{"type":"tool_use","id":"call_2","name":"apply_patch","input":{"input":"*** Begin Patch\n*** Update File: internal/cli/export.go\n*** End Patch"},"result":{"output":"Success. Updated the following files:\nM internal/cli/export.go","status":"success"}},{"type":"text","text":"Patched the export cursor."}]}
`

const compactPiTranscriptFixture = `{"v":1,"agent":"pi","cli_version":"0.12.2","type":"user","ts":"2026-10-01T10:00:00Z","content":[{"type":"image","data":"entire-asset:assets/screenshot.png","mimeType":"image/png"},{"text":"Summarize the retry policy"}]}
{"v":1,"agent":"pi","cli_version":"0.12.2","type":"assistant","ts":"2026-10-01T10:00:05Z","model":"claude-sonnet-4","content":[{"type":"tool_use","id":"tc_1","name":"Read","input":{"path":"retry.go"},"result":{"output":"state.transcriptPath = \"sessions/main\"\nraw tool result must never leak","status":"success"}},{"type":"text","text":"Retries use exponential backoff with a 5-attempt cap."}]}
`

type compactTranscriptCase struct {
	name      string
	fixture   string
	requests  []string
	responses []string
	tools     [][]string
	leaked    string
}

func compactTranscriptCases() []compactTranscriptCase {
	return []compactTranscriptCase{
		{
			name: "claude-code", fixture: compactClaudeTranscriptFixture,
			requests:  []string{"Fix the flaky lock test on Windows", "Now update the docs"},
			responses: []string{"Looking at the lock timeout now.\nRoot cause: the write lock timeout is too short on Windows; raising it.", "Docs updated."},
			tools:     [][]string{{"Bash"}, nil},
			leaked:    "raw tool output",
		},
		{
			name: "codex", fixture: compactCodexTranscriptFixture,
			requests:  []string{"Why does the export cursor skip sessions?", "Now patch it"},
			responses: []string{"The cursor reuses unchanged transcripts; only new checkpoints are exported.", "Patched the export cursor."},
			tools:     [][]string{{"exec_command"}, {"apply_patch"}},
			leaked:    "transcriptPath",
		},
		{
			name: "pi", fixture: compactPiTranscriptFixture,
			requests:  []string{"Summarize the retry policy"},
			responses: []string{"Retries use exponential backoff with a 5-attempt cap."},
			tools:     [][]string{{"Read"}},
			leaked:    "raw tool result",
		},
	}
}

func TestScanConversationCompactTranscript(t *testing.T) {
	for _, c := range compactTranscriptCases() {
		t.Run(c.name, func(t *testing.T) {
			scan, err := scanConversationTranscript(writeConversationFixture(t, c.fixture))
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if len(scan.Exchanges) != len(c.requests) || scan.Incomplete != 0 {
				t.Fatalf("exchanges = %d incomplete = %d, want %d/0: %+v", len(scan.Exchanges), scan.Incomplete, len(c.requests), scan.Exchanges)
			}
			for i, exchange := range scan.Exchanges {
				if exchange.Request != c.requests[i] {
					t.Errorf("exchange %d request = %q, want %q", i+1, exchange.Request, c.requests[i])
				}
				if exchange.Response != c.responses[i] {
					t.Errorf("exchange %d response = %q, want %q", i+1, exchange.Response, c.responses[i])
				}
				if strings.Contains(exchange.Response, c.leaked) {
					t.Errorf("exchange %d leaked tool output into the response: %q", i+1, exchange.Response)
				}
				if !slices.Equal(exchange.ToolNames, c.tools[i]) {
					t.Errorf("exchange %d tool names = %v, want %v", i+1, exchange.ToolNames, c.tools[i])
				}
			}
		})
	}
}

func TestExtractHistoryJSONFragmentsCompactTranscript(t *testing.T) {
	for _, c := range compactTranscriptCases() {
		t.Run(c.name, func(t *testing.T) {
			sources := map[string]int{}
			var requests []string
			for line := range strings.SplitSeq(strings.TrimSpace(c.fixture), "\n") {
				var obj map[string]any
				if err := json.Unmarshal([]byte(line), &obj); err != nil {
					t.Fatal(err)
				}
				for _, fragment := range extractHistoryJSONFragments(obj) {
					sources[fragment.Source]++
					if fragment.Source == "user_prompt" {
						requests = append(requests, fragment.Text)
					}
				}
			}
			if len(requests) != len(c.requests) {
				t.Errorf("user_prompt fragments = %q, want %d (wrapper turns filtered)", requests, len(c.requests))
			}
			if sources["assistant_message"] == 0 {
				t.Errorf("no assistant_message fragments: %v", sources)
			}
			for _, names := range c.tools {
				for _, name := range names {
					if sources[historyToolCallSource(name)] == 0 {
						t.Errorf("no %s fragment: %v", historyToolCallSource(name), sources)
					}
				}
			}
			// Compact transcripts inline tool output as tool_use.result, the
			// only place its code facts survive.
			if sources["tool_result_fact"] == 0 {
				t.Errorf("tool_use.result output not mined for code facts: %v", sources)
			}
		})
	}
}

func TestHistoryIndexBuildsRecordsFromCompactTranscript(t *testing.T) {
	for _, c := range compactTranscriptCases() {
		t.Run(c.name, func(t *testing.T) {
			brainDir, source := buildConversationBrainFixtureWithTranscript(t, "sess-"+c.name, c.fixture)
			if source.Exchanges != len(c.requests) || source.IncompleteExchanges != 0 {
				t.Fatalf("manifest counts = %d exchanges / %d incomplete, want %d/0", source.Exchanges, source.IncompleteExchanges, len(c.requests))
			}
			if records := conversationRecordsFromIndex(t, brainDir, source); len(records) != len(c.requests) {
				t.Fatalf("exchange records = %d, want %d", len(records), len(c.requests))
			}
			index, err := historystore.LoadIndex(brainDir, source)
			if err != nil {
				t.Fatal(err)
			}
			kinds := map[string]int{}
			for _, record := range index.Records {
				kinds[record.Kind]++
			}
			for _, kind := range []string{"request", "tool_call", "code_fact", conversationKind} {
				if kinds[kind] == 0 {
					t.Errorf("no %s records from a compact %s transcript: %v", kind, c.name, kinds)
				}
			}
		})
	}
}

// TestHistoryIndexesDefaultCompactExport runs the path real users hit: a v1
// checkpoint carrying both the native full.jsonl and the CLI's compact
// transcript.jsonl, exported with the default (compact) transcript mode.
// Every other export fixture runs raw or has no compact transcript.
func TestHistoryIndexesDefaultCompactExport(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	root := checkpointPath("aaa111aaa111")
	files := map[string]string{
		".entire/settings.json":      `{"enabled":true}`,
		root + "/metadata.json":      `{"branch":"main","sessions":[{"metadata":"/` + root + `/0/metadata.json","transcript":"/` + root + `/0/full.jsonl","compact_transcript":"/` + root + `/0/transcript.jsonl"}]}`,
		root + "/0/metadata.json":    `{"checkpoint_id":"aaa111aaa111","session_id":"session-one","branch":"main","created_at":"2026-01-01T00:00:00Z"}`,
		root + "/0/full.jsonl":       conversationClaudeFixture,
		root + "/0/transcript.jsonl": compactClaudeTranscriptFixture,
	}
	for rel, data := range files {
		full := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("add", ".")
	git("-c", "commit.gpgsign=false", "commit", "-qm", "checkpoint fixture")
	git("update-ref", v1MainRef, "HEAD")

	opts := Options{Version: "test", Runner: ExecRunner{}, Env: EntireEnv{RepoRoot: repo, PluginDataDir: t.TempDir(), PluginCacheDir: t.TempDir(), PluginStateDir: t.TempDir(), PluginConfigDir: t.TempDir()}, Now: func() time.Time { return time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC) }}
	cmd := &cobra.Command{}
	cmd.SetOut(io.Discard)
	if err := runExport(context.Background(), cmd, opts, exportCommandOptions{scope: exportScopeAll, entireBinary: "entire", historyIndex: true}); err != nil {
		t.Fatal(err)
	}
	storage, err := repoStoragePaths(context.Background(), opts.Runner, opts.Env, repo)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := bmanifest.Load(storage.BrainDir)
	if err != nil {
		t.Fatal(err)
	}
	if mode := manifest.Sources.Sessions.TranscriptMode; mode != "compact" {
		t.Fatalf("transcript mode = %q, want the default compact export", mode)
	}
	index, _, _, err := buildBrainHistoryIndexSnapshotContext(context.Background(), storage.BrainDir, opts.Now(), nil, manifest)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, record := range index.Records {
		kinds[record.Kind]++
	}
	if kinds[conversationKind] != 2 {
		t.Errorf("conversation exchanges = %d, want 2 (records by kind: %v)", kinds[conversationKind], kinds)
	}
	for _, kind := range []string{"request", "tool_call", "learning"} {
		if kinds[kind] == 0 {
			t.Errorf("no %s records from the default compact export: %v", kind, kinds)
		}
	}
}
