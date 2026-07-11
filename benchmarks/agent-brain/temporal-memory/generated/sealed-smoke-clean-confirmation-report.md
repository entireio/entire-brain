# Temporal Memory Phase 0A Sealed Smoke

- Decision: **NO-GO**
- Rows: **24**
- Validation passes: **23/24**
- Protocol passes: **20/24**
- Source cache: `1dd2312593bd40ce7e66f748`

## Gates

| Gate | Pass | Evidence |
|---|---:|---|
| `complete_matrix` | True | 24/24 rows |
| `runner_matrix` | True | actual=['claude-sonnet-high', 'codex-gpt-5.3-codex-spark-low']; expected=['claude-sonnet-high', 'codex-gpt-5.3-codex-spark-low'] |
| `row_integrity` | False | 19/24 rows |
| `fact_positive_channel` | True | 2/2 facts_only rows passed validation and protocol |
| `stale_memory_rejected` | True | 6/6 stale/conflict memory rows restored shipped behavior |
| `neutral_correctness` | False | 5/8 neutral rows passed validation and protocol |
| `positive_task_headroom` | False | no fact-positive no_brain row failed while a protocol-valid memory row passed |
| `task_agent_token_accounting` | True | 24/24 rows |
| `distillation_token_accounting` | False | distillation retained call count and duration but not reported token usage |
| `patch_artifact_retention` | False | 0/24 exact agent patches retained and checksum-verified |
| `clean_run_provenance` | True | 24/24 rows have clean harness and source worktrees |
| `portable_source_artifact` | True | private source archive checksum verified |

## Outcomes

| Task | Stratum | Runner | Condition | Validation | Protocol | Tokens | Seconds | Searches |
|---|---|---|---|---:|---:|---:|---:|---:|
| `temporal-memory-sealed-expand-opt-in` | `fact_positive` | `claude-sonnet-high` | `no_brain` | True | True | 544358 | 159.5 | 16 |
| `temporal-memory-sealed-expand-opt-in` | `fact_positive` | `claude-sonnet-high` | `raw_history` | True | True | 405186 | 55.1 | 6 |
| `temporal-memory-sealed-expand-opt-in` | `fact_positive` | `claude-sonnet-high` | `facts_only` | True | True | 549191 | 86.6 | 15 |
| `temporal-memory-sealed-expand-opt-in` | `fact_positive` | `claude-sonnet-high` | `history_facts` | True | False | 1326938 | 172.8 | 34 |
| `temporal-memory-sealed-expand-opt-in` | `fact_positive` | `codex-gpt-5.3-codex-spark-low` | `no_brain` | True | True | 156950 | 37.6 | 2 |
| `temporal-memory-sealed-expand-opt-in` | `fact_positive` | `codex-gpt-5.3-codex-spark-low` | `raw_history` | True | False | 467728 | 60.4 | 6 |
| `temporal-memory-sealed-expand-opt-in` | `fact_positive` | `codex-gpt-5.3-codex-spark-low` | `facts_only` | True | True | 157996 | 16.8 | 2 |
| `temporal-memory-sealed-expand-opt-in` | `fact_positive` | `codex-gpt-5.3-codex-spark-low` | `history_facts` | True | True | 152031 | 13.7 | 1 |
| `temporal-memory-sealed-semantic-default-stale-history` | `stale_conflict` | `claude-sonnet-high` | `no_brain` | True | True | 848240 | 126.6 | 27 |
| `temporal-memory-sealed-semantic-default-stale-history` | `stale_conflict` | `claude-sonnet-high` | `raw_history` | True | True | 666297 | 117.6 | 16 |
| `temporal-memory-sealed-semantic-default-stale-history` | `stale_conflict` | `claude-sonnet-high` | `facts_only` | True | True | 684112 | 62.9 | 15 |
| `temporal-memory-sealed-semantic-default-stale-history` | `stale_conflict` | `claude-sonnet-high` | `history_facts` | True | True | 634672 | 113.0 | 13 |
| `temporal-memory-sealed-semantic-default-stale-history` | `stale_conflict` | `codex-gpt-5.3-codex-spark-low` | `no_brain` | True | True | 1013790 | 42.6 | 10 |
| `temporal-memory-sealed-semantic-default-stale-history` | `stale_conflict` | `codex-gpt-5.3-codex-spark-low` | `raw_history` | True | True | 832940 | 88.3 | 8 |
| `temporal-memory-sealed-semantic-default-stale-history` | `stale_conflict` | `codex-gpt-5.3-codex-spark-low` | `facts_only` | True | True | 561183 | 35.3 | 4 |
| `temporal-memory-sealed-semantic-default-stale-history` | `stale_conflict` | `codex-gpt-5.3-codex-spark-low` | `history_facts` | True | True | 938705 | 47.6 | 10 |
| `temporal-memory-sealed-windows-drive-neutral` | `neutral` | `claude-sonnet-high` | `no_brain` | True | True | 469686 | 158.8 | 10 |
| `temporal-memory-sealed-windows-drive-neutral` | `neutral` | `claude-sonnet-high` | `raw_history` | True | True | 1490131 | 170.2 | 14 |
| `temporal-memory-sealed-windows-drive-neutral` | `neutral` | `claude-sonnet-high` | `facts_only` | True | False | 872954 | 178.3 | 16 |
| `temporal-memory-sealed-windows-drive-neutral` | `neutral` | `claude-sonnet-high` | `history_facts` | True | True | 1009216 | 133.5 | 15 |
| `temporal-memory-sealed-windows-drive-neutral` | `neutral` | `codex-gpt-5.3-codex-spark-low` | `no_brain` | False | True | 397978 | 39.4 | 4 |
| `temporal-memory-sealed-windows-drive-neutral` | `neutral` | `codex-gpt-5.3-codex-spark-low` | `raw_history` | True | False | 394459 | 41.6 | 5 |
| `temporal-memory-sealed-windows-drive-neutral` | `neutral` | `codex-gpt-5.3-codex-spark-low` | `facts_only` | True | True | 254936 | 26.5 | 3 |
| `temporal-memory-sealed-windows-drive-neutral` | `neutral` | `codex-gpt-5.3-codex-spark-low` | `history_facts` | True | True | 342923 | 31.3 | 3 |

## Relative Cost

Positive percentages are overhead versus the same task/runner no-Brain row.

| Task | Runner | Condition | Token delta | Time delta | Search delta |
|---|---|---|---:|---:|---:|
| `temporal-memory-sealed-expand-opt-in` | `claude-sonnet-high` | `raw_history` | -25.6% | -65.4% | -10 |
| `temporal-memory-sealed-expand-opt-in` | `claude-sonnet-high` | `facts_only` | +0.9% | -45.7% | -1 |
| `temporal-memory-sealed-expand-opt-in` | `claude-sonnet-high` | `history_facts` | +143.8% | +8.3% | +18 |
| `temporal-memory-sealed-expand-opt-in` | `codex-gpt-5.3-codex-spark-low` | `raw_history` | +198.0% | +60.6% | +4 |
| `temporal-memory-sealed-expand-opt-in` | `codex-gpt-5.3-codex-spark-low` | `facts_only` | +0.7% | -55.2% | +0 |
| `temporal-memory-sealed-expand-opt-in` | `codex-gpt-5.3-codex-spark-low` | `history_facts` | -3.1% | -63.6% | -1 |
| `temporal-memory-sealed-semantic-default-stale-history` | `claude-sonnet-high` | `raw_history` | -21.4% | -7.1% | -11 |
| `temporal-memory-sealed-semantic-default-stale-history` | `claude-sonnet-high` | `facts_only` | -19.3% | -50.3% | -12 |
| `temporal-memory-sealed-semantic-default-stale-history` | `claude-sonnet-high` | `history_facts` | -25.2% | -10.8% | -14 |
| `temporal-memory-sealed-semantic-default-stale-history` | `codex-gpt-5.3-codex-spark-low` | `raw_history` | -17.8% | +107.4% | -2 |
| `temporal-memory-sealed-semantic-default-stale-history` | `codex-gpt-5.3-codex-spark-low` | `facts_only` | -44.6% | -17.2% | -6 |
| `temporal-memory-sealed-semantic-default-stale-history` | `codex-gpt-5.3-codex-spark-low` | `history_facts` | -7.4% | +11.8% | +0 |
| `temporal-memory-sealed-windows-drive-neutral` | `claude-sonnet-high` | `raw_history` | +217.3% | +7.2% | +4 |
| `temporal-memory-sealed-windows-drive-neutral` | `claude-sonnet-high` | `facts_only` | +85.9% | +12.3% | +6 |
| `temporal-memory-sealed-windows-drive-neutral` | `claude-sonnet-high` | `history_facts` | +114.9% | -15.9% | +5 |
| `temporal-memory-sealed-windows-drive-neutral` | `codex-gpt-5.3-codex-spark-low` | `raw_history` | -0.9% | +5.7% | +1 |
| `temporal-memory-sealed-windows-drive-neutral` | `codex-gpt-5.3-codex-spark-low` | `facts_only` | -35.9% | -32.6% | -1 |
| `temporal-memory-sealed-windows-drive-neutral` | `codex-gpt-5.3-codex-spark-low` | `history_facts` | -13.8% | -20.4% | -1 |

## Interpretation

Fact-positive channel gate: 2/2 facts_only rows passed validation and protocol. Stale-memory gate: 6/6 stale/conflict memory rows restored shipped behavior. Neutral-correctness gate: 5/8 neutral rows passed validation and protocol.

Only 19/24 rows passed the frozen integrity checks; failed rows are retained below and preclude treatment-effect estimation.

No-Brain hidden validation passed in 5/6 rows. Positive-task headroom gate: no fact-positive no_brain row failed while a protocol-valid memory row passed. Memory cost is heterogeneous, ranging from useful search/token reductions on some cells to substantial overhead on others. With one repetition, this smoke cannot estimate a population treatment effect.

The scaled temporal-memory study is a no-go under the preregistered gates. The channel-materialization mechanism ran end to end, but a harder sealed task sample with no-Brain headroom, reported distillation token accounting, checksum-verified agent patch retention, and repeated task-clustered runs are required before a paper claim is promoted.

## Integrity Findings

- claude-sonnet-high/temporal-memory-sealed-expand-opt-in/history_facts: record is not marked ok
- claude-sonnet-high/temporal-memory-sealed-expand-opt-in/history_facts: protocol audit failed: [{'kind': 'forbidden_memory_artifact_access'}]
- codex-gpt-5.3-codex-spark-low/temporal-memory-sealed-expand-opt-in/raw_history: record is not marked ok
- codex-gpt-5.3-codex-spark-low/temporal-memory-sealed-expand-opt-in/raw_history: protocol audit failed: [{'kind': 'memory_search_call_count', 'actual': 0, 'expected': 1}, {'kind': 'memory_search_was_not_first_tool'}, {'kind': 'unexpected_brain_command', 'commands': []}]
- claude-sonnet-high/temporal-memory-sealed-windows-drive-neutral/facts_only: record is not marked ok
- claude-sonnet-high/temporal-memory-sealed-windows-drive-neutral/facts_only: protocol audit failed: [{'kind': 'memory_search_call_count', 'actual': 3, 'expected': 1}, {'kind': 'unexpected_brain_command', 'commands': ['path', 'search']}]
- codex-gpt-5.3-codex-spark-low/temporal-memory-sealed-windows-drive-neutral/no_brain: hidden validation failed
- codex-gpt-5.3-codex-spark-low/temporal-memory-sealed-windows-drive-neutral/no_brain: record is not marked ok
- codex-gpt-5.3-codex-spark-low/temporal-memory-sealed-windows-drive-neutral/raw_history: record is not marked ok
- codex-gpt-5.3-codex-spark-low/temporal-memory-sealed-windows-drive-neutral/raw_history: protocol audit failed: [{'kind': 'memory_search_call_count', 'actual': 0, 'expected': 1}, {'kind': 'memory_search_was_not_first_tool'}, {'kind': 'unexpected_brain_command', 'commands': []}]
