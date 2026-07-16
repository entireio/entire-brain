# Development relevance query fixture v1

This contract enables an unpaid, development-only retrieval evaluation after an authorized safe
export supplies the 14 reviewed query texts. The repository intentionally does **not** contain
`development-relevance-queries-v1.json` or the accompanying temporal-policy receipt. Both paths are
ignored by Git; do not commit, publish, or paste either artifact into logs.

The fixture is not confirmatory evidence, does not open a fresh holdout, and does not authorize a
provider API request or paid agent-CLI invocation.

## Fixed contract

The JSON schema is `schemas/development-relevance-queries-v1.schema.json`. The exact top-level
fields are `schema_version`, `fixture_id`, `purpose`, `created_at`, `source_bindings`, `coverage`,
`items`, and `fixture_sha256`. `purpose` is
`development_retrieval_evaluation_only`, and `fixture_id` is pinned to
`entire-brain-development-relevance-queries-v1`. Coverage is fixed at 14 queries, 13 tasks, 12
answerable product queries, one corpus-closed product null, and one oracle query.

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

with the fixture's actual array and cutoff. This derivation is necessary but not sufficient: every
derived hash must also match a separately authenticated temporal-policy receipt.

The verifier requires every reviewed label query ID exactly once. Product query text hashes must
equal both `query_sha256` and the task inventory's `artifacts.prompt_sha256`; config hashes must
equal `artifacts.config_sha256`. The oracle text is bound directly to the existing reviewed label.
The null query's task, query hash, and temporal-policy hash are additionally bound to the null-review
ledger. User-prompt-derived tasks are unique; the sole oracle may share its product task.

## Machine-pinned source receipt

`development_relevance_queries.py` pins these six repo-relative paths and raw SHA-256 values in
code. The fixture must echo the exact machine trust root; fixture-controlled substitutions fail
before source I/O. Synthetic tests alone may inject a different receipt through private test-only
helpers.

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

## Temporal-policy receipt

The six permitted sources do not externally commit all 14 cutoff/exclusion policies. The fixture
therefore cannot authenticate its own `temporal_cutoff` and `exclude_session_ids` values. A second
authorized export must create the fixed private file
`benchmarks/agent-brain/confirmatory/development-relevance-temporal-policy-receipt-v1.json` under
`schemas/development-relevance-temporal-policy-receipt-v1.schema.json`.

That receipt contains exactly 14 records of `query_id`, `task_id`, and
`temporal_policy_sha256`; it contains neither query text nor cutoff/exclusion plaintext. Its fixed
identifier is `entire-brain-development-relevance-temporal-policy-v1`. Its `receipt_sha256` uses the
same canonical-JSON rule after removing only the top-level self-hash field.

After authorization and independent review, pin the receipt's **raw file SHA-256** in
`DEFAULT_TEMPORAL_RECEIPT_SHA256`. It is deliberately `None` now. Until that follow-up pin exists,
the production loader fails before reading the plaintext query fixture. A fixture self-hash or a
fixture-provided temporal hash can never substitute for this external receipt.

## Safe export and verification

Safe export remains blocked until a human explicitly authorizes approved sources for both the 14
development query texts and all 14 temporal policies. The exporter must not enumerate or open any
path whose component is `holdout` (case-insensitive), reconstruct values from hashes, or follow task
inventory `config_path` values. It must write both fixed-path files atomically as current-owner
regular files with mode `0600` and never emit private values to stdout or stderr.

After that separately authorized export, run only the local verifier:

```sh
chmod 600 benchmarks/agent-brain/confirmatory/development-relevance-queries-v1.json
chmod 600 \
  benchmarks/agent-brain/confirmatory/development-relevance-temporal-policy-receipt-v1.json
python3 benchmarks/agent-brain/confirmatory/development_relevance_queries.py \
  --repo-root .
```

The loader accepts the plaintext fixture only at its fixed repo-relative path. It rejects non-owner,
non-regular, symlinked, or non-`0600` files before content read. The verifier lexically validates
**all** six source paths before any source I/O, rejects symlinks in every repo-relative component,
and rechecks resolved components before secure open. Absolute paths, `..`, `//`, `/./`, backslashes,
duplicate paths, and any `holdout` component fail closed. It authenticates raw source bytes,
recomputes snapshot, fixture, receipt, and temporal-policy hashes, and then enforces coverage,
label, inventory, oracle, null-review, and external temporal bindings. Success output contains only
the authenticated fixture hash and aggregate counts; indexed failures never echo fixture-controlled
IDs or text.

Run the synthetic contract tests and the existing unpaid protocol checker with:

```sh
python3 -m unittest benchmarks/agent-brain/confirmatory/test_development_relevance_queries.py
python3 benchmarks/agent-brain/confirmatory/check_protocol.py
```
