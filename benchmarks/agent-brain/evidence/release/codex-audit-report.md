# Codex Benchmark Audit (independent re-check)

- Suites audited: **2**
- Agent records audited (prep excluded): **16**
- **Hard integrity flags: 0**
- Integrity-verified MCP datapoints (real calls + parentless baseline + server-log backed): **4**
- Records with required provenance (source base/head + harness/config/tool hashes): **16/16**
- Stable proof-ready comparisons: **2**

- Proof-ready comparisons by scope: `history`=1, `mcp`=1

## Release Gate
**PASS.** This audit satisfies the configured release-evidence gate.

## Hard integrity flags

**None.** No record failed an integrity re-check.

## Per-suite
| Suite | Records | Flagged | Provenance OK | Status |
|---|---:|---:|---:|---|
| release-candidate-entire-brain-schema-contract-20260610T0115Z | 8 | 0 | 8 | PASS |
| release-candidate-entire-cli-mcp-manual-attribution-20260610Tprogress | 8 | 0 | 8 | PASS |

## Flagged records (detail)
None. All audited records passed independent re-checks.
