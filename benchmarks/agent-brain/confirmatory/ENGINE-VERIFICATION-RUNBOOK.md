# Engine launch and verification runbook

This runbook is deterministic/offline after the binary, frozen facts root, development query, and
pinned EmbeddingGemma GGUF are available. It does not invoke a coding agent. Final evidence must be
created by `verify_engines.py`; the individual commands below remain useful diagnostics but cannot
by themselves satisfy the gate.

## Preflight

```sh
go build -o /tmp/entire-brain-ws6 ./cmd/entire-brain
sha256sum /tmp/entire-brain-ws6
python3 benchmarks/agent-brain/confirmatory/check_protocol.py
```

Set `FROZEN_CONFIG`, `FROZEN_DATA`, `FROZEN_STATE`, and `FROZEN_REPO_ROOT` to a read-only frozen
corpus resolution. Set `FROZEN_FACTS`, `SESSION_DATES`, `QUERY`, `QUERY_ID`, and `K` from the exposed
development task. Never open a fresh holdout for this step.

Fact vectors are stored below `ENTIRE_PLUGIN_DATA_DIR`, not `ENTIRE_PLUGIN_CACHE_DIR`. A final
verification must therefore use a separate derived data root for every arm, copied from the same
frozen facts source, and persist each semantic arm's own vector artifact. The wrapper also derives
separate config, state, and cache roots so no command can write to the frozen inputs. A read-only
in-memory rebuild is useful as a smoke test but cannot satisfy vector provenance.

## Fail-closed retained run

The production engine matrix pins `http://127.0.0.1:11500`. The wrapper refuses to reuse a process
that already owns that endpoint: it must launch and stop the supported server itself with the pinned
GGUF. Free the endpoint through the owner of that process before running; do not kill an unrelated
or unowned process merely to make this check pass.

Choose `ARTIFACT_ROOT` as the repository root and `OUTPUT` as a new directory below it. The output
includes a copy of the 333.6 MB GGUF, binary, two independent vector files, recall/server logs, three
derived runtime roots, and the final manifest. Do not mark the gate pass unless those bytes will be
durably retained by the repository's artifact strategy. A temporary or untracked output is only a
diagnostic run even when the wrapper succeeds.

```sh
python3 benchmarks/agent-brain/confirmatory/verify_engines.py \
  --binary /tmp/entire-brain-ws6 \
  --frozen-config-dir "$FROZEN_CONFIG" \
  --frozen-data-dir "$FROZEN_DATA" \
  --frozen-state-dir "$FROZEN_STATE" \
  --frozen-facts "$FROZEN_FACTS" \
  --repo-root "$FROZEN_REPO_ROOT" \
  --session-dates "$SESSION_DATES" \
  --query-id "$QUERY_ID" \
  --query "$QUERY" \
  --branch main \
  --k "$K" \
  --eligible-before "$CUTOFF" \
  --exclude-session-id "$EXCLUDED_SESSION_ID" \
  --expected-facts-sha256 "$FACTS_SHA256" \
  --expected-session-dates-sha256 "$SESSION_DATES_SHA256" \
  --expected-prefilter-count "$PREFILTER_COUNT" \
  --expected-eligible-count "$ELIGIBLE_COUNT" \
  --embedding-model "$EMBEDDINGGEMMA_GGUF" \
  --expected-embedding-model-sha256 "$EMBEDDINGGEMMA_SHA256" \
  --artifact-root "$ARTIFACT_ROOT" \
  --output-dir "$OUTPUT"
```

Publication is atomic at the evidence-contract level: `engine-verification.json` is written only
after all three arms pass runtime identity, eligibility reconciliation, vector-header attribution,
source-integrity rechecks, artifact hashing, and `check_protocol.validate_engine_verification`.
On any failure, no manifest is emitted.

## Lexical hand-rolled arm

```sh
env -u ENTIRE_BRAIN_EMBEDDER -u ENTIRE_BRAIN_EMBED_URL \
  ENTIRE_BRAIN_FACTS_BM25=0 \
  ENTIRE_PLUGIN_CONFIG_DIR="$FROZEN_CONFIG" \
  ENTIRE_PLUGIN_DATA_DIR="$RUN_ROOT/data/lexical_handrolled-v1" \
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
  ENTIRE_PLUGIN_DATA_DIR="$RUN_ROOT/data/model2vec_rrf-v1" \
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
ENTIRE_PLUGIN_DATA_DIR="$RUN_ROOT/data/embeddinggemma_rrf-v1" \
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

The wrapper tests are hermetic and do not load the real model:

```sh
python3 -m unittest benchmarks/agent-brain/confirmatory/test_verify_engines.py
```
