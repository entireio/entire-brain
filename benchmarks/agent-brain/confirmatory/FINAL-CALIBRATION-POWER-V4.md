# Final development calibration and power analysis v4

## Scope

Power v4 is a standalone development lane for sizing the next four-arm
agent-brain benchmark. It consumes completed product-cycle v1 evidence and a
hash-bound projection of task-population v2. It does not revise, reinterpret,
or replace the locked three-arm preregistration or power-v3 artifacts.

The lane is offline and non-authorizing:

- it makes no provider or model calls;
- it authorizes no budget;
- it accepts no optimization or confirmatory-holdout members;
- it exposes no holdout plaintext; and
- it cannot emit a benchmark outcome verdict.

The implementation is
[`power_analysis_v4.py`](power_analysis_v4.py). Its input and output contracts
are
[`final-calibration-v1.schema.json`](schemas/final-calibration-v1.schema.json)
and
[`power-analysis-v4.schema.json`](schemas/power-analysis-v4.schema.json).

## Preconditions and evidence boundary

Development calibration may begin only after the candidate product identity
has been locked. The calibration records a self-hashed candidate-lock receipt
that binds the candidate identity, the task-population contract, and an
outcome-free pre-calibration plan. That plan hashes both product identities,
the task inventory, shared execution identity, exact schedule/design, and
zero-retry cell ceiling. It deliberately excludes cell outcomes, so its hash
can exist before calibration evidence is opened. The completed product-cycle
manifest is bound separately after collection.

The task-population projection must contain at least 12 independent, active,
reviewed-clear tasks. Every member must have membership
`development_calibration`. The projection rejects optimization members,
confirmatory-holdout members, duplicate task identities, duplicate member
references, and duplicate task-overlap commitments. Its task IDs must match
the product-cycle task inventory exactly. Verified task-selection,
split-assignment, and overlap-commitment statuses must each carry their
non-placeholder task-population v2 receipt hash.

The input binds the bytes and identities of both upstream contracts. In
particular, it records:

- the raw product-cycle v1 schema SHA-256;
- the canonical product-cycle evidence SHA-256;
- the raw task-population v2 schema SHA-256;
- the raw task-population contract file SHA-256;
- the task-population contract identity; and
- the canonical task-population projection SHA-256.

The currently pinned raw schema hashes are:

| Contract | SHA-256 |
| --- | --- |
| product-cycle v1 | `3e029222e76091740bb1e218a7b1e23fe464f98aa2c83797d3fe71ad0b97c288` |
| task-population v2 | `2846906e0caa450e6c4dbc206648346ababbb91fed4675c0c7eca03cf506a8b9` |

A byte change to either schema requires an explicit contract revision; it
cannot silently flow into power v4.

## Locked execution identity

Every calibration is bound to the product-cycle contract, task-population
contract, candidate product and packet format, corpus, retrieval engine,
prompt template and prompt-parity algorithm, cache policy, runner, model ID,
effort, schedule, price quote, and pricing policy. The validator compares each
field with the completed product-cycle evidence and fails closed on drift.

This means the calibration results cannot be relabeled after collection as
evidence for another runner, model, effort level, prompt, engine, cache policy,
schedule, or pricing basis.

## Exact four-arm design

The arm vocabulary and order are immutable:

1. `no_memory`
2. `placebo_packet`
3. `preoptimization_memory`
4. `retrieved_memory`

For `N` independent tasks and `R` repetitions per arm, the requested-cell and
maximum-agent-invocation count is exactly:

```text
4 * N * R
```

The explicit product-cycle schedule must contain every task/repetition/arm
cell exactly once. Agent retries, replacement cells, and reserve cells remain
zero. Repetitions are never treated as independent task clusters.

## Contrasts and estimands

Power v4 keeps two contrasts separate:

| Contrast | Role | May affect the primary benchmark power decision? |
| --- | --- | --- |
| `retrieved_memory` vs `no_memory` | primary development contrast | yes |
| `retrieved_memory` vs `preoptimization_memory` | product-progress diagnostic | no |

Let `Y[t,a,r]` be one endpoint for task `t`, arm `a`, and repetition `r`.
Power v4 first forms the arithmetic mean inside each task/arm cell:

```text
Ybar[t,a] = arithmetic_mean_r(Y[t,a,r])
```

Only then does it form a task-level contrast. The three endpoint estimands
are intentionally different.

### Elapsed time

Elapsed time is the paired-task geometric mean ratio:

```text
exp(arithmetic_mean_t(log(Tbar[t,retrieved] / Tbar[t,denominator])))
```

Elapsed values must be positive.

### Normalized cost

Normalized cost is the ratio of the two equal-task-weighted arithmetic arm
means:

```text
arithmetic_mean_t(Cbar[t,retrieved])
------------------------------------------------
arithmetic_mean_t(Cbar[t,denominator])
```

This is not the geometric mean of per-task cost ratios. Authenticated
structural-zero treatment costs are valid; the aggregate and resampled
denominator must remain positive. The report explicitly records
`geometric_mean_task_ratios_used: false` and
`structural_zero_treatment_cost_supported: true`.

### Code quality

Code quality is the paired-task arithmetic mean difference:

```text
arithmetic_mean_t(Qbar[t,retrieved] - Qbar[t,denominator])
```

## Deterministic shared cluster resampling

The method is
`paired_cluster_residual_bootstrap_shared_draws_v1`. A SHA-256-domain-separated
stream deterministically produces task-cluster index draws from the bound
seed, source task count, candidate task count, and resample count.

One index draw is reused across:

- elapsed time, normalized cost, and code quality; and
- the primary and product-diagnostic contrasts.

This preserves observed endpoint and contrast dependence. Repetitions are
already averaged inside task/arm, so the resampling unit is the independent
task. The joint power estimate is the frequency with which all three
one-sided endpoint bounds pass in the same draw. Marginal powers are not
multiplied, and no parametric endpoint-independence shortcut is permitted.

All calibration estimates, confidence-bound inputs, marginal power values,
and joint power values must be finite and non-null before an evaluated row can
pass validation.

## Power and decision gates

For each candidate task count, both contrasts receive three marginal power
estimates and one direct all-endpoint joint estimate. A statistical gate is
true only when all four values are at least `0.80`:

```text
min(power_time, power_cost, power_quality, power_joint) >= 0.80
```

The first increasing candidate task count that clears the full gate is the
selected count. A marginal result cannot compensate for joint power below
0.80, and joint power cannot compensate for any marginal below 0.80.

The calibration state further constrains the decision:

| State/evidence | Alternatives | Power results | Decision eligible |
| --- | --- | --- | --- |
| `pending` | must be null | must be null | no |
| `candidate` | required | evaluated | no |
| `frozen` + synthetic fixture | required | evaluated | no |
| `frozen` + development measurement + owner approval | required | evaluated | yes |

Thus a candidate result can be inspected before approval but cannot pass the
decision gate. Synthetic fixtures may exercise and even clear the statistical
logic, but they are never decision eligible. Only owner-approved, frozen,
development-measurement evidence can pass. The product-improvement diagnostic
always carries `alters_benchmark_primary_verdict: false`.

## Workflow

1. Lock the candidate product identity and record the lock receipt before
   development calibration.
2. Produce the reviewed task-population v2 contract and its calibration-only
   projection with at least 12 independent active tasks.
3. Collect the exact four-arm product-cycle v1 grid under one locked execution
   identity. Do not use optimization or holdout tasks.
4. Assemble a `pending` final-calibration v1 artifact with null planning
   alternatives, then run preflight.
5. Derive and review the final planning alternatives using development
   calibration evidence only. Move the artifact to `candidate` and inspect the
   evaluated power report.
6. If the contract and alternatives are accepted, record owner approval,
   freeze the task-population projection unopened, and move the artifact to
   `frozen`.
7. Run analysis again and retain both the self-hashed calibration and
   self-hashed report. A passing power decision only sizes a future run; it
   does not authorize or execute that run.

## Commands

Both commands are local validation/analysis operations. Neither command
contacts a provider or model.

```bash
python3 benchmarks/agent-brain/confirmatory/power_analysis_v4.py \
  preflight path/to/final-calibration-v1.json

python3 benchmarks/agent-brain/confirmatory/power_analysis_v4.py \
  analyze path/to/final-calibration-v1.json

python3 benchmarks/agent-brain/confirmatory/power_analysis_v4.py \
  check path/to/final-calibration-v1.json path/to/power-analysis-v4.json
```

`check` validates the report and then deterministically recomputes it from the
bound calibration, requiring exact equality. A report whose self-hash and
internal booleans are consistent but whose numeric power values were edited
still fails this check.

Run the synthetic regression suite with:

```bash
python3 -m unittest \
  benchmarks/agent-brain/confirmatory/test_power_analysis_v4.py -v
```

The suite uses 12-task, 96-cell synthetic fixtures. It covers the pending,
candidate, frozen, and synthetic eligibility boundaries; exact four-arm
arithmetic; deterministic shared resampling; marginal and direct-joint gates;
all three estimands; structural-zero cost; upstream byte binding; execution
identity drift; and exclusion of optimization and holdout evidence. It does
not run paid benchmarks or access private/holdout data.
