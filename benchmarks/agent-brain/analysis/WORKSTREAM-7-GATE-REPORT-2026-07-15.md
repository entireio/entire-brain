# Workstream 7 ranking gate report — 2026-07-15

## Decision

**Blocked gate: ranking change not justified.** No production scoring, retrieval, fusion, or packet
assembly behavior was changed.

The repository does not contain the prerequisites required by section 11 of
`ENTIRE-BRAIN-BENCHMARK-REPAIR-EXECUTION-PLAN-2026-07-15.md`: a controlled full-versus-tight
delivered-packet comparison, a reproducible relevance degradation after confound removal, or a WS6
development relevance set paired with an untouched ranking holdout. The three prior winner tasks were
not used for tuning and no model/agent cell was run.

## Baseline and isolation

- Work began from `4d2aff60` and brought in the required
  `origin/wip/memory-lifecycle-plan-handoff-20260712` baseline at `bbe1bdf5` via merge commit
  `c507c5b9` on `codex/workstream7-ranking-gate`.
- `git merge-base --is-ancestor bbe1bdf5 HEAD` exits 0.
- `entire-sem` was not read or modified as part of this workstream.
- The diagnostic analyzer is offline and non-mutating. Its output records
  `production_behavior_changed: false`.

## Section 11 production-change gate

| Required evidence | Status | Audit result |
|---|---:|---|
| Identical-cell full-versus-tight corpus comparison changes the delivered packet | **Absent** | No artifact supplies paired prompt, engine, task config, temporal eligibility, and delivered packet hashes for both corpus sizes. |
| Solving-fact rank or packet precision degrades reproducibly | **Absent** | No WS6 qrels/relevance set and no counterbalanced repeated packet evidence were found. |
| Engine, prompt, temporal, cache, and contamination confounds excluded | **Absent** | Existing evidence does not jointly verify those controls; the required repaired-workstream artifacts are not integrated here. |
| WS6 development relevance set and untouched holdout designated | **Absent** | Repository and `entire-plan` searches found no WS6 ranking dataset/holdout designation. |

Because all four requirements must be true, no candidate ranking implementation is authorized.

## Current mechanics audit

### Retrieval engines

The code and user documentation identify four distinguishable baselines, but no common WS6 relevance
dataset exists on which to measure them:

- hand-rolled lexical score: `factQueryScore` plus `60` per shared locus token and the closed-negative
  kind boost;
- explicit lexical-only: `recall ... --no-semantic` or `brief ... --no-semantic`;
- Model2Vec RRF: bundled default when `ENTIRE_BRAIN_EMBEDDER` is unset;
- EmbeddingGemma RRF: `ENTIRE_BRAIN_EMBEDDER=ollama`, with model default `embeddinggemma`.

An engine-effect table would be misleading without identical frozen candidate sets, effective-engine
records, query labels, qrels, vector namespaces, and packet outputs. The analyzer therefore does not
invent engine measurements from environment intent.

### Closed-negative boost

Production applies `factClosedNegativeBoost=30` only after a fact already has a positive lexical
score. The same `factLexicalScore` feeds both `rankFacts` and the lexical side of `rankFactsFused`.

The checked-in deterministic equal-match probe supplies the pre-kind lexical score rather than
reimplementing the production tokenizer. Both facts score `240` before the kind boost; the newer
convention wins the unboosted tie. Applying `+30` changes the closed-negative score to `270`, moves it
from rank 2 to rank 1, and changes top-1 membership. This is a mechanism probe, not evidence that the
boost improves or harms a representative corpus. The checked-in output reports one affected
closed-negative out of one synthetic closed-negative.

### Multi-query packet aggregation

`collect_frozen_facts` deduplicates by fact ID (falling back to case-folded text), then orders by:

1. lowest per-query rank;
2. highest number of query hits for equal best rank;
3. stable first-seen order for a complete tie.

In the deterministic two-query probe, `fact:bbb` has per-query ranks 2 and 1, so its best rank is 1
and its hit count is 2. It is delivered first, ahead of `fact:aaa`, which has best rank 1 but only one
hit. `fact:ccc` has best rank 2 and is delivered third. This demonstrates why a per-query rank cannot
be reported as delivered packet rank.

### Near-duplicate occupancy

The analyzer measures, but does not suppress, near-duplicate packet slots. On the synthetic packet,
transitive token-Jaccard at threshold `0.8` clusters `fact:aaa` and `fact:ccc`; the cluster occupies 2
of 3 delivered slots (`0.6666666666666666`). This validates the diagnostic field only. It is not a
production-corpus estimate and does not authorize cluster suppression.

## Added artifacts

- `analysis/ranking_diagnostics.py`: schema-checked offline boost, aggregation, and cluster-occupancy
  analyzer.
- `analysis/fixtures/ranking-baseline-input-v1.json`: deterministic mechanism fixture.
- `analysis/artifacts/ranking-baseline-output-v1.json`: reproducible machine-readable output.
- `analysis/test_ranking_diagnostics.py`: focused tests for rank/top-k effects, aggregation order, and
  duplicate occupancy.

Reproduce the artifact with:

```sh
python3 benchmarks/agent-brain/analysis/ranking_diagnostics.py \
  benchmarks/agent-brain/analysis/fixtures/ranking-baseline-input-v1.json \
  --output /tmp/ranking-baseline-output-v1.json
cmp /tmp/ranking-baseline-output-v1.json \
  benchmarks/agent-brain/analysis/artifacts/ranking-baseline-output-v1.json
```

Artifact SHA-256 values:

- input fixture: `5d430e6ae7475450bc1464e1558848bcb07795efc10c234605dc68866163396e`;
- output artifact: `0aa85efdd0419cb3567b2f608d426fd30605044733b90d511b776f51db60f2cc`;
- analyzer: `be602a48519f01607f28690cdf2074a15977cf5212818cc698fb8ab330d2a3c9`.

## Verification

The following unpaid deterministic checks passed:

```sh
python3 -m unittest benchmarks/agent-brain/analysis/test_ranking_diagnostics.py
# Ran 3 tests ... OK

go test ./internal/cli -run 'TestRankFactsClosedNegativeOutranksPeers' -count=1
# ok github.com/ashtom/entire-brain/internal/cli

(cd benchmarks/agent-brain && python3 run_test.py -k collect_frozen_facts)
# Ran 2 tests ... OK

(cd benchmarks/agent-brain && python3 run_test.py)
# Ran 246 tests in 13.851s ... OK

git diff --check
# clean
```

Running `python3 -m unittest benchmarks/agent-brain/run_test.py -k collect_frozen_facts` from the
repository root was also attempted, but the harness's sibling-module imports require its own directory
on `sys.path`; that invocation failed during import. Re-running from `benchmarks/agent-brain`, as shown
above, passed. This is a command-context issue, not a test failure.

## Evidence required to reopen the gate

Provide all of the following, content-addressed and generated under integrated WS2–WS6 controls:

1. Paired full- and tight-corpus run manifests with identical task/prompt hashes, effective engine,
   vector namespace, temporal eligible-set hash, cache policy, query source, K, and aggregation rule.
2. Exact per-query ranked fact IDs and scores plus final delivered packet IDs/order/hash for every
   pair and repetition.
3. Development qrels larger than the three anecdotal winner tasks, including solving IDs, relevant
   alternatives, hard distractors, closed-negative tasks, and null queries.
4. A frozen untouched ranking holdout with content hash and a contamination ledger proving it was not
   inspected or optimization-used.
5. Counterbalanced repeated or deterministic offline evidence showing the full corpus worsens solving
   fact rank or packet precision, with the preregistered threshold and uncertainty/aggregation rule.
6. Machine-readable proof that engine mismatch, oracle query leakage, post-top-K temporal filtering,
   cache namespace changes, and task contamination do not explain the difference.

Until those artifacts exist and pass their gates, the required integration action is to retain current
production ranking unchanged.
