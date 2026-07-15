# Engine launch and verification runbook

This runbook is deterministic/offline after the binary, frozen facts root, relevance dataset, and
pinned EmbeddingGemma GGUF are available. It does not invoke a coding agent.

## Preflight

```sh
go build -o /tmp/entire-brain-ws6 ./cmd/entire-brain
sha256sum /tmp/entire-brain-ws6
python3 benchmarks/agent-brain/confirmatory/check_protocol.py
```

Set `FROZEN_CONFIG`, `FROZEN_DATA`, `FROZEN_STATE`, and `FROZEN_REPO_ROOT` to a read-only frozen
corpus resolution. Set `QUERY` and `K` from the development relevance dataset. Never reuse a cache
directory across arms.

Fact vectors are stored below `ENTIRE_PLUGIN_DATA_DIR`, not `ENTIRE_PLUGIN_CACHE_DIR`. A final
semantic verification must therefore use a separate derived data root for each semantic arm, copied
from the same frozen facts source, and persist that arm's own vector artifact. A read-only in-memory
rebuild is useful as a smoke test but cannot satisfy vector provenance.

## Lexical hand-rolled arm

```sh
env -u ENTIRE_BRAIN_EMBEDDER -u ENTIRE_BRAIN_EMBED_URL \
  ENTIRE_BRAIN_FACTS_BM25=0 \
  ENTIRE_PLUGIN_CONFIG_DIR="$FROZEN_CONFIG" \
  ENTIRE_PLUGIN_DATA_DIR="$FROZEN_DATA" \
  ENTIRE_PLUGIN_STATE_DIR="$FROZEN_STATE" \
  ENTIRE_PLUGIN_CACHE_DIR="$RUN_ROOT/cache/lexical_handrolled-v1" \
  ENTIRE_REPO_ROOT="$FROZEN_REPO_ROOT" \
  /tmp/entire-brain-ws6 recall "$QUERY" --k "$K" --no-semantic --json
```

## Bundled Model2Vec RRF arm

```sh
env -u ENTIRE_BRAIN_EMBEDDER -u ENTIRE_BRAIN_EMBED_URL \
  ENTIRE_BRAIN_FACTS_BM25=0 \
  ENTIRE_PLUGIN_CONFIG_DIR="$FROZEN_CONFIG" \
  ENTIRE_PLUGIN_DATA_DIR="$FROZEN_DATA" \
  ENTIRE_PLUGIN_STATE_DIR="$FROZEN_STATE" \
  ENTIRE_PLUGIN_CACHE_DIR="$RUN_ROOT/cache/model2vec_rrf-v1" \
  ENTIRE_REPO_ROOT="$FROZEN_REPO_ROOT" \
  /tmp/entire-brain-ws6 recall "$QUERY" --k "$K" --json
```

## Pinned EmbeddingGemma RRF arm

Before starting the server, record the GGUF file SHA-256 and exact model ID. The server must bind
loopback. The current `scripts/bench/embed-server.mjs` is the supported local endpoint.

```sh
sha256sum "$EMBEDDINGGEMMA_GGUF"
GGUF="$EMBEDDINGGEMMA_GGUF" HOST=127.0.0.1 PORT=11500 node scripts/bench/embed-server.mjs
```

In a second shell:

```sh
ENTIRE_BRAIN_FACTS_BM25=0 \
ENTIRE_BRAIN_EMBEDDER=ollama \
ENTIRE_BRAIN_EMBED_URL=http://127.0.0.1:11500 \
ENTIRE_PLUGIN_CONFIG_DIR="$FROZEN_CONFIG" \
ENTIRE_PLUGIN_DATA_DIR="$FROZEN_DATA" \
ENTIRE_PLUGIN_STATE_DIR="$FROZEN_STATE" \
ENTIRE_PLUGIN_CACHE_DIR="$RUN_ROOT/cache/embeddinggemma_rrf-v1" \
ENTIRE_REPO_ROOT="$FROZEN_REPO_ROOT" \
/tmp/entire-brain-ws6 recall "$QUERY" --k "$K" --json
```

## Required verification record

For every command, retain stdout/stderr and a record conforming to
`schemas/engine-verification.schema.json`. Effective engine must come from machine-readable runtime
output, not inferred environment intent. Recall JSON now exposes the observed `effective_engine`,
embedder ID/dimension, BM25 and fallback state, cache backend/path, and vector counts. The verification
wrapper must still bind those observations to the requested namespace and retain the corpus, vector,
binary, model, stdout, stderr, command, and environment hashes required by the schema. Final freeze
remains blocked until one real record for each arm passes those checks.

Reject a cell when `fallback_used=true`, semantic was requested but unavailable, BM25 differs from the
arm declaration, namespaces overlap, source facts change, eligible-candidate counts differ between
arms, or any recorded hash fails verification.
