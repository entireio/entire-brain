# Temporal Memory Phase 0A Development Report

- Status: **complete_with_protocol_deviation**
- Task: `temporal-memory-default-fact-merge-confidence`
- Source cache: `1dd2312593bd40ce7e66f748`
- Session transcript SHA-256: `55b9a4a04a2cfa2402898b132f43bd215f5c948fb56367aa0ad25b4d80470192`
- History index SHA-256: `06f3ac5c0520e396dccac3761a559be157b0120abc657b264837f107d37bcd35`
- Fact artifact SHA-256: `20bf8d495f6d6d656ad2ee9391667081d3170afeffacedbb75bf346910bd3dd8`
- Distilled facts: **10**

## Retrieval Probe

| Condition | Results | Expected ranks |
|---|---:|---|
| `raw_history` | 10 | 1, 3 |
| `facts_only` | 8 | none |
| `history_facts` | 10 | 2, 6 |

## Agent Smoke

| Runner | Resolved model | Condition | Validation | Protocol | Score | Tokens | Seconds | Brain calls |
|---|---|---|---:|---:|---:|---:|---:|---:|
| `codex-gpt-5.3-codex-spark-low` | `not output-confirmed` | `no_brain` | True | True | 91 | 3411588 | 143.7 | 0 |
| `codex-gpt-5.3-codex-spark-low` | `not output-confirmed` | `raw_history` | True | True | 97 | 154233 | 27.2 | 1 |
| `codex-gpt-5.3-codex-spark-low` | `not output-confirmed` | `facts_only` | False | True | 71 | 979125 | 42.7 | 1 |
| `codex-gpt-5.3-codex-spark-low` | `not output-confirmed` | `history_facts` | True | True | 97 | 137049 | 22.0 | 1 |
| `claude-sonnet-high` | `claude-sonnet-5` | `no_brain` | True | False | 90 | 6051900 | 530.5 | 0 |
| `claude-sonnet-high` | `claude-sonnet-5` | `raw_history` | True | True | 97 | 198070 | 41.7 | 1 |
| `claude-sonnet-high` | `claude-sonnet-5` | `facts_only` | False | True | 40 | 2402315 | 360.6 | 1 |
| `claude-sonnet-high` | `claude-sonnet-5` | `history_facts` | True | True | 97 | 210792 | 36.0 | 1 |

## Interpretation

Indexed history retained and retrieved the decisive pre-cutoff value, while the distilled fact store omitted it. This task therefore supports history-channel feasibility and records a fact-compression miss; it must not be used as a positive durable-facts task.

Both history-bearing arms passed validation for 2/2 backends; facts-only failed for 2/2 backends. `claude-sonnet-high` used 96.7% fewer tokens with history and 96.5% fewer with history plus facts than its no-Brain arm; `codex-gpt-5.3-codex-spark-low` used 95.5% fewer tokens with history and 96.0% fewer with history plus facts than its no-Brain arm. Protocol deviations occurred in `claude-sonnet-high/no_brain`; comparisons using those rows are diagnostic only even when validation passed. With one authored task and one repetition per backend, these are mechanism and failure-mode observations, not treatment-effect estimates.

This development task is diagnostic and not publication-level evidence.
