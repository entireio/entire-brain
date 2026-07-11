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

// emptyEmbedder stands in for an unavailable backend (e.g. Ollama down): every
// call returns a nil vector, the failure mode the vector-rank guards must handle.
type emptyEmbedder struct{}

func (emptyEmbedder) Embed(string) []float32 { return nil }
func (emptyEmbedder) Dim() int               { return 768 }
func (emptyEmbedder) ID() string             { return "empty-embedder" }

func TestVectorRankedReturnsNothingWhenEmbedderUnavailable(t *testing.T) {
	dir := t.TempDir()
	facts := []factRecord{{ID: "fact:a", Text: "alpha"}, {ID: "fact:b", Text: "beta"}}
	if out := factsVectorRanked(dir, "main", facts, "q", emptyEmbedder{}, 10); len(out) != 0 {
		t.Fatalf("facts: empty embedder must yield no semantic results, got %d (arbitrary top-N)", len(out))
	}
	idx := docIndex{Records: []docRecord{{ID: "d1", Text: "x"}, {ID: "d2", Text: "y"}}}
	if out := docsVectorRanked(dir, idx, "q", emptyEmbedder{}, 10); len(out) != 0 {
		t.Fatalf("docs: empty embedder must yield no semantic results, got %d", len(out))
	}
}

func TestGetUnifiedBatchPreservesOrderAndReportsMissing(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, docDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	idx := docIndex{Records: []docRecord{{ID: "aaa", Text: "alpha"}, {ID: "bbb", Text: "beta"}}}
	data, _ := json.Marshal(idx)
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(docIndexPath)), data, 0o600); err != nil {
		t.Fatal(err)
	}
	found, missing, err := getUnifiedBatch("", dir, "main", []string{"doc:bbb", "doc:nope", "doc:aaa"})
	if err != nil {
		t.Fatalf("getUnifiedBatch: %v", err)
	}
	if len(found) != 2 || found[0].ID != "doc:bbb" || found[1].ID != "doc:aaa" {
		t.Fatalf("expected input-order [doc:bbb doc:aaa], got %+v", found)
	}
	if len(missing) != 1 || missing[0] != "doc:nope" {
		t.Fatalf("expected missing [doc:nope], got %v", missing)
	}
}

func TestUnifiedIDsNotDoublePrefixed(t *testing.T) {
	// fact/history ids are already source-prefixed; docs are not.
	f := factsToUnified([]factRecord{{ID: "fact:abc", Text: "x"}})
	if f[0].ID != "fact:abc" {
		t.Fatalf("fact id double-prefixed: %s", f[0].ID)
	}
	h := historyToUnified([]scoredHistoryRecord{{Record: historyRecord{ID: "history:def", Summary: "y"}}})
	if h[0].ID != "history:def" {
		t.Fatalf("history id double-prefixed: %s", h[0].ID)
	}
	if d := docToUnified(docRecord{ID: "ghi", Text: "z"}); d.ID != "doc:ghi" {
		t.Fatalf("doc id: %s", d.ID)
	}
}

func TestRRFMergeUnifiedFavorsCrossListMatches(t *testing.T) {
	lists := [][]unifiedResult{
		{{ID: "a"}, {ID: "b"}},
		{{ID: "b"}, {ID: "c"}},
	}
	out := rrfMergeUnified(lists, 10)
	if len(out) != 3 || out[0].ID != "b" {
		t.Fatalf("expected b (in both lists) ranked first, got %v", out)
	}
}

func TestFactsVectorRankedRanksActiveButCachesAll(t *testing.T) {
	dir := t.TempDir()
	e := defaultEmbedder()
	facts := []factRecord{
		{ID: "fact:act", Text: "active checkpoint logic", Status: factStatusActive},
		{ID: "fact:sup", Text: "superseded note", Status: factStatusSuperseded},
	}
	out := factsVectorRanked(dir, "main", facts, "checkpoint", e, 10)
	// vsearch ranks active facts only.
	if len(out) != 1 || out[0].ID != "fact:act" {
		t.Fatalf("expected only the active fact ranked, got %v", out)
	}
	// But every present fact is cached on the shared store, matching the reranker's
	// retain-all convention (so vsearch doesn't churn the recall/brief cache).
	cache := newEmbedStore(dir, "main", e.ID(), e.Dim()).load()
	if _, ok := cache["fact:act"]; !ok {
		t.Fatal("active fact vector should be cached")
	}
	if _, ok := cache["fact:sup"]; !ok {
		t.Fatal("superseded fact vector should still be cached, not evicted")
	}
}

func TestPruneToPresentDropsDeparted(t *testing.T) {
	cache := map[string][]float32{"a": {1}, "b": {2}, "gone": {3}}
	present := map[string]struct{}{"a": {}, "b": {}}
	if !pruneToPresent(cache, present) {
		t.Fatal("expected pruneToPresent to report a removal")
	}
	if _, ok := cache["gone"]; ok {
		t.Fatal("departed id should be pruned")
	}
	if len(cache) != 2 {
		t.Fatalf("expected 2 entries after prune, got %d", len(cache))
	}
}

func TestQMDOutputFormatAlias(t *testing.T) {
	got, err := outputWantsJSON(false, "json")
	if err != nil || !got {
		t.Fatalf("--format json should request JSON, got %t err=%v", got, err)
	}
	got, err = outputWantsJSON(true, "cli")
	if err != nil || got {
		t.Fatalf("--format cli should force CLI output, got %t err=%v", got, err)
	}
	if _, err := outputWantsJSON(false, "text"); err == nil {
		t.Fatal("--format text should be rejected; supported formats are json or cli")
	}
	if _, err := outputWantsJSON(false, "xml"); err == nil {
		t.Fatal("unknown --format should error")
	}
}

func TestQMDAliasesAcrossRetrievalVerbs(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}
	storage, err := repoStoragePaths((&cobra.Command{}).Context(), runner, env, repoDir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	paths := normalizeFactPaths([]string{"architecture.data.flow"})
	facts := []factRecord{
		{ID: factRecordID("alpha checkpoint retrieval contract", paths), Text: "alpha checkpoint retrieval contract", Paths: paths, Branch: "feature", Status: factStatusActive, UpdatedAt: now},
		{ID: factRecordID("alpha checkpoint second result", paths), Text: "alpha checkpoint second result", Paths: paths, Branch: "feature", Status: factStatusActive, UpdatedAt: now},
	}
	if err := writeFacts(storage.BrainDir, "feature", facts); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name       string
		args       []string
		wantResult bool
		wantLimit  bool
	}{
		{name: "search short number json", args: []string{"search", "alpha checkpoint", "-n", "1", "--format", "json"}, wantResult: true, wantLimit: true},
		{name: "query long number json", args: []string{"query", "alpha checkpoint", "--number", "1", "--format", "json"}, wantResult: true, wantLimit: true},
		{name: "vsearch format json", args: []string{"vsearch", "alpha checkpoint", "-n", "1", "--format", "json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := execute(t, NewRootCommand(opts), tc.args...)
			if err != nil {
				t.Fatalf("%s: %v\n%s", strings.Join(tc.args, " "), err, out)
			}
			var payload struct {
				Branch  string          `json:"branch"`
				Results []unifiedResult `json:"results"`
			}
			if err := json.Unmarshal([]byte(out), &payload); err != nil {
				t.Fatalf("decode retrieval JSON: %v\n%s", err, out)
			}
			if payload.Branch != "feature" {
				t.Fatalf("branch = %q, want feature in %s payload", payload.Branch, tc.name)
			}
			if tc.wantResult && len(payload.Results) == 0 {
				t.Fatalf("expected results for %s, got none", tc.name)
			}
			if tc.wantLimit && len(payload.Results) != 1 {
				t.Fatalf("number alias should limit results to 1, got %d: %+v", len(payload.Results), payload.Results)
			}
			if len(payload.Results) > 1 {
				t.Fatalf("number alias should cap results at 1, got %d", len(payload.Results))
			}
		})
	}

	getOut, err := execute(t, NewRootCommand(opts), "get", facts[0].ID, "--format", "json")
	if err != nil {
		t.Fatalf("get --format json: %v\n%s", err, getOut)
	}
	var getPayload struct {
		Branch  string          `json:"branch"`
		Results []unifiedResult `json:"results"`
		Missing []string        `json:"missing"`
	}
	if err := json.Unmarshal([]byte(getOut), &getPayload); err != nil {
		t.Fatalf("decode get JSON: %v\n%s", err, getOut)
	}
	if len(getPayload.Results) != 1 || getPayload.Results[0].ID != facts[0].ID || len(getPayload.Missing) != 0 {
		t.Fatalf("unexpected get payload: %+v", getPayload)
	}
	if getPayload.Branch != "feature" {
		t.Fatalf("get branch = %q, want feature", getPayload.Branch)
	}

	multiOut, err := execute(t, NewRootCommand(opts), "multi-get", facts[1].ID, "fact:missing", "--format", "json")
	if err != nil {
		t.Fatalf("multi-get --format json: %v\n%s", err, multiOut)
	}
	var multiPayload struct {
		Branch  string          `json:"branch"`
		Results []unifiedResult `json:"results"`
		Missing []string        `json:"missing"`
	}
	if err := json.Unmarshal([]byte(multiOut), &multiPayload); err != nil {
		t.Fatalf("decode multi-get JSON: %v\n%s", err, multiOut)
	}
	if len(multiPayload.Results) != 1 || multiPayload.Results[0].ID != facts[1].ID || len(multiPayload.Missing) != 1 || multiPayload.Missing[0] != "fact:missing" {
		t.Fatalf("unexpected multi-get payload: %+v", multiPayload)
	}
	if multiPayload.Branch != "feature" {
		t.Fatalf("multi-get branch = %q, want feature", multiPayload.Branch)
	}
}

func TestQMDBranchOverrideAcrossRetrievalVerbs(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}
	storage, err := repoStoragePaths((&cobra.Command{}).Context(), runner, env, repoDir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	paths := normalizeFactPaths([]string{"architecture.branch.override"})
	branch := "feature/qmd"
	featureFact := factRecord{ID: factRecordID("zephyr branch override feature contract", paths), Text: "zephyr branch override feature contract", Paths: paths, Branch: branch, Status: factStatusActive, UpdatedAt: now}
	mainFact := factRecord{ID: factRecordID("zephyr branch override main contract", paths), Text: "zephyr branch override main contract", Paths: paths, Branch: "main", Status: factStatusActive, UpdatedAt: now}
	if err := writeFacts(storage.BrainDir, branch, []factRecord{featureFact}); err != nil {
		t.Fatal(err)
	}
	if err := writeFacts(storage.BrainDir, "main", []factRecord{mainFact}); err != nil {
		t.Fatal(err)
	}

	type payload struct {
		Branch  string          `json:"branch"`
		Results []unifiedResult `json:"results"`
		Missing []string        `json:"missing"`
	}
	assertPayload := func(t *testing.T, out string, wantBranch string, wantID string, forbiddenID string) payload {
		t.Helper()
		var got payload
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("decode JSON: %v\n%s", err, out)
		}
		if got.Branch != wantBranch {
			t.Fatalf("branch = %q, want %q in payload %+v", got.Branch, wantBranch, got)
		}
		found := false
		for _, result := range got.Results {
			if result.ID == wantID {
				found = true
			}
			if forbiddenID != "" && result.ID == forbiddenID {
				t.Fatalf("payload included wrong-branch result %s: %+v", forbiddenID, got.Results)
			}
		}
		if wantID != "" && !found {
			t.Fatalf("payload missing %s: %+v", wantID, got)
		}
		return got
	}

	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "search", args: []string{"search", "zephyr branch override", "--branch", branch, "--format", "json", "-n", "5"}},
		{name: "query", args: []string{"query", "zephyr branch override", "--branch", branch, "--format", "json", "-n", "5"}},
		{name: "vsearch", args: []string{"vsearch", "zephyr branch override feature", "--branch", branch, "--format", "json", "-n", "5"}},
		{name: "get", args: []string{"get", featureFact.ID, "--branch", branch, "--format", "json"}},
		{name: "multi-get", args: []string{"multi-get", featureFact.ID, mainFact.ID, "--branch", branch, "--format", "json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := execute(t, NewRootCommand(opts), tc.args...)
			if err != nil {
				t.Fatalf("%s: %v\n%s", strings.Join(tc.args, " "), err, out)
			}
			got := assertPayload(t, out, branch, featureFact.ID, mainFact.ID)
			if tc.name == "multi-get" && (len(got.Missing) != 1 || got.Missing[0] != mainFact.ID) {
				t.Fatalf("multi-get should report wrong-branch id missing, got %+v", got.Missing)
			}
		})
	}

	out, err := execute(t, NewRootCommand(opts), "get", featureFact.ID, "--branch", "main", "--format", "json")
	if err != nil {
		t.Fatalf("get wrong branch: %v\n%s", err, out)
	}
	got := assertPayload(t, out, "main", "", featureFact.ID)
	if len(got.Results) != 0 || len(got.Missing) != 1 || got.Missing[0] != featureFact.ID {
		t.Fatalf("wrong-branch get should miss feature fact, got %+v", got)
	}
}

func TestQMDLimitAliasesAndPrecedence(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}
	storage, err := repoStoragePaths((&cobra.Command{}).Context(), runner, env, repoDir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	paths := normalizeFactPaths([]string{"architecture.data.flow"})
	facts := []factRecord{
		{ID: factRecordID("alpha checkpoint first contract", paths), Text: "alpha checkpoint first contract", Paths: paths, Branch: "feature", Status: factStatusActive, UpdatedAt: now},
		{ID: factRecordID("alpha checkpoint second contract", paths), Text: "alpha checkpoint second contract", Paths: paths, Branch: "feature", Status: factStatusActive, UpdatedAt: now},
	}
	if err := writeFacts(storage.BrainDir, "feature", facts); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{name: "limit", args: []string{"search", "alpha checkpoint", "--limit", "1", "--format", "json"}, want: 1},
		{name: "limit then number", args: []string{"search", "alpha checkpoint", "--limit", "2", "-n", "1", "--format", "json"}, want: 1},
		{name: "number then limit", args: []string{"search", "alpha checkpoint", "-n", "1", "--limit", "2", "--format", "json"}, want: 2},
		{name: "query limit then number", args: []string{"query", "alpha checkpoint", "--limit", "2", "-n", "1", "--format", "json"}, want: 1},
		{name: "query number then limit", args: []string{"query", "alpha checkpoint", "-n", "1", "--limit", "2", "--format", "json"}, want: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := execute(t, NewRootCommand(opts), tc.args...)
			if err != nil {
				t.Fatalf("%s: %v\n%s", strings.Join(tc.args, " "), err, out)
			}
			var payload struct {
				Results []unifiedResult `json:"results"`
			}
			if err := json.Unmarshal([]byte(out), &payload); err != nil {
				t.Fatalf("decode retrieval JSON: %v\n%s", err, out)
			}
			if len(payload.Results) != tc.want {
				t.Fatalf("%s returned %d results, want %d: %+v", strings.Join(tc.args, " "), len(payload.Results), tc.want, payload.Results)
			}
		})
	}
}

func TestQMDHelpContractsForRetrievalVerbs(t *testing.T) {
	for _, tc := range []struct {
		verb string
		want []string
	}{
		{verb: "search", want: []string{"--format string", "--json", "--limit int", "-n, --number int", "--branch string"}},
		{verb: "vsearch", want: []string{"--format string", "--json", "--limit int", "-n, --number int", "--branch string"}},
		{verb: "query", want: []string{"--format string", "--json", "--limit int", "-n, --number int", "--branch string"}},
		{verb: "get", want: []string{"--format string", "--json", "--branch string"}},
		{verb: "multi-get", want: []string{"--format string", "--json", "--branch string"}},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			out, err := execute(t, NewRootCommand(Options{Version: "test"}), tc.verb, "--help")
			if err != nil {
				t.Fatalf("%s --help: %v\n%s", tc.verb, err, out)
			}
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Fatalf("%s --help missing %q:\n%s", tc.verb, want, out)
				}
			}
			if (tc.verb == "get" || tc.verb == "multi-get") && strings.Contains(out, "-n, --number") {
				t.Fatalf("%s --help should not expose result-count alias:\n%s", tc.verb, out)
			}
		})
	}
}

func TestQMDTopLevelHelpListsRetrievalVerbs(t *testing.T) {
	out, err := execute(t, NewRootCommand(Options{Version: "test"}), "--help")
	if err != nil {
		t.Fatalf("root --help: %v\n%s", err, out)
	}
	for _, want := range []string{"search", "vsearch", "query", "get", "multi-get"} {
		if !strings.Contains(out, want) {
			t.Fatalf("root help missing QMD verb %q:\n%s", want, out)
		}
	}
}

func TestQMDFormatCLIOverridesJSONAndReportsMissingIDs(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}
	storage, err := repoStoragePaths((&cobra.Command{}).Context(), runner, env, repoDir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	paths := normalizeFactPaths([]string{"architecture.data.flow"})
	fact := factRecord{
		ID:        factRecordID("alpha cli output contract", paths),
		Text:      "alpha cli output contract",
		Paths:     paths,
		Branch:    "feature",
		Status:    factStatusActive,
		UpdatedAt: now,
	}
	if err := writeFacts(storage.BrainDir, "feature", []factRecord{fact}); err != nil {
		t.Fatal(err)
	}

	out, err := execute(t, NewRootCommand(opts), "search", "alpha", "--json", "--format", "cli", "-n", "1")
	if err != nil {
		t.Fatalf("search --format cli: %v\n%s", err, out)
	}
	if json.Valid([]byte(out)) {
		t.Fatalf("--format cli should force human-readable output even with --json:\n%s", out)
	}
	for _, want := range []string{"[fact]", fact.ID, "alpha cli output contract"} {
		if !strings.Contains(out, want) {
			t.Fatalf("CLI search output missing %q:\n%s", want, out)
		}
	}

	getOut, err := execute(t, NewRootCommand(opts), "get", "fact:missing", "--format", "json")
	if err != nil {
		t.Fatalf("get missing --format json: %v\n%s", err, getOut)
	}
	var getPayload struct {
		Results []unifiedResult `json:"results"`
		Missing []string        `json:"missing"`
	}
	if err := json.Unmarshal([]byte(getOut), &getPayload); err != nil {
		t.Fatalf("decode missing get JSON: %v\n%s", err, getOut)
	}
	if len(getPayload.Results) != 0 || len(getPayload.Missing) != 1 || getPayload.Missing[0] != "fact:missing" {
		t.Fatalf("unexpected missing get payload: %+v", getPayload)
	}

	multiOut, err := execute(t, NewRootCommand(opts), "multi-get", fact.ID, "fact:missing", "--format", "cli")
	if err != nil {
		t.Fatalf("multi-get --format cli: %v\n%s", err, multiOut)
	}
	for _, want := range []string{fact.ID, "not found: fact:missing"} {
		if !strings.Contains(multiOut, want) {
			t.Fatalf("CLI multi-get output missing %q:\n%s", want, multiOut)
		}
	}
}

func TestQMDUnsupportedFormatRejectedAcrossRetrievalVerbs(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "search", args: []string{"search", "alpha", "--format", "csv"}},
		{name: "query", args: []string{"query", "alpha", "--format", "csv"}},
		{name: "vsearch", args: []string{"vsearch", "alpha", "--format", "csv"}},
		{name: "get", args: []string{"get", "fact:alpha", "--format", "csv"}},
		{name: "multi-get", args: []string{"multi-get", "fact:alpha", "doc:beta", "--format", "csv"}},
		{name: "text alias", args: []string{"search", "alpha", "--format", "text"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := execute(t, NewRootCommand(Options{Version: "test"}), tc.args...)
			if err == nil {
				t.Fatalf("%s accepted unsupported --format:\n%s", strings.Join(tc.args, " "), out)
			}
			if !strings.Contains(err.Error(), "--format must be json or cli") {
				t.Fatalf("unexpected error for %s: %v\n%s", strings.Join(tc.args, " "), err, out)
			}
		})
	}
}
