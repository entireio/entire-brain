# Phase 2 Brain Benchmark Campaign Ledger

Generated: 2026-06-02

This ledger records the expensive repetition campaign evidence collected so far on branch `phase-2-brain-benchmark-plan`. Raw run directories remain under ignored `benchmarks/agent-brain/results/`; this file is the committed evidence summary.

## Retention Rule

A task-runner-condition comparison is retained when `agent + brain` is materially better than `agent + no brain` under repeated runs:

- Score win: positive score delta with `p_value_approx < 0.05`, or a positive success-rate lift.
- Operational win: score and success are not worse, and at least one of seconds, tokens, cost, or turns improves with `p < 0.05`.
- Reject: score regresses, success regresses, or the improvement is saturated/noisy after repetitions.

## Current Proof Counts

Counts below are computed from the combined report over:

- `phase2-proof-batch-codex-medium-r3-20260601`
- `phase2-layer-c-schema-lower-cost-r3-20260601`
- `phase2-layer-c-schema-codex-lower-cost-r4-r8-20260601`
- `phase2-layer-b-swe-schema-matrix-r3-20260601`
- `phase2-layer-b-swe-history-matrix-r3-20260601`
- `phase2-layer-a-history-matrix-r3-20260601`
- `phase2-layer-a-entire-cli-history-matrix-r3-20260602`
- `phase2-layer-b-swe-claude-history-matrix-r3-20260602`
- `phase2-layer-b-history-mini-haiku-fill-r3-20260602`
- `phase2-repo-name-semantic-fill-r3-20260602` (partial, used as a reject/saturation control)
- `phase2-manual-commit-attribution-fill-r3-20260602` (partial, used as a reject/surface-gap control)
- `phase2-codex-default-high-history-fill-r3-20260602`

Important accounting correction: the 20-per-layer acceptance criterion is a
scenario-level target, independent of model/runner. The earlier version of this
ledger incorrectly treated task-runner-condition comparisons as scenarios and
also used the wrong Layer C definition.

Correct Phase 2 layers:

- A: project-native tasks in repos with Entire session data (`entire-brain` and
  `entire-cli`).
- B: project-native GitHub CLI tasks. Use the full-brain condition as the
  complete available brain package with `history_available=false`; do not claim
  full-history value for this layer.
- C: SWE-bench-style issue tasks.

The previous model/effort/cost layer is cut from Phase 2. Model/cost evidence is
a later overlay on retained scenarios, not a source of scenario count. Phase 2
now focuses only on Claude Code with its default model.

| Legacy bucket | Retained comparisons | Rejected/weak comparisons | Target scenarios | Status |
|---|---:|---:|---:|---|
| Old A project-native | 20 | 18 | 20 | comparison count only |
| Old B SWE-style | 20 | 7 | 20 | comparison count only |
| Old C model/effort/cost | 23 | 22 | 20 | removed from Phase 2 |

Scenario-level counts under the corrected A/B/C split, deduped by task prompt
independent of model/runner:

| Layer | Tested scenarios | Retained scenarios, all runners | Retained scenarios, Claude default | Target | Status |
|---|---:|---:|---:|---:|---|
| A Entire-data project-native | 7 | 5 | 3 | 20 | short by 17 for active target |
| B GitHub CLI project-native | 2 | 1 | 0 | 20 | short by 20 for active target |
| C SWE-style | 6 | 5 | 4 | 20 | short by 16 for active target |

The 20-per-layer scenario target is not met. The campaign found repeated
evidence for a smaller set of brain-positive scenarios, then overcounted that
evidence by runner/model. The all-runner counts are diagnostic only; the active
Phase 2 target is Claude default.

## Metrics Appendices

Two aggregate metric tables are committed:

- [`comparison-metrics.csv`](comparison-metrics.csv) is the wide A/B comparison
  table. Each row compares one brain condition against its no-brain baseline,
  so no-brain values appear in `*_no_brain` columns and the compared brain
  condition appears in `*_brain` columns.
- [`condition-metrics.csv`](condition-metrics.csv) is the long condition table.
  It has explicit `no_brain`, `semantic_brain`, and `full_brain` rows with
  aggregate metrics per suite/task/runner/condition.

The wide comparison table includes all 65 task/runner/condition comparisons
from the combined Phase 2 report, not only the retained rows. Columns include
score, success rate, agent seconds, total tokens, turns, cost, deltas, p-values,
sample counts, and retention status. Coverage:

- Score and success: 65 of 65 comparisons.
- Agent seconds: 64 of 65 comparisons.
- Total tokens: 64 of 65 comparisons.
- Turns: 23 of 65 comparisons.
- Cost: 23 of 65 comparisons.

The long condition table includes 136 aggregate condition rows:

- `no_brain`: 68 rows.
- `semantic_brain`: 6 rows.
- `full_brain`: 62 rows.

The missing token/seconds row is a rejected Haiku regression where the agent run
did not expose complete usage. Cost is blank for Codex rows where the CLI did
not report cost and no pricing file was supplied for estimation; Claude rows
with reported cost are preserved.

## Representative Retained Comparisons

| Layer | Task | Runner | Brain | Delta | Success | Signal |
|---|---|---|---|---:|---|---|
| A | `entire-brain-history-codex-schema-contract` | `codex-medium` | full | +37.00 | 0.00 -> 1.00 | score |
| A | `github-cli-repo-name-trims-dotgit` | `codex-medium` | semantic | +9.00 | 1.00 -> 1.00 | score/tokens/seconds |
| A | `entire-brain-history-bundle-sha256` | `codex-medium` | full | +9.67 | 1.00 -> 1.00 | score/seconds |
| A | `entire-brain-history-claude-bare-auth` | `codex-medium` | full | +4.67 | 1.00 -> 1.00 | score |
| A | `entire-brain-history-claude-seed-agent` | `codex-medium` | full | +7.33 | 1.00 -> 1.00 | score/seconds |
| A | `entire-brain-history-github-visibility` | `codex-medium` | full | +4.00 | 1.00 -> 1.00 | score |
| B | `swe-style-entire-brain-history-codex-schema-contract` | `codex-medium` | full | +26.00 | 0.33 -> 1.00 | score |
| B | `swe-style-entire-brain-history-codex-schema-contract` | `codex-gpt-5.3-codex-medium` | full | +27.00 | 0.33 -> 1.00 | score |
| B | `swe-style-entire-brain-history-codex-schema-contract` | `claude-sonnet-4-6-low` | full | +3.33 | 0.67 -> 1.00 | operational |
| B | `swe-style-entire-brain-history-bundle-sha256` | `codex-medium` | full | +12.00 | 1.00 -> 1.00 | score/seconds |
| B | `swe-style-entire-brain-history-bundle-sha256` | `codex-gpt-5.3-codex-medium` | full | +6.00 | 1.00 -> 1.00 | score |
| B | `swe-style-entire-brain-history-github-visibility` | `codex-medium` | full | +6.33 | 1.00 -> 1.00 | score |
| B | `swe-style-entire-brain-history-github-visibility` | `codex-gpt-5.3-codex-medium` | full | +2.67 | 1.00 -> 1.00 | score |
| B | `swe-style-entire-brain-history-github-visibility` | `claude-sonnet-4-6-low` | full | +3.00 | 1.00 -> 1.00 | operational |
| C | `entire-brain-history-codex-schema-contract` | `codex-gpt-5.4-mini-medium` | full | +15.75 | 0.62 -> 1.00 | score |
| C | `entire-brain-history-codex-schema-contract` | `codex-gpt-5.3-codex-medium` | full | +17.75 | 0.62 -> 1.00 | score |
| C | `entire-brain-history-codex-schema-contract` | `claude-sonnet-4-6-low` | full | +8.00 | 0.67 -> 1.00 | operational |
| C | `entire-brain-history-codex-schema-contract` | `claude-haiku-4-5-low` | full | +3.00 | 0.67 -> 1.00 | success lift |
| C | `entire-brain-history-bundle-sha256` | `codex-gpt-5.4-mini-medium` | full | +4.33 | 1.00 -> 1.00 | score |
| C | `entire-brain-history-claude-bare-auth` | `codex-gpt-5.4-mini-medium` | full | +17.00 | 0.33 -> 1.00 | score/tokens |
| C | `entire-brain-history-claude-bare-auth` | `claude-sonnet-4-6-low` | full | +21.33 | 0.33 -> 1.00 | score/cost/tokens/turns |
| C | `entire-brain-history-claude-bare-auth` | `claude-haiku-4-5-low` | full | +29.67 | 0.00 -> 1.00 | score |
| C | `entire-brain-history-claude-seed-agent` | `codex-gpt-5.4-mini-medium` | full | +4.67 | 0.67 -> 0.67 | tokens |
| C | `entire-brain-history-claude-seed-agent` | `claude-sonnet-4-6-low` | full | +13.00 | 0.67 -> 1.00 | score |
| C | `entire-brain-history-github-visibility` | `codex-gpt-5.4-mini-medium` | full | +7.33 | 1.00 -> 1.00 | score/tokens/seconds |

## Count-Closing Comparisons

The final fill suite used the supported default Codex model at high reasoning effort (`codex-default-high=codex::high`). Explicit `gpt-5.1-codex` runner names were rejected by the account API and are not counted.

| Layer | Task | Runner | Score | Success | Seconds | Tokens | Signal |
|---|---|---|---|---|---|---|---|
| A | `entire-brain-history-bundle-sha256` | `codex-default-high` | 84.00 -> 95.33 | 1.00 -> 1.00 | 148.96 -> 92.73 | 590,370 -> 572,174 | score |
| A | `entire-brain-history-claude-bare-auth` | `codex-default-high` | 77.67 -> 95.67 | 0.67 -> 1.00 | 130.20 -> 112.78 | 604,923 -> 508,132 | success lift |
| A | `entire-brain-history-codex-schema-contract` | `codex-default-high` | 78.67 -> 94.00 | 0.67 -> 1.00 | 123.48 -> 140.11 | 637,861 -> 692,240 | success lift |
| B | `swe-style-entire-brain-history-bundle-sha256` | `codex-default-high` | 84.67 -> 96.33 | 1.00 -> 1.00 | 122.21 -> 95.49 | 470,683 -> 521,079 | score |
| B | `swe-style-entire-brain-history-claude-bare-auth` | `codex-default-high` | 55.33 -> 95.00 | 0.00 -> 1.00 | 96.97 -> 115.91 | 476,470 -> 551,836 | score |
| B | `swe-style-entire-brain-history-codex-schema-contract` | `codex-default-high` | 89.33 -> 94.33 | 1.00 -> 1.00 | 133.13 -> 130.83 | 622,506 -> 658,432 | score |

## Clear Rejects And Saturation

These should not be used to pad the proof count:

- `github-cli-http-scopes-suggestion` with `codex-medium + semantic_brain`: saturated or slightly worse in the newer run.
- `swe-style-entire-brain-history-codex-schema-contract` with `codex-gpt-5.4-mini-medium`: mixed; no success-rate lift and noisy score.
- `swe-style-entire-brain-history-bundle-sha256` with `claude-sonnet-4-6-low`: operationally cheaper, but score regressed.
- `entire-brain-history-claude-bare-auth` with `codex-gpt-5.3-codex-medium`: full brain regressed and success fell to 0.
- `entire-brain-history-claude-seed-agent` with `codex-gpt-5.3-codex-medium`: both sides failed.
- `entire-brain-history-claude-seed-agent` with `claude-haiku-4-5-low`: full brain regressed.
- `entire-brain-history-github-visibility` with `codex-gpt-5.3-codex-medium`: both sides failed.
- `entire-brain-history-github-visibility` with `claude-sonnet-4-6-low`: score regressed despite fewer turns.
- `entire-brain-history-github-visibility` with `claude-haiku-4-5-low`: full brain regressed badly.
- `github-cli-repo-name-trims-dotgit` with lower-cost semantic brain: direct project-native task saturated or regressed; high-capability agents solved it from local code.
- `swe-style-github-cli-repo-name-trims-dotgit` with `codex-gpt-5.4-mini-medium`: mixed lift (+5 mean score) but not statistically retained.
- `entire-cli-manual-commit-attribution-base` with `codex-medium + full_brain`: full session history contained the production diagnosis, but the current brain surface did not expose it reliably; full brain regressed from 18 to 14.
- `codex-gpt-5.2-low` and default Codex low-effort fills: both no-brain and full-brain missed known-good history scenarios; these runs mark a capability floor rather than retained wins.

## Session-Log Inspect Validation

The `entire-cli` brain has full session data and a fresh semantic snapshot:

- Brain path: `/Users/thomi/.local/share/entire/brain/gh/entireio/cli`
- Sessions scanned in manifest: 3,753 checkpoints.
- Semantic snapshot: 9,130 symbols, 179,717 relations, 760 files.

Initial validation, before the structured parser fix, used:

```sh
/tmp/entire-brain-phase2 history-index /Users/thomi/Projects/cli
```

That history source reported:

- 2,000 indexed records.
- 101 decisions.
- 9 learnings.
- 151 validations.
- 1,739 tool calls.
- Warning: `history index truncated at record limit`.

Command probes:

- `inspect tool-paths "apply_patch" --json` returned 22 matches and correctly localized raw patch calls.
- `inspect history "checkpoints_v2_only" --json` returned 25 matches including useful rationale text about project-scoped settings.
- `inspect validation "go test" --json` returned 25 matches.
- `inspect decisions "provenance" --json` returned 0 matches.
- `inspect architecture "manual commit" --json` returned 0 matches.
- `inspect history "transcript re-resolution" --json` returned 0 matches.
- Raw session search over the exported sessions did find manual-commit and transcript-related material, so the miss was in indexing/ranking, not source availability.

Follow-up fix on 2026-06-02:

- Parsed JSONL sessions by record type/role instead of treating every line as
  raw text.
- Excluded user/system prompt content from decision, architecture, learning,
  validation, and tool-path records.
- Indexed tool invocations separately from tool output, with tool-name-aware
  validation so `apply_patch` text containing `go test` is not treated as a
  validation run.
- Normalized phrase matching so `manual_commit` can match `manual commit`
  without arbitrary token co-occurrence matches.
- Processed session files newest-first with per-kind caps and an explicit scan
  budget warning for very large exports.

Validated live brains after the fix:

- `entire-brain`: 1,304 records, 157 decisions, 9 learnings, 88 validations,
  1,002 tool calls; history-index took 0.96s.
- `entire-cli`: 44,951 records, 10,289 decisions, 627 learnings, 10,068
  validations, 20,000 tool calls; history-index took 28.95s and warned that it
  scanned the newest 311 session files / 535,058,459 bytes before skipping older
  sessions at the scan budget.
- `entire-cli inspect decisions "source_signal"` returned 9 matches with a
  first result about migrated/manual rows lacking usable `source_signal`.
- `entire-cli inspect validation "go test"` returned 25 matches with a first
  result from a Bash `go test` command.
- `entire-cli inspect architecture "checkpoint"` returned 25 matches with RFD
  decision/architecture rationale.
- `entire-brain inspect validation "go test"` now returns actual `exec_command`
  validation instead of README patch text.

Learning: the history index is no longer just raw phrase recall; it now exposes
useful session-derived decisions, architecture rationale, validation commands,
and tool paths for both repos with Entire data. The next optimization should be
a parsed session context graph:

- Parse JSONL by role/type instead of indexing raw lines.
- Normalize identifiers across `manual_commit`, `manual commit`, and file/symbol names.
- Rank by recency, branch relevance, touched files, and outcome.
- Link decisions to files, tool calls, validations, failures, and final commits.
- Expose those links through `entire brain brief` and `entire brain inspect history`, not as more top-level commands.

## Phase 2 Outcome

The Phase 2 benchmark target is not complete under the corrected scenario-level
requirement:

- Layer A: 3 retained Claude-default scenarios, target 20.
- Layer B: 0 retained Claude-default scenarios, target 20.
- Layer C: 4 retained Claude-default scenarios, target 20.

The strongest retained scenarios are history/rationale tasks where the current checkout alone omits a prior implementation decision, validation trace, or contract detail. The weakest scenarios are simple semantic navigation tasks and full-session tasks whose relevant decision is present only as unstructured transcript text.
