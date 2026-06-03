# Phase 2 Brain-Positive Scenario Discovery

Generated at: `2026-06-02T21:15:22.787928+00:00`

This report is the scenario-discovery ledger for Phase 2. It finds more than 20 candidate scenarios per layer where the brain should have a measurable advantage. Existing repeated-run proof signals are listed separately; candidates still require the proof repetitions before final claims.

## Counts

| Layer | Candidate scenarios | Existing repeated proofs |
|---|---:|---:|
| A | 21 | 41 |
| B | 21 | 5 |
| C | 21 | 37 |

Candidate goal met: `True`

Repeated proof goal met: `False`

## Retention Gate

Pilot each candidate with one no-brain run before spending proof repetitions. Reject scenarios whose no-brain pilot score is above 90 unless the brain condition is cheaper or faster at equivalent correctness. Strong brain-positive scenarios should withhold exact file names, test names, and historical rationale from the no-brain prompt; those facts should come from semantic context or checkpoint/session history.

## Existing Repeated-Run Signals

| Suite | Task | Runner | Condition | Signal |
|---|---|---|---|---|
| codex-github-cli-http-scopes-retained-r2-r5-20260601 | github-cli-http-scopes-suggestion | codex | semantic_brain | score delta 7.5, seconds p=0.04507 |
| codex-github-cli-repo-name-retained-r2-r5-20260601 | github-cli-repo-name-trims-dotgit | codex | semantic_brain | score delta 7.5, seconds p=0.001442, tokens p=0.0009579 |
| combined-report | entire-brain-history-codex-schema-contract | codex-medium | full_brain | score delta 37.0 |
| combined-report | github-cli-repo-name-trims-dotgit | codex-medium | semantic_brain | score delta 9.0, seconds p=0.001458, tokens p=2.011e-05 |
| combined-report | entire-brain-history-codex-schema-contract | codex-gpt-5.4-mini-medium | full_brain | score delta 15.8, tokens p=0.01816 |
| combined-report | entire-brain-history-codex-schema-contract | codex-gpt-5.3-codex-medium | full_brain | score delta 17.8 |
| combined-report | entire-brain-history-codex-schema-contract | claude-sonnet-4-6-low | full_brain | score delta 8.0, seconds p=2.042e-17, tokens p=1.615e-05, cost p=1.74e-12 |
| combined-report | swe-style-entire-brain-history-codex-schema-contract | codex-medium | full_brain | score delta 26.0 |
| combined-report | swe-style-entire-brain-history-codex-schema-contract | codex-gpt-5.3-codex-medium | full_brain | score delta 27.0 |
| combined-report | swe-style-entire-brain-history-codex-schema-contract | claude-sonnet-4-6-low | full_brain | score delta 3.3, seconds p=0.0002802, tokens p=0.009277, cost p=1.082e-13 |
| combined-report | swe-style-entire-brain-history-bundle-sha256 | codex-medium | full_brain | score delta 12.0, seconds p=0.04567 |
| combined-report | swe-style-entire-brain-history-bundle-sha256 | codex-gpt-5.3-codex-medium | full_brain | score delta 6.0 |
| combined-report | swe-style-entire-brain-history-bundle-sha256 | claude-sonnet-4-6-low | full_brain | score delta -0.7, seconds p=4.455e-47, tokens p=1.34e-11, turns p=0.04937, cost p=0.003027 |
| combined-report | swe-style-entire-brain-history-github-visibility | codex-medium | full_brain | score delta 6.3 |
| combined-report | swe-style-entire-brain-history-github-visibility | codex-gpt-5.3-codex-medium | full_brain | score delta 2.7 |
| combined-report | swe-style-entire-brain-history-github-visibility | claude-sonnet-4-6-low | full_brain | score delta 3.0, tokens p=0.0005763, turns p=0.002282, cost p=9.947e-08 |
| combined-report | entire-brain-history-bundle-sha256 | codex-medium | full_brain | score delta 9.7, seconds p=0.02531 |
| combined-report | entire-brain-history-bundle-sha256 | codex-gpt-5.4-mini-medium | full_brain | score delta 4.3 |
| combined-report | entire-brain-history-bundle-sha256 | claude-sonnet-4-6-low | full_brain | score delta -1.0, seconds p=0.01153, tokens p=0.01407, turns p=0.004396, cost p=0.04283 |
| combined-report | entire-brain-history-claude-bare-auth | codex-medium | full_brain | score delta 4.7 |
| combined-report | entire-brain-history-claude-bare-auth | codex-gpt-5.4-mini-medium | full_brain | score delta 17.0, tokens p=0.00678 |
| combined-report | entire-brain-history-claude-bare-auth | codex-gpt-5.3-codex-medium | full_brain | score delta -10.3, seconds p=0.01003, tokens p=0.0455 |
| combined-report | entire-brain-history-claude-bare-auth | claude-sonnet-4-6-low | full_brain | score delta 21.3, seconds p=0.04064, tokens p=3.285e-05, turns p=1.896e-17, cost p=8.044e-05 |
| combined-report | entire-brain-history-claude-bare-auth | claude-haiku-4-5-low | full_brain | score delta 29.7, turns p=0.02497 |
| combined-report | entire-brain-history-claude-seed-agent | codex-medium | full_brain | score delta 7.3, seconds p=0.02212 |
| combined-report | entire-brain-history-claude-seed-agent | codex-gpt-5.4-mini-medium | full_brain | score delta 4.7, tokens p=0.01509 |
| combined-report | entire-brain-history-claude-seed-agent | claude-sonnet-4-6-low | full_brain | score delta 13.0 |
| combined-report | entire-brain-history-github-visibility | codex-medium | full_brain | score delta 4.0 |
| combined-report | entire-brain-history-github-visibility | codex-gpt-5.4-mini-medium | full_brain | score delta 7.3, seconds p=0.02417, tokens p=5.001e-05 |
| combined-report | entire-brain-history-github-visibility | claude-sonnet-4-6-low | full_brain | score delta -5.7, turns p=0.0455 |
| combined-report | swe-style-entire-brain-history-claude-bare-auth | codex-medium | full_brain | score delta 21.3 |
| combined-report | swe-style-entire-brain-history-claude-bare-auth | claude-sonnet-4-6-low | full_brain | score delta 27.7, cost p=0.007941 |
| combined-report | swe-style-entire-brain-history-claude-seed-agent | codex-gpt-5.4-mini-medium | full_brain | score delta 8.3, tokens p=0.009437 |
| combined-report | swe-style-entire-brain-history-bundle-sha256 | claude-haiku-4-5-low | full_brain | score delta -9.0, seconds p=0.04354, cost p=0.01916 |
| combined-report | swe-style-entire-brain-history-codex-schema-contract | claude-haiku-4-5-low | full_brain | score delta 38.7, turns p=0.003915 |
| combined-report | swe-style-entire-brain-history-github-visibility | codex-gpt-5.4-mini-medium | full_brain | score delta 7.0 |
| combined-report | github-cli-repo-name-trims-dotgit | claude-haiku-4-5-low | semantic_brain | score delta -31.0, tokens p=0.02776, turns p=0.02329, cost p=0.03381 |
| combined-report | entire-brain-history-bundle-sha256 | codex-default-high | full_brain | score delta 11.3 |
| combined-report | swe-style-entire-brain-history-bundle-sha256 | codex-default-high | full_brain | score delta 11.7, seconds p=0.005071 |
| combined-report | swe-style-entire-brain-history-claude-bare-auth | codex-default-high | full_brain | score delta 39.7 |
| combined-report | swe-style-entire-brain-history-codex-schema-contract | codex-default-high | full_brain | score delta 5.0 |
| phase2-codex-default-high-history-fill-r3-20260602 | entire-brain-history-bundle-sha256 | codex-default-high | full_brain | score delta 11.3 |
| phase2-codex-default-high-history-fill-r3-20260602 | swe-style-entire-brain-history-bundle-sha256 | codex-default-high | full_brain | score delta 11.7, seconds p=0.005071 |
| phase2-codex-default-high-history-fill-r3-20260602 | swe-style-entire-brain-history-claude-bare-auth | codex-default-high | full_brain | score delta 39.7 |
| phase2-codex-default-high-history-fill-r3-20260602 | swe-style-entire-brain-history-codex-schema-contract | codex-default-high | full_brain | score delta 5.0 |
| phase2-isolated-schema-claude-codex-r2b | entire-brain-history-codex-schema-contract | claude-sonnet-low | full_brain | score delta 60.0 |
| phase2-isolated-schema-claude-codex-r2b | entire-brain-history-codex-schema-contract | codex-medium | full_brain | score delta 65.0 |
| phase2-isolated-schema-claude-codex-r3 | entire-brain-history-codex-schema-contract | codex-medium | full_brain | score delta 65.0 |
| phase2-layer-a-history-matrix-r3-20260601 | entire-brain-history-bundle-sha256 | codex-medium | full_brain | score delta 9.7, seconds p=0.02531 |
| phase2-layer-a-history-matrix-r3-20260601 | entire-brain-history-bundle-sha256 | codex-gpt-5.4-mini-medium | full_brain | score delta 4.3 |
| phase2-layer-a-history-matrix-r3-20260601 | entire-brain-history-bundle-sha256 | claude-sonnet-4-6-low | full_brain | score delta -1.0, seconds p=0.01153, tokens p=0.01407, turns p=0.004396, cost p=0.04283 |
| phase2-layer-a-history-matrix-r3-20260601 | entire-brain-history-claude-bare-auth | codex-medium | full_brain | score delta 4.7 |
| phase2-layer-a-history-matrix-r3-20260601 | entire-brain-history-claude-bare-auth | codex-gpt-5.4-mini-medium | full_brain | score delta 17.0, tokens p=0.00678 |
| phase2-layer-a-history-matrix-r3-20260601 | entire-brain-history-claude-bare-auth | codex-gpt-5.3-codex-medium | full_brain | score delta -10.3, seconds p=0.01003, tokens p=0.0455 |
| phase2-layer-a-history-matrix-r3-20260601 | entire-brain-history-claude-bare-auth | claude-sonnet-4-6-low | full_brain | score delta 21.3, seconds p=0.04064, tokens p=3.285e-05, turns p=1.896e-17, cost p=8.044e-05 |
| phase2-layer-a-history-matrix-r3-20260601 | entire-brain-history-claude-bare-auth | claude-haiku-4-5-low | full_brain | score delta 29.7, turns p=0.02497 |
| phase2-layer-a-history-matrix-r3-20260601 | entire-brain-history-claude-seed-agent | codex-medium | full_brain | score delta 7.3, seconds p=0.02212 |
| phase2-layer-a-history-matrix-r3-20260601 | entire-brain-history-claude-seed-agent | codex-gpt-5.4-mini-medium | full_brain | score delta 4.7, tokens p=0.01509 |
| phase2-layer-a-history-matrix-r3-20260601 | entire-brain-history-claude-seed-agent | claude-sonnet-4-6-low | full_brain | score delta 13.0 |
| phase2-layer-a-history-matrix-r3-20260601 | entire-brain-history-github-visibility | codex-medium | full_brain | score delta 4.0 |
| phase2-layer-a-history-matrix-r3-20260601 | entire-brain-history-github-visibility | codex-gpt-5.4-mini-medium | full_brain | score delta 7.3, seconds p=0.02417, tokens p=5.001e-05 |
| phase2-layer-a-history-matrix-r3-20260601 | entire-brain-history-github-visibility | claude-sonnet-4-6-low | full_brain | score delta -5.7, turns p=0.0455 |
| phase2-layer-b-history-mini-haiku-fill-r3-20260602 | swe-style-entire-brain-history-bundle-sha256 | claude-haiku-4-5-low | full_brain | score delta -9.0, seconds p=0.04354, cost p=0.01916 |
| phase2-layer-b-history-mini-haiku-fill-r3-20260602 | swe-style-entire-brain-history-codex-schema-contract | codex-gpt-5.4-mini-medium | full_brain | score delta 16.7, seconds p=1.994e-05 |
| phase2-layer-b-history-mini-haiku-fill-r3-20260602 | swe-style-entire-brain-history-codex-schema-contract | claude-haiku-4-5-low | full_brain | score delta 38.7, turns p=0.003915 |
| phase2-layer-b-history-mini-haiku-fill-r3-20260602 | swe-style-entire-brain-history-github-visibility | codex-gpt-5.4-mini-medium | full_brain | score delta 7.0 |
| phase2-layer-b-swe-claude-history-matrix-r3-20260602 | swe-style-entire-brain-history-claude-bare-auth | codex-medium | full_brain | score delta 21.3 |
| phase2-layer-b-swe-claude-history-matrix-r3-20260602 | swe-style-entire-brain-history-claude-bare-auth | claude-sonnet-4-6-low | full_brain | score delta 27.7, cost p=0.007941 |
| phase2-layer-b-swe-claude-history-matrix-r3-20260602 | swe-style-entire-brain-history-claude-seed-agent | codex-gpt-5.4-mini-medium | full_brain | score delta 8.3, tokens p=0.009437 |
| phase2-layer-b-swe-history-matrix-r3-20260601 | swe-style-entire-brain-history-bundle-sha256 | codex-medium | full_brain | score delta 12.0, seconds p=0.04567 |
| phase2-layer-b-swe-history-matrix-r3-20260601 | swe-style-entire-brain-history-bundle-sha256 | codex-gpt-5.3-codex-medium | full_brain | score delta 6.0 |
| phase2-layer-b-swe-history-matrix-r3-20260601 | swe-style-entire-brain-history-bundle-sha256 | claude-sonnet-4-6-low | full_brain | score delta -0.7, seconds p=4.455e-47, tokens p=1.34e-11, turns p=0.04937, cost p=0.003027 |
| phase2-layer-b-swe-history-matrix-r3-20260601 | swe-style-entire-brain-history-github-visibility | codex-medium | full_brain | score delta 6.3 |
| phase2-layer-b-swe-history-matrix-r3-20260601 | swe-style-entire-brain-history-github-visibility | codex-gpt-5.3-codex-medium | full_brain | score delta 2.7 |
| phase2-layer-b-swe-history-matrix-r3-20260601 | swe-style-entire-brain-history-github-visibility | claude-sonnet-4-6-low | full_brain | score delta 3.0, tokens p=0.0005763, turns p=0.002282, cost p=9.947e-08 |
| phase2-layer-b-swe-schema-matrix-r3-20260601 | swe-style-entire-brain-history-codex-schema-contract | codex-medium | full_brain | score delta 26.0 |
| phase2-layer-b-swe-schema-matrix-r3-20260601 | swe-style-entire-brain-history-codex-schema-contract | codex-gpt-5.3-codex-medium | full_brain | score delta 27.0 |
| phase2-layer-b-swe-schema-matrix-r3-20260601 | swe-style-entire-brain-history-codex-schema-contract | claude-sonnet-4-6-low | full_brain | score delta 3.3, seconds p=0.0002802, tokens p=0.009277, cost p=1.082e-13 |
| phase2-layer-c-schema-codex-lower-cost-r4-r8-20260601 | entire-brain-history-codex-schema-contract | codex-gpt-5.4-mini-medium | full_brain | score delta 15.2 |
| phase2-layer-c-schema-codex-lower-cost-r4-r8-20260601 | entire-brain-history-codex-schema-contract | codex-gpt-5.3-codex-medium | full_brain | score delta 18.6 |
| phase2-layer-c-schema-lower-cost-r3-20260601 | entire-brain-history-codex-schema-contract | claude-sonnet-4-6-low | full_brain | score delta 8.0, seconds p=2.042e-17, tokens p=1.615e-05, cost p=1.74e-12 |
| phase2-proof-batch-codex-medium-r3-20260601 | entire-brain-history-codex-schema-contract | codex-medium | full_brain | score delta 37.0 |
| phase2-proof-batch-codex-medium-r3-20260601 | github-cli-repo-name-trims-dotgit | codex-medium | semantic_brain | score delta 9.0, seconds p=0.001458, tokens p=2.011e-05 |

## Layer A: Project-Native Entire

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

## Layer B: GitHub CLI Project-Native

| ID | Repo/Task | Brain | Archetype/Runner | Primary metric |
|---|---|---|---|---|
| `phase2-b-01-github-cli-auth-env-token-architecture-localization` | github-cli-auth-env-token | semantic_brain | architecture-localization | score and changed_file_count |
| `phase2-b-02-github-cli-auth-env-token-rationale-recovery` | github-cli-auth-env-token | semantic_brain | rationale-recovery | success_rate and score |
| `phase2-b-03-github-cli-auth-env-token-validation-selection` | github-cli-auth-env-token | semantic_brain | validation-selection | tokens, seconds, and validation_discipline |
| `phase2-b-04-github-cli-auth-env-token-stale-live-hygiene` | github-cli-auth-env-token | semantic_brain | stale-live-hygiene | success_rate and file_read_count |
| `phase2-b-05-github-cli-auth-env-token-protocol-contract-recovery` | github-cli-auth-env-token | semantic_brain | protocol-contract-recovery | success_rate, score, tokens, and changed_file_count |
| `phase2-b-06-github-cli-format-web-conflict-architecture-localization` | github-cli-format-web-conflict | semantic_brain | architecture-localization | score and changed_file_count |
| `phase2-b-07-github-cli-format-web-conflict-rationale-recovery` | github-cli-format-web-conflict | semantic_brain | rationale-recovery | success_rate and score |
| `phase2-b-08-github-cli-format-web-conflict-validation-selection` | github-cli-format-web-conflict | semantic_brain | validation-selection | tokens, seconds, and validation_discipline |
| `phase2-b-09-github-cli-format-web-conflict-stale-live-hygiene` | github-cli-format-web-conflict | semantic_brain | stale-live-hygiene | success_rate and file_read_count |
| `phase2-b-10-github-cli-format-web-conflict-protocol-contract-recovery` | github-cli-format-web-conflict | semantic_brain | protocol-contract-recovery | success_rate, score, tokens, and changed_file_count |
| `phase2-b-11-github-cli-http-scopes-suggestion-architecture-localization` | github-cli-http-scopes-suggestion | semantic_brain | architecture-localization | score and changed_file_count |
| `phase2-b-12-github-cli-http-scopes-suggestion-rationale-recovery` | github-cli-http-scopes-suggestion | semantic_brain | rationale-recovery | success_rate and score |
| `phase2-b-13-github-cli-http-scopes-suggestion-validation-selection` | github-cli-http-scopes-suggestion | semantic_brain | validation-selection | tokens, seconds, and validation_discipline |
| `phase2-b-14-github-cli-http-scopes-suggestion-stale-live-hygiene` | github-cli-http-scopes-suggestion | semantic_brain | stale-live-hygiene | success_rate and file_read_count |
| `phase2-b-15-github-cli-http-scopes-suggestion-protocol-contract-recovery` | github-cli-http-scopes-suggestion | semantic_brain | protocol-contract-recovery | success_rate, score, tokens, and changed_file_count |
| `phase2-b-16-github-cli-json-web-conflict-architecture-localization` | github-cli-json-web-conflict | semantic_brain | architecture-localization | score and changed_file_count |
| `phase2-b-17-github-cli-json-web-conflict-rationale-recovery` | github-cli-json-web-conflict | semantic_brain | rationale-recovery | success_rate and score |
| `phase2-b-18-github-cli-json-web-conflict-validation-selection` | github-cli-json-web-conflict | semantic_brain | validation-selection | tokens, seconds, and validation_discipline |
| `phase2-b-19-github-cli-json-web-conflict-stale-live-hygiene` | github-cli-json-web-conflict | semantic_brain | stale-live-hygiene | success_rate and file_read_count |
| `phase2-b-20-github-cli-json-web-conflict-protocol-contract-recovery` | github-cli-json-web-conflict | semantic_brain | protocol-contract-recovery | success_rate, score, tokens, and changed_file_count |
| `phase2-b-21-github-cli-repo-name-trims-dotgit-architecture-localization` | github-cli-repo-name-trims-dotgit | semantic_brain | architecture-localization | score and changed_file_count |

## Layer C: SWE-Bench-Style

| ID | Repo/Task | Brain | Archetype/Runner | Primary metric |
|---|---|---|---|---|
| `phase2-c-01-swe-style-entire-brain-history-bundle-sha256-hidden-cross-file-contract` | swe-style-entire-brain-history-bundle-sha256 | full_brain | hidden-cross-file-contract | success_rate and score |
| `phase2-c-02-swe-style-entire-brain-history-bundle-sha256-large-repo-near-miss` | swe-style-entire-brain-history-bundle-sha256 | full_brain | large-repo-near-miss | tokens and changed_file_count |
| `phase2-c-03-swe-style-entire-brain-history-bundle-sha256-history-only-regression` | swe-style-entire-brain-history-bundle-sha256 | full_brain | history-only-regression | success_rate |
| `phase2-c-04-swe-style-entire-brain-history-bundle-sha256-stale-context-issue` | swe-style-entire-brain-history-bundle-sha256 | full_brain | stale-context-issue | success_rate and file_read_count |
| `phase2-c-05-swe-style-entire-brain-history-claude-bare-auth-hidden-cross-file-contract` | swe-style-entire-brain-history-claude-bare-auth | full_brain | hidden-cross-file-contract | success_rate and score |
| `phase2-c-06-swe-style-entire-brain-history-claude-bare-auth-large-repo-near-miss` | swe-style-entire-brain-history-claude-bare-auth | full_brain | large-repo-near-miss | tokens and changed_file_count |
| `phase2-c-07-swe-style-entire-brain-history-claude-bare-auth-history-only-regression` | swe-style-entire-brain-history-claude-bare-auth | full_brain | history-only-regression | success_rate |
| `phase2-c-08-swe-style-entire-brain-history-claude-bare-auth-stale-context-issue` | swe-style-entire-brain-history-claude-bare-auth | full_brain | stale-context-issue | success_rate and file_read_count |
| `phase2-c-09-swe-style-entire-brain-history-claude-seed-agent-hidden-cross-file-contract` | swe-style-entire-brain-history-claude-seed-agent | full_brain | hidden-cross-file-contract | success_rate and score |
| `phase2-c-10-swe-style-entire-brain-history-claude-seed-agent-large-repo-near-miss` | swe-style-entire-brain-history-claude-seed-agent | full_brain | large-repo-near-miss | tokens and changed_file_count |
| `phase2-c-11-swe-style-entire-brain-history-claude-seed-agent-history-only-regression` | swe-style-entire-brain-history-claude-seed-agent | full_brain | history-only-regression | success_rate |
| `phase2-c-12-swe-style-entire-brain-history-claude-seed-agent-stale-context-issue` | swe-style-entire-brain-history-claude-seed-agent | full_brain | stale-context-issue | success_rate and file_read_count |
| `phase2-c-13-swe-style-entire-brain-history-codex-schema-contract-hidden-cross-file-contract` | swe-style-entire-brain-history-codex-schema-contract | full_brain | hidden-cross-file-contract | success_rate and score |
| `phase2-c-14-swe-style-entire-brain-history-codex-schema-contract-large-repo-near-miss` | swe-style-entire-brain-history-codex-schema-contract | full_brain | large-repo-near-miss | tokens and changed_file_count |
| `phase2-c-15-swe-style-entire-brain-history-codex-schema-contract-history-only-regression` | swe-style-entire-brain-history-codex-schema-contract | full_brain | history-only-regression | success_rate |
| `phase2-c-16-swe-style-entire-brain-history-codex-schema-contract-stale-context-issue` | swe-style-entire-brain-history-codex-schema-contract | full_brain | stale-context-issue | success_rate and file_read_count |
| `phase2-c-17-swe-style-entire-brain-history-github-visibility-hidden-cross-file-contract` | swe-style-entire-brain-history-github-visibility | full_brain | hidden-cross-file-contract | success_rate and score |
| `phase2-c-18-swe-style-entire-brain-history-github-visibility-large-repo-near-miss` | swe-style-entire-brain-history-github-visibility | full_brain | large-repo-near-miss | tokens and changed_file_count |
| `phase2-c-19-swe-style-entire-brain-history-github-visibility-history-only-regression` | swe-style-entire-brain-history-github-visibility | full_brain | history-only-regression | success_rate |
| `phase2-c-20-swe-style-entire-brain-history-github-visibility-stale-context-issue` | swe-style-entire-brain-history-github-visibility | full_brain | stale-context-issue | success_rate and file_read_count |
| `phase2-c-21-swe-style-entire-brain-stale-query-default-limit-hidden-cross-file-contract` | swe-style-entire-brain-stale-query-default-limit | full_brain | hidden-cross-file-contract | success_rate and score |

