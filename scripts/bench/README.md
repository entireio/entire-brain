# Embedder benchmark (Model2Vec vs EmbeddingGemma)

Measures fact-retrieval quality of the bundled pure-Go Model2Vec embedder against
the Stage 1b transformer (EmbeddingGemma-300M), across one or more repos. Same
lexical arm and same provenance-labeled task set per repo, so the delta isolates
the embedder. This is the gate for the Stage 1b cgo bet (see `../../` and the
alignment plan's "Embedder gate").

## Result (2026-06-08)

EmbeddingGemma beats Model2Vec **+14% useful/1k pooled** over 60 tasks on two
diverse repos (`entire-brain` +15%, `podcasts` +13%) — the gain generalizes
beyond the one repo, magnitude-weighted from a few large wins + zero-rescues
(near-even win/loss count).

## Run it

```sh
# 1. build the brain binary
(cd ../.. && go build -o entire-brain ./cmd/entire-brain)

# 2. start the EmbeddingGemma embed server (qmd's runner, no cgo)
npm install node-llama-cpp
curl -L -o /tmp/eg.gguf \
  https://huggingface.co/ggml-org/embeddinggemma-300M-GGUF/resolve/main/embeddinggemma-300M-Q8_0.gguf
GGUF=/tmp/eg.gguf PORT=11500 node embed-server.mjs &

# 3. benchmark a list of repos (each needs exported Entire session history)
./run.sh ~/Projects/entire-brain ~/Projects/podcasts

# 4. print the per-repo + pooled table
./agg.py
```

## Notes

- `run.sh` distills facts with a cheap model (`MODEL=gpt-5.4-mini`, override via
  env). Distilling is the slow step (one agent run per repo).
- The GGUF is the **canonical** ggml-org build (open, not Gemma-gated). The
  Ollama-pulled blob is *not* portable to plain llama.cpp (314 vs 316 tensors).
- The brain's opt-in EmbeddingGemma backend is selected with
  `ENTIRE_BRAIN_EMBEDDER=ollama` + `ENTIRE_BRAIN_EMBED_URL` (any endpoint with
  Ollama's `/api/embed` shape — this server, or a real Ollama).
- Caveat: tasks are self-labeled from each repo's session provenance; `eval-gen
  --refine` (agent-judged) tightens labels but costs agent calls.
