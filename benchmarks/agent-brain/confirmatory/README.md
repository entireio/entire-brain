# Confirmatory benchmark preparation

This directory contains the unpaid, preparatory Workstream 6 artifacts. It does **not** authorize a
paid agent run and it does not contain or open a fresh confirmatory holdout.

Current state: `integrated_methodology_draft_power_redesign_required`. The WS2 treatment contract,
WS3 pre-ranking temporal filter, WS4 scheduling/cache controls, and WS5 evidence controls have been
integrated and verified by content-addressed source and test evidence. Final freeze remains blocked
on fresh-task validity review, real three-engine records, relevance labels and holdouts, a defensible
powered design, and model pricing/budget approval. The confirmatory analyzer is implemented and
path-locked.

## Artifacts

- `PREREGISTRATION.md` and `preregistration.json`: human- and machine-readable candidate protocol.
- `task-inventory.json`: content-addressed reconciliation of all 46 unique C0701 tasks.
- `schemas/task-inventory.schema.json`: task exposure/contamination contract.
- `schemas/relevance-dataset.schema.json`: offline relevance labels and sealed-holdout contract.
- `offline-relevance-dataset.json`: unopened draft shell; it intentionally contains no holdout labels.
- `engine-matrix.json` and `ENGINE-VERIFICATION-RUNBOOK.md`: exact named arms and verification steps.
- `schemas/engine-verification.schema.json`: required machine output for every engine execution.
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
- `check_protocol.py`: offline integrity checker. `--freeze` additionally enforces final-freeze gates.

Run the non-paid checks with:

```sh
python3 benchmarks/agent-brain/confirmatory/check_protocol.py
python3 -m unittest benchmarks/agent-brain/confirmatory/test_check_protocol.py
```

The first command must pass now. The following command must fail until fresh-task review, relevance
labeling, fresh holdout sealing, powered-design repair, final pricing/budget approval, and durable
engine verification are complete:

```sh
python3 benchmarks/agent-brain/confirmatory/check_protocol.py --freeze
```
