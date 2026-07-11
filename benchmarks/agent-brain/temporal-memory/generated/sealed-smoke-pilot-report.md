# Temporal Memory Phase 0A Sealed Smoke

- Decision: **NO-GO**
- Rows: **24**
- Validation passes: **24/24**
- Protocol passes: **24/24**
- Source cache: `1dd2312593bd40ce7e66f748`

## Gates

| Gate | Pass | Evidence |
|---|---:|---|
| `complete_matrix` | True | 24/24 rows |
| `runner_matrix` | True | actual=['claude-sonnet-high', 'codex-gpt-5.3-codex-spark-low']; expected=['claude-sonnet-high', 'codex-gpt-5.3-codex-spark-low'] |
| `row_integrity` | True | 24/24 rows |
| `fact_positive_channel` | True | 2/2 facts_only rows passed validation and protocol |
| `stale_memory_rejected` | True | 6/6 stale/conflict memory rows restored shipped behavior |
| `neutral_correctness` | True | 8/8 neutral rows passed validation and protocol |
| `positive_task_headroom` | False | no fact-positive no_brain row failed while a protocol-valid memory row passed |
| `task_agent_token_accounting` | True | 24/24 rows |
| `distillation_token_accounting` | False | distillation retained call count and duration but not reported token usage |
| `patch_artifact_retention` | False | 0/24 exact agent patches retained and checksum-verified |
| `clean_run_provenance` | False | 0/24 rows have clean harness and source worktrees |
| `portable_source_artifact` | True | private source archive checksum verified |

## Outcomes

| Task | Stratum | Runner | Condition | Validation | Protocol | Tokens | Seconds | Searches |
|---|---|---|---|---:|---:|---:|---:|---:|
| `temporal-memory-sealed-expand-opt-in` | `fact_positive` | `claude-sonnet-high` | `no_brain` | True | True | 705631 | 72.2 | 18 |
| `temporal-memory-sealed-expand-opt-in` | `fact_positive` | `claude-sonnet-high` | `raw_history` | True | True | 931579 | 203.2 | 15 |
| `temporal-memory-sealed-expand-opt-in` | `fact_positive` | `claude-sonnet-high` | `facts_only` | True | True | 416662 | 48.0 | 6 |
| `temporal-memory-sealed-expand-opt-in` | `fact_positive` | `claude-sonnet-high` | `history_facts` | True | True | 1467113 | 187.2 | 14 |
| `temporal-memory-sealed-expand-opt-in` | `fact_positive` | `codex-gpt-5.3-codex-spark-low` | `no_brain` | True | True | 188895 | 19.2 | 2 |
| `temporal-memory-sealed-expand-opt-in` | `fact_positive` | `codex-gpt-5.3-codex-spark-low` | `raw_history` | True | True | 420258 | 33.8 | 3 |
| `temporal-memory-sealed-expand-opt-in` | `fact_positive` | `codex-gpt-5.3-codex-spark-low` | `facts_only` | True | True | 287180 | 31.3 | 4 |
| `temporal-memory-sealed-expand-opt-in` | `fact_positive` | `codex-gpt-5.3-codex-spark-low` | `history_facts` | True | True | 213159 | 16.7 | 2 |
| `temporal-memory-sealed-semantic-default-stale-history` | `stale_conflict` | `claude-sonnet-high` | `no_brain` | True | True | 510741 | 191.4 | 12 |
| `temporal-memory-sealed-semantic-default-stale-history` | `stale_conflict` | `claude-sonnet-high` | `raw_history` | True | True | 462067 | 122.5 | 6 |
| `temporal-memory-sealed-semantic-default-stale-history` | `stale_conflict` | `claude-sonnet-high` | `facts_only` | True | True | 547532 | 114.2 | 5 |
| `temporal-memory-sealed-semantic-default-stale-history` | `stale_conflict` | `claude-sonnet-high` | `history_facts` | True | True | 698292 | 138.0 | 15 |
| `temporal-memory-sealed-semantic-default-stale-history` | `stale_conflict` | `codex-gpt-5.3-codex-spark-low` | `no_brain` | True | True | 865116 | 40.7 | 11 |
| `temporal-memory-sealed-semantic-default-stale-history` | `stale_conflict` | `codex-gpt-5.3-codex-spark-low` | `raw_history` | True | True | 481637 | 40.2 | 4 |
| `temporal-memory-sealed-semantic-default-stale-history` | `stale_conflict` | `codex-gpt-5.3-codex-spark-low` | `facts_only` | True | True | 623170 | 33.7 | 5 |
| `temporal-memory-sealed-semantic-default-stale-history` | `stale_conflict` | `codex-gpt-5.3-codex-spark-low` | `history_facts` | True | True | 499811 | 38.6 | 5 |
| `temporal-memory-sealed-windows-drive-neutral` | `neutral` | `claude-sonnet-high` | `no_brain` | True | True | 650448 | 175.6 | 16 |
| `temporal-memory-sealed-windows-drive-neutral` | `neutral` | `claude-sonnet-high` | `raw_history` | True | True | 643875 | 168.7 | 12 |
| `temporal-memory-sealed-windows-drive-neutral` | `neutral` | `claude-sonnet-high` | `facts_only` | True | True | 535746 | 170.3 | 7 |
| `temporal-memory-sealed-windows-drive-neutral` | `neutral` | `claude-sonnet-high` | `history_facts` | True | True | 501385 | 165.9 | 7 |
| `temporal-memory-sealed-windows-drive-neutral` | `neutral` | `codex-gpt-5.3-codex-spark-low` | `no_brain` | True | True | 302101 | 25.8 | 3 |
| `temporal-memory-sealed-windows-drive-neutral` | `neutral` | `codex-gpt-5.3-codex-spark-low` | `raw_history` | True | True | 405741 | 25.1 | 4 |
| `temporal-memory-sealed-windows-drive-neutral` | `neutral` | `codex-gpt-5.3-codex-spark-low` | `facts_only` | True | True | 502878 | 57.1 | 7 |
| `temporal-memory-sealed-windows-drive-neutral` | `neutral` | `codex-gpt-5.3-codex-spark-low` | `history_facts` | True | True | 529326 | 28.2 | 4 |

## Relative Cost

Positive percentages are overhead versus the same task/runner no-Brain row.

| Task | Runner | Condition | Token delta | Time delta | Search delta |
|---|---|---|---:|---:|---:|
| `temporal-memory-sealed-expand-opt-in` | `claude-sonnet-high` | `raw_history` | +32.0% | +181.3% | -3 |
| `temporal-memory-sealed-expand-opt-in` | `claude-sonnet-high` | `facts_only` | -41.0% | -33.5% | -12 |
| `temporal-memory-sealed-expand-opt-in` | `claude-sonnet-high` | `history_facts` | +107.9% | +159.2% | -4 |
| `temporal-memory-sealed-expand-opt-in` | `codex-gpt-5.3-codex-spark-low` | `raw_history` | +122.5% | +76.1% | +1 |
| `temporal-memory-sealed-expand-opt-in` | `codex-gpt-5.3-codex-spark-low` | `facts_only` | +52.0% | +63.0% | +2 |
| `temporal-memory-sealed-expand-opt-in` | `codex-gpt-5.3-codex-spark-low` | `history_facts` | +12.8% | -13.0% | +0 |
| `temporal-memory-sealed-semantic-default-stale-history` | `claude-sonnet-high` | `raw_history` | -9.5% | -36.0% | -6 |
| `temporal-memory-sealed-semantic-default-stale-history` | `claude-sonnet-high` | `facts_only` | +7.2% | -40.3% | -7 |
| `temporal-memory-sealed-semantic-default-stale-history` | `claude-sonnet-high` | `history_facts` | +36.7% | -27.9% | +3 |
| `temporal-memory-sealed-semantic-default-stale-history` | `codex-gpt-5.3-codex-spark-low` | `raw_history` | -44.3% | -1.3% | -7 |
| `temporal-memory-sealed-semantic-default-stale-history` | `codex-gpt-5.3-codex-spark-low` | `facts_only` | -28.0% | -17.1% | -6 |
| `temporal-memory-sealed-semantic-default-stale-history` | `codex-gpt-5.3-codex-spark-low` | `history_facts` | -42.2% | -5.2% | -6 |
| `temporal-memory-sealed-windows-drive-neutral` | `claude-sonnet-high` | `raw_history` | -1.0% | -3.9% | -4 |
| `temporal-memory-sealed-windows-drive-neutral` | `claude-sonnet-high` | `facts_only` | -17.6% | -3.0% | -9 |
| `temporal-memory-sealed-windows-drive-neutral` | `claude-sonnet-high` | `history_facts` | -22.9% | -5.5% | -9 |
| `temporal-memory-sealed-windows-drive-neutral` | `codex-gpt-5.3-codex-spark-low` | `raw_history` | +34.3% | -3.0% | +1 |
| `temporal-memory-sealed-windows-drive-neutral` | `codex-gpt-5.3-codex-spark-low` | `facts_only` | +66.5% | +121.1% | +4 |
| `temporal-memory-sealed-windows-drive-neutral` | `codex-gpt-5.3-codex-spark-low` | `history_facts` | +75.2% | +9.2% | +1 |

## Interpretation

Fact-positive channel gate: 2/2 facts_only rows passed validation and protocol. Stale-memory gate: 6/6 stale/conflict memory rows restored shipped behavior. Neutral-correctness gate: 8/8 neutral rows passed validation and protocol.

Every row passed the frozen integrity checks.

No-Brain hidden validation passed in 6/6 rows. Positive-task headroom gate: no fact-positive no_brain row failed while a protocol-valid memory row passed. Memory cost is heterogeneous, ranging from useful search/token reductions on some cells to substantial overhead on others. With one repetition, this smoke cannot estimate a population treatment effect.

The scaled temporal-memory study is a no-go under the preregistered gates. The channel-materialization mechanism ran end to end, but a harder sealed task sample with no-Brain headroom, clean committed-run provenance, reported distillation token accounting, checksum-verified agent patch retention, and repeated task-clustered runs are required before a paper claim is promoted.
