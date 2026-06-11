# Release Evidence

This directory is the nonignored retention lane for replay-lab evidence audits.
The current retained manifest is intentionally in `no_release_claim` mode: the
artifacts are kept so the auditor can explain why they are not citable release
proof yet.

`mise run release:evidence` writes an independent Codex benchmark audit report
to a temporary directory and compares it with the committed report without
rewriting this directory. Use `mise run release:evidence:update` only when
intentionally refreshing retained reports. In proof-required mode, the task
fails unless the selected release-candidate suites are provenance-complete,
audit-clean, validated by non-empty validation command results, backed by at
least one stable proof-ready comparison per retained suite, and have at least 4
repetitions per side, retained panel provenance, and the manifest's required
proof scopes. In the current no-claim mode, the task instead requires zero
proof-ready comparisons and allows only explicitly listed blocker flags.

For MCP-backed proof-required scopes, the proof-ready comparison must be backed
by matching MCP-verified condition records; a separate suite's MCP count cannot
satisfy the comparison backing. Server-named `tool:` and completed
`tool_result:` proof is required before Radar agent-lift evidence can be cited.
The retained no-claim artifacts currently cover historical `history`, generic
`mcp`, and `mcp_radar_location_only` candidate suites, but none are citable
replay-lab proof until B1 is cleared and the suites are rerun cleanly.
Historical, exploratory, quarantine, or `release-local-*` suites under
`benchmarks/agent-brain/results/` do not become release evidence just because
they exist; they must be regenerated from a committed panel into an explicit
`release-candidate-*` suite and pass the manifest-enforced gate.

Regression Radar and the MCP retrieval surface are also checked separately by
`mise run radar:evidence`, which audits deterministic Go/MCP tool-contract
evidence under `benchmarks/agent-brain/evidence/radar-tool`. That gate proves
the local detector, QMD-inspired MCP retrieval tools including branch-scoped facts,
hinted changed/deleted Radar loci, unsafe workspace pairing skips, workspace
Radar redaction, and MCP safety contracts. The retained release lane also
keeps one audited Radar agent-lift candidate for a deletion-attribution task,
but that candidate is demoted by B1 and is not release proof. Radar records
must carry their deletion-policy bit in record provenance; the audit does not
infer it from mutable task files. Future workspace Radar proof must also bind
server `tool_args.workspace` to `provenance.run_config.workspace_name`.

The committed manifest records the proof/no-claim policy. Generated audit
reports should only be committed after the gate passes under the intended
policy. The retained generated reports are `codex-audit-report.{json,md}`.

Retained suites may be pruned to the artifacts the independent auditor needs:
`summary.json`, `records.ndjson`, and per-run `record.json` files. Large copied
tool binaries and raw agent stdout/stderr stay in ignored local `results/`
unless a future audit explicitly needs them.

Current retained no-claim artifacts:

- `release-candidate-entire-brain-schema-contract-20260610T0115Z`: one focused
  history/full-brain schema-contract task, 4 repetitions per side. It is
  retained as invalidated candidate evidence because B1 finds answer-bearing
  `brain_queries` and task hash drift in the committed artifact lineage.
- `release-candidate-entire-cli-mcp-manual-attribution-20260610Tprogress`: one
  focused MCP-history manual-attribution task, 4 repetitions per side. The
  numbers are retained for audit history, but the comparison is not citable
  because the brain arm received answer-bearing query terms that the no-brain
  arm did not receive.
- `release-candidate-cli-radar-mcp-del-20260610-r2`:
  one deletion-attribution Radar task, 4 repetitions per side. Its server-side
  MCP/Radar activity is useful audit context, but the agent-lift comparison is
  not citable while B1 remains open.

Radar agent-lift status: no retained replay-lab agent-lift suite is currently
release-citable. The deterministic MCP/Radar tool-contract evidence remains
separate and citable for local tool behavior; agent-lift claims require clean
reruns without answer-bearing brain-only hints. Workspace Radar and broader
multi-task Radar claims still need separate retained proof.
