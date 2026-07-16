# Development relevance query fixture v1

This contract enables an unpaid, development-only retrieval evaluation after an authorized safe
export supplies the 14 reviewed query texts. The repository intentionally does **not** contain
`development-relevance-queries-v1.json`. That path is ignored by Git because the eventual fixture
contains product-query plaintext; do not commit, publish, or paste it into logs.

The fixture is not confirmatory evidence, does not open a fresh holdout, and does not authorize a
provider or paid call.

## Fixed contract

The JSON schema is `schemas/development-relevance-queries-v1.schema.json`. The exact top-level
fields are `schema_version`, `fixture_id`, `purpose`, `created_at`, `source_bindings`, `coverage`,
`items`, and `fixture_sha256`. `purpose` is
`development_retrieval_evaluation_only`; coverage is fixed at 14 queries, 13 tasks, 12 answerable
product queries, one corpus-closed product null, and one oracle query.

Each item has exactly these fields:

`query_id`, `task_id`, `query_source`, `null_query`, `query_text`, `query_sha256`,
`task_prompt_sha256`, `source_config_sha256`, `temporal_cutoff`, `exclude_session_ids`, and
`temporal_policy_sha256`.

`fixture_sha256` is SHA-256 of UTF-8 canonical JSON (`sort_keys=True`, separators `,` and `:`,
Unicode retained) after removing only the top-level `fixture_sha256` field. A temporal-policy hash
uses the same canonical encoding of:

```json
{"exclude_session_ids":[],"temporal_cutoff":"..."}
```

with the fixture's actual array and cutoff.

The verifier requires every reviewed label query ID exactly once. Product query text hashes must
equal both `query_sha256` and the task inventory's `artifacts.prompt_sha256`; config hashes must
equal `artifacts.config_sha256`. The oracle text is bound directly to the existing reviewed label.
The null query's task, query hash, and temporal-policy hash are additionally bound to the null-review
ledger. User-prompt-derived tasks are unique; the sole oracle may share its product task.

## Source receipt

The safe exporter must pin these six repo-relative source paths and raw SHA-256 values:

| Role | Path | SHA-256 |
|---|---|---|
| labels | `benchmarks/agent-brain/confirmatory/offline-relevance-development-labels.json` | `1702cd625e71a9f5ad69ef41b5f5f1ef314bfa6df160d9aa002380f7a4ee77fe` |
| review ledger | `benchmarks/agent-brain/confirmatory/offline-relevance-review-ledger.json` | `67124c453fdfef5b8fe2602c6095e45078b093acb1b8a48dfaa73b0adb0c1f55` |
| null-review ledger | `benchmarks/agent-brain/confirmatory/offline-relevance-null-review-ledger.json` | `d9e7d71334a88a60cdbf4f71c8808c5383d616f54bc7fcda59ef7460e5d01e0b` |
| fact snapshot | `benchmarks/agent-brain/confirmatory/offline-relevance-fact-snapshot.json` | `a4c2061d2eaf8acde60594e7db7339c6ea6fa81304925f6d257341580eb07d79` |
| engine pins | `benchmarks/agent-brain/confirmatory/engine-verification-pins.json` | `c4e4989ffe22cefa2e71211ba2baa4027634daf07d19ff5e92a8b1289243db11` |
| task inventory | `benchmarks/agent-brain/confirmatory/task-inventory.json` | `082ff79f18a05035084ff12ea7f6d75a3b5ba17e6a54fccd2d6f1814f13c6d6c` |

`source_bindings.label_set_id` is
`exposed-c0701-manual-review-2026-07-15-v3`. `source_bindings.snapshot_roots` contains the exact eight
same-named fields from the fact snapshot:

```json
{
  "source_facts_sha256": "084f5170c07a8b843e6ffc7eac1939df0fa9c13d45ade9f5061d7c185195a793",
  "source_session_dates_sha256": "0ccd0a2ec938b354fa512fea090f2f68ca0e7b958f17ffa50784fb13108ed18a",
  "source_active_fact_count": 2531,
  "source_active_fact_catalog_sha256": "22ded28af2c42f9f49bcb518a21b41381f247e7d0c3d35896b914230e1c6023c",
  "fact_count": 44,
  "facts_sha256": "4f2f5269770444a912d53b759f86ae43bc17b6e4a16096eeb115206fae748410",
  "session_date_count": 39,
  "session_dates_sha256": "5293a15ee69018a5c1ba639f30cb2f7c67e325dca4c674ea54b7eee17f74456a"
}
```

If any receipt changes, stop and review the upstream artifact; do not silently refresh it.

## Safe export and verification

Safe export remains blocked until a human explicitly authorizes an approved source for the 14
development query texts. The exporter must not enumerate or open any path whose component is
`holdout` (case-insensitive), must not reconstruct text from hashes, and must not follow task
inventory `config_path` values. It should write the fixture atomically with mode `0600` and never
emit query text to stdout or stderr.

After that separately authorized export, run only the local verifier:

```sh
chmod 600 benchmarks/agent-brain/confirmatory/development-relevance-queries-v1.json
python3 benchmarks/agent-brain/confirmatory/development_relevance_queries.py \
  benchmarks/agent-brain/confirmatory/development-relevance-queries-v1.json \
  --repo-root .
```

The verifier lexically validates **all** six source paths before any source I/O. Absolute paths,
`..`, backslashes, non-canonical paths, duplicate paths, and any `holdout` component fail closed.
It authenticates raw source bytes, recomputes snapshot and fixture canonical roots, and then enforces
coverage, label, inventory, oracle, null-review, and temporal-policy bindings. Its success output is
metadata-only and omits query text, task IDs, session IDs, and source paths.

Run the synthetic contract tests and the existing unpaid protocol checker with:

```sh
python3 -m unittest benchmarks/agent-brain/confirmatory/test_development_relevance_queries.py
python3 benchmarks/agent-brain/confirmatory/check_protocol.py
```
