# Confirmatory benchmark preparation

This directory contains the unpaid, preparatory Workstream 6 artifacts. It does **not** authorize a
paid agent run and it does not contain or open a fresh confirmatory holdout.

Current state: `integrated_methodology_draft_power_redesign_required`. The WS2 treatment contract,
WS3 pre-ranking temporal filter, WS4 scheduling/cache controls, and WS5 evidence controls have been
integrated and verified by content-addressed source and test evidence. An exposed-only offline
development relevance set now meets the 12-query floor exactly but has only 11 of the required 12
unique tasks. No three-engine metrics have been computed, no corpus-closed null query exists, and the
relevance holdout remains unopened and empty. Final freeze remains blocked on one additional labeled
development task, fresh-task validity review, real three-engine records and development threshold,
null-query coverage, relevance holdout sealing, a defensible powered design, and model pricing/budget
approval. The confirmatory analyzer is implemented and path-locked.

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
  all 12 queries; no label claims unavailable historical packet bytes as evidence.
- `offline-relevance-source-membership.json`: complete authenticated catalog of 2,621 facts (2,531
  active) and 2,248 session dates.
- `relevance-source-contract.json`: small reviewed contract for the complete membership catalog; its
  raw digest is pinned independently in `check_protocol.py`, outside routine preregistration hashes.
- `offline-relevance-fact-snapshot.json`: content-addressed 37-fact/34-date excerpt of the pinned
  full quarantine, sufficient for offline rebuild and validation.
- `relevance_dataset.py` and `test_relevance_dataset.py`: exposed-only proposal, snapshot,
  materialization, and fail-closed validation workflow.
- `offline-relevance-dataset.json`: generated 12-query/11-task development set with 38 judgments and
  zero corpus-closed nulls; its sealed holdout intentionally contains no plaintext labels.
- `engine-matrix.json` and `ENGINE-VERIFICATION-RUNBOOK.md`: exact named arms and verification steps.
- `engine-verification-pins.json`: gate-authoritative corpus, query, GGUF, resolved Node, server
  script, package/lockfile, and dependency-inventory expectations; the runner does not accept these
  values from its caller.
- `schemas/engine-verification.schema.json`: required machine output for every engine execution.
- `verify_engines.py`: fail-closed retained runner with isolated arms, owned-server continuity
  attestation, byte-complete artifacts, and checker-before-atomic-publication semantics.
- `ENGINE-PROBE-READINESS-2026-07-15.md`: unpaid three-path runtime smoke and the remaining vector
  isolation/evidence blocker; it is not final engine verification.
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
  --review-ledger benchmarks/agent-brain/confirmatory/offline-relevance-review-ledger.json
python3 -m unittest benchmarks/agent-brain/confirmatory/test_relevance_dataset.py
```

The preparation and relevance validation commands must pass now. The following command must fail
until the one-task development shortfall and null-query coverage are repaired, fresh-task review and
three-engine development evaluation/threshold selection are complete, the relevance holdout is
sealed, the powered design is repaired, final pricing/budget is approved, and durable engine
verification exists:

```sh
python3 benchmarks/agent-brain/confirmatory/check_protocol.py --freeze
```
