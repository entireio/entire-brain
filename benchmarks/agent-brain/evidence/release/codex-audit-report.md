# Codex Benchmark Audit (independent re-check)

- Suites audited: **3**
- Agent records audited (prep excluded): **24**
- **Hard integrity flags: 35**
- Integrity-verified MCP datapoints (real calls + parentless baseline + server-log backed): **0**
- Named-tool MCP datapoints (server log names the required brain tool): **4**
- Completed named-tool MCP datapoints (required tool_result-backed for Radar proof): **4**
- Records with required provenance (source base/head + harness/config/tool hashes): **24/24**
- Stable proof-ready comparisons: **0**

## Release Gate
**PASS (NO RELEASE CLAIM).** This audit satisfies the retained no-claim gate and is not citable proof.

## Hard integrity flags (potential cheating/bias)
- `H:task_config_sha256_mismatch`: 16
- `J:answer_bearing_brain_queries`: 12
- `G:proof_ready_without_matching_records`: 3
- `G:proof_ready_without_mcp_verified_condition_records`: 2
- `G:proof_ready_without_named_mcp_tool_condition_records`: 1
- `G:proof_ready_without_completed_named_mcp_tool_condition_records`: 1

## Soft notes (honest failures / context, NOT cheating)
- `N:mcp_call_count_mismatch`: 8
- `B:mcp_history_partial_missing_brain_brief`: 4
- `B:mcp_history_partial_missing_brain_search`: 4

## Per-suite
| Suite | Records | Flagged | Provenance OK | Status |
|---|---:|---:|---:|---|
| release-candidate-cli-radar-mcp-del-20260610-r2 | 8 | 8 | 8 | FLAG |
| release-candidate-entire-brain-schema-contract-20260610T0115Z | 8 | 4 | 8 | FLAG |
| release-candidate-entire-cli-mcp-manual-attribution-20260610Tprogress | 8 | 8 | 8 | FLAG |

## Flagged records (detail)
### release-candidate-cli-radar-mcp-del-20260610-r2
- `no_brain_r1` [no_brain] valid=True score=94 mcp=0 -> H:task_config_sha256_mismatch
- `no_brain_r2` [no_brain] valid=False score=50 mcp=0 -> H:task_config_sha256_mismatch
- `no_brain_r3` [no_brain] valid=False score=54 mcp=0 -> H:task_config_sha256_mismatch
- `no_brain_r4` [no_brain] valid=True score=94 mcp=0 -> H:task_config_sha256_mismatch
- `mcp_history_r1` [mcp_history] valid=True score=93 mcp=2 -> H:task_config_sha256_mismatch, J:answer_bearing_brain_queries(count=6)
- `mcp_history_r2` [mcp_history] valid=True score=93 mcp=2 -> H:task_config_sha256_mismatch, J:answer_bearing_brain_queries(count=6)
- `mcp_history_r3` [mcp_history] valid=True score=93 mcp=2 -> H:task_config_sha256_mismatch, J:answer_bearing_brain_queries(count=6)
- `mcp_history_r4` [mcp_history] valid=True score=91 mcp=2 -> H:task_config_sha256_mismatch, J:answer_bearing_brain_queries(count=6)
- COMPARISON entireio-cli-radar-manual-attribution-deletions/codex-gpt-5.4-mini-low/mcp_history verdict=brain_positive -> G:proof_ready_without_mcp_verified_condition_records, G:proof_ready_without_named_mcp_tool_condition_records, G:proof_ready_without_completed_named_mcp_tool_condition_records, G:proof_ready_without_matching_records
### release-candidate-entire-brain-schema-contract-20260610T0115Z
- `entire-brain-history-codex-schema-contract__codex-gpt-5.5-high__full_brain__r1` [full_brain] valid=True score=91 mcp=0 -> J:answer_bearing_brain_queries(count=23)
- `entire-brain-history-codex-schema-contract__codex-gpt-5.5-high__full_brain__r2` [full_brain] valid=True score=92 mcp=0 -> J:answer_bearing_brain_queries(count=23)
- `entire-brain-history-codex-schema-contract__codex-gpt-5.5-high__full_brain__r3` [full_brain] valid=True score=92 mcp=0 -> J:answer_bearing_brain_queries(count=23)
- `entire-brain-history-codex-schema-contract__codex-gpt-5.5-high__full_brain__r4` [full_brain] valid=True score=91 mcp=0 -> J:answer_bearing_brain_queries(count=23)
- COMPARISON entire-brain-history-codex-schema-contract/codex-gpt-5.5-high/full_brain verdict=brain_positive -> G:proof_ready_without_matching_records
### release-candidate-entire-cli-mcp-manual-attribution-20260610Tprogress
- `entireio-cli-manual-commit-attribution-base__codex-gpt-5.4-mini-medium__no_brain__r1` [no_brain] valid=False score=72 mcp=0 -> H:task_config_sha256_mismatch
- `entireio-cli-manual-commit-attribution-base__codex-gpt-5.4-mini-medium__no_brain__r2` [no_brain] valid=False score=68 mcp=0 -> H:task_config_sha256_mismatch
- `entireio-cli-manual-commit-attribution-base__codex-gpt-5.4-mini-medium__no_brain__r3` [no_brain] valid=False score=72 mcp=0 -> H:task_config_sha256_mismatch
- `entireio-cli-manual-commit-attribution-base__codex-gpt-5.4-mini-medium__no_brain__r4` [no_brain] valid=True score=93 mcp=0 -> H:task_config_sha256_mismatch
- `entireio-cli-manual-commit-attribution-base__codex-gpt-5.4-mini-medium__mcp_history__r1` [mcp_history] valid=True score=91 mcp=4 -> H:task_config_sha256_mismatch, J:answer_bearing_brain_queries(count=6)
- `entireio-cli-manual-commit-attribution-base__codex-gpt-5.4-mini-medium__mcp_history__r2` [mcp_history] valid=True score=93 mcp=4 -> H:task_config_sha256_mismatch, J:answer_bearing_brain_queries(count=6)
- `entireio-cli-manual-commit-attribution-base__codex-gpt-5.4-mini-medium__mcp_history__r3` [mcp_history] valid=True score=91 mcp=4 -> H:task_config_sha256_mismatch, J:answer_bearing_brain_queries(count=6)
- `entireio-cli-manual-commit-attribution-base__codex-gpt-5.4-mini-medium__mcp_history__r4` [mcp_history] valid=True score=93 mcp=4 -> H:task_config_sha256_mismatch, J:answer_bearing_brain_queries(count=6)
- COMPARISON entireio-cli-manual-commit-attribution-base/codex-gpt-5.4-mini-medium/mcp_history verdict=brain_positive -> G:proof_ready_without_mcp_verified_condition_records, G:proof_ready_without_matching_records
