# Confirmatory co-primary power design — updated 2026-07-16

## Decision

**Power is pending and the protocol remains no-go.** The v3 machine artifact supports the three
co-primary estimands—end-to-end elapsed time, normalized billed cost, and task-normalized code
quality—but intentionally reports no numeric power. No retained dataset supplies exchangeable
paired task-level variance for all three endpoints under the final runner, v2 treatment, price,
timeout, and quality contracts.

The provisional practical floors are a 10% elapsed-time reduction (ratio at most `0.90`), a 12%
normalized-cost reduction (ratio at most `0.88`), and a `+0.05` quality-score difference. They are
planning inputs, not approved claim thresholds. `check_protocol.py --freeze` rejects them until a
methodology owner explicitly freezes all three together.

## Required calibration

Before choosing task count or repetitions, collect an unpaid development-only calibration that
matches:

- the final runner, model, effort, and runner version;
- `no_memory` and `retrieved_memory` v2 treatment contracts;
- the user-visible timing boundary and timeout rule;
- the five mutually exclusive billing categories, frozen quote aliases,
  inclusion/absence semantics, and all-attempt retry accounting under one priceable model row;
- the task-normalized quality rubric and critical-failure-zero policy.

The calibration must retain at least 12 independent task clusters and provide:

- the SD of paired task-level mean log elapsed-time ratios;
- the SD of paired task-level mean log normalized-cost ratios;
- the SD of paired task-level mean quality differences;
- joint covariance or retained task rows sufficient to simulate the intersection-union rule.

Power planning must require each component to reach at least 0.80 and must assess joint success
under a frozen dependence model or conservative simulation. Repetitions are averaged within each
task/arm before the paired contrast; they do not become independent clusters.

## Why legacy outcomes cannot fill the gap

`power-calibration-exploratory-v1.json` authenticates 14 paired cluster instances over 12 unique
legacy tasks, but those records use selected tasks, legacy treatments, non-exchangeable runners,
dirty harnesses, and do not contain the v2 time/cost/quality contract. They remain useful variability
diagnostics and are programmatically prohibited from passing the power gate or shrinking the design.

## States that must remain closed

Until calibration and explicit floor approval are complete:

- `power.completed = false` and `power_target_met = fail`;
- all practical-floor and timeout statuses remain `provisional`;
- the fresh holdout remains unopened and unsealed;
- no paid confirmatory calls or budget authorization may begin.

Reproduce the machine artifact with:

```sh
python3 benchmarks/agent-brain/confirmatory/power_analysis.py \
  --check benchmarks/agent-brain/confirmatory/power-analysis.json
```
