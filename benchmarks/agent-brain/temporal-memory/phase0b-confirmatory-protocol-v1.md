# Phase 0B Temporal-Memory Confirmatory Protocol — v1 (PREREGISTRATION DRAFT)

Status: **DRAFT for review.** Nothing here is confirmatory until this document is
frozen (committed + content-hashed), the product heads are frozen, and the sealed
task set is constructed and sealed — all BEFORE the first confirmatory run.

This protocol turns the dev finding ("delivered project memory helps a coding
agent recover a hidden project decision, at large token savings") into a claim by
scaling the axis that matters — **tasks**, not reps — under a fixed, preregistered
design. It supersedes ad-hoc `run.py` dev pilots; those existed only to de-risk the
harness, which is now done.

## 0. Why this design (the load-bearing decisions)

- **The unit of analysis is the TASK, not the repetition.** Reps estimate a single
  task's per-condition pass rate; they do not generalize. One diagnostic task at any
  n is not evidence. Power comes from the number of independent tasks; the analysis
  clusters by task. Reps only need to be enough to estimate each task×condition cell
  stably after exclusions.
- **The effect is large, so n-per-task is small.** The dev pilot (harness lane) had
  memory arms saturated (4/4 pass, ~96 score, ~28× fewer tokens) vs a ~50% baseline.
  A per-task pass-rate contrast of that size needs only ~6 valid reps/cell; the
  statistical work is across tasks.
- **Confirmatory integrity is procedural, not statistical.** The result is only
  trustworthy if the design, task identities, product heads, and analysis code are
  all frozen before the run and never tuned to outcomes.

## 1. Hypotheses

Primary (H1): For a coding agent at a fixed current-code snapshot, delivering the
frozen project **history + durable facts** (`history_facts`) improves the rate of
recovering a deliberately hidden project decision over the no-memory baseline
(`no_brain`), measured by validation pass-rate, across the sealed task population.

Null (H0): mean per-task Δpass-rate(`history_facts` − `no_brain`) = 0.

Secondary (preregistered, Holm-controlled family):
- S1: `raw_history` vs `no_brain` (pass-rate) — history channel alone.
- S2: `facts_only` vs `no_brain` (pass-rate) — distilled-facts channel alone.
- S3: token efficiency — mean per-task ratio of total tokens (each memory arm vs
  `no_brain`); directional prediction: memory arms use fewer tokens.
- S4: composite score delta per arm.

Exploratory (not confirmatory, reported as such): turns, wall time, `facts_only`
failure-mode analysis (the dev signal that distilled facts can mislead).

## 2. Design

- Factor: memory condition ∈ {`no_brain`, `raw_history`, `facts_only`,
  `history_facts`}. Within-task (every task runs all four).
- **Delivery lane: HARNESS (`memory_delivery: harness`).** The harness performs the
  one frozen retrieval and injects the packet; the agent does not self-retrieve.
  This is mandatory — the agent_tool lane makes outcomes hostage to the agent
  choosing to search first, which invalidated an earlier pilot. (Fixed/known.)
- Everything except the delivered memory channel is held identical across arms:
  task base commit, regression patch, prompt scaffold, hidden validation, runner
  model/effort/timeout/retry, semantic/seed/docs/patterns disabled, one frozen
  retrieval with the same query and result limit, and — critically — the agent
  worktree is stripped of the repo's OWN committed memory side-channels (`.entire/`,
  `.codex/`) and checkpoint ref in EVERY arm, so all conditions start from an
  identical memory-free worktree (harness `should_remove_agent_visible_entire_history`).

## 3. Sealed task set (the critical-path work)

**Target: 12 tasks across ≥ 4 distinct Entire-instrumented repositories.
Minimum for a valid run: 8 valid tasks (see §6).**

Constraints per task:
1. Repo must be **Entire-instrumented** (real session transcripts, checkpoints, and
   durable facts exist to deliver as memory).
2. Repo set must be **disjoint from the dev tasks** (dev used `entire-brain` /
   `temporal-memory-default-fact-merge-confidence`). No sealed task may reuse a dev
   repo+decision.
3. Each task encodes ONE **hidden project decision** made in the history that is
   recoverable from delivered memory but NOT determinable from current code alone
   (so `no_brain` must genuinely struggle), with a deterministic hidden validation.
4. Each task pins a frozen source bundle: `task_base_commit`, `checkpoint_ref_commit`,
   `cutoff_at`, `retrieval_branch`, `session_ids`, and content hashes
   (`transcript_sha256`, `history_sha256`, `fact_artifact_sha256`) so all runners get
   byte-identical memory inputs. Facts are trusted by content hash (the distiller
   binary is NOT pinned — vendor-updated agents move/change).

**Sealing procedure (prevents tuning to outcomes):**
- Tasks are authored and their `expected`/validation/hidden-decision fields are
  committed but **redacted/hashed** in the public protocol; the raw identities are
  sealed (stored out-of-band, or in a sealed artifact whose hash is committed here).
- The confirmatory run consumes only the sealed hashes; the author does not inspect
  scored outcomes before the score boundary, and no product or prompt is changed
  after any task is scored.
- Dev tasks are explicitly non-confirmatory and may not be promoted into this set.

## 4. Runners / product heads (frozen before the run)

- Primary runner: `claude:sonnet:high` (pin exact resolved model + effort in the
  run manifest). Optional second runner for cross-runner robustness if a pinned
  model is available in the run environment (report per-runner; do not pool across
  runners in the primary test).
- **Brain product head FROZEN** to the immutable, byte-reproducible build recorded
  in `product-optimization-v7/brain-product-freeze-v7.json` (commit
  `4cd6dca044…`, binary sha `9efadf08…`). The confirmatory run consumes that exact
  head; no product change after freezing.
- Harness commit pinned in the run manifest. Analysis code (this protocol's scoring
  + `run.summarize`) pinned by commit hash before the run.

## 5. Repetitions and totals

- **6 reps per task × condition.** (≥4 valid required after exclusions, §6; 6 gives
  headroom.)
- Primary design: 12 tasks × 4 conditions × 6 reps = **288 runs / runner**.
- Budget tier (if cost-constrained): 2 conditions (`no_brain` + `history_facts`) ×
  12 tasks × 6 reps = 144 runs/runner — tests H1 only, drops the channel
  decomposition (S1/S2).

## 6. Validity gates and exclusions (preregistered)

Applied by the frozen harness/analysis; not discretionary:
- **Infrastructure non-outcomes** (`analysis_excluded`, F2): agent never produced an
  outcome (harness delivery/isolation failure). Excluded from the cell; counted.
- **Adherence-invalid runs** (`temporal_memory_condition_audit.ok == False`): agent
  violated the condition (probed forbidden artifacts, used Brain in the harness
  lane). Excluded from the cell; counted.
- **Cell validity:** a task×condition cell is valid iff ≥ 4 valid reps remain.
- **Task validity:** a task is valid iff ALL of its cells are valid. Invalid tasks
  are dropped from the primary analysis and reported.
- **Run validity:** the confirmatory run is valid iff ≥ 8 valid tasks remain. Below
  8 → the run is inconclusive; do not weaken the gate post-hoc.
- **Hard stop / invalidation (not a rerun):** any sealed-identity/validation leak
  before the score boundary; any product/prompt change after a task is scored; >25%
  of runs infrastructure-excluded (indicates a harness fault, not evidence).

## 7. Analysis plan (fixed before the run; no peeking)

- For each task t and condition c: `p_{t,c}` = valid passes / valid reps.
- **Primary test:** per-task Δ_t = `p_{t,history_facts}` − `p_{t,no_brain}`; test
  mean_t(Δ_t) > 0 with a **paired test across tasks** (Wilcoxon signed-rank as the
  primary, clustered by task; paired t as a sensitivity check). α = 0.05, one-sided
  (directional H1). Report the effect (mean Δ, per-task distribution, 95% CI).
- **Secondary family (S1–S4):** same per-task paired structure; **Holm-Bonferroni**
  across the secondary family. Secondaries are confirmatory only within that
  corrected family; everything else is exploratory.
- **Preregistered robustness (secondary):** a mixed-effects logistic regression,
  pass ~ condition + (1 | task), condition effect with task random intercepts — must
  agree in sign/significance with the primary paired test to claim robustness.
- **Token efficiency (S3):** per-task ratio mean_c(tokens)/mean_no_brain(tokens);
  test the log-ratio < 0 (paired across tasks). Report as descriptive within a
  single backend only (cross-backend token totals are not calibrated cost).
- **Fixed n, single analysis.** No interim looks, no optional stopping, no adding
  tasks/reps after seeing results.

## 8. Power (for the preregistered n)

Assume the dev-observed magnitude (per-task Δpass ≈ +0.4, between-task SD ≈ 0.25):
- 12 tasks, paired test, α=0.05 one-sided → power > 0.99; minimum detectable effect
  (80% power) ≈ Δpass +0.21. 8 tasks → MDE ≈ +0.26.
- So 12 tasks is well-powered even if the true effect is roughly half the dev
  estimate; 8 (the floor) still detects a moderate effect. Reps=6/cell keep each
  `p_{t,c}` estimate stable (SE ≤ ~0.2) after exclusions. If the true effect is much
  smaller than dev suggests, the run is honestly underpowered — that is the correct
  outcome, not a reason to add tasks post-hoc.

## 9. Provenance frozen in the run manifest

Harness commit; brain product-freeze (commit + binary sha); runner resolved model +
effort; per-task source bundle hashes; environment (OS, Go/toolchain, agent CLI
versions); this protocol's content hash; analysis-code commit; the sealed-task-set
hash. The run is reproducible from the manifest.

## 10. Cost / time (order-of-magnitude, primary design, 1 runner)

288 runs; from dev, `no_brain` is the token-heavy arm (~6–7M tokens, ~$3),
memory arms far cheaper (~0.2–2.8M). Rough ≈ **$300–500 and ~1–2 days** with modest
parallelism (single-runner, sequential ≈ 30+ hrs). Two runners ≈ 2×. This is a
deliberate, budgeted, one-shot run — not incremental.

## 11. Open items to resolve before freezing

1. **Sealed task authoring is the critical path** — identify ≥ 4 Entire-instrumented,
   dev-disjoint repos and one recoverable hidden decision each; author + freeze +
   seal 12 tasks. This is the bulk of the remaining work and gates everything.
2. Decide the runner set (claude-only vs claude + a pinned second model available in
   the run environment).
3. Decide primary vs budget tier (all 4 conditions vs `no_brain`+`history_facts`).
4. Choose the sealing mechanism (out-of-band identities vs committed sealed-hash
   artifact) and who holds the score boundary.
5. Confirm the run environment + budget + parallelism.
