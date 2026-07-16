# Candidate preregistration: engine matrix and fresh confirmatory holdout

Status: **joint-superiority methodology draft; floors, quote, and power calibration pending; not frozen; no paid run
authorized**. Drafted 2026-07-15 before mining or opening a fresh confirmatory holdout.

The confirmatory analyzer implementation and schemas are now frozen by the path-bound aggregate in
`analyzer-lock.json`. Freezing the analyzer does not freeze or authorize the wider protocol.

## Scope and split policy

The 46 legacy C0701 tasks are development or secondary-validation material only. Their prompts and
configs were available before methodology freeze, and several received retrieval probes or agent
runs. `4dd458656` was used in ranking/cap optimization and is development-only. A fresh confirmatory
set will be mined only after this methodology and the WS2-WS5 interfaces are frozen. Its task prompts,
hidden validation, solving fact IDs, temporal cutoffs, and split assignments will be committed by
content hash without exposing holdout labels to ranking work.

The offline relevance dataset now contains 14 development queries over 13 exposed legacy tasks: 13
product queries, one oracle upper-bound query, and 46 explicit judgments including hard topical
distractors. Twelve product queries over 12 unique tasks are answerable, closing the preregistered
answerable-product-task floor. `dev-product-b72a6e621` is the one corpus-closed product null: an
exhaustive retained review covers all 2,531 exact active+eligible facts with four hard topical
distractors, 2,527 irrelevant decisions, and zero positives. The retained
`dev-product-8828752a7` query has a genuine branch-oriented relevant alternative and remains
non-null. Every null requires one retained grade decision for every exact active+eligible fact
derived from the authenticated provenance membership, task cutoff, exclusions, and session dates.
Counts and hashes are derived rather than reviewer declarations, and the null-review ledger has a
separate hard-pinned contract. These labels do not pass the offline engine threshold, choose
K/aggregation, or substitute for the still-empty sealed holdout. The development query,
answerable-product-task, and corpus-closed-null floors are now met. The final dataset must contain at
least 12 sealed holdout queries over at least 12 unique untouched tasks. The fresh agent holdout target is 24
unique tasks. The pre-run design must achieve at least 80% planned power for every co-primary
endpoint—elapsed time, normalized billed cost, and code quality—and at least 80% power for their
overall intersection-union joint success event under a frozen dependence model before sealing. Task
count or repetitions must increase if that joint design is underpowered; neither may change after
the holdout is opened.

All 14 label decisions are retained as per-query/per-judgment manual review records in
`offline-relevance-review-ledger.json`; unavailable historical packet bytes are not claimed as label
evidence. `offline-relevance-source-membership.json` authenticates all 2,621 source facts (2,531
active), their provenance-session membership, and all 2,248 session dates. Its small reviewed
`relevance-source-contract.json` is pinned by
raw SHA-256 in `check_protocol.py`, independently of routinely updated preregistration artifact
hashes. Normal validation therefore rejects a fabricated fact or session date even if labels,
snapshot, dataset, ledger, and preregistration hashes are refreshed together.

## Retrieval matrix

Offline development evaluates exactly these primary arms with independent vector/cache namespaces:

1. `lexical_handrolled`: `--no-semantic`, `ENTIRE_BRAIN_FACTS_BM25=0`.
2. `model2vec_rrf`: bundled Model2Vec, semantic RRF, `ENTIRE_BRAIN_FACTS_BM25=0`.
3. `embeddinggemma_rrf`: explicit loopback embedder, pinned GGUF hash, semantic RRF,
   `ENTIRE_BRAIN_FACTS_BM25=0`.

BM25 is not silently mixed into these arms. Any `bm25_plus_<semantic>` run is an optional separate
factor and claim family. Environment absence is never evidence of an engine: every retained output
must satisfy `schemas/engine-verification.schema.json`, including effective engine, embedder identity,
dimension, namespace, corpus/vector hashes, semantic availability, and fallback status. A fallback or
missing engine field invalidates the cell.

The machine-readable preregistration binds the production expectations to the canonical
`benchmarks/agent-brain/confirmatory/engine-verification-pins.json` path and its raw SHA-256. Normal
preparation checks fail on a missing, redirected, symlinked, or byte-changed pin set. Because that
binding is inside `preregistration.json`, the final protocol content hash freezes the exact engine-pin
contract before any holdout is opened or paid run is authorized.

Development metrics are Recall@1/5/10, MRR, nDCG@10, packet precision@K, kind distribution,
duplicate/cluster occupancy, and temporal exclusions. K and packet aggregation are chosen only on
development data. The provisional selection threshold is Recall@5 >= 0.80 on answerable product
queries, null-query false-positive rate <= 0.10, temporal leakage count = 0, and no more than one item
from a near-duplicate cluster in the selected packet unless the preregistered aggregator explicitly
needs multiple members. The threshold must be frozen before the relevance holdout is opened.

## Agent treatments and estimands

Comparable prompts are treatment-neutral. Primary treatments are `no_memory`, `placebo_packet`, and
`retrieved_memory` using product (user-prompt-derived) queries. `oracle_retrieval` is an optional upper
bound, analyzed separately and excluded from the product-effect claim.

The v2 analysis hierarchy is fixed:

1. Execution and evidence integrity: every requested cell must be present, balanced, and verified.
   Once the causal treatment timer starts at the retrieval/no-op boundary, the attempt remains in
   every endpoint even if no model request is sent; there is no success-only efficiency analysis.
2. End-to-end user-visible elapsed time is co-primary. Its timer begins immediately before
   harness-owned packet retrieval/delivery (a no-op at the same logical point for `no_memory`) and
   stops at the agent CLI completion boundary captured immediately when the provider process returns,
   or at the observed treatment failure / timeout-termination boundary when no final response exists,
   before usage parsing, hashing, or artifact writes. Worktree/cache setup, secret preflight,
   hidden validation, and full-cell cleanup are separate diagnostics. The paired task geometric-mean ratio must have a
   one-sided 95% upper bound below the provisional 0.90 practical floor.
3. Normalized billed model cost is co-primary. Each run retains mutually exclusive uncached-input,
   cache-read-input, cache-write-input, visible-output, and reasoning-output counts. Inclusive
   provider totals are subtracted exactly once; no cache-write or reasoning usage may be omitted.
   Visible output is the provider output total after subtracting reasoning only when the provider
   declares reasoning included; reasoning output always comes from its own canonical counter.
   Every category is bound to a frozen direct price or explicit price alias, and the quote freezes
   whether input/output counters include subcategories plus whether an absent cache-read,
   cache-write, or reasoning counter is contractually zero. No category is inferred from a generic
   total-token number. Confirmatory provider retries are frozen at zero, so each provider-entered
   cell has exactly one retained invocation with its own usage and output artifacts. A pre-provider
   treatment failure is an authenticated structural zero only when harness control flow proves
   `run_agent` was never entered; all five categories are then explicitly zero and quote-bound.
   Missing or ambiguous usage after provider-path entry still invalidates the suite. The ratio of
   equal-task-weighted task-arm mean costs must have a one-sided 95% upper bound below the
   provisional 0.88 floor.
4. Task-normalized code quality is co-primary on [0,1]. The rubric normalizes only predeclared
   output criteria: hidden-validation/outcome points and patch-focus points (including task-specific
   validation already declared by the task). Agent process behavior such as running tests or checking
   a diff, plus time, tokens, and treatment-specific tool-use points, remains diagnostic and is
   excluded. Hidden-validation, forbidden-file, or integrity-critical failures force the attempt
   score to zero. The paired task mean difference must have a one-sided 95% lower bound above the
   provisional +0.05 floor.

The confirmatory verdict is an intersection-union test: all three component bounds must clear their
frozen practical floors. Retrieved memory versus no memory is the only claim-bearing contrast.
Placebo and product-baseline contrasts are diagnostic. Raw validation/pass rates, process-behavior
signals, raw token usage, harness agent-interval timing, and provider API duration remain visible
diagnostics but cannot gate, replace, or rescue a co-primary endpoint. A timed-out executed attempt
retains its measured monotonic end-to-end interval; an agent-component timeout limit is never
substituted for that total. A harness-attested early retrieval/delivery failure before provider-path
entry records known-zero billed model usage rather than missing usage. Missing billed usage after
provider-path entry, an ambiguous launch state, or missing quality fields invalidates the suite;
there is no inferred zero-cost or usage-imputation rule.

The frozen runner must expose one priceable actual model row per invocation. The current strict v2
adapter supports Codex's cumulative `turn.completed.usage` counters only when optional-counter
presence is stable across snapshots. A Claude invocation is eligible only when it contains exactly
one terminal `result`, whose `modelUsage` contains exactly one actual model matching the pinned model;
multiple terminal results or model rows are otherwise ineligible. Claude
does not generally expose hidden reasoning separately, so absent `reasoningTokens` is unknown unless
the frozen provider contract explicitly establishes absence as zero; the parser never assumes this.

The provisional target design is 24 fresh tasks x 3 primary treatments x 4 repetitions = 288
requested cells, counterbalanced by WS4. Agent retries, replacement-cell attempts, and reserve-cell
attempts are frozen at zero, so 288 is also the maximum agent-CLI invocation/cell-attempt envelope.
This design is mirrored
in `pricing-budget.json` for drift detection but is explicitly not approved for budgeting while the
power gate is unresolved. Dollar cost remains null until the final powered design, exact
provider, runner ID/version, agent CLI ID/version, requested and resolved model, effort, schedule,
byte-hashed current quote, category aliases, and per-invocation token envelope are pinned. The
machine calculation separately prices uncached input, cache-read input, cache-write input, visible
output, and reasoning output for every requested agent invocation. A named budget
owner must approve a non-expired USD cap at least as large as that
computed maximum. See `PRICING-BUDGET-READINESS-2026-07-15.md`; no run may start with an unbounded,
stale, or blank dollar cap.

The checked-in v3 power artifact deliberately does **not** substantiate this target design. No
retained source supplies exchangeable paired task-level variability for all three endpoints under
the final runner, v2 treatment, timeout, price, and quality contracts. Therefore elapsed-time,
normalized-cost, and quality marginal power—and joint intersection-union power—are all null/pending
rather than invented from legacy token/pass-rate data. The three claim floors remain separate from
the owner-frozen, strictly better true planning alternatives, which are also currently null. The
power gate remains failed and `power.completed` remains false.

The 12 independent task clusters required by the calibration contract are a minimum variance-
calibration floor, not a powered development design. Until compatible variance and endpoint-
dependence evidence exists—or a methodology owner explicitly freezes a documented assumption
set—no numeric power-sized development or confirmatory task count is defensible. In particular, the
provisional 24-task design and legacy two-endpoint 118 x 4 / 295 x 1 sensitivity rows cannot be
treated as power results.

The v2 artifact also analyzes byte-verified retained exploratory outcomes under
`power-calibration-exploratory-v1.json`. Those records use selected tasks, legacy treatments,
non-exchangeable runners/harnesses, and only 12 unique paired task IDs across four heterogeneous
sources. They may reveal variability risk but are programmatically quarantined: they cannot select
confirmatory assumptions, reduce the design, be pooled, or pass the gate. Before any holdout is
sealed, a methodology owner must either explicitly accept an assumption-only design and its call
envelope or preregister and budget a separate development-only calibration under the final runner
and treatment contracts. See `POWER-DESIGN-OPTIONS-2026-07-15.md`.

Use 10,000 task-clustered bootstrap resamples with a checked-in seed. The intersection-union rule
controls the joint claim at one-sided alpha 0.05 by requiring every component null to be rejected;
no across-endpoint multiplicity adjustment is required. Placebo contrasts are diagnostics only. All
executed attempts and all numerical observations remain in the primary analysis; there is no
performance outlier deletion. Failures during replaceable setup before the causal treatment timer
starts may be rerun without counting as an attempt. Once the timer starts at the harness-owned
retrieval/no-op boundary, the attempt is retained even if retrieval fails before any model request.
If such an attempt lacks billed usage, the suite stops as invalid rather than silently replacing the
cell. There is no efficacy peeking or optional stopping. Stop only at the fixed cell count, paid budget
cap, or an integrity stop condition; integrity stops pause the entire suite and require a new versioned
preregistration if methodology changes.

## Dependencies and freeze rule

The final freeze requires contracts, fixtures, and passing evidence from:

- WS2: treatment-neutral prompt snapshot, task-validity review, packet/placebo identity fields.
- WS3: eligible-before-ranking counts and completeness regression across all three engines.
- WS4: persisted balanced schedule, cache namespace/policy, planned/actual order, timing definitions.
- WS5: content-addressed suite/run manifests, common executed-run predicate, frozen analyzer hash, and
  successful evidence verification.

The implementation contracts are integrated and backed by content-addressed source and unpaid test
evidence in `integration-verification.json`, but they are not substitutes for fresh prompt reviews,
actual engine records, or final experimental inputs. `check_protocol.py --freeze` must remain red
while any task-validity review, engine verification, development threshold, powered design,
model/budget cap, or fresh holdout commitment is pending or failed.
