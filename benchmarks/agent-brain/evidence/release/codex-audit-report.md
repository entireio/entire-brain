# Codex Benchmark Audit (independent re-check)

- Suites audited: **3**
- Agent records audited (prep excluded): **24**
- **Hard integrity flags: 16**
- Integrity-verified MCP datapoints (real calls + isolated Git baseline + server-log backed): **4**
- Named-tool MCP datapoints (server log names the required brain tool): **4**
- Completed named-tool MCP datapoints (required tool_result-backed for Radar proof): **4**
- Records with required provenance (source base/head + harness/config/tool hashes): **24/24**
- Stable proof-ready comparisons: **0**

## Release Gate
**PASS (NO RELEASE CLAIM).** This audit satisfies the retained no-claim gate and is not citable proof.

## Hard integrity flags (potential cheating/bias)
- `H:task_config_sha256_mismatch`: 16

## Soft notes (honest failures / context, NOT cheating)
- `C:legacy_parentless_baseline`: 24
- `N:mcp_call_count_mismatch`: 8

## Per-suite
| Suite | Records | Flagged | Provenance OK | Status |
|---|---:|---:|---:|---|
| release-candidate-cli-radar-mcp-del-clean-20260611T0412Z | 8 | 0 | 8 | PASS |
| release-candidate-entire-brain-schema-contract-clean-20260611T0412Z | 8 | 8 | 8 | FLAG |
| release-candidate-entire-cli-mcp-manual-attribution-clean-20260611T0412Z | 8 | 8 | 8 | FLAG |

## Flagged records (detail)
### release-candidate-entire-brain-schema-contract-clean-20260611T0412Z
- `entire-brain-history-codex-schema-contract__codex-gpt-5.5-high__no_brain__r1` [no_brain] valid=False score=79 mcp=0 -> H:task_config_sha256_mismatch
- `entire-brain-history-codex-schema-contract__codex-gpt-5.5-high__no_brain__r2` [no_brain] valid=False score=78 mcp=0 -> H:task_config_sha256_mismatch
- `entire-brain-history-codex-schema-contract__codex-gpt-5.5-high__no_brain__r3` [no_brain] valid=True score=95 mcp=0 -> H:task_config_sha256_mismatch
- `entire-brain-history-codex-schema-contract__codex-gpt-5.5-high__no_brain__r4` [no_brain] valid=True score=94 mcp=0 -> H:task_config_sha256_mismatch
- `entire-brain-history-codex-schema-contract__codex-gpt-5.5-high__full_brain__r1` [full_brain] valid=False score=75 mcp=0 -> H:task_config_sha256_mismatch
- `entire-brain-history-codex-schema-contract__codex-gpt-5.5-high__full_brain__r2` [full_brain] valid=True score=93 mcp=0 -> H:task_config_sha256_mismatch
- `entire-brain-history-codex-schema-contract__codex-gpt-5.5-high__full_brain__r3` [full_brain] valid=True score=91 mcp=0 -> H:task_config_sha256_mismatch
- `entire-brain-history-codex-schema-contract__codex-gpt-5.5-high__full_brain__r4` [full_brain] valid=True score=92 mcp=0 -> H:task_config_sha256_mismatch
### release-candidate-entire-cli-mcp-manual-attribution-clean-20260611T0412Z
- `entireio-cli-manual-commit-attribution-base__codex-gpt-5.4-mini-medium__no_brain__r1` [no_brain] valid=True score=94 mcp=0 -> H:task_config_sha256_mismatch
- `entireio-cli-manual-commit-attribution-base__codex-gpt-5.4-mini-medium__no_brain__r2` [no_brain] valid=True score=96 mcp=0 -> H:task_config_sha256_mismatch
- `entireio-cli-manual-commit-attribution-base__codex-gpt-5.4-mini-medium__no_brain__r3` [no_brain] valid=True score=94 mcp=0 -> H:task_config_sha256_mismatch
- `entireio-cli-manual-commit-attribution-base__codex-gpt-5.4-mini-medium__no_brain__r4` [no_brain] valid=True score=93 mcp=0 -> H:task_config_sha256_mismatch
- `entireio-cli-manual-commit-attribution-base__codex-gpt-5.4-mini-medium__mcp_history__r1` [mcp_history] valid=True score=94 mcp=4 -> H:task_config_sha256_mismatch
- `entireio-cli-manual-commit-attribution-base__codex-gpt-5.4-mini-medium__mcp_history__r2` [mcp_history] valid=True score=95 mcp=4 -> H:task_config_sha256_mismatch
- `entireio-cli-manual-commit-attribution-base__codex-gpt-5.4-mini-medium__mcp_history__r3` [mcp_history] valid=True score=81 mcp=4 -> H:task_config_sha256_mismatch
- `entireio-cli-manual-commit-attribution-base__codex-gpt-5.4-mini-medium__mcp_history__r4` [mcp_history] valid=True score=95 mcp=4 -> H:task_config_sha256_mismatch
