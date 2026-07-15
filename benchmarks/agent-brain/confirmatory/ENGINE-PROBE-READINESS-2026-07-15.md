# Retrieval-engine probe readiness — 2026-07-15

Status: **runtime smoke passed; fail-closed wrapper ready; freeze evidence incomplete; no paid calls**.

Read-only probes used the already-exposed development task `6699ec40a` against the full rolling
quarantine. All three runtime paths reported `identity_verified=true`, BM25 disabled, no fallback,
and the same temporal eligibility counts:

| Engine | Embedder | Dimension | Prefilter | Eligible | Excluded at/after cutoff | Delivered |
|---|---|---:|---:|---:|---:|---:|
| `lexical_handrolled` | none | — | 2,621 | 2,529 | 92 | 5 |
| `model2vec_rrf` | `minishlab/potion-retrieval-32M` | 512 | 2,621 | 2,529 | 92 | 5 |
| `embeddinggemma_rrf` | `ollama:embeddinggemma` | 768 | 2,621 | 2,529 | 92 | 5 |

The source facts SHA-256 is
`084f5170c07a8b843e6ffc7eac1939df0fa9c13d45ade9f5061d7c185195a793`; the session-date map is
`0ccd0a2ec938b354fa512fea090f2f68ca0e7b958f17ffa50784fb13108ed18a`. The locally available
EmbeddingGemma GGUF is 333,590,944 bytes with SHA-256
`b5ce9d77a3fc4b3b39ccb5643c36777911cc4eb46a66962eadfa3f5f60490d63`.

These observations are diagnostic, not the three retained records required by
`all_engines_machine_verified`. `ENTIRE_PLUGIN_CACHE_DIR` does not isolate fact vectors: semantic
vectors live under `ENTIRE_PLUGIN_DATA_DIR` in `facts/<branch>/embeddings/vectors.bin`. The frozen
vector artifact is EmbeddingGemma; the read-only Model2Vec probe therefore rebuilt vectors in memory
and produced no attributable Model2Vec artifact.

Before the gate can pass:

1. Create separate derived data roots for Model2Vec and EmbeddingGemma without changing the frozen
   source facts.
2. Persist and hash one vector artifact per semantic arm.
3. Retain the exact binary, stdout, stderr, vector artifacts, and GGUF through a durable evidence
   location that the protocol checker can byte-verify.
4. Emit exactly one schema record per arm and confirm identical corpus/eligibility identities,
   isolated namespaces, `fallback_used=false`, and the requested effective engine.

No fresh relevance or agent holdout was opened by these probes.

## Verification-wrapper checkpoint

`verify_engines.py` now implements the retained run without inferring identity from requested
environment or accepting caller-defined expected values:

- it loads the reproducible binary build/source-tree contract, production facts/session/GGUF/count,
  server-script, resolved-Node, package/lockfile, bounded health interval, and complete
  dependency-inventory expectations only from checked-in
  `engine-verification-pins.json`;
- it verifies the canonical binary and all pinned source and runtime bytes before creating output;
- it accepts only a query/cutoff/exclusion tuple from exposed development task `6699ec40a`;
- it creates distinct derived data, config, state, and cache roots for all three arms and never
  supplies the frozen data root to the binary;
- it starts and stops its own loopback EmbeddingGemma server, and refuses an endpoint already owned
  by another process; fresh nonce/counter health requests attest the same PID, ownership token,
  model hash, dimension, and Node version, with strict RFC3339 pre/start/during/finish/post ordering
  and an explicit during-health request that must finish while recall is still active;
- it independently derives and pins 2,529 temporally eligible facts and the exact 2,442
  active+eligible semantic candidate IDs; it rejects fallback, BM25, partial semantic coverage,
  inherited clean-namespace vectors, temporal/status drift, results above `K`, vector paths outside
  an arm's derived data root, and vector header/model/dimension/count/ID drift;
- it retains and hashes the facts/session sources, every derived facts file, binary/build
  attestation, recall and
  server logs, one vector artifact per semantic arm, GGUF, exact Node executable, server script,
  package and lockfile, dependency tree/inventory, and server attestation; and
- it recursively applies the exact manifest and record schemas, independently parses retained recall
  stdout plus every EBV1 float/model/dimension/count/fact ID, rejects non-finite/truncated/trailing
  payloads, binds all commands to the same canonical retained binary, and reconciles delivered facts
  to the retained active+eligible set; and
- it accepts only the exact v2 regular-file manifest wrapper, validates a temporary manifest with
  the protocol checker, and atomically renames it only after all three canonical records pass.
  Failed validation leaves no manifest.

Twenty-eight hermetic wrapper tests pass, including a full synthetic three-arm publication, exact
manifest-shape rejection, production rejection of test-fixture evidence, binary/stdout/vector
substitution, recursive extra-field rejection, one-vector coverage shrink, inactive/ineligible fact
delivery, delivered-count drift, NaN/infinity/truncation/trailing vectors, retained-source and
derived-facts rehashing, atomic publication failure, live-process health monitoring, recall-overlap
and timestamp-ordering failures, server PID/phase discontinuity, fallback, input-pin,
development-query, and endpoint-ownership cases.

The real retained run was preflighted against development task `6699ec40a`. It correctly refused
before creating an evidence directory because a pre-existing, unowned Node process (PID 70028 at the
time of the check) already held the matrix-pinned `127.0.0.1:11500` endpoint. That process was neither
reused nor stopped. The source facts, session-date map, and GGUF hashes remained unchanged.

The gate remains **pending** for two exact reasons:

1. The pinned endpoint must be made available by its owner so the wrapper can perform a clean,
   controlled three-arm run.
2. The resulting repo-relative bytes—especially the 333,590,944-byte GGUF—need an approved durable
   repository artifact strategy. A successful temporary or untracked run is not pass evidence.
