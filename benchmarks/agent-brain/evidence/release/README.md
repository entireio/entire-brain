# Release Evidence

This directory is the nonignored retention lane for replay-lab evidence that is
safe to cite in release materials.

`mise run release:evidence` writes independent Codex and Regression Radar audit
reports to a temporary directory and compares them with the committed reports
without rewriting this directory. Use `mise run release:evidence:update` only
when intentionally refreshing retained reports after the gate passes. The task
fails unless the selected release-candidate suites are provenance-complete,
audit-clean, validated by non-empty validation command results, and backed by at
least one stable proof-ready comparison per retained suite, with at least 4
repetitions per side, retained panel provenance, and the manifest's required
proof scopes.
For MCP-backed scopes, the proof-ready comparison must be backed by matching
MCP-verified condition records; a separate suite's MCP count cannot satisfy the
comparison backing. Radar proof also requires the matching server-side MCP tool
name for Radar scopes, rechecks `summary.json` pass rates and record counts
against the audited record rows, and rejects deletion Radar records whose
captured safe boolean tool arguments did not include `include_deletions: true`,
so a stale or hand-edited summary cannot become citable evidence.
Today those scopes are `history`, `mcp`, and `mcp_radar_location_only`, so this
directory cannot be used as semantic or facts proof by aggregation. Historical,
exploratory, quarantine, or `release-local-*` suites under
`benchmarks/agent-brain/results/` do not become release evidence just because
they exist; they must be regenerated from a committed panel into an explicit
`release-candidate-*` suite and pass the manifest-enforced gate.

The committed manifest records the citable suite policy. Generated audit reports
should only be committed after the gate passes for a real release-candidate run.
The retained generated reports are `codex-audit-report.{json,md}` and
`radar-candidate-report.{json,md}`.

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
  retained audit requires 4 MCP-verified Radar records, server-side `tool:
  brain_regressions` proof, summary-vs-record agreement, and deletion records
  that keep `include_deletions: true`. This supports a location-only
  deletion-Radar pass-rate claim, not an answer-assisted fix claim.

To reproduce or refresh the retained Radar proof, run the committed panel from a
local `cli-bench` checkout at source/base/head commit
`af713665091693a8301520159f3b4097a6ec36f9`:

```sh
python3 benchmarks/agent-brain/run.py panel \
  release-entire-cli-radar-mcp-manual-attribution-deletions \
  --suite-name release-candidate-entire-cli-radar-mcp-manual-attribution-deletions-<timestamp> \
  --runners codex:gpt-5.4-mini:low \
  --conditions no_brain,mcp_history \
  --repetitions 4 \
  --refresh-brain-cache
```

That panel carries `BENCH_RADAR_LOCATION_ONLY=1` in its hashed config
(`d19bd0d68ebf07ebe8d37f8f5f15928f7e21ae241e6a23264f015a4ede233a43`).
Future citable reruns should retain `summary.json`, `records.ndjson`, each
per-run `record.json`, and the MCP `mcp-server.log` files. After copying a
sanitized release-candidate suite into this directory, run
`mise run release:evidence:update`, then `mise run release:evidence` and
`mise run radar:evidence`.
