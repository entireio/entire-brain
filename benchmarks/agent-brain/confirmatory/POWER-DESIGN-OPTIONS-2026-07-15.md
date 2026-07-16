# Confirmatory co-primary power design — updated 2026-07-16

## Decision

**Power is pending and the protocol remains no-go.** The v3 machine artifact supports the three
co-primary estimands—end-to-end elapsed time, normalized billed cost, and task-normalized code
quality—but intentionally reports no numeric power. No retained dataset supplies exchangeable
paired task-level variance for all three endpoints under the final runner, v2 treatment, price,
timeout, and quality contracts. Consequently, neither a power-sized development task count nor a
power-sized confirmatory task count is currently defensible. The 24-task x 4-repetition design is a
provisional arithmetic placeholder, not a powered design.

The provisional claim/null floors are a 10% elapsed-time reduction (ratio at most `0.90`), a 12%
normalized-cost reduction (ratio at most `0.88`), and a `+0.05` quality-score difference. They are
not power-planning alternatives. All three true planning alternatives remain `null` and
`pending_owner_approval`; the methodology owner must freeze values strictly better than the claim
floors before numeric marginal or joint power can be computed.

## Required calibration

Before choosing task count or repetitions, collect an unpaid development-only calibration that
matches:

- the full frozen provider, runner ID/version, agent CLI ID/version, requested and resolved model,
  effort, schedule, and quote identity;
- `no_memory` and `retrieved_memory` v2 treatment contracts;
- the user-visible timing boundary and timeout rule;
- the five mutually exclusive billing categories, frozen quote aliases,
  inclusion/absence semantics, authenticated pre-provider structural zeros, and zero confirmatory
  retries under one priceable model row;
- the task-normalized quality rubric and critical-failure-zero policy.

The calibration must retain at least 12 independent task clusters and provide:

- the SD of paired task-level mean log elapsed-time ratios;
- paired task-arm mean-cost rows sufficient for the equal-task-weighted ratio-of-means bootstrap;
- the SD of paired task-level mean quality differences;
- joint covariance or retained task rows sufficient to simulate the intersection-union rule.

Twelve clusters is a minimum calibration floor, not a claim that 12 tasks are powered and not a
license to reduce the provisional 24-task design. Power planning must require every marginal
component and the overall intersection-union joint success probability to each reach at least 0.80
under a frozen dependence model or conservative simulation. Repetitions are averaged within each
task/arm before the paired contrast; they do not become independent clusters.

## Defensible counts and agent-invocation ceiling

Without final-contract calibration or an explicitly owner-approved assumption set, the defensible
power-sized task counts are `null` for both development and confirmation. The only numeric
development statement is the 12-cluster minimum needed to attempt calibration; the resulting
variance and dependence estimates must determine whether more calibration tasks are needed before
any design can be sized.

The current placeholder arithmetic is 24 tasks x 3 treatments x 4 repetitions = 288 requested
cells. Agent retries, replacement-cell attempts, and reserve-cell attempts are frozen at zero, so
the maximum agent-CLI invocation/cell-attempt envelope is 288. Authenticated pre-provider
structural-zero cells consume a requested cell but do not enter the provider path. The former 10%
reserve and its 317-attempt ceiling are invalid under the current contract.

## Why legacy outcomes cannot fill the gap

`power-calibration-exploratory-v1.json` authenticates 14 paired cluster instances over 12 unique
legacy tasks, but those records use selected tasks, legacy treatments, non-exchangeable runners,
dirty harnesses, and do not contain the v2 time/cost/quality contract. They remain useful variability
diagnostics and are programmatically prohibited from passing the power gate or shrinking the design.
Legacy two-endpoint sensitivity counts such as 118 tasks x 4 repetitions or 295 tasks x 1 repetition
do not cover the v3 time/cost/quality joint endpoint and cannot be promoted into either budget or
confirmation.

## States that must remain closed

Until calibration and explicit floor approval are complete:

- `power.completed = false` and `power_target_met = fail`;
- all claim-floor and timeout statuses remain `provisional`, and all planning alternatives remain
  null and unapproved;
- the fresh holdout remains unopened and unsealed;
- no paid confirmatory calls or budget authorization may begin.

Reproduce the machine artifact with:

```sh
python3 benchmarks/agent-brain/confirmatory/power_analysis.py \
  --check benchmarks/agent-brain/confirmatory/power-analysis.json
```
