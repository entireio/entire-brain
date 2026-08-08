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
and complete analysis plan have been locked. The plan binds both contrasts'
floors and planning alternatives, the exact resampling method, seed, resample
count, exhaustive candidate grid, repetitions, task population and receipt
bytes, execution contract, and v4 implementation lock. It also binds the
outcome-free product preplan: both product identities, task inventory, shared
execution identity, exact schedule/design, and zero-retry cell ceiling. Cell
outcomes are deliberately excluded from the preplan and the completed raw
product-cycle manifest is bound separately after collection.

The repository currently has no configured signing or authentication trust
root. Candidate-lock and owner-approval receipts are therefore structurally
validated but explicitly carry
`unauthenticated_no_trust_anchor_fail_closed`. They preserve an auditable
immutable plan, but cannot make a statistical result decision eligible.

Power v4 loads and validates the raw task-population contract and raw review
ledger with the full task-population v2 validator. It also loads the raw
selection, assignment, and overlap-verification receipts, recomputes their
raw hashes, checks their verification subjects, and derives the calibration
projection itself. Every receipt uses a domain-separated projection of the
complete population, binding the selected member universe, exact
member-to-split assignments, overlap commitments and relation edges, summary,
and review-ledger identity. Only the population self-hash and three raw
receipt-hash slots are nulled to avoid a cryptographic cycle. Embedded
projections are never accepted on assertion.

Independence is the transitive closure of family, fix, source-session, and
material related-task edges over the entire validated population, including
paths through excluded members. The active calibration set must contain at
least 12 reviewed-clear members and, for unambiguous future cell sizing, each
must occupy its own derived cluster. Product task hashes must also be unique.
Task IDs must match the raw product-cycle inventory exactly. Optimization and
holdout members remain present only in the upstream population contract; they
are never projected into or used by calibration.

The input binds the bytes and identities of both upstream contracts. In
particular, it records:

- the raw product-cycle evidence and v1 schema SHA-256;
- the canonical product-cycle evidence SHA-256;
- the raw task-population v2 schema SHA-256;
- the raw task-population contract file SHA-256;
- the task-population contract identity; and
- the raw review ledger and its schema SHA-256;
- every raw population/candidate/owner receipt SHA-256; and
- the canonical task-population projection SHA-256.

The currently pinned raw schema hashes are:

| Contract | SHA-256 |
| --- | --- |
| product-cycle v1 | `3e029222e76091740bb1e218a7b1e23fe464f98aa2c83797d3fe71ad0b97c288` |
| task-population v2 | `2846906e0caa450e6c4dbc206648346ababbb91fed4675c0c7eca03cf506a8b9` |

All eight schemas plus the power-v4 analyzer, offline Draft 2020-12 validator,
product-cycle validator, and task-population validator are covered by a
dedicated self-hashed v4 implementation lock. A byte change cannot silently
flow into an existing calibration or report. The locked v3 analyzer contract
is not modified.

## Locked execution identity

Every calibration is bound to the product-cycle contract, task-population
contract, candidate product and packet format, corpus, retrieval engine,
prompt template and prompt-parity algorithm, cache policy, runner, provider,
agent CLI and version, requested and resolved model IDs, effort, timeout
policy and limits, schedule, price quote, and pricing policy. One self-hashed
execution attestation is required for every product cell. The validator
checks exact provider/CLI/model/runner/effort/timeout parity against the
execution contract and the cell's execution and timing identities.

This means the calibration results cannot be relabeled after collection as
evidence for another runner, model, effort level, prompt, engine, cache policy,
schedule, or pricing basis.

## Exact four-arm design

The arm vocabulary and order are immutable:

1. `no_memory`
2. `placebo_packet`
3. `preoptimization_memory`
4. `retrieved_memory`

For `N` derived singleton task clusters and `R` repetitions per arm, the requested-cell and
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

This is not the geometric mean of per-task cost ratios. Structural-zero
treatment costs are valid, and no cost log is taken. The observed aggregate
denominator must be positive, but an individual task or a bootstrap draw may
have a zero denominator. Such a draw has undefined cost, automatically fails
the cost and joint gates, and is excluded from the conditional cost-CI
quantile. Its count and the exact policy
`automatic_cost_and_joint_failure_excluded_from_ci_quantile_v1` are recorded.
The report also records
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
stream deterministically produces derived-cluster index draws from the bound
seed, source cluster count, candidate cluster count, and resample count.

One index draw is reused across:

- elapsed time, normalized cost, and code quality; and
- the primary and product-diagnostic contrasts.

This preserves observed endpoint and contrast dependence. Repetitions are
already averaged inside task/arm, so the resampling unit is the validated
derived independence cluster. The current sizing contract requires one active
calibration task per cluster. The joint power estimate is the frequency with which all three
one-sided endpoint bounds pass in the same draw. Marginal powers are not
multiplied, and no parametric endpoint-independence shortcut is permitted.

All observed calibration estimates, confidence-bound inputs, marginal power
values, and joint power values must be finite. The only permitted bootstrap
null is the explicitly handled zero-denominator cost draw described above.

For `development_measurement`, the complete plan is fixed before opening to
exactly 10,000 resamples, seed `0x4542563453454544`, and candidate cluster grid
`[12, 16, 20, 24, 32, 40, 48, 64, 80, 96, 128, 160, 192, 256, 320, 384, 512]`.
Synthetic tests may use a smaller explicitly bound plan.

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
| `pending` | required and already locked | must be null | no |
| `candidate` | required | evaluated | no |
| `frozen` + synthetic fixture | invalid | invalid | no |
| `frozen` + development measurement + structural owner receipt | required | evaluated | no (no trust anchor) |

Thus a candidate result can be inspected but cannot pass the decision gate.
Synthetic fixtures may exercise and even clear the statistical logic, but
they are never decision eligible. Under the current no-trust-anchor contract,
even frozen development evidence remains nonpassing with reason
`no_authenticated_owner_approval_trust_anchor`. A future contract revision
must add and validate a real trust root before decision eligibility is
possible. The product-improvement diagnostic always carries
`alters_benchmark_primary_verdict: false`.

## Workflow

1. Produce the reviewed task-population v2 contract, raw review ledger, and
   raw selection/assignment/overlap receipts.
2. Derive at least 12 mutually independent calibration tasks from the complete
   relation closure; do not use optimization or holdout evidence.
3. Fix the endpoint floors, planning alternatives, production seed, exact
   resample count/grid, repetitions, execution contract, and implementation
   bytes before any calibration outcome is opened.
4. Lock the candidate product identity and complete-plan hash in the raw
   candidate receipt. With no trust root this is structural, not authenticated.
5. Collect the exact four-arm product-cycle v1 grid under the locked execution
   identity and per-cell attestations.
6. Use `pending` as the withheld-evaluation state; the complete plan remains
   present but power rows remain null. Move to `candidate` to inspect results.
7. A frozen development artifact additionally requires the raw structural
   owner receipt. It remains nonpassing until a future trust-anchor contract
   exists. Retain the self-hashed calibration and report; neither authorizes
   or executes a future benchmark.

## Commands

Both commands are local validation/analysis operations. Neither command
contacts a provider or model.

```bash
ARTIFACTS=(
  --product-cycle path/to/product-cycle-v1.json
  --task-population path/to/task-population-v2.json
  --review-ledger path/to/task-review-ledger-v2.json
  --selection-receipt path/to/selection-receipt-v1.json
  --assignment-receipt path/to/assignment-receipt-v1.json
  --overlap-receipt path/to/overlap-receipt-v1.json
  --candidate-lock-receipt path/to/candidate-lock-receipt-v1.json
)

python3 benchmarks/agent-brain/confirmatory/power_analysis_v4.py \
  preflight path/to/final-calibration-v1.json "${ARTIFACTS[@]}"

python3 benchmarks/agent-brain/confirmatory/power_analysis_v4.py \
  analyze path/to/final-calibration-v1.json "${ARTIFACTS[@]}"

python3 benchmarks/agent-brain/confirmatory/power_analysis_v4.py \
  check path/to/final-calibration-v1.json path/to/power-analysis-v4.json \
  "${ARTIFACTS[@]}"
```

For a frozen artifact, append
`--owner-approval-receipt path/to/owner-approval-receipt-v1.json`.

`check` validates the report and then deterministically recomputes it from the
bound calibration, requiring exact equality. A report whose self-hash and
internal booleans are consistent but whose numeric power values were edited
still fails this check.

Run the synthetic regression suite with:

```bash
python3 -m unittest \
  benchmarks/agent-brain/confirmatory/test_power_analysis_v4_hardening.py -v
```

The suite uses raw 12-task, 96-cell synthetic fixtures. It covers actual Draft
2020-12 validation, the full task-population/review path, receipt subjects and
raw hashes, complete pre-open planning, relation closure (including excluded
bridges), duplicate task hashes, execution parity, dedicated implementation
pinning, deterministic shared resampling, marginal/direct-joint gates, all
three estimands, zero numerator and zero denominator behavior, and no-trust
fail-closed decisions. It does not run paid benchmarks or access private or
holdout data.
