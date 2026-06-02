# Phase 2 Brain-Positive Scenario Discovery

Generated at: `2026-06-01T20:15:12.887105+00:00`

This report is the scenario-discovery ledger for Phase 2. It finds more than 20 candidate scenarios per layer where the brain should have a measurable advantage. Existing repeated-run proof signals are listed separately; candidates still require the proof repetitions before final claims.

Superseded layer map: this generated ledger predates the 2026-06-02 Phase 2
reset. It uses the obsolete map where Layer A included all project-native repos,
Layer B was SWE-bench-style, and Layer C was model/effort/cost. The current
Phase 2 map is:

- Layer A: project-native tasks in `entire-brain` and `entire-cli`.
- Layer B: project-native GitHub CLI tasks.
- Layer C: SWE-bench-style issue tasks.

Do not use this report to claim current Phase 2 layer coverage until it is
regenerated under the corrected map and Claude-default-only target.

## Counts

| Layer | Candidate scenarios | Existing repeated proofs |
|---|---:|---:|
| A | 21 | 5 |
| B | 21 | 0 |
| C | 21 | 0 |

Candidate goal met: `True`

Repeated proof goal met: `False`

## Existing Repeated-Run Signals

| Suite | Task | Runner | Condition | Signal |
|---|---|---|---|---|
| codex-github-cli-http-scopes-retained-r2-r5-20260601 | github-cli-http-scopes-suggestion | codex | semantic_brain | score delta 7.5, seconds p=0.04507 |
| codex-github-cli-repo-name-retained-r2-r5-20260601 | github-cli-repo-name-trims-dotgit | codex | semantic_brain | score delta 7.5, seconds p=0.001442, tokens p=0.0009579 |
| phase2-isolated-schema-claude-codex-r2b | entire-brain-history-codex-schema-contract | claude-sonnet-low | full_brain | score delta 60.0 |
| phase2-isolated-schema-claude-codex-r2b | entire-brain-history-codex-schema-contract | codex-medium | full_brain | score delta 65.0 |
| phase2-isolated-schema-claude-codex-r3 | entire-brain-history-codex-schema-contract | codex-medium | full_brain | score delta 65.0 |

## Layer A: Project-Native

| ID | Repo/Task | Brain | Archetype/Runner | Primary metric |
|---|---|---|---|---|
| `phase2-a-01-entire-brain-brain-command-surface-architecture-localization` | entire-brain / brain command surface | full_brain | architecture-localization | score and changed_file_count |
| `phase2-a-02-entire-brain-brain-command-surface-rationale-recovery` | entire-brain / brain command surface | full_brain | rationale-recovery | success_rate and score |
| `phase2-a-03-entire-brain-brain-command-surface-validation-selection` | entire-brain / brain command surface | full_brain | validation-selection | tokens, seconds, and validation_discipline |
| `phase2-a-04-entire-brain-brain-command-surface-stale-live-hygiene` | entire-brain / brain command surface | full_brain | stale-live-hygiene | success_rate and file_read_count |
| `phase2-a-05-entire-brain-semantic-freshness-architecture-localization` | entire-brain / semantic freshness | semantic_brain | architecture-localization | score and changed_file_count |
| `phase2-a-06-entire-brain-semantic-freshness-rationale-recovery` | entire-brain / semantic freshness | semantic_brain | rationale-recovery | success_rate and score |
| `phase2-a-07-entire-brain-semantic-freshness-validation-selection` | entire-brain / semantic freshness | semantic_brain | validation-selection | tokens, seconds, and validation_discipline |
| `phase2-a-08-entire-brain-semantic-freshness-stale-live-hygiene` | entire-brain / semantic freshness | semantic_brain | stale-live-hygiene | success_rate and file_read_count |
| `phase2-a-09-entire-brain-seed-agent-contract-architecture-localization` | entire-brain / seed agent contract | full_brain | architecture-localization | score and changed_file_count |
| `phase2-a-10-entire-brain-seed-agent-contract-rationale-recovery` | entire-brain / seed agent contract | full_brain | rationale-recovery | success_rate and score |
| `phase2-a-11-entire-brain-seed-agent-contract-validation-selection` | entire-brain / seed agent contract | full_brain | validation-selection | tokens, seconds, and validation_discipline |
| `phase2-a-12-entire-brain-seed-agent-contract-stale-live-hygiene` | entire-brain / seed agent contract | full_brain | stale-live-hygiene | success_rate and file_read_count |
| `phase2-a-13-entire-brain-bundle-integrity-architecture-localization` | entire-brain / bundle integrity | full_brain | architecture-localization | score and changed_file_count |
| `phase2-a-14-entire-brain-bundle-integrity-rationale-recovery` | entire-brain / bundle integrity | full_brain | rationale-recovery | success_rate and score |
| `phase2-a-15-entire-brain-bundle-integrity-validation-selection` | entire-brain / bundle integrity | full_brain | validation-selection | tokens, seconds, and validation_discipline |
| `phase2-a-16-entire-brain-bundle-integrity-stale-live-hygiene` | entire-brain / bundle integrity | full_brain | stale-live-hygiene | success_rate and file_read_count |
| `phase2-a-17-entire-cli-review-provenance-env-filtering-architecture-localization` | entire-cli / review provenance env filtering | full_brain | architecture-localization | score and changed_file_count |
| `phase2-a-18-entire-cli-review-provenance-env-filtering-rationale-recovery` | entire-cli / review provenance env filtering | full_brain | rationale-recovery | success_rate and score |
| `phase2-a-19-entire-cli-review-provenance-env-filtering-validation-selection` | entire-cli / review provenance env filtering | full_brain | validation-selection | tokens, seconds, and validation_discipline |
| `phase2-a-20-entire-cli-review-provenance-env-filtering-stale-live-hygiene` | entire-cli / review provenance env filtering | full_brain | stale-live-hygiene | success_rate and file_read_count |
| `phase2-a-21-entire-cli-manual-commit-hooks-architecture-localization` | entire-cli / manual commit hooks | full_brain | architecture-localization | score and changed_file_count |

## Layer B: SWE-Bench-Style

| ID | Repo/Task | Brain | Archetype/Runner | Primary metric |
|---|---|---|---|---|
| `phase2-b-01-entire-brain-brain-command-surface-hidden-cross-file-contract` | entire-brain / brain command surface | full_brain | hidden-cross-file-contract | success_rate and score |
| `phase2-b-02-entire-brain-brain-command-surface-large-repo-near-miss` | entire-brain / brain command surface | full_brain | large-repo-near-miss | tokens and changed_file_count |
| `phase2-b-03-entire-brain-brain-command-surface-history-only-regression` | entire-brain / brain command surface | full_brain | history-only-regression | success_rate |
| `phase2-b-04-entire-brain-brain-command-surface-stale-context-issue` | entire-brain / brain command surface | full_brain | stale-context-issue | success_rate and file_read_count |
| `phase2-b-05-entire-brain-semantic-freshness-hidden-cross-file-contract` | entire-brain / semantic freshness | semantic_brain | hidden-cross-file-contract | success_rate and score |
| `phase2-b-06-entire-brain-semantic-freshness-large-repo-near-miss` | entire-brain / semantic freshness | semantic_brain | large-repo-near-miss | tokens and changed_file_count |
| `phase2-b-07-entire-brain-semantic-freshness-history-only-regression` | entire-brain / semantic freshness | semantic_brain | history-only-regression | success_rate |
| `phase2-b-08-entire-brain-semantic-freshness-stale-context-issue` | entire-brain / semantic freshness | semantic_brain | stale-context-issue | success_rate and file_read_count |
| `phase2-b-09-entire-brain-seed-agent-contract-hidden-cross-file-contract` | entire-brain / seed agent contract | full_brain | hidden-cross-file-contract | success_rate and score |
| `phase2-b-10-entire-brain-seed-agent-contract-large-repo-near-miss` | entire-brain / seed agent contract | full_brain | large-repo-near-miss | tokens and changed_file_count |
| `phase2-b-11-entire-brain-seed-agent-contract-history-only-regression` | entire-brain / seed agent contract | full_brain | history-only-regression | success_rate |
| `phase2-b-12-entire-brain-seed-agent-contract-stale-context-issue` | entire-brain / seed agent contract | full_brain | stale-context-issue | success_rate and file_read_count |
| `phase2-b-13-entire-brain-bundle-integrity-hidden-cross-file-contract` | entire-brain / bundle integrity | full_brain | hidden-cross-file-contract | success_rate and score |
| `phase2-b-14-entire-brain-bundle-integrity-large-repo-near-miss` | entire-brain / bundle integrity | full_brain | large-repo-near-miss | tokens and changed_file_count |
| `phase2-b-15-entire-brain-bundle-integrity-history-only-regression` | entire-brain / bundle integrity | full_brain | history-only-regression | success_rate |
| `phase2-b-16-entire-brain-bundle-integrity-stale-context-issue` | entire-brain / bundle integrity | full_brain | stale-context-issue | success_rate and file_read_count |
| `phase2-b-17-entire-cli-review-provenance-env-filtering-hidden-cross-file-contract` | entire-cli / review provenance env filtering | full_brain | hidden-cross-file-contract | success_rate and score |
| `phase2-b-18-entire-cli-review-provenance-env-filtering-large-repo-near-miss` | entire-cli / review provenance env filtering | full_brain | large-repo-near-miss | tokens and changed_file_count |
| `phase2-b-19-entire-cli-review-provenance-env-filtering-history-only-regression` | entire-cli / review provenance env filtering | full_brain | history-only-regression | success_rate |
| `phase2-b-20-entire-cli-review-provenance-env-filtering-stale-context-issue` | entire-cli / review provenance env filtering | full_brain | stale-context-issue | success_rate and file_read_count |
| `phase2-b-21-entire-cli-manual-commit-hooks-hidden-cross-file-contract` | entire-cli / manual commit hooks | full_brain | hidden-cross-file-contract | success_rate and score |

## Layer C: Model/Effort/Cost

| ID | Repo/Task | Brain | Archetype/Runner | Primary metric |
|---|---|---|---|---|
| `phase2-c-01-entire-brain-history-codex-schema-contract-codex-gpt-5-4-mini-medium` | entire-brain-history-codex-schema-contract | full_brain | codex-gpt-5.4-mini-medium | success_rate with tokens, seconds, turns, and cost |
| `phase2-c-02-entire-brain-history-codex-schema-contract-codex-gpt-5-3-codex-medium` | entire-brain-history-codex-schema-contract | full_brain | codex-gpt-5.3-codex-medium | success_rate with tokens, seconds, turns, and cost |
| `phase2-c-03-entire-brain-history-codex-schema-contract-codex-gpt-5-2-low` | entire-brain-history-codex-schema-contract | full_brain | codex-gpt-5.2-low | success_rate with tokens, seconds, turns, and cost |
| `phase2-c-04-entire-brain-history-codex-schema-contract-claude-sonnet-4-6-low` | entire-brain-history-codex-schema-contract | full_brain | claude-sonnet-4-6-low | success_rate with tokens, seconds, turns, and cost |
| `phase2-c-05-entire-brain-history-codex-schema-contract-claude-haiku-4-5-low` | entire-brain-history-codex-schema-contract | full_brain | claude-haiku-4-5-low | success_rate with tokens, seconds, turns, and cost |
| `phase2-c-06-entire-brain-history-bundle-sha256-codex-gpt-5-4-mini-medium` | entire-brain-history-bundle-sha256 | full_brain | codex-gpt-5.4-mini-medium | success_rate with tokens, seconds, turns, and cost |
| `phase2-c-07-entire-brain-history-bundle-sha256-codex-gpt-5-3-codex-medium` | entire-brain-history-bundle-sha256 | full_brain | codex-gpt-5.3-codex-medium | success_rate with tokens, seconds, turns, and cost |
| `phase2-c-08-entire-brain-history-bundle-sha256-codex-gpt-5-2-low` | entire-brain-history-bundle-sha256 | full_brain | codex-gpt-5.2-low | success_rate with tokens, seconds, turns, and cost |
| `phase2-c-09-entire-brain-history-bundle-sha256-claude-sonnet-4-6-low` | entire-brain-history-bundle-sha256 | full_brain | claude-sonnet-4-6-low | success_rate with tokens, seconds, turns, and cost |
| `phase2-c-10-entire-brain-history-bundle-sha256-claude-haiku-4-5-low` | entire-brain-history-bundle-sha256 | full_brain | claude-haiku-4-5-low | success_rate with tokens, seconds, turns, and cost |
| `phase2-c-11-entire-brain-history-github-visibility-codex-gpt-5-4-mini-medium` | entire-brain-history-github-visibility | full_brain | codex-gpt-5.4-mini-medium | success_rate with tokens, seconds, turns, and cost |
| `phase2-c-12-entire-brain-history-github-visibility-codex-gpt-5-3-codex-medium` | entire-brain-history-github-visibility | full_brain | codex-gpt-5.3-codex-medium | success_rate with tokens, seconds, turns, and cost |
| `phase2-c-13-entire-brain-history-github-visibility-codex-gpt-5-2-low` | entire-brain-history-github-visibility | full_brain | codex-gpt-5.2-low | success_rate with tokens, seconds, turns, and cost |
| `phase2-c-14-entire-brain-history-github-visibility-claude-sonnet-4-6-low` | entire-brain-history-github-visibility | full_brain | claude-sonnet-4-6-low | success_rate with tokens, seconds, turns, and cost |
| `phase2-c-15-entire-brain-history-github-visibility-claude-haiku-4-5-low` | entire-brain-history-github-visibility | full_brain | claude-haiku-4-5-low | success_rate with tokens, seconds, turns, and cost |
| `phase2-c-16-entire-cli-review-base-flag-scope-codex-gpt-5-4-mini-medium` | entire-cli-review-base-flag-scope | full_brain | codex-gpt-5.4-mini-medium | success_rate with tokens, seconds, turns, and cost |
| `phase2-c-17-entire-cli-review-base-flag-scope-codex-gpt-5-3-codex-medium` | entire-cli-review-base-flag-scope | full_brain | codex-gpt-5.3-codex-medium | success_rate with tokens, seconds, turns, and cost |
| `phase2-c-18-entire-cli-review-base-flag-scope-codex-gpt-5-2-low` | entire-cli-review-base-flag-scope | full_brain | codex-gpt-5.2-low | success_rate with tokens, seconds, turns, and cost |
| `phase2-c-19-entire-cli-review-base-flag-scope-claude-sonnet-4-6-low` | entire-cli-review-base-flag-scope | full_brain | claude-sonnet-4-6-low | success_rate with tokens, seconds, turns, and cost |
| `phase2-c-20-entire-cli-review-base-flag-scope-claude-haiku-4-5-low` | entire-cli-review-base-flag-scope | full_brain | claude-haiku-4-5-low | success_rate with tokens, seconds, turns, and cost |
| `phase2-c-21-entire-cli-review-prompt-uncommitted-scope-codex-gpt-5-4-mini-medium` | entire-cli-review-prompt-uncommitted-scope | full_brain | codex-gpt-5.4-mini-medium | success_rate with tokens, seconds, turns, and cost |
