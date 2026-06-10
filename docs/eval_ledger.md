# Eval ledger

One row per retrieval-quality measurement that gated a decision. The task sets,
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

## Closed negatives — do not re-litigate without new evidence

- **Query expansion:** early d≈0.47 was an n≈8 artifact; collapsed at n=121
  (d=0.10, p=0.26). Stays opt-in.
- **RRF fusion-weight tuning:** swept; all cells within noise. k0=60, equal
  weight stands.
- **Facts FTS5 BM25 as default:** parity, not better (1.43 vs 1.50). Opt-in.
- **History coverage-gate relaxation (3-of-N → 2):** added filler-word noise,
  regressed precision; BM25 is the fix, not gate tuning.
- **Scoped-floor (recall floor via backfill facts):** dead end; backfill facts
  don't help.

## Adding a row

When a measurement gates a ship/park decision, append it here the same day:
date, corpus + task count, what was compared, the headline number with its
significance, the decision it served, and where the repro lives.
