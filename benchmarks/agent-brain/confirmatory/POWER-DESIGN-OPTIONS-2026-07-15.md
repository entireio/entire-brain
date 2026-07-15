# Confirmatory power redesign options — 2026-07-15

## Decision

**The power gate remains failed and the protocol remains no-go.** The checked-in v2 analysis adds a
task/repetition tradeoff table and byte-verifiable diagnostics from retained exploratory runs, but it
does not turn those runs into confirmatory calibration. No proposed design is approved. Every design
row below is sensitivity-only until a methodology owner chooses a defensible assumption source.

The smallest defensible next decision is therefore not a task count. Keep the fresh holdout unopened
and choose one of these assumption policies before resizing or budgeting the confirmatory run:

1. Explicitly accept the declared conservative assumptions and their call envelope as a policy
   decision, without describing them as empirically calibrated; or
2. Preregister and budget a separate development-only calibration using the final runner and frozen
   `no_memory`, `placebo_packet`, and `retrieved_memory` contracts. Those development cells must not
   be reused as confirmatory cells.

The second path is recommended because the retained outcomes are too sparse and non-exchangeable to
support either a smaller design or the current 24 x 4 design. This document does not authorize that
future paid calibration.

## Confirmatory decision assumptions

The power decision remains independent of all benchmark outcomes. Its conservative scenario assumes:

- token endpoint: between-task log-ratio SD `0.25`, within-attempt log-token SD `0.30`, and zero
  cross-treatment residual correlation;
- correctness endpoint: pass probability `0.75` in both arms, within-arm attempt ICC `0.20`, and
  zero cross-treatment task-mean correlation;
- power target `0.80`, token ratio alternative `0.88`, correctness non-inferiority margin `-0.10`,
  and planning alpha `0.025` for the first Holm step.

These are hypothetical screening values, not estimates. Under them, the current 24-task x
4-repetition design has token power `0.370207` and correctness non-inferiority power `0.243511`.
Even infinitely many repetitions at 24 tasks cannot reach either target because the assumptions
retain irreducible task-level variance.

## Task/repetition tradeoffs

Each row gives the minimum task count needed for both marginal targets under the conservative
scenario. Cells include all three primary treatments. The reserve is `ceil(cells x 1.10)`.

| Repetitions per treatment | Tasks for token | Tasks for correctness NI | Tasks for both | Requested cells | Calls with reserve |
|---:|---:|---:|---:|---:|---:|
| 1 | 142 | 295 | 295 | 885 | 974 |
| 2 | 89 | 177 | 177 | 1,062 | 1,169 |
| 3 | 72 | 138 | 138 | 1,242 | 1,367 |
| 4 | 63 | 118 | 118 | 1,416 | 1,558 |
| 6 | 54 | 99 | 99 | 1,782 | 1,961 |
| 8 | 50 | 89 | 89 | 2,136 | 2,350 |
| 12 | 46 | 79 | 79 | 2,844 | 3,129 |

The arithmetic cell minimum in this listed grid is 295 tasks x 1 repetition = 885 cells. It is not
a recommendation: it exchanges replication for hundreds of independently mined and reviewed task
clusters, assumes the same hypothetical variance model, and has no approved task supply or budget.
At four repetitions, the conditional sensitivity result is 118 tasks and 1,416 cells; it must not be
silently promoted into the preregistration.

The inverse view makes the task/repetition asymmetry explicit:

| Fixed tasks | Minimum repetitions for token | Minimum repetitions for correctness NI / both | Requested cells for both |
|---:|---:|---:|---:|
| 24 | Not attainable | Not attainable | — |
| 48 | 9 | Not attainable | — |
| 64 | 4 | 46 | 8,832 |
| 80 | 3 | 12 | 2,880 |
| 96 | 2 | 7 | 2,016 |
| 118 | 2 | 4 | 1,416 |
| 128 | 2 | 4 | 1,536 |

Independent task clusters are more cell-efficient than repetitions under positive ICC and
irreducible task heterogeneity. Feasibility, task validity, and cost are separate gates; this table
does not decide them.

## Quarantined exploratory calibration

`power-calibration-exploratory-v1.json` hashes every analyzed raw file and hard-codes that the data
may describe variability risk but may not select assumptions, reduce the design, or pass the power
gate. The analyzer uses the shared executed-run predicate and reports each source separately. Pooling
is prohibited because treatments, runners, repositories, task selection, and harness state differ.

| Legacy source | Paired task clusters | Task log-ratio SD | Task pass-difference SD | Within-task/arm log-token residual SD | Why it cannot calibrate the confirmatory design |
|---|---:|---:|---:|---:|---|
| Semantic Layer B pilot | 7 | 0.812434 | 0.000000 | 0.000000 | One repetition; correctness saturated; incomplete pairs; mixed dirty provenance |
| Semantic Layer B proof | 2 | 0.381915 | 0.176777 | 0.452065 | Two selected tasks; legacy semantic treatment; dirty harness |
| Legacy full CLI compact | 3 | 0.398176 | 0.288675 | 0.354692 | Three selected tasks; different repository/treatment; dirty harness |
| Legacy full-brain replay | 2 | 1.030708 | 0.707107 | 0.413534 | Two outcome-conditioned proof tasks; legacy treatment; dirty harness |

There are 14 paired task-cluster instances but only 12 unique task IDs because two Layer B tasks
appear in both pilot and proof sources. The pilot's zero residual SD is structural—one observation
per task/arm—not evidence of zero run variance. Its zero correctness SD is saturation—all retained
paired attempts passed—not evidence that correctness is stable. The other sources have only two or
three task clusters, so their SDs are highly unstable.

The older `existing-winner-suites-exploratory.md` report was audited but excluded from numeric
calibration. It references 29 raw records at paths that are not retained in this checkout, so its
aggregate table cannot be independently reconstructed from those claimed inputs.

These observations are consistent with substantial heterogeneity; they do not establish its
confirmatory magnitude. In particular, neither a low observed value from a saturated source nor a
high value from an outcome-selected source is a defensible plug-in assumption.

## Required human decision and next artifact

Before any fresh holdout is sealed, the methodology owner must record one versioned choice:

- **Assumption-acceptance path:** identify the exact scenario, task/repetition design, maximum calls,
  and rationale for accepting an assumption-only design; or
- **Development-calibration path:** pin the runner/model, sampling frame, treatment/query contracts,
  calibration budget, variance estimators, conservative bound rule, and rule for converting bounds
  to a confirmatory task/repetition count.

Until then, retain all of the following states:

- `power_target_met = fail`;
- `design_decision_required = true`;
- `fresh_holdout.opened_at = null` and `holdout_opened = false`;
- no paid confirmatory authorization.

Reproduce the machine artifact with:

```sh
python3 benchmarks/agent-brain/confirmatory/power_analysis.py
python3 benchmarks/agent-brain/confirmatory/power_analysis.py \
  --check benchmarks/agent-brain/confirmatory/power-analysis.json
```
