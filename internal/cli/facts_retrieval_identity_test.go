package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type recallIdentityPayload struct {
	EffectiveEngine string                `json:"effective_engine"`
	RetrievalEngine recallRetrievalEngine `json:"retrieval_engine"`
}

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
	if err := writeFacts(storage.BrainDir, "main", []factRecord{fact}); err != nil {
		t.Fatal(err)
	}
	return opts, storage.BrainDir
}

func runRecallIdentityTest(t *testing.T, opts Options, resolve func() Embedder, extraArgs ...string) (recallIdentityPayload, map[string]json.RawMessage) {
	t.Helper()
	return runRecallIdentityQueryTest(t, opts, resolve, "target regression", extraArgs...)
}

func runRecallIdentityQueryTest(t *testing.T, opts Options, resolve func() Embedder, query string, extraArgs ...string) (recallIdentityPayload, map[string]json.RawMessage) {
	t.Helper()
	args := []string{query, "--json", "--read-only-semantic-cache"}
	args = append(args, extraArgs...)
	out, err := execute(t, newRecallCommandWithEmbedder(opts, resolve), args...)
	if err != nil {
		t.Fatalf("recall: %v\n%s", err, out)
	}
	var payload recallIdentityPayload
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("parse recall JSON: %v\n%s", err, out)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatal(err)
	}
	return payload, raw
}

func TestRecallJSONEffectiveEngineLexicalHandrolled(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_EMBEDDER", "ollama") // --no-semantic must win over intent.
	t.Setenv("ENTIRE_BRAIN_FACTS_BM25", "0")
	opts, _ := setupRecallIdentityTest(t)
	called := false
	payload, _ := runRecallIdentityTest(t, opts, func() Embedder {
		called = true
		return testEmbedder(t)
	}, "--no-semantic")
	if called {
		t.Fatal("--no-semantic must not resolve or probe an embedder")
	}
	got := payload.RetrievalEngine
	if payload.EffectiveEngine != recallEngineLexicalHandrolled || got.EffectiveEngine != recallEngineLexicalHandrolled {
		t.Fatalf("effective engine = top-level %q metadata %q", payload.EffectiveEngine, got.EffectiveEngine)
	}
	if !got.IdentityVerified || got.SemanticRequested || got.SemanticApplied || got.SemanticAvailable || got.BM25Enabled || got.BM25Applied || got.FallbackUsed {
		t.Fatalf("unexpected lexical identity: %+v", got)
	}
}

func TestRecallJSONEffectiveEngineBundledModel2Vec(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_EMBEDDER", "")
	t.Setenv("ENTIRE_BRAIN_FACTS_BM25", "0")
	opts, _ := setupRecallIdentityTest(t)
	e := testEmbedder(t)
	payload, _ := runRecallIdentityTest(t, opts, func() Embedder { return e })
	got := payload.RetrievalEngine
	if payload.EffectiveEngine != recallEngineModel2VecRRF || got.EffectiveEngine != recallEngineModel2VecRRF {
		t.Fatalf("effective engine = top-level %q metadata %q", payload.EffectiveEngine, got.EffectiveEngine)
	}
	if !got.IdentityVerified || !got.SemanticRequested || !got.SemanticApplied || !got.SemanticAvailable || got.FallbackUsed {
		t.Fatalf("unexpected Model2Vec identity: %+v", got)
	}
	if got.EmbedderID != e.ID() || got.EmbeddingModelID != e.ID() || got.EmbeddingDimension == nil || *got.EmbeddingDimension != e.Dim() {
		t.Fatalf("Model2Vec metadata = %+v", got)
	}
	if got.VectorCount == nil || *got.VectorCount != 1 || got.VectorCandidateCount == nil || *got.VectorCandidateCount != 1 {
		t.Fatalf("vector accounting = %+v", got)
	}
	if got.LoadedVectorCount == nil || *got.LoadedVectorCount != 0 || got.ResidentVectorCount == nil || *got.ResidentVectorCount != 1 {
		t.Fatalf("cache counts = %+v", got)
	}
	if got.VectorCacheBackend == "" || got.VectorCachePath == "" || got.VectorCacheReadOnly == nil || !*got.VectorCacheReadOnly {
		t.Fatalf("cache metadata = %+v", got)
	}
}

func TestRecallJSONEffectiveEngineEmbeddingGemmaAfterRealEmbeddings(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_EMBEDDER", "ollama")
	t.Setenv("ENTIRE_BRAIN_FACTS_BM25", "0")
	seenQuery, seenDocument := false, false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model string `json:"model"`
			Input string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if request.Model != defaultOllamaEmbedModel {
			http.Error(w, "wrong model", http.StatusBadRequest)
			return
		}
		if strings.HasPrefix(request.Input, "task: search result | query:") {
			seenQuery = true
		} else if strings.HasPrefix(request.Input, "title: none | text:") {
			seenDocument = true
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{{1, 0}}})
	}))
	defer srv.Close()

	opts, _ := setupRecallIdentityTest(t)
	e := &ollamaEmbedder{model: defaultOllamaEmbedModel, url: srv.URL, dim: 2, hc: srv.Client()}
	payload, _ := runRecallIdentityTest(t, opts, func() Embedder { return e })
	got := payload.RetrievalEngine
	if payload.EffectiveEngine != recallEngineEmbeddingGemmaRRF || got.EffectiveEngine != recallEngineEmbeddingGemmaRRF {
		t.Fatalf("effective engine = top-level %q metadata %q", payload.EffectiveEngine, got.EffectiveEngine)
	}
	if !seenQuery || !seenDocument {
		t.Fatalf("identity was emitted without observing both real embedding paths: query=%v document=%v", seenQuery, seenDocument)
	}
	if !got.IdentityVerified || !got.SemanticAvailable || got.FallbackUsed || got.EmbedderID != "ollama:"+defaultOllamaEmbedModel || got.EmbeddingModelID != defaultOllamaEmbedModel || got.EmbeddingDimension == nil || *got.EmbeddingDimension != 2 {
		t.Fatalf("unexpected EmbeddingGemma identity: %+v", got)
	}
}

func TestRecallJSONReportsActualModel2VecWhenOllamaIntentFellBack(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_EMBEDDER", "ollama")
	t.Setenv("ENTIRE_BRAIN_FACTS_BM25", "0")
	opts, _ := setupRecallIdentityTest(t)
	e := testEmbedder(t) // configuredEmbedder's actual fallback result.
	payload, _ := runRecallIdentityTest(t, opts, func() Embedder { return e })
	got := payload.RetrievalEngine
	if payload.EffectiveEngine != recallEngineModel2VecRRF || got.EffectiveEngine != recallEngineModel2VecRRF {
		t.Fatalf("Ollama intent must not override actual backend: %+v", got)
	}
	if !got.IdentityVerified || !got.SemanticAvailable || !got.FallbackUsed || got.RequestedEmbedder != "ollama" {
		t.Fatalf("fallback metadata = %+v", got)
	}
}

func TestRecallJSONPreservesUnknownEmbedderIntentAsFallback(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_EMBEDDER", "custom-local-backend")
	t.Setenv("ENTIRE_BRAIN_FACTS_BM25", "0")
	opts, _ := setupRecallIdentityTest(t)
	e := testEmbedder(t)
	payload, _ := runRecallIdentityTest(t, opts, func() Embedder { return e })
	got := payload.RetrievalEngine
	if payload.EffectiveEngine != recallEngineModel2VecRRF || got.EffectiveEngine != recallEngineModel2VecRRF {
		t.Fatalf("actual backend should remain observable: %+v", got)
	}
	if !got.IdentityVerified || !got.FallbackUsed || got.RequestedEmbedder != "custom-local-backend" {
		t.Fatalf("unknown request must be preserved and marked as fallback: %+v", got)
	}
}

func TestRecallJSONFailsClosedWhenSemanticQueryFails(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_EMBEDDER", "ollama")
	t.Setenv("ENTIRE_BRAIN_FACTS_BM25", "0")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{}})
	}))
	defer srv.Close()

	opts, _ := setupRecallIdentityTest(t)
	e := &ollamaEmbedder{model: defaultOllamaEmbedModel, url: srv.URL, dim: 2, hc: srv.Client()}
	payload, _ := runRecallIdentityTest(t, opts, func() Embedder { return e })
	got := payload.RetrievalEngine
	if payload.EffectiveEngine != recallEngineLexicalHandrolled || got.EffectiveEngine != recallEngineLexicalHandrolled {
		t.Fatalf("failed query must report the actual lexical fallback: %+v", got)
	}
	if !got.IdentityVerified || got.SemanticApplied || got.SemanticAvailable || !got.FallbackUsed {
		t.Fatalf("failed-query metadata = %+v", got)
	}
}

func TestRecallJSONFailsClosedWhenDocumentVectorsArePartial(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_EMBEDDER", "ollama")
	t.Setenv("ENTIRE_BRAIN_FACTS_BM25", "0")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		vectors := [][]float32{}
		if strings.HasPrefix(request.Input, "task: search result | query:") {
			vectors = [][]float32{{1, 0}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": vectors})
	}))
	defer srv.Close()

	opts, _ := setupRecallIdentityTest(t)
	e := &ollamaEmbedder{model: defaultOllamaEmbedModel, url: srv.URL, dim: 2, hc: srv.Client()}
	payload, raw := runRecallIdentityTest(t, opts, func() Embedder { return e })
	got := payload.RetrievalEngine
	if _, exists := raw["effective_engine"]; exists {
		t.Fatalf("partial semantic run must omit top-level effective_engine: %s", raw["effective_engine"])
	}
	if got.EffectiveEngine != "" || got.IdentityVerified || !got.SemanticApplied || got.SemanticAvailable || !got.FallbackUsed {
		t.Fatalf("partial-vector identity did not fail closed: %+v", got)
	}
	if got.IdentityFailureReason != "semantic_vectors_incomplete_or_invalid" || got.VectorCount == nil || *got.VectorCount != 0 || got.VectorCandidateCount == nil || *got.VectorCandidateCount != 1 {
		t.Fatalf("partial-vector diagnostics = %+v", got)
	}
}

func TestRecallJSONFailsClosedForOptionalBM25Factor(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_EMBEDDER", "")
	t.Setenv("ENTIRE_BRAIN_FACTS_BM25", "1")
	opts, _ := setupRecallIdentityTest(t)
	payload, raw := runRecallIdentityTest(t, opts, func() Embedder { return testEmbedder(t) })
	got := payload.RetrievalEngine
	if _, exists := raw["effective_engine"]; exists {
		t.Fatalf("optional BM25 combination must not impersonate a primary arm: %s", raw["effective_engine"])
	}
	if payload.EffectiveEngine != "" || got.IdentityVerified || !got.BM25Enabled || got.IdentityFailureReason != "optional_bm25_factor_not_a_primary_engine" {
		t.Fatalf("BM25 identity did not fail closed: %+v", got)
	}
}

func TestRecallJSONFailsClosedWhenBM25ConfiguredButBypassed(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_EMBEDDER", "")
	t.Setenv("ENTIRE_BRAIN_FACTS_BM25", "1")
	opts, _ := setupRecallIdentityTest(t)
	called := false
	payload, raw := runRecallIdentityTest(t, opts, func() Embedder {
		called = true
		return testEmbedder(t)
	}, "--no-semantic")
	if called {
		t.Fatal("--no-semantic must not resolve the embedder")
	}
	got := payload.RetrievalEngine
	if _, exists := raw["effective_engine"]; exists {
		t.Fatalf("configured BM25 must fail the primary identity even when bypassed: %s", raw["effective_engine"])
	}
	if !got.BM25Enabled || got.BM25Applied || got.IdentityVerified || got.IdentityFailureReason != "optional_bm25_factor_not_a_primary_engine" {
		t.Fatalf("bypassed BM25 metadata = %+v", got)
	}
}

func TestRecallJSONFailsClosedWhenBM25HasZeroHits(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_EMBEDDER", "")
	t.Setenv("ENTIRE_BRAIN_FACTS_BM25", "1")
	opts, _ := setupRecallIdentityTest(t)
	payload, raw := runRecallIdentityQueryTest(t, opts, func() Embedder { return testEmbedder(t) }, "zzzz-no-match")
	got := payload.RetrievalEngine
	if _, exists := raw["effective_engine"]; exists {
		t.Fatalf("zero BM25 hits must not erase configured BM25 state: %s", raw["effective_engine"])
	}
	if !got.BM25Enabled || got.IdentityVerified || got.IdentityFailureReason != "optional_bm25_factor_not_a_primary_engine" {
		t.Fatalf("zero-hit BM25 metadata = %+v", got)
	}
}
