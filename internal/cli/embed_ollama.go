package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"time"
)

// embed_ollama.go is the Stage 1b embedder spike: it shells the EmbeddingGemma
// GGUF to a local Ollama server (the plan's runner option 2 — trivial, at the
// cost of a running service) so the transformer embedder can be A/B'd against the
// Model2Vec baseline on `facts eval` before committing to the cgo llama.cpp build.
//
// Enabled with ENTIRE_BRAIN_EMBEDDER=ollama; the model defaults to
// "embeddinggemma" (override with ENTIRE_BRAIN_OLLAMA_MODEL). It applies
// EmbeddingGemma's asymmetric task prefixes — a different instruction for queries
// than documents — via the optional queryEmbedder interface; getting that wrong
// is the documented way recall degrades.

type ollamaEmbedder struct {
	model string
	url   string
	dim   int
	hc    *http.Client
}

func newOllamaEmbedder() *ollamaEmbedder {
	model := os.Getenv("ENTIRE_BRAIN_OLLAMA_MODEL")
	if model == "" {
		model = "embeddinggemma"
	}
	// Default to Ollama's embed API; ENTIRE_BRAIN_EMBED_URL points instead at any
	// endpoint that accepts {"model","input"} and returns {"embeddings":[[...]]}
	// — e.g. the node-llama-cpp spike server (qmd's in-process GGUF runner).
	url := os.Getenv("ENTIRE_BRAIN_EMBED_URL")
	if url == "" {
		url = "http://localhost:11434/api/embed"
	}
	return &ollamaEmbedder{
		model: model,
		url:   url,
		hc:    &http.Client{Timeout: 60 * time.Second},
	}
}

func (o *ollamaEmbedder) ID() string { return "ollama:" + o.model }

func (o *ollamaEmbedder) Dim() int {
	if o.dim == 0 {
		o.embed("title: none | text: probe")
	}
	return o.dim
}

// Embed uses EmbeddingGemma's document/retrieval prefix.
func (o *ollamaEmbedder) Embed(text string) []float32 {
	return o.embed("title: none | text: " + text)
}

// EmbedQuery uses EmbeddingGemma's search-query prefix — distinct from Embed's
// document prefix, which is the asymmetry the model is trained for.
func (o *ollamaEmbedder) EmbedQuery(text string) []float32 {
	return o.embed("task: search result | query: " + text)
}

func (o *ollamaEmbedder) embed(input string) []float32 {
	body, err := json.Marshal(map[string]any{"model": o.model, "input": input})
	if err != nil {
		return nil
	}
	resp, err := o.hc.Post(o.url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var out struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || len(out.Embeddings) == 0 {
		return nil
	}
	v := out.Embeddings[0]
	if o.dim == 0 {
		o.dim = len(v)
	}
	return v
}
