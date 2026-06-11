# Embedder benchmark (Model2Vec vs EmbeddingGemma)

Measures fact-retrieval quality of the bundled pure-Go Model2Vec embedder against
the Stage 1b transformer (EmbeddingGemma-300M), across one or more repos. Same
lexical arm and same provenance-labeled task set per repo, so the delta isolates
the embedder. This is the gate for the Stage 1b cgo bet (see `../../` and the
alignment plan's "Embedder gate").

## Historical Local Result (2026-06-08)

An operator-run sweep observed EmbeddingGemma beating Model2Vec by **+14%
useful/1k pooled** over 60 tasks on two repos (`entire-brain` +15%, `podcasts`
+13%). The raw artifacts are not committed here, so treat this as a historical
local note, not release evidence. Re-run the current harness and commit
sanitized summaries before using the number in public claims.

## Run it

```sh
# 1. build the brain binary
(cd ../.. && go build -o entire-brain ./cmd/entire-brain)

# 2. optionally start the EmbeddingGemma embed server (qmd's runner, no cgo).
#    This setup downloads a model; skip it for offline/no-egress release checks.
#    Run npm from THIS directory — package.json here anchors the install so npm
#    can't resolve a parent directory (e.g. $HOME) as the package root.
npm install
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
