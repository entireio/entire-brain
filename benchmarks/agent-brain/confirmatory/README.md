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
- `power_analysis.py` and `power-analysis.json`: deterministic unpaid sensitivity calculation. The
  current 24-task x 4-repetition design fails the conservative planning scenario; this is a no-go,
  not a reason to adopt the scenario's task-count estimate as empirical truth.
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
