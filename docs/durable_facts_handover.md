# Durable Facts — Handover

Working handover for continuing the durable-facts work on a **stacked branch**.
For the design and the full phasing, read [`durable_facts_plan.md`](durable_facts_plan.md);
this file is the operational state: what's landed, what's measured, how to repro,
and what to do next.

## Branch topology

- **Base work (merged):** `claude/durable-facts-and-distill` landed via **PR #5**.
  Phase A + the shipped Appendix-D structural and evaluation pieces are now on
  `main`.
- **This branch:** `claude/durable-facts-phase-d` → **PR #9**, rebased onto and
  targeting `main` (no longer stacked). New Phase D work goes here.

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

## Phase D (embeddings) — in progress on this branch

Backend decided and a first significant win landed. Embeddings address *both*
measured failure modes:

- **Lift the 0.667 reachability ceiling** — semantic recall of the ~33% of
  relevant facts no query term reaches (incl. the 18% of tasks at zero).
- **Close the 0.244→0.667 ranking gap** — rerank within the reachable set, where
  lexical scoring ties relevant and irrelevant facts together.

### Backend decision: Option A — pure-Go static embeddings (agreed)

`entire-sem` provides **no embedder** (it is a tree-sitter structural provider:
symbols + relations + FTS; `entire sem` has no `embed` command and there are no
vectors anywhere in the stack). So "reuse entire-sem" was a non-option for
embeddings. The chosen backend is a **bundled Model2Vec static model** run in
pure Go — no cgo, no ONNX/llama.cpp runtime, no network, single static binary —
behind an `Embedder` interface so a transformer bi-encoder (ONNX) or a
provider-shelled embedder can replace it later *if* the harness shows headroom.
(Rejected: llama.cpp/GGUF, sqlite-vec — a C extension incompatible with the
pure-Go `modernc.org/sqlite` — and auto-download, which breaks offline-default.)

### Landed slices

1. **Static embedding backend** (`internal/cli/embed.go`, `embed_accents.go`,
   root `embedmodel.go` + `assets/embedmodel.bin`, converter
   `scripts/convert_embedmodel.py`). BERT WordPiece → mean-pool → L2-normalize,
   int8-quantized table, golden-parity tested against reference vectors.
2. **RRF fusion** (`embed_rank.go`): `rankFactsFused` fuses a lexical list with
   a semantic list that ranks the *entire* active candidate set (the lever
   against the reachability ceiling). It first landed flag-gated, then shipped
   default-on for `recall`/`brief` (`--no-semantic` to opt out) while `eval`
   keeps `--semantic` explicit for the A/B — see *Default-on + disk cache* below.
   Nil reranker == existing lexical `rankFacts`.
3. **Measured + model adoption.** base reproduces the handover exactly
   (recall@10=0.244, useful/1k=3.53). potion-base-8M was only directional
   (useful/1k p=0.082). **potion-retrieval-32M** (retrieval-tuned, swapped behind
   the interface) is a real win over the 121-task benchmark:

   | metric | base | semantic | Δ | p | Holm |
   |---|---|---|---|---|---|
   | useful_per_1k | 3.531 | 3.990 | +0.459 | 0.005 | **sig** |
   | precision | 0.218 | 0.243 | +0.025 | 0.013 | **sig** |
   | recall@10 | 0.244 | 0.276 | +0.032 | 0.043 | marginal |
   | tokens | 536.7 | 542.6 | +5.9 | 0.363 | n.s. |

   Headline metric +13% and precision survive Holm; recall is raw-significant;
   **no significant token cost** (contrast: expansion was cost-only). Repro:
   `~/.local/share/entire/facts-benchmark/run_semantic_ab.sh` (deterministic, no
   agent). Pooled summaries persisted as `combined_semantic_*.json`.

### Fusion tuning — resolved (no lever)

Recall was the soft spot (+0.032, marginal under Holm), so the obvious knobs
were swept over the 121-task benchmark (potion-retrieval-32M): a 3×3 RRF grid
(`k0 ∈ {10,30,60} × wSem ∈ {1,2,3}`) plus embedding taxonomy paths into the
fact vector. **All are non-levers.** Every cell beats base, but cells differ
from each other by ≤0.012 recall and ≤0.05 useful/1k — noise at n=121. The best
cell (k30,w3) vs the shipped default (k60,w1) is recall +0.012 (p=0.16, n.s.)
and useful +0.047 (p=0.59, n.s.); embedding paths slightly raises recall but
*lowers* useful/1k. Picking any cell would overfit one task set. The principled
default (k0=60, equal weight, text-only) stands; the env-knob build used for the
sweep was discarded. Full grid: `facts-benchmark/semantic_fusion_sweep.md`.

**Takeaway:** the remaining recall headroom is *not* in fusion tuning. It is in
a stronger embedder / cross-encoder reranker (heavier, evidence-gated) or in
fact-quality/structure work (Appendix D: taxonomy under-discrimination,
locus-indexed facts) — a different axis from retrieval scoring.

### Default-on + disk cache (shipped)

`recall` and `brief` rerank semantically **by default** (RRF fusion), degrading
silently to lexical when the embedder is unavailable; `--no-semantic` opts out.
`eval` keeps `--semantic` explicit so the base-vs-semantic A/B still measures
both arms.

Fact vectors are persisted under `facts/<branch>/embeddings/vectors.bin`
(model-id + dim keyed in the header → a model swap invalidates cleanly; ids are
content-derived; the flush prunes vectors for facts no longer present). Measured
on the entire-brain `main` branch (~1500 facts): cold recall (embed + write
cache) **1.20s → warm 0.14s**, vs 0.08s lexical — so a warm semantic recall adds
only ~60ms (33MB model decode + cache read) over lexical. The cache is
in-`ENTIRE_PLUGIN_DATA_DIR` derived state, never committed; eval still uses the
in-memory reranker so it never writes caches into the stores.

### Open next steps

- **`facts gc` should prune `embeddings/`** for dropped branches (it currently
  only touches `facts.ndjson`); stale vectors are otherwise harmless and
  self-prune on the next write.
- **Stronger-embedder / cross-encoder rerank** — only if a measured target
  justifies the weight; the `Embedder` interface already supports the swap.
- **Asset size:** 32.9MB int8 — revisit (harder quantization / smaller model)
  only if it becomes a distribution concern; the win justifies it for now. (Note:
  the large asset needs `git -c http.version=HTTP/1.1` to push reliably.)

Do **not** re-litigate expansion, lexical weight tuning, or **RRF fusion
tuning** — all three are closed negatives.

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
