# Durable Facts — Handover

Working handover for continuing the durable-facts work on a **stacked branch**.
For the design and the full phasing, read [`durable_facts_plan.md`](durable_facts_plan.md);
this file is the operational state: what's landed, what's measured, how to repro,
and what to do next.

## Part B (Appendix D) — fact quality & structure — IN PROGRESS

Branch `feat/fact-quality-appendix-d`. Executing the full Appendix-D redesign
(KIND dimension + stored LOCUS index + synthesized outline + audience A/B eval)
in one staged pass. None of it needs the Entire-CLI turn-signing change. Plan:
the staged B0→B4 in the session plan file.

### B0 — frozen baseline (captured 2026-06-09)

Before any change, the current `facts eval` numbers over the live `entire-brain`
`main` corpus (930 facts @ `3391f76`, **17** deterministic provenance-labeled
tasks from `facts eval-gen --branch main`, k=10). Every later stage compares
against this to prove no-regression.

| Arm | useful/1k | precision | tokens |
|---|---|---|---|
| Lexical (default) | **1.063** | 0.0706 | 678 |
| Model2Vec fused (`--semantic`) | 1.013 | 0.0706 | 645 |

By stratum (lexical): concept 0.977 (n=11), code 1.043 (n=3), convention 1.357
(n=2), howto 1.477 (n=1). Note these are **lower than the alignment.md Stage-0
baseline (1.50)** because that set was a 179-fact distill; the corpus has since
grown to 930 facts, so there is more to discriminate — which is exactly the
two-bucket problem Part B attacks. Repro: `facts eval-gen --branch main` then
`facts eval --tasks <f> --branch main --k 10 --json` (± `--semantic`),
deterministic, no agent.

### B1 — first-class KIND dimension (landed)

Added `Kind` to `factRecord` (additive metadata — **not** in the id hash, so ids /
provenance / vector cache are stable). Closed set
`decision|invariant|gotcha|preference|convention`. Hybrid source: the distill /
`remember` prompts emit a leading `kind<TAB>` column (parser tolerant of the
legacy 2-field form), and `inferFactKind` (taxonomy prior + high-precision text
cues) backfills the rest for free. New surfaces: `facts reclassify` (no-agent
backfill), `recall --kind`, `facts tree --kind`, `[kind]` in `recall`/`blame`,
`by_kind` in the manifest, kind on unified `brain_get`/search results.

**Backfill on the live `main` corpus (948 facts):** invariant 414, decision 273,
convention 196, preference 64, **gotcha 1**. The kind axis discriminates far
better than the two taxonomy buckets, but the deterministic `gotcha` cue
**under-fires** (1) — gotchas are the highest-value class and the hardest to
detect from a path/keyword prior, so the agent-labeled path (distill prompt) is
where that recall comes from going forward. B4 measures whether kind filtering
lifts useful/1k despite the inference being coarse.

**B1 review fixes (applied).** A high-effort review found the kind column could
silently *drop* facts; fixed with a robust `parseFactLine` grammar that recovers
a fact whether the agent emits an exact kind, a near-miss synonym, or no kind
(plus kind-aware literal-`\t` recovery). Also: `remember --kind` now wins on an
existing fact and reports the persisted kind; `inferFactKind` tie-breaks by
kind-priority (a `constraints.*` co-tag infers invariant, not decision) and
allocates nothing on the hot path; `reclassify` always refreshes the manifest;
`by_kind` counts active facts only.

**Known limitation (by design):** a re-distill where the agent emits a
*different valid* kind for an existing fact does not overwrite the stored kind
(the anti-thrash rule in `upsertFact` only fills an absent/invalid one). Use
`facts reclassify --force` or `remember --kind` to change a stored kind. This is
deliberate — it stops an inference-fallback from clobbering an agent label across
runs — but it means agent re-labels are not automatically picked up.

Gotcha-labeling quality on the existing corpus is **deferred to a post-B4 call**
(the eval decides whether kind labels lift retrieval before we invest in agent
backfill).

### B2 — stored LOCUS + semantic tie-in (landed)

Added `Locus []string` to `factRecord` — the code identifiers/paths a fact is
about (WHERE). Computed at creation via the existing `factLocus(text)` and
backfilled by `facts reclassify` (locus is purely a function of text, so it is
always reconciled; kind only when missing). Surfaces:

- **`inspect changes` → relevant facts:** `factsRelevantToChange` matches a
  fact's locus against the changed files and the semantic symbols defined in
  them, so `inspect changes --json` gains a `facts` field — "what the brain knows
  about the code you're touching".
- **`recall --locus <path|symbol>`** filters facts by locus.
- Ranking (`recall`/`brief`) now reads the stored locus
  (`locusOverlapTokens(queryLocus, factLocusOf(f))`) instead of recomputing it
  per query.

Backfilled the live `main` corpus: 797/948 facts carry a locus (the rest are
pure prose with no identifiers).

**Branch-scoping note:** `inspect changes` reads facts for the *current* branch
(consistent with `recall`/`brief`), so on a feature branch with no distilled
facts the `facts` field is empty until facts are distilled or `promote`d onto it.
Appendix D's "merged knowledge graduates to a code locus, visible regardless of
branch" is the larger fix and is not in B2.

### B3 — synthesized hierarchical outline (landed)

`facts/<branch>/outline.json` — a locus-tiered tree (`internal/cli`, `docs`, …)
built deterministically from the facts' loci; cross-cutting and non-file facts
home at the root. `facts outline` fills each node with a one-sentence agent
rollup summary, **incremental** by a bottom-up subtree fingerprint (only changed
subtrees re-summarize), gated on `--model`/`--effort`/`--budget`, best-effort
(a failed call leaves a node unsummarized), and `--agent none` builds
structure-only with zero tokens. `facts map [--path --depth]` renders it
summary-first with progressive disclosure; it **always rebuilds the structure
from live facts** and overlays a stored summary only when the node fingerprint
still matches, so the map is never structurally stale and no outline run is
required to see it.

**On the live `main` corpus (948 facts):** 31 nodes; the only deep tier is
`internal/cli` (48 facts) — **most facts (≈880) home at the root** because their
text names no concrete source file. So the locus tiering is honest but **shallow
on this corpus**: durable facts here are mostly conceptual/cross-cutting, not
file-anchored. That's a real input-quality signal for B4 — the outline's value
depends on facts carrying a code locus, which the current distilled corpus often
lacks. A taxonomy-fallback tier (group rootless facts by category) is a candidate
enhancement if the eval wants more structure.

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
