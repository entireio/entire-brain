# Benchmarks

Selected measurements of Brain, their limits, and commands for evaluating your
own repository. Historical results include both gains and closed negatives.

**What this page is not:** a head-to-head against another memory product. We
have not run one under conditions we would defend, and [saying so](#head-to-head-against-other-products)
is more useful than a number nobody can reproduce.

## Run it yourself

You should not have to take our numbers on trust. These two need no
configuration, no API key and no network, and run against whatever repository
you point them at:

```sh
entire brain bench scale        # index size and query latency vs codebase size
entire brain bench semantic     # indexing throughput and memory
```

Both require a clean worktree — the indexer refuses uncommitted content — and
`--json` emits the full record.

Retrieval quality needs a task set, because a quality number without one is a
number about nothing:

```sh
entire brain facts eval-gen --out tasks.json     # build a task set from this brain
entire brain facts eval --tasks tasks.json       # deterministic when tasks carry labels
entire brain facts eval-compare --a <a.json> --b <b.json>
```

`facts eval` is deterministic and model-free when the task set carries relevance
labels. It calls an agent only if you pass `--judge`, which asks a model to
label relevance instead — that costs tokens, and it is opt-in for exactly that
reason.

## Scale: historical measurements

These September 2026 summaries cover five repositories from 174k to 4.8M lines
on one Apple M-series laptop, using the `syntax-only` profile. They describe the
original benchmark, not the current build. Raw per-query samples and exact
corpus revisions were not retained; these rows cannot be independently
reproduced byte-for-byte.

| Repository | kLOC | Index | KB/kLOC | code search p50 |
| --- | ---: | ---: | ---: | ---: |
| entire-api | 174 | 40 MB | 237 | 299 ms |
| entire.io | 485 | 103 MB | 218 | 1,329 ms |
| entiredb | 628 | 112 MB | 183 | 1,407 ms |
| entire-brain | 746 | 193 MB | 265 | 1,315 / 1,945 ms |
| kubernetes | 4,786 | 1,171 MB | 251 | 12,294 ms |

The largest tested repository had a 1,171 MB index and a 12.3-second median
code search. The two entire-brain runs differed by 48%, so read the order of
magnitude rather than the digits. These observations do not establish a scaling
law, a capacity limit, or an advantage over a competitor.

**100M lines has not been measured.** No competitor implementation was run
under this workload. Full method, variance and caveats are in [scale](scale.md).
Re-run the benchmark to evaluate your current build and corpus.

## Retrieval quality: what moved the numbers

These are Brain measured against Brain — one retrieval arm against another on
the same corpus and task set. They are not comparisons with other products.

| Finding | Evidence | Outcome |
| --- | --- | --- |
| BM25 over substring matching on history | 2.7x–5.1x useful/1k across strata on entire-cli; BM25 gains also observed on two other repositories | BM25 is the no-embedder default |
| EmbeddingGemma fusion over BM25 | +13% useful/1k (t=7.08) and precision +0.016 (t=7.68) at 418k records / 2,584 tasks; replicated at +12.5% (t=5.09) on a second repo | Fused history arm ships, gated on a configured Gemma-class embedder |
| Semantic fusion on facts | +0.459 useful/1k (p=0.005, Holm-significant), precision +0.025, no token cost, n=121 | Fusion default-on for `recall` and `brief` |
| Locus-scoped retrieval | +59% useful/1k (p=0.027 raw, marginal under Holm), −112 tokens (p<0.001), recall −0.029 | Scoped retrieval shipped; outline kept as a map, not a ranker |

Full rows, task sets and corpus identities are in the [eval ledger](eval_ledger.md).

## Agent outcome: one proof-ready result

Retrieval metrics measure retrieval. Whether a brain changes what an agent
*does* is a different question, and a much harder one to answer honestly.

**One measurement has passed our proof gate.** A correctness task whose decided
value exists only in retained history, run five times per side with
`claude:sonnet:high` in the retained August 2026 panel:

| Condition | Validation pass rate |
| --- | ---: |
| no brain | **0 / 5** |
| semantic_history_brain (history channel) | **5 / 5** |

`proof_ready=true` — the lift survives drop-one — with **0 hard integrity flags**
and 20/20 records provenance-backed across the two tasks in the panel. Without the brain the agent cannot recover
the exact decided value; with it, it reads it from history and restores it.

This flipped the history-channel correctness scope from no-claim to claimable.
It is one task on one repository with n=5 per side, scoped to this runner and
history channel. The other task saturated at 5/5 in both arms and is not proof
of lift. The [retained audit](../benchmarks/agent-brain/evidence/replay-lab-clean/codex-audit-report.md)
and its manifest are checked by `mise run clean-proof:evidence`. This narrow
result does not establish MCP, Radar, codex-runner, or efficiency agent lift.

## What we deliberately do not claim

Brain's benchmark evidence runs under a **claim policy with an auditor that
refuses citations that have not earned them**. Most retained evidence sits in
`no_release_claim` mode on purpose:

```
claim_policy: no_release_claim
proof_ready_comparisons: 0
```

`mise run release:evidence` fails if a claim-bearing suite is not
provenance-complete, audit-clean, backed by a stable proof-ready comparison, and
run at least four times per side against the *current* task definitions.

So the MCP, Radar, codex-runner and efficiency scopes have numbers, and we do
not cite them. They have not passed the gate. The retained artifacts are kept
specifically so an auditor can see why they are not citable yet.

A clean no-claim audit validates the disclosure; it does not establish an
agent-quality improvement. The separate history-channel lane above has its own
proof-required gate.

## Closed negatives

Things we tried, measured, and dropped. A benchmark page with no losses on it
is a marketing page.

- **Query expansion.** An early d≈0.47 was an n≈8 artifact; at n=121 it
  collapsed to d=0.10, p=0.26. Stays opt-in.
- **Model2Vec fusion on history.** Significantly *worse* than BM25 on precision
  at production scale (t=−6.33 on 418k records, t=−2.84 on a second repo) with
  no useful/1k gain. A promising small-repo +31% was noise.
- **Conversation-exchange fusion.** Traded exact-match precision for one extra
  paraphrase hit. Ships dark behind a flag; `query` stays BM25-only.
- **RRF fusion-weight tuning.** Swept; every cell within noise. Equal weight,
  k0=60 stands.
- **Facts FTS5 BM25 as default.** Parity, not better (1.43 vs 1.50). Opt-in.
- **Scoped-floor via backfill facts.** Dead end.

## Head-to-head against other products

We have not run one.

A defensible head-to-head would need the same corpus, the same task set, the
same judge, and the same token accounting on both sides, with enough repetitions
to survive drop-one — the bar our own claim gate applies. Published numbers from
memory products are generally measured on their own corpora with their own
scorers, so quoting them side by side with ours would compare two different
experiments and call it a comparison.

What we can say from measurement rather than assertion:

- **Historical scale costs are substantial**, as [above](#scale-historical-measurements);
  a comparison against another implementation remains unmeasured.
- **Ingest is deterministic and uses no model**, so indexing cost is wall-clock
  and disk rather than tokens. `bench scale` reports both; there is no API bill
  to compare against.
- **Everything here runs locally**, so you can point our benchmarks at your own
  repository and get your numbers rather than ours.

If you are evaluating Brain against something else, the commands at the top of
this page let you measure your codebase; runtime depends on its size. That is the comparison that
should decide it.

## Method notes

- The scale and retrieval rows are historical summaries, not fresh measurements
  of the current build. The scale source states which reproduction artifacts
  are missing; the eval ledger identifies the retrieval corpora and task sets.
- Results are comparable only within a row's own baseline. Task sets, corpus
  snapshots and metrics differ between rows; the eval ledger states this and it
  applies here too.
- `useful/1k` is useful retrieved content per 1,000 tokens delivered — a
  quality-per-cost measure, so a result can improve precision and still lose on
  it by spending more context.
