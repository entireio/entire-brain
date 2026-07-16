# Confirmatory benchmark preparation

This directory contains the unpaid, preparatory Workstream 6 artifacts. It does **not** authorize a
paid agent run and it does not contain or open a fresh confirmatory holdout.

Current state: `integrated_methodology_draft_power_redesign_required`. The WS2 treatment contract,
WS3 pre-ranking temporal filter, WS4 scheduling/cache controls, and WS5 evidence controls have been
integrated and verified by content-addressed source and test evidence. The exposed-only offline
development relevance set now contains 14 queries over 13 tasks, meets the 12-answerable-product-task
floor, and includes one corpus-closed product null. No three-engine metrics have been computed, and
the relevance holdout remains unopened and empty. Final freeze remains blocked on fresh-task validity
review, real three-engine records and development-threshold selection, relevance holdout sealing, a
defensible powered design, and model pricing/budget approval. The confirmatory analyzer is implemented
and path-locked.

## Artifacts

- `PREREGISTRATION.md` and `preregistration.json`: human- and machine-readable candidate protocol.
- `task-inventory.json`: content-addressed reconciliation of all 46 unique C0701 tasks.
- `schemas/task-inventory.schema.json`: task exposure/contamination contract.
- `schemas/relevance-dataset.schema.json`: generated offline relevance dataset and sealed-holdout
  contract; the adjacent relevance schemas cover reviewed labels, retained review evidence, the fact
  excerpt, and the complete source-membership contract.
- `OFFLINE-RELEVANCE-DEVELOPMENT-2026-07-15.md`: counts, contamination boundary, retained evidence
  binding, reproducible commands, and exact remaining count/threshold/holdout blockers.
- `offline-relevance-development-labels.json`: reviewed exposed-task label source.
- `offline-relevance-review-ledger.json`: retained per-query/per-judgment manual review evidence for
  all 14 queries; no label claims unavailable historical packet bytes as evidence.
- `offline-relevance-source-membership.json`: complete authenticated catalog of 2,621 facts (2,531
  active), their canonical provenance-session IDs, and 2,248 session dates; it derives exact
  active+eligible catalogs for each task policy.
- `relevance-source-contract.json`: small reviewed contract for the complete membership catalog; its
  raw digest is pinned independently in `check_protocol.py`, outside routine preregistration hashes.
- `offline-relevance-null-review-ledger.json` and `relevance-null-review-contract.json`: retain the
  exhaustive 2,531-decision review for `dev-product-b72a6e621` and its separately
  hard-pinned contract: four hard topical distractors, 2,527 irrelevant facts, and zero positives.
- `offline-relevance-fact-snapshot.json`: content-addressed 44-fact/39-date excerpt of the pinned
  full quarantine, sufficient for offline rebuild and validation.
- `relevance_dataset.py` and `test_relevance_dataset.py`: exposed-only proposal, snapshot,
  materialization, and fail-closed validation workflow.
- `offline-relevance-dataset.json`: generated 14-query/13-task development set with 46 judgments and
  one corpus-closed null; its sealed holdout intentionally contains no plaintext labels.
- `engine-matrix.json` and `ENGINE-VERIFICATION-RUNBOOK.md`: exact named arms and verification steps.
- `engine-verification-pins.json`: gate-authoritative corpus, query, reproducible binary build,
  GGUF, resolved Node, server script, package/lockfile, health interval, and dependency-inventory
  expectations, including exact eligible and active+eligible semantic-candidate set commitments;
  the runner does not accept these values from its caller.
- `schemas/engine-verification.schema.json` and
  `schemas/engine-verification-manifest.schema.json`: required per-arm output and the exact
  three-record manifest wrapper.
- `verify_engines.py`: fail-closed retained runner with isolated arms, owned-server continuity
  attestation overlapping live recall, byte-complete artifacts, and
  checker-before-atomic-publication semantics.
- `ENGINE-PROBE-READINESS-2026-07-15.md`: unpaid controlled three-engine verification result and
  the remaining durable repository-retention blocker; the external local bundle is not freeze
  evidence until its exact bytes are retained through the approved artifact strategy.
- `power_analysis.py`, `power-analysis.json`, and `POWER-DESIGN-OPTIONS-2026-07-15.md`: deterministic
  unpaid sensitivity and task/repetition tradeoffs. The current 24-task x 4-repetition design fails;
  no alternative row is approved without a human choice of calibration basis.
- `power-calibration-exploratory-v1.json`: content-addressed manifest for sparse retained legacy
  outcomes. The derived diagnostics are quarantined from confirmatory assumptions and cannot pass
  the power gate.
- `pricing-budget.json`, `pricing_budget.py`, and `schemas/pricing-budget.schema.json`: provider-neutral
  pricing quote, token-envelope arithmetic, staleness, and explicit budget-approval contract. All
  human/model/price fields remain pending. `PRICING-BUDGET-READINESS-2026-07-15.md` lists the exact
  decisions and evidence needed to close the two budget gates.
- `go-no-go.json`: paid-run gate. Every item must be `pass`; `pending` is a hard no-go.
- `integration-verification.json` and `integration-logs/`: exact WS2-WS5 source commits, content
  hashes, unpaid commands, and captured test outputs for the locked integration commit.
- `analyzer-lock.json`: path-bound hash of the exact confirmatory analyzer sources and schemas.
- `check_protocol.py`: offline integrity checker. It validates the independently pinned complete
  fact/date membership and per-query ledger bytes, validates the versioned relevance artifact and
  schema hashes, applies every checked-in relevance schema, and rematerializes the dataset. `--freeze`
  additionally enforces final-freeze gates, including the development task and null-query floors.

Run the non-paid checks with:

```sh
python3 benchmarks/agent-brain/confirmatory/check_protocol.py
python3 -m unittest benchmarks/agent-brain/confirmatory/test_check_protocol.py
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
```

The preparation and relevance validation commands must pass now. The following command must fail
until fresh-task review and three-engine development evaluation/threshold selection are complete, the
relevance holdout is sealed, the powered design is repaired, final pricing/budget is approved, and
durable engine verification exists:

```sh
python3 benchmarks/agent-brain/confirmatory/check_protocol.py --freeze
```
