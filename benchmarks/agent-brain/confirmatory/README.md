# Confirmatory benchmark preparation

This directory contains the unpaid, preparatory Workstream 6 artifacts. It does **not** authorize a
paid agent run and it does not contain or open a fresh confirmatory holdout.

Current state: `draft_pending_ws2_ws5`. The candidate methodology is recorded, but final freeze is
blocked until the WS2 treatment/task-validity, WS3 temporal-completeness, WS4 scheduling/cache, and
WS5 evidence schemas and controls are integrated and verified.

## Artifacts

- `PREREGISTRATION.md` and `preregistration.json`: human- and machine-readable candidate protocol.
- `task-inventory.json`: content-addressed reconciliation of all 46 unique C0701 tasks.
- `schemas/task-inventory.schema.json`: task exposure/contamination contract.
- `schemas/relevance-dataset.schema.json`: offline relevance labels and sealed-holdout contract.
- `offline-relevance-dataset.json`: unopened draft shell; it intentionally contains no holdout labels.
- `engine-matrix.json` and `ENGINE-VERIFICATION-RUNBOOK.md`: exact named arms and verification steps.
- `schemas/engine-verification.schema.json`: required machine output for every engine execution.
- `go-no-go.json`: paid-run gate. Every item must be `pass`; `pending` is a hard no-go.
- `check_protocol.py`: offline integrity checker. `--freeze` additionally enforces final-freeze gates.

Run the non-paid checks with:

```sh
python3 benchmarks/agent-brain/confirmatory/check_protocol.py
python3 -m unittest benchmarks/agent-brain/confirmatory/test_check_protocol.py
```

The first command must pass now. The following command must fail until upstream integration,
relevance labeling, fresh holdout sealing, analyzer freezing, and engine verification are complete:

```sh
python3 benchmarks/agent-brain/confirmatory/check_protocol.py --freeze
```
