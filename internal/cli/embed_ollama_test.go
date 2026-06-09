package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakePlainEmbedder implements Embedder but NOT queryEmbedder (like Model2Vec).
type fakePlainEmbedder struct{ last string }

func (f *fakePlainEmbedder) Embed(t string) []float32 { f.last = t; return []float32{1, 0} }
func (f *fakePlainEmbedder) Dim() int                 { return 2 }
func (f *fakePlainEmbedder) ID() string               { return "fake-plain" }

// fakeAsymEmbedder implements queryEmbedder (like EmbeddingGemma): a query is
// embedded with a different prefix/vector than a document.
type fakeAsymEmbedder struct{ lastQuery, lastDoc string }

func (f *fakeAsymEmbedder) Embed(t string) []float32      { f.lastDoc = t; return []float32{1, 0} }
func (f *fakeAsymEmbedder) EmbedQuery(t string) []float32 { f.lastQuery = t; return []float32{0, 1} }
func (f *fakeAsymEmbedder) Dim() int                      { return 2 }
func (f *fakeAsymEmbedder) ID() string                    { return "fake-asym" }

func TestEmbedQueryUsesQueryEmbedderWhenAvailable(t *testing.T) {
	fe := &fakeAsymEmbedder{}
	rr := newSemanticReranker(fe)
	v := rr.embedQuery("find facts")
	if len(v) != 2 || v[1] != 1 {
		t.Fatalf("expected the EmbedQuery vector {0,1}, got %v", v)
	}
	if fe.lastQuery != "find facts" || fe.lastDoc != "" {
		t.Fatalf("query should route to EmbedQuery only: q=%q d=%q", fe.lastQuery, fe.lastDoc)
	}
}

func TestEmbedQueryFallsBackToEmbed(t *testing.T) {
	fe := &fakePlainEmbedder{}
	rr := newSemanticReranker(fe)
	v := rr.embedQuery("find facts")
	if len(v) != 2 || v[0] != 1 {
		t.Fatalf("expected the Embed vector {1,0}, got %v", v)
	}
	if fe.last != "find facts" {
		t.Fatalf("query should route to Embed: %q", fe.last)
	}
}

func TestConfiguredEmbedderUsesOllamaWhenReachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{{0.1, 0.2}}})
	}))
	defer srv.Close()
	t.Setenv("ENTIRE_BRAIN_EMBEDDER", "ollama")
	t.Setenv("ENTIRE_BRAIN_EMBED_URL", srv.URL)
	e, warn := configuredEmbedder()
	if _, ok := e.(*ollamaEmbedder); !ok {
		t.Fatalf("expected the ollama embedder when reachable, got %T", e)
	}
	if warn != "" {
		t.Fatalf("no warning expected when reachable, got %q", warn)
	}
}

func TestConfiguredEmbedderFallsBackWhenOllamaUnreachable(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_EMBEDDER", "ollama")
	// Port 1 refuses immediately, so the probe fails fast and the selector must
	// fall back to the bundled Model2Vec model rather than returning the (dead)
	// ollama embedder or silently dropping the semantic arm.
	t.Setenv("ENTIRE_BRAIN_EMBED_URL", "http://127.0.0.1:1/api/embed")
	e, warn := configuredEmbedder()
	if e == nil {
		t.Fatal("expected a fallback embedder, got nil")
	}
	if _, ok := e.(*ollamaEmbedder); ok {
		t.Fatal("expected fallback to the static embedder, got the ollama embedder")
	}
	if warn == "" {
		t.Fatal("expected a fallback warning when the opt-in server is unreachable")
	}
}

func TestConfiguredEmbedderDefaultsToStatic(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_EMBEDDER", "")
	e, warn := configuredEmbedder()
	if e == nil {
		t.Fatal("expected the bundled static embedder by default")
	}
	if _, ok := e.(*ollamaEmbedder); ok {
		t.Fatal("default must not select the ollama embedder")
	}
	if warn != "" {
		t.Fatalf("no warning expected on the default path, got %q", warn)
	}
}

func TestOllamaEmbedderURLOverride(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_EMBED_URL", "http://127.0.0.1:11500")
	if got := newOllamaEmbedder().url; got != "http://127.0.0.1:11500" {
		t.Fatalf("url override = %q", got)
	}
	t.Setenv("ENTIRE_BRAIN_EMBED_URL", "")
	if got := newOllamaEmbedder().url; got != "http://localhost:11434/api/embed" {
		t.Fatalf("default url = %q", got)
	}
}
