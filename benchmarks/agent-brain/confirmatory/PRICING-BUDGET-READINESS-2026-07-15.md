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
   `design.approved_for_budgeting=true`. The sensitivity-only `118 x 4` and `295 x 1` rows are not
   approved designs and must not be copied into the budget contract without that decision.
2. **Pin the exact execution identity.** Record provider, runner ID, immutable runner version, exact
   model ID or snapshot, and effort setting. If the runner calls an omitted effort setting
   "default", record the literal resolved value or an explicit `default` identifier; do not leave it
   null.
3. **Attach a pricing quote.** Save a byte-stable quote snapshot in a repo-relative evidence path and
   record its SHA-256, authoritative source URI or contract reference, quote as-of time, retrieval
   time, expiry time, and a maximum-age policy. Normalize all prices to USD per 1,000,000 tokens and
   separately record uncached-input, cached-input, and output prices as exact decimal strings. JSON
   floating-point numbers and unverified web-page recollections are rejected.
4. **Choose the token-envelope policy.** Either:
   - enter explicit per-call caps for uncached input, cached input, and output with a written
     rationale; or
   - attach byte-hashed empirical evidence collected under the exact pinned runner/model/effort,
     name the statistic and quantile, and choose an explicit safety multiplier of at least 1.

   The empirical path deterministically rounds each observed category upward after applying the
   multiplier. Legacy heterogeneous outcomes are not silently accepted as final-run evidence.
5. **Recompute the maximum.** Run the command below and copy its complete JSON output into the
   contract's `calculation` field; do not hand-round it.

   ```sh
   python3 benchmarks/agent-brain/confirmatory/pricing_budget.py \
     benchmarks/agent-brain/confirmatory/pricing-budget.json
   ```

   The calculation prices every requested and reserve call at the frozen per-call envelope:

   `maximum_calls_with_reserve * (uncached_input*input_price + cached_input*cached_price + output*output_price) / 1,000,000`

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

No pricing lookup, model selection, purchase, fresh-holdout access, or paid agent call was performed
to create this contract.
