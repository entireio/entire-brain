package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseExpansion(t *testing.T) {
	if got := parseExpansion("# notes\nbrain path repo key plugin data dir storage\n"); got != "brain path repo key plugin data dir storage" {
		t.Fatalf("got %q", got)
	}
	if got := parseExpansion("\n\n  one   two  three \n"); got != "one two three" {
		t.Fatalf("whitespace collapse failed: %q", got)
	}
	if parseExpansion("") != "" {
		t.Fatalf("empty should be empty")
	}
}

func TestExpandedQuery(t *testing.T) {
	if got := expandedQuery("add path command", "brain repo key storage"); got != "add path command brain repo key storage" {
		t.Fatalf("got %q", got)
	}
	if got := expandedQuery("add path command", "  "); got != "add path command" {
		t.Fatalf("empty expansion should leave query unchanged, got %q", got)
	}
}

func TestExpandQueryCaches(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "exp.json")
	cache := loadExpansionCache(cachePath)
	calls := 0
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		calls++
		return "term one two three\n", nil
	}
	got, err := expandQuery(context.Background(), run, []string{"fake"}, "/repo", "a query", cache)
	if err != nil || got != "term one two three" {
		t.Fatalf("expandQuery: %q err=%v", got, err)
	}
	// Second call for the same query is served from cache (no agent call).
	if _, err := expandQuery(context.Background(), run, []string{"fake"}, "/repo", "a query", cache); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 agent call, cache should serve the rest, got %d", calls)
	}
}

func TestRunFactsEvalWithExpander(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	p := normalizeFactPaths([]string{"architecture.data.flow"})
	// Fact text shares no words with the task, but does with the expansion.
	f := factRecord{ID: factRecordID("the mirror ref reconciliation", p), Paths: p, Text: "the mirror ref reconciliation", Branch: "main", Status: factStatusActive, UpdatedAt: now}
	if err := writeFacts(brainDir, "main", []factRecord{f}); err != nil {
		t.Fatal(err)
	}
	tasks := []evalTask{{ID: "t1", Task: "fix the sync bug", Branch: "main", Relevant: []string{f.ID}}}

	// Without expansion the query "fix the sync bug" doesn't match the fact.
	base, err := runFactsEval(context.Background(), Options{}, brainDir, "/repo", "main", tasks, 10, false, nil, nil, loadJudgeCache(""), nil, nil, evalRetrieverFacts)
	if err != nil {
		t.Fatal(err)
	}
	if base[0].RelevantSurfaced != 0 {
		t.Fatalf("base query should not surface the fact, got %d", base[0].RelevantSurfaced)
	}

	// An expander that adds the fact's vocabulary lets recall find it.
	expander := func(query string) (string, error) {
		time.Sleep(20 * time.Millisecond)
		return "mirror ref reconciliation", nil
	}
	exp, err := runFactsEval(context.Background(), Options{}, brainDir, "/repo", "main", tasks, 10, false, nil, nil, loadJudgeCache(""), expander, nil, evalRetrieverFacts)
	if err != nil {
		t.Fatal(err)
	}
	if exp[0].RelevantSurfaced != 1 {
		t.Fatalf("expansion should surface the fact, got %d", exp[0].RelevantSurfaced)
	}
	if exp[0].ExpansionLatencyMS <= 0 {
		t.Fatalf("expansion latency should be captured, got %+v", exp[0])
	}
	if exp[0].EndToEndLatencyMS < exp[0].ExpansionLatencyMS || exp[0].EndToEndLatencyMS < exp[0].LatencyMS {
		t.Fatalf("end-to-end latency should include expansion and retrieval, got %+v", exp[0])
	}
	data, err := json.Marshal(summarizeEval(exp))
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		MeanExpansionLatencyMS float64 `json:"mean_expansion_latency_ms"`
		MeanEndToEndLatencyMS  float64 `json:"mean_end_to_end_latency_ms"`
		Results                []struct {
			ExpansionLatencyMS int64 `json:"expansion_latency_ms"`
			EndToEndLatencyMS  int64 `json:"end_to_end_latency_ms"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.MeanExpansionLatencyMS <= 0 || payload.MeanEndToEndLatencyMS <= 0 || len(payload.Results) != 1 || payload.Results[0].ExpansionLatencyMS <= 0 || payload.Results[0].EndToEndLatencyMS <= 0 {
		t.Fatalf("summary JSON should include additive latency fields: %s", data)
	}
}

func TestRecallExpandOllamaUsesLoopbackRunner(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
	const wantModel = "llama3.2:test"
	type ollamaRequest struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
		Stream bool   `json:"stream"`
	}
	seen := make(chan ollamaRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/generate" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var req ollamaRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		seen <- req
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"response":"mirror ref reconciliation\n"}`))
	}))
	defer server.Close()
	t.Setenv("ENTIRE_BRAIN_OLLAMA_URL", server.URL+"/api/generate")

	repoDir := t.TempDir()
	dataDir := t.TempDir()
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "rev-parse", "--show-toplevel"): {stdout: repoDir + "\n"},
		fakeCommandKey("git", "remote", "get-url", "origin"):  {stdout: "git@github.com:example/repo.git\n"},
		fakeCommandKey("git", "branch", "--show-current"):     {stdout: "main\n"},
	}}
	opts := Options{
		Version: "test",
		Env: EntireEnv{
			RepoRoot:      repoDir,
			PluginDataDir: dataDir,
		},
		Runner: runner,
		Now:    func() time.Time { return now },
	}
	storage, err := repoStoragePaths(context.Background(), runner, opts.Env, repoDir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	paths := normalizeFactPaths([]string{"architecture.data.flow"})
	fact := factRecord{
		ID:        factRecordID("the mirror ref reconciliation", paths),
		Paths:     paths,
		Text:      "the mirror ref reconciliation",
		Branch:    "main",
		Status:    factStatusActive,
		UpdatedAt: now,
	}
	if err := writeFacts(storage.BrainDir, "main", []factRecord{fact}); err != nil {
		t.Fatal(err)
	}
	if err := writeBrainManifestAndReadme(storage.BrainDir, exportManifest{SchemaVersion: brainManifestSchemaVersion, GeneratedAt: now}); err != nil {
		t.Fatal(err)
	}

	out, err := execute(t, NewRootCommand(opts), "recall", "sync bug", "--expand", "--agent", "ollama", "--model", wantModel, "--json")
	if err != nil {
		t.Fatalf("recall --expand ollama: %v\n%s", err, out)
	}
	if !strings.Contains(out, fact.ID) {
		t.Fatalf("expanded recall did not surface expected fact:\n%s", out)
	}
	select {
	case req := <-seen:
		if req.Model != wantModel {
			t.Fatalf("ollama model = %q, want %q", req.Model, wantModel)
		}
		if req.Stream {
			t.Fatal("recall ollama expansion must request non-streaming output")
		}
		if !strings.Contains(req.Prompt, "sync bug") {
			t.Fatalf("query not sent to ollama prompt: %q", req.Prompt)
		}
	default:
		t.Fatal("ollama server did not receive expansion request")
	}
	for _, call := range runner.calls {
		if call.name == "ollama" {
			t.Fatalf("recall must use loopback HTTP runner, not PATH ollama binary: %+v", call)
		}
	}
	if _, err := os.Stat(filepath.Join(dataDir, repoStoreDirName, "gh", "example", "repo")); err != nil {
		t.Fatalf("brain dir missing: %v", err)
	}
}
