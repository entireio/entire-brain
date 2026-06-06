# Durable Facts — Handover

Working handover for continuing the durable-facts work on a **stacked branch**.
For the design and the full phasing, read [`durable_facts_plan.md`](durable_facts_plan.md);
this file is the operational state: what's landed, what's measured, how to repro,
and what to do next.

## Branch topology

- **Feature branch:** `claude/durable-facts-and-distill` → **PR #5** (open, base `main`).
  34 commits / 42 files. This is Phase A + the shipped Appendix-D structural and
  evaluation pieces. Treat it as feature-complete and in review.
- **This stacked branch:** `claude/durable-facts-phase-d`, branched off
  `claude/durable-facts-and-distill` @ `1b725f4`. New work goes here. When the
  feature PR merges, rebase this onto `main`; until then its PR should target
  `claude/durable-facts-and-distill`.

## What's landed (feature-complete on the base branch)

- **Store & pipeline:** content-derived fact ids, three-level taxonomy,
  checkpoint-level provenance, branch-scoped NDJSON storage, `distill`
  (agent-required) with agent-judged **merge/supersede reconcile**, incremental
  cache (cache-on-success only).
- **Read/write surface:** `remember`, `recall` (keyword + taxonomy + code-locus
  ranking; `--scope`; `--expand`), `inspect facts` / `inspect blame`,
  `facts tree`, `facts review` / `promote` / `retract` / `gc`. `brief` and
  `status` integrate facts (brief sizes to `--limit`).
- **Evaluation harness:** `facts eval-gen` (provenance-labeled benchmark from the
  brain's own sessions), `facts eval` (precision / recall / **useful-per-1k**),
  `facts eval-compare` (paired t-test via regularized incomplete beta, verified
  vs scipy; Holm correction; Cohen's d).

## What's measured (and resolved)

Benchmark: **121 provenance-labeled tasks across 3 repos** — entire-brain (8),
entire-cli (37), entire.io (76). Both findings below are recorded in the plan
doc's Phasing block.

1. **Query expansion — resolved negative.** Early Cohen's d≈0.47 was an n≈8
   artifact. At n=121: useful-per-1k delta **+0.205, t=1.14, p=0.26, d=0.10**;
   no significant stratum; sign flips on the original set. `recall --expand`
   stays opt-in/experimental — **not** defaulted.
2. **Lexical retrieval tuning — resolved, no headroom.**
   - k-sweep: useful-per-1k is monotone in k (k=5→4.44, k=10→3.53, k=30→2.83),
     purely trading against recall (0.16→0.24→0.46). No free optimum; k=10 stands.
   - 16-cell ranking-weight grid: moved recall@10 by ≤+0.006 (noise); the
     substring weight changed nothing.
   - **Decisive measurement:** lexical recall **ceiling = 0.667** (18% of tasks
     have *zero* lexically-reachable relevant facts) while recall@10 = 0.244. The
     0.244→0.667 gap is a ranking gap among reachable facts that lexical features
     can't separate.

**Conclusion:** every lexical lever is exhausted. The only remaining retrieval
headroom is **semantic (Phase D embeddings)**.

## Next work on this branch — Phase D (embeddings)

Evidence-backed because embeddings address *both* measured failure modes:

- **Lift the 0.667 reachability ceiling** — semantic recall of the ~33% of
  relevant facts no query term reaches (incl. the 18% of tasks at zero).
- **Close the 0.244→0.667 ranking gap** — rerank within the reachable set, where
  lexical scoring ties relevant and irrelevant facts together.

Suggested first steps:
1. **Pick a local embedding backend** (the open blocker in the plan). Constraint:
   the brain is local-only and must stay offline-capable — no network-dependent
   embedding calls in the default path. Options to evaluate: a bundled small
   local model vs. reuse of whatever `entire-sem` already has.
2. **Scope retrieval shape:** embed facts at ingest; rerank *within* a code
   locus / taxonomy node rather than over a flat per-branch list (see plan
   Appendix D / Phase D).
3. **Measure against the existing harness** — the benchmark and `eval-compare`
   are ready; target lifting recall@10 toward the 0.667 ceiling *and* pushing the
   ceiling itself up. Report with the same paired-t rigor.

Do **not** re-litigate expansion or weight tuning — both are closed negatives.

## Reproducing the evaluation

Persisted benchmark artifacts (survive reboot):
`~/.local/share/entire/facts-benchmark/` — the three `*_raw.json` task sets,
base/expand summaries, expand caches, and the driver scripts (`run_ab.sh`,
`ksweep.sh`, `pool.py`).

Brain stores live at
`~/.local/share/entire/plugins/data/brain/repos/gh/<org>/<repo>`.

The eval reads facts from the store for the repo named by `ENTIRE_REPO_ROOT`, so
drive it **plugin-direct** (bypasses the host + mise-trust prompts):

```sh
go build -o /tmp/eb-test ./cmd/entire-brain
export ENTIRE_PLUGIN_DATA_DIR="$HOME/.local/share/entire/plugins/data/brain"
export ENTIRE_REPO_ROOT="$HOME/Projects/cli"          # selects the repo's store

# regenerate a benchmark (recall-oriented labels; add --refine for precision-clean)
/tmp/eb-test facts eval-gen --out /tmp/cli_raw.json

# score it (deterministic with provenance labels)
/tmp/eb-test facts eval --tasks /tmp/cli_raw.json --json --k 10

# A/B two configs over the same tasks
/tmp/eb-test facts eval-compare --a base.json --b variant.json
```

To pool across repos for one large-n test, namespace task ids per repo and
concatenate each summary's `results` array into a combined `base`/`variant`
summary, then feed `eval-compare` (it pairs by id and recomputes from `results`).
`pool.py` in the persisted folder does exactly this.

## Repro gotchas (learned the hard way)

- **mise-untrusted / host wrapper:** running `entire brain distill` via the host
  inside another repo can hit `mise.toml are not trusted`. Run `/tmp/eb-test`
  directly with `ENTIRE_PLUGIN_DATA_DIR` + `ENTIRE_REPO_ROOT` set.
- **Agent PATH:** the plugin's exec env may lack `codex`/`claude`. `codex` is at
  `/usr/local/bin`; `claude` is under `~/.local/share/mise/installs/node/lts/bin`.
  Prepend both to `PATH` for any agent-using step (`--refine`, `--expand`,
  `distill`).
- **Agent rate limits on large transcripts:** distilling ~200-chunk branches with
  6 parallel workers rate-limits **both** codex and claude (a rolling window, not
  a token-exhaustion — a fresh single call works). 16/21 entire-cli branches have
  facts; the 5 largest are still at 0 and are not worth force-completing. Run
  heavy distills sequentially or with low parallelism.
- **`--mcp-config` for claude-code:** must be `{"mcpServers":{}}` — bare `{}` is
  rejected by the current CLI. (Fixed in `distill.go`/`seed.go`.)

## Open / deferred (not blockers for Phase D)

- Synthesized hierarchy summaries (Appendix D).
- Turn-level signed provenance + `verify` (Phase B; needs Entire CLI changes).
  Distilled-fact provenance stays checkpoint-level until then.
- Taxonomy concentration: ~60% of facts fall into two catch-all buckets
  (Appendix C/D) — a labeling/ingestion-quality follow-up, independent of
  retrieval.
