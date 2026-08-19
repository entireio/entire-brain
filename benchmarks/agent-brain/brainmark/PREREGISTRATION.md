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

_(none yet)_
