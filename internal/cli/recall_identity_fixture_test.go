package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/command/facts"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
)

// setupRecallIdentityTest builds a one-fact brain and the Options to run
// recall against it. Shared by the evidence-recall tests and the retrieval
// identity regression below.
func setupRecallIdentityTest(t *testing.T) (Options, string) {
	t.Helper()
	repoDir := t.TempDir()
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "rev-parse", "--show-toplevel"): {stdout: repoDir + "\n"},
		fakeCommandKey("git", "remote", "get-url", "origin"):  {stdout: "git@github.com:example/repo.git\n"},
		fakeCommandKey("git", "branch", "--show-current"):     {stdout: "main\n"},
	}}
	opts := Options{
		Version: "test",
		Env: EntireEnv{
			RepoRoot:        repoDir,
			PluginConfigDir: t.TempDir(),
			PluginDataDir:   t.TempDir(),
			PluginStateDir:  t.TempDir(),
			PluginCacheDir:  t.TempDir(),
		},
		Runner: runner,
		Now:    func() time.Time { return time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC) },
	}
	storage, err := repoStoragePaths(context.Background(), runner, opts.Env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	text := "target regression uses a stable retrieval identity"
	fact := factRecord{
		ID:        factRecordID(text, []string{"architecture.data.flow"}),
		Paths:     []string{"architecture.data.flow"},
		Text:      text,
		Branch:    "main",
		Status:    factStatusActive,
		UpdatedAt: opts.Now(),
	}
	if err := factstore.Write(storage.BrainDir, "main", []factRecord{fact}); err != nil {
		t.Fatal(err)
	}
	return opts, storage.BrainDir
}

// Post-removal retrieval identity contract: the engine is always BM25 lexical,
// the semantic fields stay in the schema reporting inert values, and the
// removed flags are accepted as warning no-ops rather than errors.
func TestRecallIdentityIsBM25Lexical(t *testing.T) {
	opts, _ := setupRecallIdentityTest(t)
	out, err := execute(t, facts.NewRecall(factsCommandConfig(opts)), "target regression", "--json", "--no-semantic")
	if err != nil {
		t.Fatalf("recall: %v\n%s", err, out)
	}
	var payload struct {
		EffectiveEngine string                      `json:"effective_engine"`
		Engine          facts.RecallRetrievalEngine `json:"retrieval_engine"`
		Facts           []json.RawMessage           `json:"facts"`
	}
	if i := strings.Index(out, "{"); i > 0 {
		out = out[i:]
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("parse: %v\n%s", err, out)
	}
	if payload.EffectiveEngine != facts.RecallEngineBM25Lexical || !payload.Engine.IdentityVerified {
		t.Fatalf("engine identity = %+v", payload.Engine)
	}
	if payload.Engine.SemanticApplied || payload.Engine.SemanticAvailable || payload.Engine.SemanticRequested {
		t.Fatalf("semantic fields must be inert: %+v", payload.Engine)
	}
	if len(payload.Facts) != 1 {
		t.Fatalf("recall lost the fixture fact: %s", out)
	}
	if !payload.Engine.BM25Applied || !payload.Engine.BM25Enabled {
		t.Fatalf("bm25 identity missing: %+v", payload.Engine)
	}
}
