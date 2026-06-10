# Release Evidence

This directory is the nonignored retention lane for replay-lab evidence that is
safe to cite in release materials.

`mise run release:evidence` writes the independent Codex audit report to a
temporary directory and compares it with the committed reports without rewriting
this directory. Use `mise run release:evidence:update` only when intentionally
refreshing retained reports after the gate passes. The task fails unless
the selected release-candidate suites are provenance-complete, audit-clean,
validated by non-empty validation command results, and backed by at least one
stable proof-ready comparison per retained suite, with at least 4 repetitions
per side and retained panel provenance. Historical, exploratory, quarantine, or
`release-local-*` suites under `benchmarks/agent-brain/results/` do not become
release evidence just because they exist; they must be regenerated from a
committed panel into an explicit `release-candidate-*` suite and pass the
manifest-enforced gate.

The committed manifest records the citable suite policy. Generated audit reports
should only be committed after the gate passes for a real release-candidate run.

Retained suites may be pruned to the artifacts the independent auditor needs:
`summary.json`, `records.ndjson`, and per-run `record.json` files. Large copied
tool binaries and raw agent stdout/stderr stay in ignored local `results/`
unless a future audit explicitly needs them.

Current retained proof:

- `release-candidate-entire-brain-schema-contract-20260610T0115Z`: one focused
  history/full-brain schema-contract task, 4 repetitions per side, audited with
  0 hard flags and 1 proof-ready comparison.
