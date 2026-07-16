# Pricing and paid-budget readiness

Status: **contract implemented; human inputs pending; neither paid-run gate passes**.

`pricing-budget.json` is a provider-neutral, fail-closed contract. It deliberately contains no model
choice, provider price, token envelope, computed dollar maximum, or approval. The current 24-task x
4-repetition design is mirrored only to make later design drift detectable. Its
`approved_for_budgeting` field is false because the power gate is failed.

## Human inputs required

The following inputs must be supplied in order. A null, stale, partial, or unapproved value keeps the
corresponding gate pending.

1. **Approve a powered design.** A methodology owner must choose the final task count and
   repetitions, update `preregistration.json`, pass the power gate, and set
   `design.approved_for_budgeting=true`. The v3 artifact currently supports no numeric power-sized
   task count. Legacy `118 x 4` and `295 x 1` rows cover an obsolete two-endpoint sensitivity model,
   not the current time/cost/quality joint endpoint, and must not be copied into the budget contract.
2. **Pin the exact execution identity.** Record provider, runner ID and immutable runner version,
   agent/CLI ID and version, requested and resolved model IDs or snapshots, effort setting, and the
   frozen schedule hash. If the runner calls an omitted effort setting "default", record the literal
   resolved value or an explicit `default` identifier; do not leave it null. Self-hash this identity;
   every observed invocation and authenticated structural-zero cell must match it and the quote.
3. **Attach a pricing quote.** Save a byte-stable quote snapshot in a repo-relative evidence path and
   record its SHA-256, authoritative source URI or contract reference, quote as-of time, retrieval
   time, expiry time, and a maximum-age policy. Normalize all prices to USD per 1,000,000 tokens and
   separately record uncached-input, cache-read-input, cache-write-input, visible-output, and
   reasoning-output prices (or explicit aliases) as exact decimal strings. JSON
   floating-point numbers and unverified web-page recollections are rejected. Freeze the provider
   counter contract at the same time: which cache counters are included in input, whether reasoning
   is included in output, and—for cache-read, cache-write, and reasoning separately—whether an
   absent counter is authoritatively zero. `false` means the counter must be present. A generic total
   cannot supply a category. The pinned runner must emit exactly one terminal result and one
   priceable actual model row; multi-result or multi-model Claude invocations are ineligible until a
   separate frozen accounting treatment exists.
4. **Choose the token-envelope policy.** Either:
   - enter explicit per-invocation caps for all five billed categories with a written
     rationale; or
   - attach byte-hashed empirical evidence collected under the exact pinned execution identity,
     name the statistic and quantile, and choose an explicit safety multiplier of at least 1.

   The empirical path deterministically rounds each observed category upward after applying the
   multiplier. Legacy heterogeneous outcomes are not silently accepted as final-run evidence.
5. **Recompute the maximum.** Run the command below and copy its complete JSON output into the
   contract's `calculation` field; do not hand-round it.

   ```sh
   python3 benchmarks/agent-brain/confirmatory/pricing_budget.py \
     benchmarks/agent-brain/confirmatory/pricing-budget.json
   ```

   The calculation prices every requested agent invocation at the frozen per-invocation envelope;
   retries, replacement-cell attempts, and reserve-cell attempts are frozen at zero:

   `maximum_agent_invocations * sum(each mutually exclusive billed category * its frozen direct or aliased price) / 1,000,000`

6. **Approve the cap.** A budget owner must record an approved USD cap at least as large as the
   computed maximum, stable approver identity, approver role, approval timestamp, and approval
   expiry. Approval cannot predate quote retrieval or outlive the quote.
7. **Close the gates explicitly.** Only after the quote is current and complete may
   `model_runner_price_pinned` reference `pricing-budget.json` and become `pass`. Only after the
   powered design, calculation, and human approval are valid may `paid_budget_cap_approved` reference
   the same artifact and become `pass`. Mirror the approved cap into
   `preregistration.json#paid_budget.maximum_usd` and set its status to `approved`.

## Machine enforcement

`check_protocol.py` validates the artifact and its source hashes during ordinary preparation. Final
freeze additionally remains blocked by the go/no-go checklist. It rejects a pass claim when any of
these bindings is missing, when design arithmetic or preregistration fields drift, when an empirical
sample names another runner, when quote or approval validity has expired, when the calculation is
stale, or when approval is below the computed maximum.

At execution time, a provider-entered cell retains its one invocation's output and normalized
billing record, selecting the final cumulative snapshot exactly once. Any entered or ambiguous
provider path with missing usage invalidates the suite; it is never treated as free. A pre-provider
treatment failure is zero cost only when content-addressed harness evidence attests that `run_agent`
was never entered and binds explicit all-zero categories to the frozen quote.

No pricing lookup, model selection, purchase, fresh-holdout access, or paid agent call was performed
to create this contract.
