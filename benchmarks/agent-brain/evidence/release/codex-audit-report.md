# Codex Benchmark Audit (independent re-check)

- Suites audited: **3**
- Agent records audited (prep excluded): **24**
- **Hard integrity flags: 0**
- Integrity-verified MCP datapoints (real calls + parentless baseline + server-log backed): **8**
- Named-tool MCP datapoints (server log names the required brain tool): **4**
- Completed named-tool MCP datapoints (required tool_result-backed for Radar proof): **4**
- Records with required provenance (source base/head + harness/config/tool hashes): **24/24**
- Stable proof-ready comparisons: **0**

## Release Gate
**PASS (NO RELEASE CLAIM).** This audit satisfies the retained no-claim gate and is not citable proof.

## Hard integrity flags

**None.** No record failed an integrity re-check.

## Soft notes (honest failures / context, NOT cheating)
- `N:mcp_call_count_mismatch`: 8
- `B:mcp_history_partial_missing_brain_search`: 4

## Per-suite
| Suite | Records | Flagged | Provenance OK | Status |
|---|---:|---:|---:|---|
| release-candidate-cli-radar-mcp-del-clean-20260611T0412Z | 8 | 0 | 8 | PASS |
| release-candidate-entire-brain-schema-contract-clean-20260611T0412Z | 8 | 0 | 8 | PASS |
| release-candidate-entire-cli-mcp-manual-attribution-clean-20260611T0412Z | 8 | 0 | 8 | PASS |

## Flagged records (detail)
None. All audited records passed independent re-checks.
