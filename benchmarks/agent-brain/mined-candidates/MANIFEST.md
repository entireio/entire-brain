# Mined memory-sensitive tasks — entire-cli (batch 1)

Confirmatory-set candidates auto-mined from real entire-cli fix commits, to replace
the hand-authored n=4 temporal-memory pilot with a powered, repo-grounded set.
See `../temporal-memory/phase0b-confirmatory-protocol-v1.md`.

## Pipeline (all read-only mining; no paid agent runs)
1. **Scan** full history for commits that, in ONE commit, modify a non-test `.go`
   source file (the guard/decision) AND add `func Test…` in a `_test.go` (free
   hidden validation), AND whose source hunks reverse-apply cleanly onto HEAD.
   → 57 clean candidates in the last 1,500 of 5,130 commits.
2. **Curate** out grab-bag/omnibus commits (`Address … findings/review`) and >3-source-file
   changes → 39 single-concern candidates.
3. **Judge** (one reviewer per commit): is the fix genuinely non-obvious after revert
   (memory-sensitive) vs reflexive/pure-feature? Draft a symptom-only prompt that does
   not name the fix. Tag flavor.
4. **Negative control** (decisive gate): in a worktree, confirm each task's hidden test is
   GREEN at HEAD and RED after applying `setup_patch`. A task that stays green does not
   exercise the guard and is dropped; a build-fail after revert is reworkable, not valid.

## Task mechanics
- `setup_patch` = reverse of the fix commit's SOURCE hunks (removes the guard AND its
  rationale comments). The commit's TEST hunks stay in the tree as hidden validation.
- The "why" lives in the session history that produced the fix, not the post-revert tree.
- `_flavor`: `recall-decision` (memory holds a constraint/why; agent must avoid the naive
  change or restore a non-obvious invariant) vs `recall-solution` (memory holds the prior fix).

## Batch 1 funnel: 13 candidates → 6 negative-control-verified
| commit | flavor | verdict |
|---|---|---|
| 8968a7639 (session-id resume/rewind) | recall-solution | **VALID** ✓ green→red |
| cebf48a79 (empty-token client reaches server) | recall-decision | **VALID** ✓ green→red |
| 0fccf698a (transcript redaction coverage) | recall-solution | **VALID** ✓ green→red |
| 1f289fb40 (off-cluster token leak) | recall-decision | **VALID** ✓ green→red |
| 8cbb97681 (Pi captureTranscript traversal) | recall-solution | **VALID** ✓ green→red |
| 17eeabea1 (dispatch repo-URL spoofing) | recall-decision | **VALID** ✓ green→red |
| 0907c4f7f (TOCTOU session-state lock) | — | dropped: still green after revert |
| 8e908d741 (opaque-token alg:none) | — | dropped: still green after revert |
| de082f61e (Windows case-insensitive path) | — | dropped: still green after revert |
| ebf27ab27 (org --role validation) | — | reworkable: build-fail after revert (test refs reverted symbol) |
| 026f6bb29 (ResolveSessionFile doc contract) | — | dropped by judge: source hunk is a comment only |
| ebdd641ae (Codex PostToolUse test) | — | dropped by judge: pure test-add, no revertable guard |
| 39754a1e8 (mirror probe drain) | — | dropped by judge: revert removes only a test-injection refactor, not the drain guard |

**Status: candidates only — NOT yet run.** Running the ablation (no_brain vs full_brain ×
reps) is a paid external-agent run and is gated on explicit greenlight.
