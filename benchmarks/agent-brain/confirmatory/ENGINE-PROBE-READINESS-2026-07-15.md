# Retrieval-engine probe readiness — 2026-07-15

Status: **runtime smoke passed; freeze evidence incomplete; no paid calls**.

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
