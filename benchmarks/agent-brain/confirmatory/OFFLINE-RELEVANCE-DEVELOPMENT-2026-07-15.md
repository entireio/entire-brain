# Offline relevance development set — 2026-07-15

Status: **development query, answerable product-task, and corpus-closed null floors met; offline engine
threshold not evaluated; holdout unopened**.

This artifact uses only legacy C0701 tasks already marked exposed in `task-inventory.json`. It does
not read, label, or commit any fresh task or relevance holdout. The historical C0701 source split is
not treated as secrecy: every selected task is assigned `development` by the reconciled inventory.

## Current contents

| Measure | Count |
|---|---:|
| Development queries | 14 |
| Unique development tasks | 13 |
| Product queries | 13 |
| Answerable product queries / unique tasks | 12 / 12 |
| Oracle upper-bound queries | 1 |
| Corpus-closed null/no-answer queries | 1 |
| Judgments | 46 |
| Unique frozen facts in the committed snapshot | 44 |
| Legacy-packet-backed queries | 0 |
| Manual frozen-corpus-review queries | 14 |
| Sealed-holdout queries / tasks | 0 / 0 |

Judgments comprise 5 `solving`, 9 `relevant_alternative`, 19 `hard_topical_distractor`, and 13
`irrelevant` grades. The 14 queries come from three `agent_run` task queries, two queries on the one
`optimization_used` task, one `retrieval_probed` task, and eight `prompt_inspected` tasks. Prompt
inspection is safe here because the inventory records every selected legacy prompt as exposed and
development-only; it would not be safe for a fresh or `unseen` task.

The preregistered **query floor** is met (14 >= 12), the **answerable product-task floor** is met
(12 >= 12), and the **corpus-closed product-null floor** is met (1 >= 1). The additional answerable
task is `dev-product-250538b1f`: its exact product prompt has a solving Last Prompt rule, a relevant
imported-turn reconstruction fact, one hard topical distractor, and one irrelevant control.
`dev-product-b72a6e621` is retained as a null after exhaustive review of all 2,531 exact
active+eligible facts found zero positives, four hard topical distractors, and 2,527 irrelevant
facts. This does not pass the `offline_development_threshold` gate. No three-engine ranked outputs
have been measured against these labels, so Recall@5, null false-positive rate, temporal leakage,
packet cluster occupancy, K, and aggregation selection remain pending.

The sealed relevance holdout remains exactly empty and unopened. Its current shortfall is 12 queries
and 12 unique tasks relative to the final-freeze minimum. No holdout commitment exists.

## Diagnostic evaluator authority boundary

`offline_relevance_eval.py` deterministically validates and scores text-free ranked IDs for the
development set. Evaluator v1 is intentionally not an authority lane. Its producer identity contains
self-hashed declarations, while retained producer/runner and policy bytes, an authenticated catalog
for every top-K fact's kind/cluster/token metadata, authoritative engine evidence, and the reviewed
source contract are not independently verified by this evaluator.

The strict report schema therefore fixes `decision_authority.status` to
`diagnostic_unattested`, fixes all evidence-verification booleans and
`authoritative_thresholds_enabled` to `false`, and retains the exact missing-evidence blockers.
Numeric comparisons are exposed only as `threshold_conditions_met`; `thresholds_passed` is always
`false`, authoritative selection fields are always null with status `non_authoritative`, and any
best K/aggregation tuple is separately named `diagnostic_best_candidate`. Inputs with unjudged
metadata, coordinated producer declarations, or arbitrary unretained vector identities remain
diagnostic and cannot produce an authoritative pass or selected winner. This containment does not
implement or pre-approve the later retained-byte/catalog/evidence authority contract.

## Evidence and derivation

- `offline-relevance-development-labels.json` is the reviewed label source. Product query text is not
  hand-tuned: the materializer requires it to equal the inventory-pinned task prompt byte for byte.
  Oracle text is explicit. Every judgment pins the canonical full-source fact hash and an exact entry
  in `offline-relevance-review-ledger.json`.
- `offline-relevance-review-ledger.json` retains all 14 per-query manual decisions, the exact judged
  fact IDs/hashes/grades/rationales, reviewer and method, and the source-membership roots. Every label
  records its repo-relative ledger path, exact query fragment, and the ledger's raw SHA-256.
- `offline-relevance-null-review-ledger.json` retains one grade decision for each of the 2,531
  active+eligible facts for `dev-product-b72a6e621`; `relevance-null-review-contract.json` pins the
  ledger bytes and full-source eligibility root, and `check_protocol.py` separately pins that
  contract at raw SHA-256
  `e606192db0f30cb091338db578cb88c7accb61e391a3ec21f06d83f6c40ecf0b`.
- `offline-relevance-fact-snapshot.json` retains only the 44 referenced records from the full frozen
  quarantine plus the 39 provenance session dates needed to re-evaluate temporal eligibility.
- The full source facts file is pinned by SHA-256
  `084f5170c07a8b843e6ffc7eac1939df0fa9c13d45ade9f5061d7c185195a793`; its full session-date map
  is pinned by `0ccd0a2ec938b354fa512fea090f2f68ca0e7b958f17ffa50784fb13108ed18a`.
- `offline-relevance-source-membership.json` retains the complete authenticated membership of 2,621
  facts (2,531 active), their canonical provenance-session IDs, and all 2,248 session dates. It binds
  every fact ID to its canonical content hash/status/provenance and every session ID to its date. The active ID/content-hash catalog remains
  `22ded28af2c42f9f49bcb518a21b41381f247e7d0c3d35896b914230e1c6023c`; the complete fact/status
  membership root is `9a3ce47d43fa2455fb1792d6a512dffebcb20ca8ae74a44e9caaf91095b95fba`, and the session/date
  root is `6b38bff80edf9c471f7fdc4c4c817bff7cf4d6b2a2d5f641ec740c113ade95ae`.
  The provenance-aware eligibility membership root is
  `d1ecd24bfbd590a800e9323868f73131b498d84bd055880505dfc0dc757174af`.
- `relevance-source-contract.json` pins that complete membership file and all source counts, raw
  hashes, and roots. Independently of routinely refreshed preregistration artifact hashes,
  `check_protocol.py` pins the contract's raw SHA-256 as
  `5708b8f6f0ade1cedf4e1f7d0b4ff499d707e2c9cd93034d9e38a0aebb9836e1`. Changing the source trust
  root therefore requires an explicit reviewed checker-code change.
- `offline-relevance-dataset.json` is generated. Query hashes are SHA-256 of exact UTF-8 query text.
  Fact hashes are SHA-256 of canonical JSON fact records (UTF-8, sorted keys, compact separators).
- `preregistration.json` pins the raw labels, review ledger, snapshot, generated dataset, and their
  relevance schema hashes. Normal `check_protocol.py` validation additionally verifies the separately
  pinned source contract and complete membership before applying the schemas and rematerializing the
  dataset. Refreshing the routine artifact hashes cannot authorize a fabricated fact or session date.
- Eligibility follows the production fail-closed rule: every provenance session must be known,
  strictly before the task's cutoff, and absent from that task's excluded session IDs. The checked-in
  judgments all pass that test. Actual engine runs must still prove that no ineligible candidate is
  delivered.
- Each judgment includes a written rationale and a near-duplicate cluster ID. All label evidence is
  retained manual-review evidence. Historical packet bytes are unavailable and are not used or
  claimed as evidence for any label.

The d9df8fcca URL-fetch fact is deliberately a hard topical distractor: fetching by URL avoids
poisoning a named remote but creates the synthetic promisor entry whose bulk-fetch participation is
the task's bug. The companion literal-URL fact remains a relevant alternative because it explains
that mechanism without prescribing the `skipFetchAll` / `skipDefaultUpdate` solution.

This is a single-reviewer development label set, suitable for choosing and debugging the offline
evaluation mechanics. The product-derived null uses `exhaustive_active_corpus_review_v2`: the checker
derives the exact active+eligible catalog from the authenticated provenance membership and requires
one retained grade decision per fact. Counts, catalog hashes, decision hash, and zero positives are
derived. The populated null-review ledger and contract are hard-pinned; changing the exhaustive
review requires an explicit trust-root update. Any later decision to require independent double
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

After human review edits semantic grades or fact IDs, first check the complete membership and its
small reviewed contract against the full pinned sources:

```sh
python3 benchmarks/agent-brain/confirmatory/relevance_dataset.py source-membership \
  --facts "$FROZEN_FACTS_NDJSON" \
  --session-dates "$FROZEN_SESSION_DATES_JSON" \
  --output benchmarks/agent-brain/confirmatory/offline-relevance-source-membership.json \
  --check

python3 benchmarks/agent-brain/confirmatory/relevance_dataset.py source-contract \
  --membership benchmarks/agent-brain/confirmatory/offline-relevance-source-membership.json \
  --membership-path benchmarks/agent-brain/confirmatory/offline-relevance-source-membership.json \
  --output benchmarks/agent-brain/confirmatory/relevance-source-contract.json \
  --check
```

Then bind every judgment and excerpt commitment, check the retained review ledger, and reproduce the
content-addressed fact excerpt and dataset:

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

python3 benchmarks/agent-brain/confirmatory/relevance_dataset.py review-ledger \
  --labels benchmarks/agent-brain/confirmatory/offline-relevance-development-labels.json \
  --source-contract benchmarks/agent-brain/confirmatory/relevance-source-contract.json \
  --reviewed-at 2026-07-15T15:30:00+02:00 \
  --reviewer codex-relevance-label-audit \
  --output benchmarks/agent-brain/confirmatory/offline-relevance-review-ledger.json \
  --check

python3 benchmarks/agent-brain/confirmatory/relevance_dataset.py materialize \
  --labels benchmarks/agent-brain/confirmatory/offline-relevance-development-labels.json \
  --inventory benchmarks/agent-brain/confirmatory/task-inventory.json \
  --snapshot benchmarks/agent-brain/confirmatory/offline-relevance-fact-snapshot.json \
  --source-membership benchmarks/agent-brain/confirmatory/offline-relevance-source-membership.json \
  --null-review-ledger benchmarks/agent-brain/confirmatory/offline-relevance-null-review-ledger.json \
  --output benchmarks/agent-brain/confirmatory/offline-relevance-dataset.json
```

Any intentional labels, ledger, snapshot, dataset, or schema change must also update
`preregistration.json.offline_dataset.development_artifact_contract` in the same reviewed commit.
Validation never refreshes those hashes automatically. The source trust root is separate: changing
`relevance-source-contract.json` also requires an explicit reviewed update to the digest embedded in
`check_protocol.py`.

CI and review need no external quarantine. They verify the independently pinned complete source
membership, retained per-query ledger bytes, and preregistered raw-artifact commitments; rebuild from
the committed excerpt; apply every checked-in relevance schema; and fail on a fabricated or drifted
fact/date, missing or mismatched ledger evidence, inventory/config/prompt hash mismatch, unsafe task
state/split, stale generated output, a null query without exhaustive corpus closure, a positive null
query, an answerable query without an eligible positive, missing hard distractors, or any plaintext
holdout item:

```sh
python3 benchmarks/agent-brain/confirmatory/relevance_dataset.py validate \
  --labels benchmarks/agent-brain/confirmatory/offline-relevance-development-labels.json \
  --inventory benchmarks/agent-brain/confirmatory/task-inventory.json \
  --snapshot benchmarks/agent-brain/confirmatory/offline-relevance-fact-snapshot.json \
  --dataset benchmarks/agent-brain/confirmatory/offline-relevance-dataset.json \
  --source-contract benchmarks/agent-brain/confirmatory/relevance-source-contract.json \
  --source-membership benchmarks/agent-brain/confirmatory/offline-relevance-source-membership.json \
  --review-ledger benchmarks/agent-brain/confirmatory/offline-relevance-review-ledger.json \
  --null-review-ledger benchmarks/agent-brain/confirmatory/offline-relevance-null-review-ledger.json \
  --null-review-contract benchmarks/agent-brain/confirmatory/relevance-null-review-contract.json

python3 -m unittest benchmarks/agent-brain/confirmatory/test_relevance_dataset.py
python3 benchmarks/agent-brain/confirmatory/check_protocol.py
```

Do not select K or an aggregation rule until the three verified engines have produced identical-
corpus ranked outputs and the preregistered development metrics are computed. Do not populate
`sealed_holdout.items` while `opened_at` is null.
