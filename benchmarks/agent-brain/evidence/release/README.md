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
per side, retained panel provenance, and the manifest's required proof scopes.
Today those scopes are `history`, `mcp`, and `mcp_radar_location_only`, so this
directory cannot be used as semantic or facts proof by aggregation. Historical,
exploratory, quarantine, or `release-local-*` suites under
`benchmarks/agent-brain/results/` do not become release evidence just because
they exist; they must be regenerated from a committed panel into an explicit
`release-candidate-*` suite and pass the manifest-enforced gate.

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
- `release-candidate-entire-cli-mcp-manual-attribution-20260610Tprogress`: one
  focused MCP-history manual-attribution task, 4 repetitions per side. No-brain
  passed 1/4, `mcp_history` passed 4/4, mean score improved 76.25 -> 92.0, and
  the retained audit requires 4 MCP-verified records. This supports a
  correctness/pass-rate claim only; the MCP arm used more time and tokens.
- `release-candidate-entire-cli-radar-mcp-manual-attribution-deletions-all-loci-rerun-20260610Tprogress`:
  one focused location-only Regression Radar MCP task, 4 repetitions per side.
  No-brain passed 1/4, `mcp_history` with `brain_regressions(location_only,
  include_deletions)` passed 4/4, mean score improved 77.25 -> 92.25, and the
  retained audit requires 4 MCP-verified Radar records and rejects deletion
  Radar records that omit `include_deletions: true`. This supports a
  location-only deletion-Radar pass-rate claim, not an answer-assisted fix
  claim.
