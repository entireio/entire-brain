# Engine launch and verification runbook

This runbook is deterministic/offline after the binary and pinned local inputs are available. It
does not invoke a coding agent. Production expectations come only from the checked-in
`engine-verification-pins.json`; callers supply source locations, never expected hashes, counts,
versions, or runtime identities. `verify_engines.py` creates restricted exact-byte diagnostic
evidence. That evidence must then be projected by `public_engine_evidence.py` into the authoritative
privacy-safe schema v4; the individual commands below remain useful diagnostics but cannot by
themselves satisfy the gate.

`preregistration.json` commits to that file by its canonical repo-relative path and raw SHA-256.
`check_protocol.py` rejects a missing, redirected, symlinked, or byte-changed binding during ordinary
preparation as well as final freeze; updating engine pins therefore requires an explicit
preregistration revision before the protocol content hash is frozen.

## Preflight

```sh
git worktree add --detach /tmp/entire-brain-build-bb68c0ac bb68c0ac7fe9690bea81c97ae9def2abf1b6a549
(cd /tmp/entire-brain-build-bb68c0ac && \
  go build -trimpath -buildvcs=false \
    -o /tmp/entire-brain-engine-evidence-bb68c0ac ./cmd/entire-brain)
go version
sha256sum /tmp/entire-brain-engine-evidence-bb68c0ac
python3 benchmarks/agent-brain/confirmatory/check_protocol.py
```

The detached checkout must resolve to source commit `bb68c0ac7fe9690bea81c97ae9def2abf1b6a549`
and tree `a15e4ec365b7887f48e46b2bc7fc386ac89c435f`. The deterministic build flags,
Go version, exact binary size, and SHA-256 are part of the production pin. The wrapper refuses a
different binary before creating its output directory and retains a machine-checked build
attestation beside the binary.

Set `FROZEN_CONFIG`, `FROZEN_DATA`, `FROZEN_STATE`, and `FROZEN_REPO_ROOT` to a read-only frozen
corpus resolution. Set `FROZEN_FACTS`, `SESSION_DATES`, `QUERY`, `QUERY_ID`, and `K` from the exposed
development task. Set `NODE_RUNTIME` to the resolved executable and `NODE_MODULES` to the dependency
tree named by the canonical pin set; a `node` found through the caller's `PATH` is not authoritative.
Never open a fresh holdout for this step.

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

Choose `ARTIFACT_ROOT` as the repository root and `OUTPUT` as a new directory below it. The
restricted diagnostic output
includes byte-identical retained copies of the facts and session-date sources, the 333.6 MB GGUF,
the resolved 126.7 MB Node executable, the approximately 46.6 MB regular-file dependency tree,
server script, package manifest and lockfile, binary, two independent vector files, recall/server
logs, three derived runtime roots, a dependency inventory, a server health attestation, and the
final manifest. Do not mark the gate pass unless those bytes will be durably retained by the
repository's restricted artifact strategy. It contains private corpus/session/host material and
must not be published. A temporary or untracked output is only a diagnostic run even when the
wrapper succeeds.

```sh
python3 benchmarks/agent-brain/confirmatory/verify_engines.py \
  --binary /tmp/entire-brain-engine-evidence-bb68c0ac \
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
  --embedding-model "$EMBEDDINGGEMMA_GGUF" \
  --node-runtime "$NODE_RUNTIME" \
  --runtime-dependency-root "$NODE_MODULES" \
  --artifact-root "$ARTIFACT_ROOT" \
  --output-dir "$OUTPUT"
```

Publication is atomic at the evidence-contract level: `engine-verification.json` is written only
after all three arms pass runtime identity, eligibility reconciliation, vector-header attribution,
retained-stdout reconciliation, source-integrity rechecks, complete dependency-byte inventory,
owned-server continuity checks,
artifact hashing, and `check_protocol.validate_engine_verification`. Validation occurs against a
temporary manifest followed by an atomic rename. On any failure, neither a final nor temporary
manifest is retained.

## Lexical hand-rolled arm

```sh
env -u ENTIRE_BRAIN_EMBEDDER -u ENTIRE_BRAIN_EMBED_URL \
  ENTIRE_BRAIN_FACTS_BM25=0 \
  ENTIRE_PLUGIN_CONFIG_DIR="$FROZEN_CONFIG" \
  ENTIRE_PLUGIN_DATA_DIR="$RUN_ROOT/data/lexical_handrolled-v1" \
  ENTIRE_PLUGIN_STATE_DIR="$FROZEN_STATE" \
  ENTIRE_PLUGIN_CACHE_DIR="$RUN_ROOT/cache/lexical_handrolled-v1" \
  ENTIRE_REPO_ROOT="$FROZEN_REPO_ROOT" \
  /tmp/entire-brain-engine-evidence-bb68c0ac recall "$QUERY" --k "$K" --no-semantic --json
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
  /tmp/entire-brain-engine-evidence-bb68c0ac recall "$QUERY" --k "$K" --json
```

## Pinned EmbeddingGemma RRF arm

Before starting a diagnostic server, verify the GGUF, Node executable, server script, package and
dependency-tree pins. The server must bind loopback. The current `scripts/bench/embed-server.mjs` is
the supported local endpoint. The authoritative wrapper additionally gives the server a random
ownership token and continuously checks `/health` for the same PID, token, GGUF hash, embedding
dimension, and Node version. Every health request has a fresh echoed nonce and monotonic counter.
The explicit during-recall observation must complete while the recall worker is demonstrably live;
timezone-aware timestamps must strictly order pre-health, recall start, during-health, recall finish,
and post-health.

```sh
sha256sum "$EMBEDDINGGEMMA_GGUF" "$NODE_RUNTIME" scripts/bench/embed-server.mjs
GGUF="$EMBEDDINGGEMMA_GGUF" HOST=127.0.0.1 PORT=11500 "$NODE_RUNTIME" scripts/bench/embed-server.mjs
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
/tmp/entire-brain-engine-evidence-bb68c0ac recall "$QUERY" --k "$K" --json
```

## Required verification record

For every restricted diagnostic command, retain stdout/stderr and a record conforming to
`schemas/engine-verification.schema.json`. Its diagnostic entry point is one regular JSON file
conforming to `schemas/engine-verification-manifest.schema.json`, with exactly `schema_version: 2`
and `records`; directories, raw arrays, single records, `record_paths`, and extra wrapper keys are
rejected. The checker recursively applies the record schema as well, including
every nested `additionalProperties: false`; a structurally valid wrapper cannot hide extra record or
requested fields. Effective engine must come from machine-readable runtime
output, not inferred environment intent. Recall JSON now exposes the observed `effective_engine`,
embedder ID/dimension, BM25 and fallback state, cache backend/path, and vector counts. The verification
wrapper must still bind those observations to the requested namespace and retain the source and
derived corpus bytes, vector, binary, model, complete server runtime, stdout, stderr, command,
environment, and health-attestation hashes required by the schema. The checker independently
re-hashes every named byte and requires production records to name the checked-in canonical pin-set
descriptor. From the pinned facts, complete session-date map, cutoff, exclusions, and active status,
it independently derives 2,529 temporally eligible facts and the exact 2,442 active+eligible semantic
candidate IDs. Both semantic arms must report candidate, valid-vector, and resident counts of 2,442,
zero inherited vectors in their clean namespaces, and EBV1 IDs exactly equal to that committed set.
The EBV1 parser decodes every float32 and rejects truncation, trailing bytes, NaN, or infinity. Every
delivered stdout/record fact ID must be active+eligible, in retained facts, in identical order, and
the delivered count must equal the ID count and remain at or below pinned K. The checker also
requires every arm command to invoke the same retained canonical binary.

The authoritative public entry point is instead
`schemas/engine-verification-public-v4.schema.json`. Create it only from an already checker-valid
restricted v2 manifest:

```sh
python3 benchmarks/agent-brain/confirmatory/public_engine_evidence.py \
  --diagnostic-manifest "$RESTRICTED_ROOT/run/engine-verification.json" \
  --artifact-root "$RESTRICTED_ROOT" \
  --output-dir "$PUBLIC_V4_OUTPUT"
```

The projector generates a fresh non-persisted 256-bit pseudonym key unless a restricted binary key
file is named. The public bundle contains only typed logical invocations, pinned component
commitments, domain-separated 128-bit candidate/session refs, independently recomputable temporal
eligibility, sanitized ordered result IDs, chunked vector-candidate commitments, raw-stream
digest/size pairs, and sanitized server lifecycle attestations. Its manifest inventories exactly
five payload files; the checker rejects missing, extra, symlinked, byte-changed, or unlisted files
and recursively rejects absolute paths, host/user/environment values, raw IDs, free-form fact/query
text, emails, and secret/token patterns. It copies no facts, session map, model, Node/runtime tree,
source namespace, vector floats, raw stdout, or raw stderr.

Final freeze remains blocked until a public v4 bundle and its restricted exact-byte attestation are
retained under an approved access/publication contract and validate from a clean hydration. The
current legacy v3 archive remains diagnostic and privacy-failed; it is not publishable evidence.

Reject a cell when `fallback_used=true`, semantic was requested but unavailable, BM25 differs from the
arm declaration, namespaces overlap, source facts change, eligible-candidate counts differ between
arms, or any recorded hash fails verification.

The wrapper tests are hermetic and do not load the real model:

```sh
python3 -m unittest discover -s benchmarks/agent-brain/confirmatory -p 'test_verify_engines.py'
python3 -m unittest discover -s benchmarks/agent-brain/confirmatory -p 'test_public_engine_evidence.py'
```
