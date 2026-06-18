# Eval ledger

One row per retrieval-quality or agent-outcome measurement that gated a decision. The task sets,
corpus snapshots, and metrics differ between rows — **numbers are comparable
only within a row's own baseline**, never across rows. Before claiming a
regression or a win, find the row whose gate you are re-testing and reproduce
its exact config; the source doc has the repro commands.

| Date | Corpus / tasks | Comparison | Headline | Decision it gated | Source |
|---|---|---|---|---|---|
| 2026-06-08 | entire-brain `main`, 179 facts, 16 provenance-labeled tasks (`facts eval-gen`, k=10) | lexical-only vs Model2Vec fused | lexical 0.86 → fused **1.50 useful/1k** (+76%); concept stratum 0 → 0.79 | Frozen Stage 0 baseline; any later embedder swap must beat 1.50 on this set. Also: facts FTS5 BM25 measured at parity (1.43), so BM25 ships **opt-in** (`ENTIRE_BRAIN_FACTS_BM25=1`), not default | `../alignment.md` (Stage 0); artifacts at `<brain>/eval/baseline-20260608/` |
| 2026-06-08 | same 16-task set | Model2Vec fused vs EmbeddingGemma-300M (Q8_0, shell-to-local-server runner) | **1.80 useful/1k (+20%)**, precision 0.106, concept stratum 1.68 (2.1×); gain is reachability (three 0.00→~1.75 rescues), not uniform lift | Stage 1b embedder **quality gate: passed**. Runtime/distribution work still parked | `../alignment.md` (Stage 1b) |
| 2026-06-08 | entire-brain (16 tasks) + podcasts (44 tasks), pooled n=60 | Model2Vec vs EmbeddingGemma, second repo | pooled 1.77 → **2.00 useful/1k (+14%)**; +13% on the non-meta repo | Confirms the Gemma gain is not a meta-domain artifact; right-sizes the effect from +20% to a moderate +14% | `../alignment.md` (multi-repo confirmation) |
| 2026-06-08 | live 32k-record history index | substring scorer vs FTS5 BM25 (history) | `matches: null` defect fixed: BM25's IDF replaces the 3-of-N term-coverage gate; Model2Vec on history measured as **noise** (negative cosine separation), EmbeddingGemma separates (0.397 > 0.317) | History ships **BM25-only by default** (Stage 1a); a history semantic arm waits for the Stage 1b embedder | `../alignment.md` (Stage 1a findings) |
| 2026-06-09 | entire-brain `main`, 930 facts, 17 deterministic tasks | frozen B0 baseline | lexical 1.063 useful/1k, semantic 1.013 | Appendix D (B1–B4) work measured against this snapshot | `durable_facts_handover.md` (B0) |
| 2026-06-09 | pooled n=118 (17 entire-brain + 101 entire-cli tasks) | flat vs locus-**scoped** vs outline retrieval arms | scoped: **+59% useful/1k** (p=0.027 raw; marginal under Holm), −112 tokens (p<0.001), recall −0.029 (p=0.004); outline not a retrieval win (p=0.51) | B4 verdict: **scoped retrieval validated**; outline kept as a human-readable map, not a ranking mechanism; scoped-floor variant killed | `durable_facts_handover.md` (B4) |
| 2026-06-09 | 121-task benchmark (potion-retrieval-32M) | lexical vs Model2Vec fused (shipped config) | **+0.459 useful/1k** (p=0.005, Holm-significant), precision +0.025 (p=0.013), no token cost | Phase D ship decision: semantic fusion **default-on** for `recall`/`brief` | `durable_facts_handover.md` (Phase D) |
| 2026-06-10 | entire-brain live brain, **history layer**: 33.8k rankable records, 13 provenance-labeled tasks (`history-eval-gen`, k=10) | substring vs BM25 vs Model2Vec-fused (`history-eval` arms) | substring 0.315 → **BM25 0.927 useful/1k (2.9×)**, precision 0.062 → 0.146 — first quantitative confirmation of the shipped BM25 default. Cutoff sweep 0.15/0.30/0.50: **within noise** → the shipped 0.30 stands. Fused (Model2Vec) 1.219 (+31% over BM25), 4W/3L/6T — **directional only**: n=13 with 6 all-zero tasks leaves 7 informative pairs, far from significant. Tension with the 2026-06-08 "M2V noise on history" probe is real but explainable: that probe measured raw cosine separation on paraphrases; RRF fusion bounds how much a weak semantic arm can hurt while letting its agreements help | History eval harness landed (`history-eval-gen` / `history-eval`); cutoff question **closed**; history semantic arm **stays unshipped** pending an EmbeddingGemma run (`ENTIRE_BRAIN_EMBEDDER=ollama … --arm fused`) and a second repo | this repo (`internal/cli/history_eval.go`); task set regenerable via `history-eval-gen` |

| 2026-06-10 | three repos, **history layer**: entire-brain (13 tasks), podcasts (16), entire-cli (**882**, 267k records) | substring vs BM25 vs Model2Vec-fused vs **EmbeddingGemma-fused** (canonical GGUF via node-llama-cpp server) | useful/1k — podcasts: 1.128 / 1.554 / 1.731 / **2.108**; entire-brain: 0.315 / 0.927 / 1.219 / 0.978; entire-cli: 0.283 / 1.016 / 1.033 / **1.158**. At n=882: M2V-fused is **parity** with BM25 (+1.7% — the small-repo +31% was noise, consistent with the original M2V-noise probe), while **Gemma-fused is +14% (329W/248L/305T, sign test p=0.00075)** — the same +14% the facts layer measured, now decision-grade on history | **History semantic arm: validated with EmbeddingGemma, not with Model2Vec.** Production wiring (fused history arm when a Gemma embedder is configured; history vectors in vec0) is now justified but not yet built. BM25 default reconfirmed at scale (3.6× substring) | this repo (`history-eval` fused arm); per-repo JSONs regenerable |

| 2026-06-10 | entire-brain live brain, **midtask stratum**: 33 tasks (20 midtask) | substring vs BM25 vs Model2Vec-fused, per stratum | midtask useful/1k: 0.317 / 0.631 / 0.881 — BM25's lift over substring is **smaller on midtask (2.0×)** than on opening concept queries (5.9×), and fusion helps midtask most in relative terms (+40% over BM25). n=20, directional | Mid-session questions are a real, harder stratum; powered confirmation queued on the entire-cli v4 re-scan and the entire.io capstone | this repo (`history-eval-gen --midtask`) |
| 2026-06-11 | entire-cli v4 index (273k records, 6.2k requests), **1,522 tasks** incl. **632 midtask** | substring vs BM25 vs Model2Vec-fused, per stratum, paired | midtask useful/1k: 0.260 / 0.709 / 0.737. BM25 over substring holds on every stratum (2.7×–5.1×) but is **weakest on midtask (2.7×)**. M2V-fused vs BM25: **overall parity (453W/425L, p=0.34)** — consistent with the three-repo finding — but **positive on the midtask stratum (181W/144L, p=0.040)**: even the weak embedder helps where questions are narrow and in-flight | Item 8 premise confirmed at power: the midtask stratum leans on semantics where opening requests do not. Evaluate retrieval levers against it before shipping hook-path defaults; the Gemma midtask read rides the entire.io capstone batch | this repo (`history-eval-gen` v4) |
| 2026-06-12 | **capstone**: entire.io v4 (**418k records, 2,584 tasks**, 1,049 midtask) + entire-cli v4 (1,522 tasks, 632 midtask), paired per-task t-tests | BM25 vs Model2Vec-fused vs **EmbeddingGemma-fused**, first read at production scale | entire.io useful/1k: BM25 0.883 / M2V 0.857 / **Gemma 0.998 (+13%, t=7.08)**; precision: Gemma **+0.016 (t=7.68)** while **M2V −0.013 (t=−6.33)** — at 418k records the weak embedder costs significant precision and buys nothing. entire-cli replicates: Gemma +12.5% useful/1k (t=5.09), precision +0.017 (t=6.38); M2V precision **−0.007 (t=−2.84)**, useful/1k parity (t=0.73). Midtask: Gemma beats BM25 on both repos (t=5.19 eio / t=2.06 cli); the 06-11 cli M2V midtask positive does **not** generalize — eio midtask M2V is flat (t=0.33). Third independent ~+13% Gemma read (facts +14%, three-repo history +14%, capstone +13%/+12.5%) | **Item 9 verdict: ship the history fused arm gated on a configured Gemma-class embedder; Model2Vec fusion on history is a closed negative at scale.** BM25 stays the no-embedder default. (Substring arm abandoned mid-run: its per-task re-scoring is O(tasks×records) — 6.5h without finishing at this scale; its baseline is already established at 2.7–5.1× on three repos) | this repo (`history-eval`); artifacts in `~/.entire-brain-eval/` (regenerable) |
| 2026-06-18 | entire-brain `main`, replay-lab clean correctness task (`entire-brain-clean-default-fact-merge-confidence`, a decided default whose value lives only in retained history), n=4/side, `claude:sonnet:high` | no_brain vs full_brain (history channel), **end-to-end agent validation pass-rate** | no_brain **0/4** → full_brain **4/4**; `brain_positive_stable`, `proof_ready=true` (pass-rate lift surviving drop-one); `audit_codex.py` 0 hard integrity flags, 16/16 provenance-backed. no_brain cannot recover the exact decided value; full_brain reads it from history and restores it | **First proof-ready replay-lab agent-lift.** Flips the history-channel correctness scope from `no_release_claim` to claimable; MCP/Radar/codex-runner/efficiency scopes stay no-claim. Co-located contract-checkpoint comparison saturated (both 4/4) | `benchmarks/agent-brain/evidence/replay-lab-clean/` (manifest + codex-audit-report); panel `p01-clean-proof-claude`; gate `mise run clean-proof:evidence` |

## Closed negatives — do not re-litigate without new evidence

**Reopening discipline (worked example, 2026-06-10):** scan-cache v3 closed
"index request records" as measured ranking noise with no consumer. Phase 2's
handoff packet and midtask mining became new consumers — new evidence — so v4
reopened it *narrowly*: requests are extracted again for trajectory surfaces
while the original noise finding stands untouched (both rankers still exclude
them from general ranking). Closed negatives are reopened by new consumers or
new measurements, never by forgetting why they closed.

- **Query expansion:** early d≈0.47 was an n≈8 artifact; collapsed at n=121
  (d=0.10, p=0.26). Stays opt-in.
- **RRF fusion-weight tuning:** swept; all cells within noise. k0=60, equal
  weight stands.
- **Facts FTS5 BM25 as default:** parity, not better (1.43 vs 1.50). Opt-in.
- **History coverage-gate relaxation (3-of-N → 2):** added filler-word noise,
  regressed precision; BM25 is the fix, not gate tuning.
- **Scoped-floor (recall floor via backfill facts):** dead end; backfill facts
  don't help.
- **Model2Vec fusion on history:** significantly *worse* than BM25 on precision
  at production scale (t=−6.33 on 418k-record entire.io, t=−2.84 on entire-cli;
  2026-06-12 capstone), with no useful/1k gain. The 06-11 midtask positive was a
  sign-test artifact that didn't generalize. History fusion requires a
  Gemma-class embedder; do not enable it with the built-in M2V.

## Adding a row

When a measurement gates a ship/park decision, append it here the same day:
date, corpus + task count, what was compared, the headline number with its
significance, the decision it served, and where the repro lives.
