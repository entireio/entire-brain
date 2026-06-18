# Codex Benchmark Audit (independent re-check)

- Suites audited: **1**
- Agent records audited (prep excluded): **16**
- **Hard integrity flags: 0**
- Integrity-verified MCP datapoints (real calls + parentless baseline + server-log backed): **0**
- Named-tool MCP datapoints (server log names the required brain tool): **0**
- Completed named-tool MCP datapoints (required tool_result-backed for Radar proof): **0**
- Records with required provenance (source base/head + harness/config/tool hashes): **16/16**
- Stable proof-ready comparisons: **1**

- Proof-ready comparisons by scope: `history`=1

## Release Gate
**PASS.** This audit satisfies the configured release-evidence gate.

## Hard integrity flags

**None.** No record failed an integrity re-check.

## Soft notes (honest failures / context, NOT cheating)
- `H:harness_dirty`: 16
- `H:source_dirty`: 16

## Per-suite
| Suite | Records | Flagged | Provenance OK | Status |
|---|---:|---:|---:|---|
| panel-p01-clean-proof-claude-20260618T195058Z | 16 | 0 | 16 | PASS |

## Flagged records (detail)
None. All audited records passed independent re-checks.
