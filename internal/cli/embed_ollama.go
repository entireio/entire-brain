package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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

const defaultOllamaEmbedModel = "embeddinggemma"
const maxOllamaEmbedResponseBytes = 8 << 20

func newOllamaEmbedder() *ollamaEmbedder {
	model := os.Getenv("ENTIRE_BRAIN_OLLAMA_MODEL")
	if model == "" {
		model = defaultOllamaEmbedModel
	}
	// Default to Ollama's embed API; ENTIRE_BRAIN_EMBED_URL may point at another
	// loopback endpoint that accepts {"model","input"} and returns
	// {"embeddings":[[...]]}, e.g. a local node-llama-cpp spike server.
	embedURL := os.Getenv("ENTIRE_BRAIN_EMBED_URL")
	if embedURL == "" {
		embedURL = "http://localhost:11434/api/embed"
	}
	parsedURL, err := url.Parse(embedURL)
	if err != nil || !isLoopbackHTTPURL(parsedURL) {
		return nil
	}
	// Disable proxies on the transport: this embedder is local-first (the default
	// URL is localhost), and honoring HTTP(S)_PROXY could route query/document text
	// off-box. Clone DefaultTransport so its dial/keepalive defaults are preserved,
	// but tolerate a non-standard DefaultTransport (a consumer/test may replace it)
	// rather than panicking on the type assertion.
	var tr *http.Transport
	if dt, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = dt.Clone()
	} else {
		tr = &http.Transport{}
	}
	tr.Proxy = nil
	client := &http.Client{
		Timeout:   60 * time.Second,
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if !isLoopbackHTTPURL(req.URL) {
				return fmt.Errorf("embed redirect must stay loopback-only: %s", req.URL.String())
			}
			return nil
		},
	}
	return &ollamaEmbedder{
		model: model,
		url:   embedURL,
		hc:    client,
	}
}

func (o *ollamaEmbedder) ID() string { return "ollama:" + o.model }

// historyFusionEligible: the external (EmbeddingGemma-class) embedder is the
// one validated for history fusion (docs/eval_ledger.md 2026-06-12 capstone).
// The bundled Model2Vec deliberately does not implement this marker — it
// measured significantly below BM25 on history precision at scale.
func (o *ollamaEmbedder) historyFusionEligible() bool { return true }

// reachable reports whether the embed server actually returns a usable embedding
// within a short timeout — not merely that it answers 200. The selector uses it
// to fall back to the bundled Model2Vec model — one consistent vector space —
// instead of selecting an embedder that will then fail every embed (server up
// but wrong model, error-shaped 200 body, or an empty `embeddings` array). The
// short context timeout overrides the client's longer per-embed timeout so an
// absent server fails fast.
func (o *ollamaEmbedder) reachable() bool {
	body, err := json.Marshal(map[string]any{"model": o.model, "input": "title: none | text: probe"})
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.url, bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.hc.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	// Require a non-empty embedding vector, the same shape Embed depends on, so a
	// 200 with an error body or `{"embeddings":[]}` is not mistaken for healthy.
	var out struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false
	}
	return len(out.Embeddings) > 0 && len(out.Embeddings[0]) > 0
}

// embeddingGemmaDim is EmbeddingGemma-300M's output dimension. Seeding it for the
// default model avoids a network probe (and its timeout latency when the embed
// server is down) before the first real embedding.
const embeddingGemmaDim = 768

func (o *ollamaEmbedder) Dim() int {
	if o.dim == 0 {
		if o.model == defaultOllamaEmbedModel {
			o.dim = embeddingGemmaDim
		} else {
			o.embed("title: none | text: probe") // unknown model: discover the dim
		}
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
	if o == nil || o.hc == nil || o.url == "" {
		return nil
	}
	u, err := url.Parse(o.url)
	if err != nil || !isLoopbackHTTPURL(u) {
		return nil
	}
	body, err := json.Marshal(map[string]any{"model": o.model, "input": input})
	if err != nil {
		return nil
	}
	req, err := http.NewRequest(http.MethodPost, o.url, bytes.NewReader(body))
	if err != nil {
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.hc.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	resp.Body = http.MaxBytesReader(nil, resp.Body, maxOllamaEmbedResponseBytes)
	// A non-200 (model not pulled, server warming up) often still returns a JSON
	// error body that decodes cleanly into an empty Embeddings — which would look
	// like a successful nil vector. Treat any non-200 as an explicit failure.
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
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
