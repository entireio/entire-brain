# Offline relevance development set — 2026-07-15

Status: **development count floor met; null coverage pending; offline engine threshold not evaluated;
holdout unopened**.

This artifact uses only legacy C0701 tasks already marked exposed in `task-inventory.json`. It does
not read, label, or commit any fresh task or relevance holdout. The historical C0701 source split is
not treated as secrecy: every selected task is assigned `development` by the reconciled inventory.

## Current contents

| Measure | Count |
|---|---:|
| Development queries | 13 |
| Unique development tasks | 12 |
| Product queries | 12 |
| Oracle upper-bound queries | 1 |
| Corpus-closed null/no-answer queries | 0 |
| Judgments | 41 |
| Unique frozen facts in the committed snapshot | 39 |
| Legacy-packet-backed queries | 4 |
| Manual frozen-corpus-review queries | 9 |
| Sealed-holdout queries / tasks | 0 / 0 |

Judgments comprise 4 `solving`, 9 `relevant_alternative`, 15 `hard_topical_distractor`, and 13
`irrelevant` grades. The 13 queries come from three `agent_run` task queries, two queries on the one
`optimization_used` task, one `retrieval_probed` task, and seven `prompt_inspected` tasks. Prompt
inspection is safe here because the inventory records every selected legacy prompt as exposed and
development-only; it would not be safe for a fresh or `unseen` task.

The preregistered **development count floor** is now met (13 >= 12 queries and 12 >= 12 unique
tasks). This is not the `offline_development_threshold` gate. No three-engine ranked outputs have
been measured against these labels. Null false-positive rate is also unmeasurable because neither
former null candidate had retained proof of exhaustive review against all 2,531 active facts. They
are now non-null queries with explicit relevant-alternative judgments. Recall@5, null coverage,
temporal leakage, packet cluster occupancy, K, and aggregation selection remain pending.

The sealed relevance holdout remains exactly empty and unopened. Its current shortfall is 12 queries
and 12 unique tasks relative to the final-freeze minimum. No holdout commitment exists.

## Evidence and derivation

- `offline-relevance-development-labels.json` is the reviewed source. Product query text is not
  hand-tuned: the materializer requires it to equal the inventory-pinned task prompt byte for byte.
  Oracle text is explicit. Every judgment pins the canonical full-source fact hash.
- `offline-relevance-fact-snapshot.json` retains only the 39 referenced records from the full frozen
  quarantine plus the 36 provenance session dates needed to re-evaluate temporal eligibility.
- The full source facts file is pinned by SHA-256
  `084f5170c07a8b843e6ffc7eac1939df0fa9c13d45ade9f5061d7c185195a793`; its full session-date map
  is pinned by `0ccd0a2ec938b354fa512fea090f2f68ca0e7b958f17ffa50784fb13108ed18a`.
- The source contains 2,531 active facts. Their sorted ID/content-hash catalog is pinned by
  `22ded28af2c42f9f49bcb518a21b41381f247e7d0c3d35896b914230e1c6023c`. Labels additionally pin
  the selected fact-array and session-date-map hashes; the snapshot repeats and recomputes them.
- `offline-relevance-dataset.json` is generated. Query hashes are SHA-256 of exact UTF-8 query text.
  Fact hashes are SHA-256 of canonical JSON fact records (UTF-8, sorted keys, compact separators).
- `preregistration.json` pins the raw labels, snapshot, generated dataset, and all three relevance
  schema hashes. Normal `check_protocol.py` validation loads all of them, applies the schemas,
  rematerializes the dataset, and rejects coordinated snapshot/label/dataset mutation unless the
  versioned source contract is explicitly updated and reviewed.
- Eligibility follows the production fail-closed rule: every provenance session must be known,
  strictly before the task's cutoff, and absent from that task's excluded session IDs. The checked-in
  judgments all pass that test. Actual engine runs must still prove that no ineligible candidate is
  delivered.
- Each judgment includes a written rationale and a near-duplicate cluster ID. The four
  legacy-packet-backed queries additionally retain the exact `agent.stdout` SHA-256 from the exposed
  suite evidence; exact packet fact text was matched back to the pinned corpus before assigning IDs.

The d9df8fcca URL-fetch fact is deliberately a hard topical distractor: fetching by URL avoids
poisoning a named remote but creates the synthetic promisor entry whose bulk-fetch participation is
the task's bug. The companion literal-URL fact remains a relevant alternative because it explains
that mechanism without prescribing the `skipFetchAll` / `skipDefaultUpdate` solution.

This is a single-reviewer development label set, suitable for choosing and debugging the offline
evaluation mechanics. A future null label must use `exhaustive_active_corpus_review_v1`: query and
temporal-policy hashes, the pinned active catalog, identical eligible/reviewed counts and catalog
hashes, and zero positives are all mandatory. Any later decision to require independent double
annotation or adjudication must be made before the dataset is frozen; these labels do not imply
inter-rater reliability.

## Reproducible workflow

Candidate generation is non-mutating and filters to inventory-assigned exposed development tasks and
temporally eligible active facts:

```sh
python3 benchmarks/agent-brain/confirmatory/relevance_dataset.py propose \
  --inventory benchmarks/agent-brain/confirmatory/task-inventory.json \
  --facts "$FROZEN_FACTS_NDJSON" \
  --session-dates "$FROZEN_SESSION_DATES_JSON" \
  --limit 15
```

After human review edits semantic grades or fact IDs, bind every judgment and excerpt commitment to
the full pinned sources, then reproduce the content-addressed fact excerpt and dataset:

```sh
python3 benchmarks/agent-brain/confirmatory/relevance_dataset.py bind \
  --labels benchmarks/agent-brain/confirmatory/offline-relevance-development-labels.json \
  --facts "$FROZEN_FACTS_NDJSON" \
  --session-dates "$FROZEN_SESSION_DATES_JSON" \
  --output benchmarks/agent-brain/confirmatory/offline-relevance-development-labels.json

python3 benchmarks/agent-brain/confirmatory/relevance_dataset.py snapshot \
  --labels benchmarks/agent-brain/confirmatory/offline-relevance-development-labels.json \
  --facts "$FROZEN_FACTS_NDJSON" \
  --session-dates "$FROZEN_SESSION_DATES_JSON" \
  --output benchmarks/agent-brain/confirmatory/offline-relevance-fact-snapshot.json

python3 benchmarks/agent-brain/confirmatory/relevance_dataset.py materialize \
  --labels benchmarks/agent-brain/confirmatory/offline-relevance-development-labels.json \
  --inventory benchmarks/agent-brain/confirmatory/task-inventory.json \
  --snapshot benchmarks/agent-brain/confirmatory/offline-relevance-fact-snapshot.json \
  --output benchmarks/agent-brain/confirmatory/offline-relevance-dataset.json
```

Any intentional artifact or schema change must also update
`preregistration.json.offline_dataset.development_artifact_contract` in the same reviewed commit.
Validation never refreshes those hashes automatically; until the explicit version bump is recorded,
normal `check_protocol.py` fails closed.

CI and review need no external quarantine. They validate the preregistered full-source/catalog and
raw-artifact commitments, rebuild from the committed excerpt, apply all three schemas, and fail on an
inventory/config/prompt hash mismatch, unsafe task state/split, stale generated output, reviewed fact
hash or snapshot-date drift, a null query without exhaustive corpus closure, a positive null query,
an answerable query without an eligible positive, missing hard distractors, or any plaintext holdout
item:

```sh
python3 benchmarks/agent-brain/confirmatory/relevance_dataset.py validate \
  --labels benchmarks/agent-brain/confirmatory/offline-relevance-development-labels.json \
  --inventory benchmarks/agent-brain/confirmatory/task-inventory.json \
  --snapshot benchmarks/agent-brain/confirmatory/offline-relevance-fact-snapshot.json \
  --dataset benchmarks/agent-brain/confirmatory/offline-relevance-dataset.json

python3 -m unittest benchmarks/agent-brain/confirmatory/test_relevance_dataset.py
python3 benchmarks/agent-brain/confirmatory/check_protocol.py
```

Do not select K or an aggregation rule until the three verified engines have produced identical-
corpus ranked outputs and the preregistered development metrics are computed. Do not populate
`sealed_holdout.items` while `opened_at` is null.
