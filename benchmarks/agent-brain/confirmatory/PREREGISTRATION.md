# Candidate preregistration: engine matrix and fresh confirmatory holdout

Status: **integrated methodology draft; power redesign required; not frozen; no paid run
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

The offline relevance dataset now contains 13 development queries over 12 exposed legacy tasks: 12
product queries, one oracle upper-bound query, explicit hard topical distractors, and no defensible
null/no-answer query. Two provisional nulls were withdrawn because they lacked exhaustive closure
against all 2,531 active facts; any future null requires the versioned active-corpus catalog plus
identical eligible/reviewed counts and hashes. This satisfies the development count floor only. It
does not pass the offline engine threshold, choose K/aggregation, or substitute for the still-empty
sealed holdout. Final freeze requires at least one corpus-closed development null query. The final
dataset must contain at least 12 sealed holdout queries over at least 12
unique untouched tasks. The fresh agent holdout target is 24 unique tasks. If the pre-run power
calculation misses 80% power for a 12% token reduction at the correctness gate, task count or
repetitions must increase before sealing; they may not change after the holdout is opened.

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

The analysis hierarchy is fixed:

1. Execution and validation integrity: all requested cells accounted for; evidence verification must
   pass. Infrastructure-invalid attempts are reported and replaced only under the interruption rule.
2. Correctness: task-level paired validation pass rate over **all valid attempts**. Retrieved memory
   must clear a -0.10 absolute non-inferiority margin versus no memory (one-sided 95% clustered
   bootstrap lower bound). Superiority is secondary.
3. Tokens: only after the correctness gate clears, compare task-level mean total tokens over all valid
   attempts, reporting the paired geometric-mean ratio and two-sided 95% task-clustered bootstrap CI.
4. Wall time: separately controlled secondary endpoint using both harness wall time and agent-reported
   API duration; neither substitutes for the other.

The provisional target design is 24 fresh tasks x 3 primary treatments x 4 repetitions = 288
requested paid cells, counterbalanced by WS4. Its 29-call operational reserve yields 317 maximum
calls; reserve is only for predeclared infrastructure-invalid replacements. This design is mirrored
in `pricing-budget.json` for drift detection but is explicitly not approved for budgeting while the
power gate is failed. Dollar cost remains null until the final powered design, exact
provider/runner/model/effort, byte-hashed current quote, and per-call token envelope are pinned. The
machine calculation separately prices uncached input, cached input, and output for every requested
and reserve call. A named budget owner must approve a non-expired USD cap at least as large as that
computed maximum. See `PRICING-BUDGET-READINESS-2026-07-15.md`; no run may start with an unbounded,
stale, or blank dollar cap.

The checked-in confirmatory decision calculation does **not** substantiate this target design. Under
its explicitly hypothetical conservative scenario, marginal power is 0.370207 for the 12% token
effect and 0.243511 for correctness non-inferiority; repetitions alone cannot overcome the assumed
task-level heterogeneity. The calculation is complete, but the power gate is failed. At four
repetitions, 118 tasks and 1,416 cells meet both marginal targets only conditionally on those
hypothetical assumptions. Across the listed tradeoff grid, the arithmetic cell minimum is 295 tasks
x 1 repetition = 885 cells; it is not an approved design or evidence that one repetition is
operationally sufficient.

The v2 artifact also analyzes byte-verified retained exploratory outcomes under
`power-calibration-exploratory-v1.json`. Those records use selected tasks, legacy treatments,
non-exchangeable runners/harnesses, and only 12 unique paired task IDs across four heterogeneous
sources. They may reveal variability risk but are programmatically quarantined: they cannot select
confirmatory assumptions, reduce the design, be pooled, or pass the gate. Before any holdout is
sealed, a methodology owner must either explicitly accept an assumption-only design and its call
envelope or preregister and budget a separate development-only calibration under the final runner
and treatment contracts. See `POWER-DESIGN-OPTIONS-2026-07-15.md`.

Use 10,000 task-clustered bootstrap resamples with a checked-in seed. Holm correction applies within
each endpoint family for retrieved-vs-no-memory and placebo-vs-no-memory comparisons. All valid
attempts and all numerical observations remain in the primary analysis; there is no performance
outlier deletion. A cell interrupted before any model request may be rerun without counting as an
attempt. Once a request is sent, the attempt is retained and classified by the common executed-run
predicate. There is no efficacy peeking or optional stopping. Stop only at the fixed cell count, paid
budget cap, or an integrity stop condition; integrity stops pause the entire suite and require a new
versioned preregistration if methodology changes.

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
