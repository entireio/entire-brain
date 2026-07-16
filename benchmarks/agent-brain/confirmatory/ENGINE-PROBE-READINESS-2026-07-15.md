# Retrieval-engine probe readiness — 2026-07-15

Status: **controlled three-arm verification passed; durable repository retention pending; no paid calls**.

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

On 2026-07-16 the controlled wrapper completed all three arms and atomically published a valid
three-record manifest at the local diagnostic artifact root
`/Users/thomi/.entire-brain-eval/retained-engine-evidence-20260716/run-bb68c0ac-v3/`.
The manifest SHA-256 is
`b7c9baae2c3f0ed9b0773226ececf5bf129bd4dd487c02ffab9945cf0fc9c17d`.
Independent production-pin validation returned zero errors. All arms used the same 2,621-fact
corpus and 2,529-fact temporal eligibility set, delivered five results, disabled BM25, and reported
no fallback. The two semantic arms retained isolated 2,442-vector EBV1 artifacts:

- Model2Vec: `5f50be06ef43e4383aeb332afd322ed1290017d6dda1d0c4538822a35218085f`;
- EmbeddingGemma: `eeac9dccb083972d948a27e7e7236fd0b2824dc70fe72a7e96f30faeae72add9`.

The 602 MB local bundle is diagnostic evidence, not yet repository-retained evidence. The
`all_engines_machine_verified` gate remains pending until an approved durable artifact strategy
retains those exact bytes at a checker-resolvable repository-relative location.

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

Twenty-nine hermetic wrapper tests pass, including a full synthetic three-arm publication, exact
manifest-shape rejection, production rejection of test-fixture evidence, binary/stdout/vector
substitution, recursive extra-field rejection, one-vector coverage shrink, inactive/ineligible fact
delivery, delivered-count drift, results above the pin-derived K limit,
NaN/infinity/truncation/trailing vectors, retained-source and
derived-facts rehashing, atomic publication failure, live-process health monitoring, recall-overlap
and timestamp-ordering failures, server PID/phase discontinuity, fallback, input-pin,
development-query, and endpoint-ownership cases.

The inherited Node server at PID 70028 was confirmed to be the benchmark's own EmbeddingGemma
service, stopped with owner authorization, and not reused as evidence. The wrapper then launched,
health-attested, and stopped its own server on `127.0.0.1:11500`. During the first complete run the
protocol checker exposed an ordering inconsistency in the dependency inventory: the runner hashed
component-sorted `Path` rows while the checker hashed serialized relative-path rows. Commit
`f125b5c2` canonicalizes the runner to serialized relative-path order, refreshes the production pin,
and adds the adversarial `a-b` versus `a/b` regression fixture. The regenerated v3 evidence passed.

The gate remains **pending** for one exact reason: the resulting bytes—especially the
333,590,944-byte GGUF—need an approved durable repository artifact strategy. A successful external
artifact-root run is not pass evidence even when its manifest validates cleanly.
