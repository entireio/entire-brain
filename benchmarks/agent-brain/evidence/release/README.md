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
`tool_result:` proof is required before Radar agent-lift evidence can be
cited.
Today those scopes are `history`, generic `mcp`, `mcp_radar_location_only`, and
`semantic`, so this directory cannot be used as facts or workspace Radar proof
by aggregation. Historical,
exploratory, quarantine, or `release-local-*` suites under
`benchmarks/agent-brain/results/` do not become release evidence just because
they exist; they must be regenerated from a committed panel into an explicit
`release-candidate-*` suite and pass the manifest-enforced gate.

Regression Radar and the MCP retrieval surface are also checked separately by
`mise run radar:evidence`, which audits deterministic Go/MCP tool-contract
evidence under `benchmarks/agent-brain/evidence/radar-tool`. That gate proves
the local detector, QMD-inspired MCP retrieval tools including branch-scoped facts,
hinted changed/deleted Radar loci, unsafe workspace pairing skips, workspace
Radar redaction, and MCP safety contracts. The retained release lane now adds
one audited Radar agent-lift suite for a deletion-attribution task. Radar
records must carry their deletion-policy bit in record provenance; the audit
does not infer it from mutable task files. Future workspace Radar proof must
also bind server `tool_args.workspace` to
`provenance.run_config.workspace_name`.

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
- `release-candidate-cli-radar-mcp-del-20260610-r2`:
  one deletion-attribution Radar task, 4 repetitions per side, audited with 0
  hard flags, 8/8 provenance-backed records, 4 MCP-verified condition records,
  4 server-named `brain_regressions` records, and 4 completed `tool_result`
  records. No-brain passed 2/4, `mcp_history` passed 4/4, mean score improved
  73.0 -> 92.5, mean tokens dropped 811,466.5 -> 408,947.75, mean search calls
  dropped 11.5 -> 6.0, and the stability tag is `brain_positive_stable`.
- `release-candidate-entire-brain-semantic-tokenized-idf-mini-low-20260610T232641Z`:
  one semantic tokenized-IDF ranking task, 4 repetitions per side, audited with
  0 hard flags and 1 semantic proof-ready comparison. No-brain passed 3/4,
  `semantic_brain` passed 4/4, mean score improved 79.0 -> 92.0, mean tokens
  dropped 663,613.75 -> 184,061.5, mean search calls dropped 13.0 -> 2.0, and
  the stability tag is `brain_positive_stable`.

Radar agent-lift status: one deletion-attribution Radar agent-lift suite is now
retained and release-citable for `mcp_radar_location_only`. Older Radar agent
evidence remains rejected because it was saturated, noisy, or lacked newer
server-side `tool_result` completion proof. Workspace Radar and broader
multi-task Radar claims still need separate retained proof.
