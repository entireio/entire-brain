# Pre-registration — mined memory-ablation at scale (entire-cli + entire-db)

Frozen **before** running any `full_brain` agent. Purpose: a big-N, cherry-pick-proof
test of whether entire-brain's project memory improves a coding agent on real,
history-derived tasks.

## Tasks
- Mined mechanically from real fix commits in entire-cli + entire-db: each commit
  adds a source guard AND a `_test.go` test in one commit; we reverse the SOURCE
  hunks (guard + rationale comments removed) and keep the test as hidden validation.
- **Negative-control verified**: every task's test is GREEN at HEAD and RED after the
  revert (75/140 candidates passed; the rest dropped).
- Prompts are **symptom-only** (no fix location, invariant, file, or test names).
- N = 75 tasks (53 entire-cli, 22 entire-db).

## Design
- Conditions: `no_brain` vs `full_brain` (product brain: history + semantic + facts).
- Shared brain prep: base = repo HEAD, guard reverted `post_brain` → brain built once
  per repo, identical across tasks (cache key excludes post_brain mutations).
- Repetitions: 2 per (task, condition). Unit of analysis = **task**.

## Primary analyses (declared now)
1. **All-tasks pooled pass rate.** Per task, pass = validation ok. Compare
   full_brain vs no_brain pass rate pooled over 75 tasks; paired bootstrap 95% CI +
   McNemar on per-task pass/fail. Reported regardless of sign.
2. **Memory-sensitive subset (pre-registered screen).** Subset = tasks where the
   `no_brain` arm does NOT already pass both reps (baseline pass rate < 100%). This
   is defined by the `no_brain` results ONLY, before looking at `full_brain`. Report
   pass-rate lift + CI on this subset.
3. **Token efficiency.** Mean tokens full_brain vs no_brain, pooled and on subset.

## Honesty commitments
- Report the full distribution incl. tasks where memory is neutral or negative.
- Report per-model tier (if more than one runner is used) separately.
- Infrastructure-failed / excluded runs counted and reported, not silently dropped.
- No task added or dropped after seeing `full_brain` results.
