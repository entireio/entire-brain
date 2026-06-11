# Codex Benchmark Audit (independent re-check)

- Suites audited: **3**
- Agent records audited (prep excluded): **24**
- **Hard integrity flags: 0**
- Integrity-verified MCP datapoints (real calls + parentless baseline + server-log backed): **8**
- Named-tool MCP datapoints (server log names the required brain tool): **4**
- Completed named-tool MCP datapoints (required tool_result-backed for Radar proof): **4**
- Records with required provenance (source base/head + harness/config/tool hashes): **24/24**
- Stable proof-ready comparisons: **3**

- Proof-ready comparisons by scope: `history`=1, `mcp`=1, `mcp_radar_location_only`=1

- Named-tool proof-ready comparisons by scope: `mcp_radar_location_only`=1

## Release Gate
**PASS.** This audit satisfies the configured release-evidence gate.

## Hard integrity flags

**None.** No record failed an integrity re-check.

## Soft notes (honest failures / context, NOT cheating)
- `N:mcp_call_count_mismatch`: 8
- `B:mcp_history_partial_missing_brain_brief`: 4
- `B:mcp_history_partial_missing_brain_search`: 4

## Per-suite
| Suite | Records | Flagged | Provenance OK | Status |
|---|---:|---:|---:|---|
| release-candidate-cli-radar-mcp-del-20260610-r2 | 8 | 0 | 8 | PASS |
| release-candidate-entire-brain-schema-contract-20260610T0115Z | 8 | 0 | 8 | PASS |
| release-candidate-entire-cli-mcp-manual-attribution-20260610Tprogress | 8 | 0 | 8 | PASS |

## Flagged records (detail)
None. All audited records passed independent re-checks.
