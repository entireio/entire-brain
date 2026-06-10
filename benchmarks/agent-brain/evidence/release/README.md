# Release Evidence

This directory is the nonignored retention lane for replay-lab evidence that is
safe to cite in release materials.

`mise run release:evidence` writes an independent Codex benchmark audit report
to a temporary directory and compares it with the committed report without
rewriting this directory. Use `mise run release:evidence:update` only when
intentionally refreshing retained reports after the gate passes. The task
fails unless the selected release-candidate suites are provenance-complete,
audit-clean, validated by non-empty validation command results, and backed by at
least one stable proof-ready comparison per retained suite, with at least 4
repetitions per side, retained panel provenance, and the manifest's required
proof scopes.
For MCP-backed scopes, the proof-ready comparison must be backed by matching
MCP-verified condition records; a separate suite's MCP count cannot satisfy the
comparison backing. The current generic MCP release manifest requires at least
four basic MCP-verified datapoints; server-named `tool:` and completed
`tool_result:` proof is required before future Radar agent-lift evidence can be
cited.
Today those scopes are `history` and `mcp`, so this directory cannot be used as
semantic, facts, or Radar proof by aggregation. Historical,
exploratory, quarantine, or `release-local-*` suites under
`benchmarks/agent-brain/results/` do not become release evidence just because
they exist; they must be regenerated from a committed panel into an explicit
`release-candidate-*` suite and pass the manifest-enforced gate.

Regression Radar and the MCP retrieval surface are checked separately by
`mise run radar:evidence`, which audits deterministic Go/MCP tool-contract
evidence under `benchmarks/agent-brain/evidence/radar-tool`. That gate proves
the local detector, QMD-style MCP retrieval tools including branch-scoped facts,
and MCP safety contracts, not agent pass-rate lift.

The committed manifest records the citable suite policy. Generated audit reports
should only be committed after the gate passes for a real release-candidate run.
The retained generated reports are `codex-audit-report.{json,md}`.

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
  correctness/pass-rate claim only; its older retained `mcp-server.log` files
  prove server `tools/call` counts but do not include named `tool:` lines, so it
  is not named-tool MCP proof. The MCP arm used more time and tokens.

Radar agent-lift status: older retained Radar agent evidence was pruned because
it lacked the newer server-side `tool_result` completion proof. Recent reruns of
the committed Radar panels were saturated or noisy on the no-brain baseline, so
they are not retained as release evidence. Keep Radar launch copy scoped to the
tool-contract evidence, and keep MCP launch copy scoped to local tool-contract
plus retained generic MCP replay proof, until a fresh non-saturated,
`tool_result`-backed Radar panel passes.
