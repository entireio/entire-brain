# Phase 2 Brain Benchmark Campaign Ledger

Generated: 2026-06-02

This ledger records the expensive repetition campaign evidence collected so far on branch `phase-2-brain-benchmark-plan`. Raw run directories remain under ignored `benchmarks/agent-brain/results/`; this file is the committed evidence summary.

## Retention Rule

A task-runner-condition comparison is retained when `agent + brain` is materially better than `agent + no brain` under repeated runs:

- Score win: positive score delta with `p_value_approx < 0.05`, or a positive success-rate lift.
- Operational win: score and success are not worse, and at least one of seconds, tokens, cost, or turns improves with `p < 0.05`.
- Reject: score regresses, success regresses, or the improvement is saturated/noisy after repetitions.

## Current Proof Counts

Counts below are deduped from the combined report over:

- `phase2-proof-batch-codex-medium-r3-20260601`
- `phase2-layer-c-schema-lower-cost-r3-20260601`
- `phase2-layer-c-schema-codex-lower-cost-r4-r8-20260601`
- `phase2-layer-b-swe-schema-matrix-r3-20260601`
- `phase2-layer-b-swe-history-matrix-r3-20260601`
- `phase2-layer-a-history-matrix-r3-20260601`

| Layer | Retained | Rejected | Target | Status |
|---|---:|---:|---:|---|
| A project-native | 6 | 1 | 20 | short by 14 |
| B SWE-style | 8 | 2 | 20 | short by 12 |
| C model/effort/cost | 11 | 9 | 20 | short by 9 |

The 20-per-layer proof target is not met yet.

## Retained Comparisons

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

## Session-Log Inspect Validation

The `entire-cli` brain has full session data and a fresh semantic snapshot:

- Brain path: `/Users/thomi/.local/share/entire/brain/gh/entireio/cli`
- Sessions scanned in manifest: 3,753 checkpoints.
- Semantic snapshot: 9,130 symbols, 179,717 relations, 760 files.

After running:

```sh
/tmp/entire-brain-phase2 history-index /Users/thomi/Projects/cli
```

the history source reported:

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
- Raw session search over the exported sessions did find manual-commit and transcript-related material, so the miss is in indexing/ranking, not source availability.

Learning: the current history index is useful for known phrase recall and tool/validation traces, but it is not yet a reliable autonomous architecture/rationale surface. For repos with entire session data, the next optimization should be a parsed session context graph:

- Parse JSONL by role/type instead of indexing raw lines.
- Normalize identifiers across `manual_commit`, `manual commit`, and file/symbol names.
- Rank by recency, branch relevance, touched files, and outcome.
- Link decisions to files, tool calls, validations, failures, and final commits.
- Expose those links through `entire brain brief` and `entire brain inspect history`, not as more top-level commands.

## Next Campaign Batches

Continue until each layer has at least 20 retained comparisons:

- Layer A: run more project-native semantic and entire-cli history scenarios, especially `entire-cli-review-*`, `entire-cli-transcript-*`, and stale-live hygiene.
- Layer B: add SWE-style wrappers for the successful Layer A history tasks and entire-cli session-derived tasks.
- Layer C: extend lower-cost model runs on retained A/B scenarios, favoring `gpt-5.4-mini`, `claude-sonnet-4-6-low`, and `claude-haiku-4-5-low` where the no-brain baseline is weak.
