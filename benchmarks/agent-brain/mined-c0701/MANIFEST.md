# Preregistered benchmark task set - entire-cli, post-cutoff C=2026-07-01 (Phase 1)

Frozen **before** any agent/brain run. Mechanically mined from real entire-cli fix
commits (cutoff C = 2026-07-01, per ENTIRE-BRAIN-BENCHMARK-PLAN-2026-07-14.md). Nothing
here is selected on retrieval or on the `full_brain` result.

## Scheme: per-task-base, forward test-only apply (v2)

Each task's `base_commit` is the fix commit's **PARENT**, where the bug exists
**natively**. `post_brain_commands` **forward**-applies only the fix commit's **TEST**
hunk (adds the failing test) via plain `git apply` — no reverse source patch, no
`--3way`, no conflict. The added test uses pre-existing public APIs so it compiles at
the parent. The rationale/decision lives in pre-C session history, not the tree.

This replaces the earlier base=HEAD + reverse-3way scheme, which lost ~74/133
candidates to later commits rewriting the same source lines (the reverse patch's
pre-image is the fix-commit blob, absent from the synthetic worktree and drifted
hundreds of commits from HEAD).

## Task mechanics
- `base_commit` = fix commit's parent SHA.
- `post_brain_commands = ["git apply <abs patch>"]` forward-applies the TEST hunk only.
- `setup_patch = ""`; patches live in `patches/entire-cli-c0701-<short>-test.patch`.
- `validation` runs the test func(s) the patch adds/exercises (`go test ./<pkg> -run ...`).
- Prompts are **symptom-only**: observable behavior/impact, no fix location, function,
  invariant, or test names.
- `memory_delivery: "frozen_brief"`, `prepare_semantic: false`.
- `brain_queries`: 3-5 natural-language queries to recall the rationale.
- `hide_expected_from_agent` / `hide_validation_from_agent`: true.

## Negative control (validity gate — already run per task)
Per-task-base negative control: with the test hunk applied on the parent, the target
test is **RED at the parent base** (bug present natively) and **GREEN at the fix**.
The original keep list recorded 45 passing tasks. The additional `d9df8fcca` config also records
`_negctl: red_at_parent_base`; it was omitted from that list and is reconciled below as task 46.

## Split (commit-hash parity: even last hex of fix-commit SHA = dev, odd = holdout)
- **dev: 22**  .  **holdout: 24**  .  **total: 46 unique tasks**

## dev tasks
| commit | subject |
|---|---|
| 21fcaf5f2 | fix(strategy): never flag imported sessions as orphaned in clean |
| 250538b1f | fix(import): imported session LastPrompt uses the most recent turn |
| 40cb4d68a | Deduplicate no-id Pi review token events |
| 570854e73 | repo mirror list: rename REPO column and --repo flag to NAME |
| 5f68588b1 | fix: quoted repo filters, stale code results, and search mode on error |
| 6050a60b3 | git-remote-entire: only suggest clone for a complete forge/owner/repo ref |
| 60576d5e8 | fix(mirror): exit non-zero when create hits an admin-suspended mirror |
| 6699ec40a | Avoid double counting Pi review cache tokens |
| 6cdbe3cc8 | fix: fan out code search across mirror placements, not just home cell |
| 855b8cdab | Route checkpoints through entire:// push-through mirrors |
| 8828752a7 | checkpoint resume: fall back to remote branches in auto-detection |
| 96ed4e121 | fix(strategy): don't purge imported sessions in shadow-branch orphan cleanup |
| 996336140 | feat(session): exempt imported sessions from staleness auto-purge |
| a9d4ce908 | checkpoint resume: harden auto-detection and clash message |
| aaeb8546c | cli/api: keep the bearer on its origin across redirects and cross-host paths |
| b3f0deb5b | clusterdiscovery: don't serve an audience-less stale entry to audience-requiring callers |
| b72a6e621 | fix: dedupe merged code-search stats across mirror cells |
| c67d74e6c | uiform: clear foreground on blurred titles/options too |
| d8df699cb | repo mirror list: filter on the owner/repo form shown in the table |
| d90849ac5 | cli/enable: gate import offer on checkpoint policy; don't auto-import non-interactively |
| d9df8fcca | checkpoint fetch: exclude URL-keyed promisor entries from bulk git fetch --all rather than switching to a named remote |
| e52b748b4 | Mention trail findings in trail help |

## holdout tasks
| commit | subject |
|---|---|
| 0730ddd75 | fix(settings): honor redaction.externalize_images in local settings merge |
| 078a63ebb | fix: TUI code search handles multi-repo filters and repo:* correctly |
| 338d79887 | git-remote-entire: case-fold the home-jurisdiction routing comparison |
| 36563bc78 | Gate migrated-ref push on the checkpoint policy |
| 3a1b973ad | cli: render mirror poll 404 cleanly and widen poll retry budget |
| 40cded153 | git-refs: compact the push queue on Drain |
| 43ae6061f | fix: match cell name against cluster URLs before jurisdiction fallback |
| 4dd458656 | cursor: replace stale-form hooks on wrapper migration |
| 55cde8f97 | git-remote-entire: identity tokens only — drop the repo-scoped fallback |
| 5edbdf073 | Guard checkpoint migration re-runs against ref regression |
| 605050d0e | migrate: re-enqueue already-imported refs so a lost enqueue still pushes |
| 7a56d8ce9 | fix(resume): exclude imported sessions from the resume picker |
| 80c7a4efd | /simplify + review: fix fallback-on-error, dedup List, share sort helper |
| 80f680618 | migrate: --dry-run no longer writes loose git objects |
| 885cf3a68 | harden review findings fallback |
| 9c2b149fd | cli/auth: add -j short form for `token --jurisdiction` |
| befdcf4fb | Normalize checkpoint metadata during branch-to-refs migration |
| c768ab0ec | add brew auto-update confirmation flag |
| c8d80aa8f | fix: jurisdiction fallback prefers default cluster |
| cf28d625c | reword checkpoint policy help to write-guard semantics |
| dd255c286 | remotehelper: re-mint and retry once on 401 instead of failing the command |
| de60a63d0 | avoid ambiguous review findings handles |
| e1112a77a | Make attach checkpoint-presence backend-aware (git-refs) |
| fe8428c24 | add review findings detail handles |

## Files
- `dev/`, `holdout/` - task JSONs (per-task-base scheme).
- `patches/` - forward TEST-hunk patches (`entire-cli-c0701-<short>-test.patch`).
- `selection-ledger.json` - the 46 unique tasks: short sha, full sha, split, subject.
- `keep_shas.txt` (scratchpad, not retained here) originally listed 45 negative-control-passing fix SHAs;
  `d9df8fcca` was an additional negative-control-passing config/patch omitted from that ledger and is
  now accounted for explicitly.

The duplicate development copy of `4dd458656` was removed. Its canonical config remains in
`holdout/` to match the mechanical parity split, but prior prompt inspection, retrieval probes, agent
runs, and ranking/cap optimization make it permanently ineligible for confirmation. See
`confirmatory/task-inventory.json` for the contamination state of every unique task.

**Status: task set only - NOT yet run.** Running arms (P3/P5) is gated per the plan.
