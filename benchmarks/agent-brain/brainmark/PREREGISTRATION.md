# BrainMark pre-registration

**Status: DRAFT — not yet frozen.** This document must be finalized and its sha256
recorded in `SEAL-MANIFEST.json` *before* the first confirmatory session runs.
`report.py` recomputes that hash and refuses to aggregate if this file changed
after sealing.

## 1. The claim under test

> A coding agent that can reuse what a previous session on the same repository
> learned will spend less effort rediscovering where the relevant code lives.

This is the product claim for entire-brain. It has never been measured.
LoCoMo/LME measure the retrieval substrate over chat transcripts; they say
nothing about whether a *second coding session* is cheaper than the first.

## 2. Design

Paired, within-instance, five arms. For each mined pair `(A, B)` drawn from the
same repository:

1. Session **A** runs once, with no memory, at a fixed model tier. Its native
   transcript is harvested and sha256-pinned.
2. Five memory sources each consume **those exact bytes** and emit one bounded
   memory packet.
3. Session **B** runs once per arm, at `base_commit_B`, with the packet
   delivered through one identical envelope.
4. B is graded by **its own** `FAIL_TO_PASS` tests in the official SWE-bench
   Docker harness.

| arm | memory source | proves |
|---|---|---|
| `no_brain` | sentinel-empty packet | baseline |
| `full_brain` | `entire-brain distill` + `refresh history` over pinned A JSONL | the product |
| `mem0` | mem0 OSS over the same bytes | vs the named competitor |
| `graphify` | Graphify over the same bytes | vs the YC code-memory competitor |
| `cmm` | codebase-memory-mcp over the same bytes | vs the OSS competitor |

A's success is **not** a filter. Filtering on it would select for pairs whose
answer was easy and bias every arm simultaneously.

## 3. PRIMARY endpoint (pre-registered, single)

**`locate_calls_pre_edit`** — the number of locate calls the agent makes strictly
before its first edit. Frozen definition in `mechmetrics.py`:

- LOCATE = `tool_use` in {`Read`, `Grep`, `Glob`}, or `Bash` whose command matches
  `^(rg|grep|egrep|git grep|find|ls|cat|head|tail|ag)\b`
- EDIT = `tool_use` in {`Edit`, `Write`, `MultiEdit`, `NotebookEdit`}
- a session with no edit has no cutoff: all its locate calls count, and it is
  flagged `no_edit`
- ratios are formed on `(count + 1)`

**Estimator**: paired log-ratios, geometric mean, with a 20,000-draw percentile
bootstrap CI (seed pinned in `config.json`) **and the median reported beside it**.
The estimator is the vendored graphmark `metrics.py`, pinned by sha256.

**Comparison**: `full_brain` vs `no_brain`.

Why a mechanism endpoint is primary: a score can move while the mechanism does
not, which is baseline drift, not an effect. Pre-registering the mechanism makes
a positive result interpretable and a negative result honest.

## 4. SECONDARY endpoints

Reported, never promoted to headline: resolved% (McNemar exact), total cost USD,
total tokens, duration, turn count, and per-arm memory **prep cost** (wall-clock,
LLM calls, USD) reported separately from session cost.

## 5. Gates (symmetric, enforced in code)

- per-session: `usd > 0` **and** non-empty patch
- **all-arms-clean pair gate**: a pair counts only if *every* arm passes. A pair
  failing in one arm is dropped from **all** arms.
- prompt-symmetry sha identical across a pair's arms, checked at launch and
  re-derived at report time
- packet sha256 matches its pin
- seal manifest verifies
- no Entire-family tooling outside `full_brain`; no `.entire` / `.benchmark` /
  checkpoint-ref inspection in any arm

## 6. Null test (mandatory, before any confirmatory spend)

3 dev pairs × 2 reps with `run_b.py --null-test`: every arm runs its real
machinery but receives the sentinel packet. The primary and token deltas **must
straddle zero**. If they do not, the machinery is not inert and no headline may
be reported until that is fixed.

## 7. Power and what will NOT be claimed

At n = 30 the minimum detectable effect is roughly 25 points.

- A confidence interval straddling zero will be reported as **inconclusive**,
  never as "no difference" and never as a win from the point estimate.
- **No sub-group of fewer than ~30 pairs inside a cell will be reported.**
- Leave-one-out range is reported; if dropping any single pair moves the headline
  across zero, the result is one instance, not an effect.
- Cross-cell comparisons (different configs, different runs) are confounded; only
  paired-on-identical-instances comparisons will be made.

## 8. Analysis decisions frozen in advance

- estimator: geometric mean of paired log-ratios (equal-weighted per pair, **not**
  pooled) — pooling and equal weighting have been observed to flip the sign of a
  result, so the choice is fixed here rather than after seeing data
- ratio offset: `+1`
- bootstrap: 20,000 draws, percentile method, seed from `config.json`
- outliers: none removed
- stopping rule: n is fixed by the seal; no data-dependent stopping
- if a harness bug is found mid-run, the **harness** is fixed and affected cells
  re-run; the **task set is never edited**

## 9. Deviations

Any deviation from this document must be appended below, dated, before the
affected analysis is run — never after seeing its result.

- **2026-08-19** — added §§10–12 (contamination argument + floor-compression
  check + fresh-split concordance + age probe; power rule; validity gate) as
  dated amendments per plan items 0.3/0.6/0.8 (NeurIPS-2027 hardening pass,
  `p0p1/brainmark`). These are **additions** placed after §8 and before this
  section; no text in §§1–8 was edited. Each new section carries its own
  "[DATED AMENDMENT — 2026-08-19]" marker so a reader can tell frozen-at-launch
  text from text added afterward without cross-referencing this log.

## 10. Contamination argument [DATED AMENDMENT — 2026-08-19]

**Added, not a rewrite of §§1–8.** Plan item 0.3 (NeurIPS-2027 hardening).
SWE-bench instances are public; a model may have memorized a gold patch from
pretraining. This section pre-registers the argument for why that does not
invalidate the paired design, plus the checks that make the argument
falsifiable rather than asserted.

**The paired-design argument.** Every arm of a pair sees the *same* pinned
session-A bytes and the *same* B base commit (§2). If the model has memorized
B's fix, that memorization is present identically in `no_brain`, `full_brain`,
`mem0`, `graphify`, and `cmm` — memorization is a property of the model and
the task, not of which memory packet was attached. A memorized task needs
near-zero locate calls to resolve **in every arm**, which compresses the
within-pair *difference* toward zero, not away from it. Contamination can
manufacture a fake **absence** of effect (both arms already near the floor,
no room for `full_brain` to show fewer locate calls than a `no_brain` that is
already at zero) — it cannot manufacture a fake **win**, because a win
requires `full_brain` to do *reliably less locate work than `no_brain` on the
same task*, and shared memorization does not differentiate the arms. A
positive headline is therefore not explainable by contamination alone; a null
or small headline might be, which is exactly what the checks below test for.

**Floor-compression check** (pre-registered here, run in `report.py`'s
per-repo/diagnostic section — never as a headline sub-group per §7's ≥30
rule): for every clean pair, record `no_brain`'s raw `locate_calls_pre_edit`
(the "floor"). Report the Spearman correlation between that floor and the
pair's `full_brain`-vs-`no_brain` log-ratio, and a scatter of the two. If the
measured effect concentrates only among near-zero-floor pairs (`no_brain`
already needed almost no locate calls — consistent with a memorized or
trivial task), that pattern is reported as a limitation regardless of what
the pooled headline says; it is diagnostic evidence for contamination-style
floor compression, not proof, and is never used to exclude pairs post hoc.

**Fresh-split concordance** (plan 0.2's `mine_fresh.py` split — SWE-rebench-
style pairs mined from post-training-cutoff merged PRs, analyzed under
identical estimator/gates/CI machinery as the main split): the fresh split
cannot be contaminated by pretraining memorization by construction (the
underlying commits postdate the model's training cutoff). It is **never
pooled** with the sealed split (§7 already forbids pooling non-identical
cells; this is the same rule applied to a different confound). Concordance is
reported as: same sign, overlapping bootstrap CIs, comparable rough
magnitude. Agreement between the two splits is evidence the sealed-split
result is not a contamination artifact; disagreement is reported as-is, not
explained away by appeal to the fresh split's smaller n.

**Age-vs-effect probe** (diagnostic, exploratory, not gated on): using each
pair's already-recorded `created_at` metadata, report whether older pairs
(more likely to fall inside a pretraining window) show systematically smaller
effects than newer sealed pairs. A negative age–effect relationship is
consistent with the contamination-toward-null argument above and is reported
alongside the headline; a flat or positive relationship is reported too — this
probe does not get to selectively appear only when it supports the headline.

## 11. Power rule [DATED AMENDMENT — 2026-08-19]

**Added, not a rewrite of §§1–8.** Plan item 0.6 (frozen here per plan item
0.7's instruction to record the rule text in this document; `power_analysis.py`
itself is implementation, out of this document's scope).

**The rule:** n is the smallest sample size giving **80% power to detect a
15% reduction** in `locate_calls_pre_edit` (`full_brain` vs `no_brain`, on
the primary paired-log-ratio estimator, §3/§8), estimated from the **pilot's**
per-pair log-ratio variance (Phase 2 of the runbook, 10 dev pairs), and
**capped by sealed supply** — the rule can ask for more pairs than the seal
contains, but can never ask for fewer than the seal's `sealed_min` (30, per
`config.json`).

**Formula** (paired two-sided test, standard normal approximation):

```
n = ceil( (z_{1-α/2} + z_{1-β})^2 * σ^2 / δ^2 )
δ = |ln(1 − 0.15)| = |ln(0.85)| ≈ 0.1625     (target effect, in log-ratio units)
α = 0.05  ->  z_{0.975} ≈ 1.960
power = 0.80  ->  z_{0.80} ≈ 0.8416
σ = the pilot's sample standard deviation of per-pair log-ratios
```

**What this section freezes now, and what it does not.** It freezes the
formula, the target effect (15%), and the power (80%) *before* pilot data
exists. It does **not** compute n — no pilot variance is available at the
time of this amendment. The computed n, once pilot variance is measured, is
itself a **separate, later dated amendment appended to §9**, frozen before
Phase 3 (confirmatory) spend begins — never computed and then silently acted
on without a dated record. If the computed n exceeds the sealed supply, the
confirmatory run proceeds at the sealed n and the **actual** achieved power
(necessarily below 80%) is reported next to the headline, consistent with
§7's existing rule that an underpowered result is reported as inconclusive,
never reframed as "no effect."

## 12. Validity gate [DATED AMENDMENT — 2026-08-19]

**Added, not a rewrite of §§1–8.** Plan item 0.8 (construct validity). Full
protocol: `validity/LABEL_PROTOCOL.md`; analysis: `validity/analyze_validity.py`.

A stratified, blind-to-arm sample of **~30 pilot B sessions** is human-labeled
(two independent raters, same independence discipline as `SEAL_PROTOCOL.md`'s
dual review) for whether each pre-edit LOCATE call re-derives knowledge
session A's transcript already established. **Spearman's rho** is computed
between the machine `locate_calls_pre_edit` count and the labeled
re-derivation count, per session, using the primary rater (both raters'
values are reported; the primary rater is fixed as the alphabetically-first
rater id, decided by this amendment rather than chosen post hoc from whichever
rater's number looks better).

**THE GATE:** if `rho < 0.5`, construct validity for the primary metric is
insufficiently established, and a **dated amendment revising or supplementing
the primary metric's definition** (`mechmetrics.py`'s `locate_calls_pre_edit`)
**must be written and frozen before Phase 3 (confirmatory) begins.** This is
the last point at which the metric can be revised without the revision being
informed by confirmatory outcomes — a validity check run after confirmatory
data exists cannot license a metric change without contaminating the result
with outcome-dependent reasoning, so §6/§7's discipline (harness bugs get
fixed and re-run; the *task set* is never edited after seeing results) extends
here to: the *metric* is never edited after seeing confirmatory results either.

If `rho >= 0.5`: reported as supporting evidence for construct validity in
`DATASHEET.md` and the paper, and the confirmatory run proceeds without a
metric amendment. The rho value, its sample size, and per-rater kappa are
reported in both cases — a validity check that clears the gate is not exempt
from being published just because it didn't force a change.
