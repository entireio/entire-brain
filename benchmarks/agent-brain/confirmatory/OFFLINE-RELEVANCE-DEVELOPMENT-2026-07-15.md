# Offline relevance development set — 2026-07-15

Status: **development count floor met; offline engine threshold not evaluated; holdout unopened**.

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
| Null/no-answer queries | 2 |
| Judgments | 40 |
| Unique frozen facts in the committed snapshot | 38 |
| Legacy-packet-backed queries | 4 |
| Manual frozen-corpus-review queries | 9 |
| Sealed-holdout queries / tasks | 0 / 0 |

Judgments comprise 5 `solving`, 7 `relevant_alternative`, 15 `hard_topical_distractor`, and 13
`irrelevant` grades. The 13 queries come from three `agent_run` task queries, two queries on the one
`optimization_used` task, one `retrieval_probed` task, and seven `prompt_inspected` tasks. Prompt
inspection is safe here because the inventory records every selected legacy prompt as exposed and
development-only; it would not be safe for a fresh or `unseen` task.

The preregistered **development count floor** is now met (13 >= 12 queries and 12 >= 12 unique
tasks). This is not the `offline_development_threshold` gate. No three-engine ranked outputs have
been measured against these labels, so Recall@5, null false-positive rate, temporal leakage, packet
cluster occupancy, K, and aggregation selection remain pending.

The sealed relevance holdout remains exactly empty and unopened. Its current shortfall is 12 queries
and 12 unique tasks relative to the final-freeze minimum. No holdout commitment exists.

## Evidence and derivation

- `offline-relevance-development-labels.json` is the reviewed source. Product query text is not
  hand-tuned: the materializer requires it to equal the inventory-pinned task prompt byte for byte.
  Oracle text is explicit.
- `offline-relevance-fact-snapshot.json` retains only the 38 referenced records from the full frozen
  quarantine plus the 35 provenance session dates needed to re-evaluate temporal eligibility.
- The full source facts file is pinned by SHA-256
  `084f5170c07a8b843e6ffc7eac1939df0fa9c13d45ade9f5061d7c185195a793`; its full session-date map
  is pinned by `0ccd0a2ec938b354fa512fea090f2f68ca0e7b958f17ffa50784fb13108ed18a`.
- `offline-relevance-dataset.json` is generated. Query hashes are SHA-256 of exact UTF-8 query text.
  Fact hashes are SHA-256 of canonical JSON fact records (UTF-8, sorted keys, compact separators).
- Eligibility follows the production fail-closed rule: every provenance session must be known,
  strictly before the task's cutoff, and absent from that task's excluded session IDs. The checked-in
  judgments all pass that test. Actual engine runs must still prove that no ineligible candidate is
  delivered.
- Each judgment includes a written rationale and a near-duplicate cluster ID. The four
  legacy-packet-backed queries additionally retain the exact `agent.stdout` SHA-256 from the exposed
  suite evidence; exact packet fact text was matched back to the pinned corpus before assigning IDs.

This is a single-reviewer development label set, suitable for choosing and debugging the offline
evaluation mechanics. Any later decision to require independent double annotation or adjudication
must be made before the dataset is frozen; these labels do not imply inter-rater reliability.

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

After human review edits the label source, reproduce the content-addressed fact excerpt and dataset:

```sh
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

CI and review need no external quarantine. They rebuild from the committed excerpt and fail on an
inventory/config/prompt hash mismatch, unsafe task state/split, stale generated output, fact hash or
kind drift, bad temporal provenance, a positive null query, an answerable query without an eligible
positive, missing hard distractors, or any plaintext holdout item:

```sh
python3 benchmarks/agent-brain/confirmatory/relevance_dataset.py validate \
  --labels benchmarks/agent-brain/confirmatory/offline-relevance-development-labels.json \
  --inventory benchmarks/agent-brain/confirmatory/task-inventory.json \
  --snapshot benchmarks/agent-brain/confirmatory/offline-relevance-fact-snapshot.json \
  --dataset benchmarks/agent-brain/confirmatory/offline-relevance-dataset.json

python3 -m unittest benchmarks/agent-brain/confirmatory/test_relevance_dataset.py
```

Do not select K or an aggregation rule until the three verified engines have produced identical-
corpus ranked outputs and the preregistered development metrics are computed. Do not populate
`sealed_holdout.items` while `opened_at` is null.
