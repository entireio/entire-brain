# Paper outline — BrainMark (plan 0.11)

Skeleton for the NeurIPS 2027 Datasets & Benchmarks submission. Every claim
below names the repo artifact that backs it — this outline is written to be
filled in from evidence that already exists (or is pre-registered to exist),
not from aspiration.

## Title (working)

*BrainMark: Does a Second Coding Session Reuse What the First Learned? A
Sealed, Mechanism-Primary Benchmark for Cross-Session Memory in Coding Agents*

## Abstract (write last)

One paragraph: the untested product claim (`PREREGISTRATION.md` §1), the
paired five-arm design (§2), the mechanism-primary metric and why (§3), the
headline result once it exists (Phase 3), the honest power/contamination
caveats (§7, §10).

## 1. Contributions

State these as falsifiable claims, each with its evidence artifact:

1. **First sealed, dual-reviewed benchmark for cross-session memory reuse in
   coding agents.** Evidence: `SEAL-MANIFEST.json` (task-set integrity),
   `SEAL_PROTOCOL.md` + `seal.py`'s `_promote_v2` (dual independent review,
   Cohen's kappa enforced, not merely reported).
2. **Mechanism-primary pre-registration**, not score-primary. Evidence:
   `PREREGISTRATION.md` §3 (why a call-count, not accuracy, is primary) +
   `validity/` (the construct-validity check that makes the choice
   accountable rather than asserted).
3. **N-system, same-bytes comparison.** Five arms (`no_brain`, `full_brain`,
   `mem0`, `graphify`, `cmm`) consume the *identical* pinned session-A bytes
   through the *identical* envelope (`memsources/base.py`, `prompts.py`'s
   symmetry gate) — competitor differences are attributable to the memory
   source, not to prompt or packet-delivery asymmetry.
4. **Explicit contamination treatment**, not silence. Evidence:
   `PREREGISTRATION.md` §10 (paired-design argument, floor-compression check,
   fresh-split concordance, age probe) and the fresh post-cutoff split
   (plan 0.2, mined separately, never pooled).
5. **A released, reproducible harness with a machine-checked publication
   gate.** Evidence: `release/make_release.py` (anonymization + grep-gate),
   `release/croissant_gen.py` (Croissant + Responsible-AI metadata),
   `MAINTENANCE.md` (versioning-by-seal so a quoted number stays resolvable).

## 2. Related work

| area | representative work | relation to BrainMark |
|---|---|---|
| conversational/long-context memory | LoCoMo, LongMemEval | measure retrieval over a **chat transcript**; say nothing about whether a **second coding session** is cheaper because of the first. BrainMark's own LoCoMo/LME numbers (memory bench, see devenv campaign notes) motivated this gap directly: strong retrieval-substrate scores did not imply the product claim was true, because nothing had tested it. |
| managed memory products | mem0 (OSS + managed), Letta (MemGPT lineage) | named competitor arms (`mem0_source.py`) or discussed as the closest prior art for "agent remembers across sessions"; BrainMark's contribution over these is the **paired task design** (A then B on the same repo) rather than a new memory mechanism. |
| code-memory tools | Graphify (YC code-memory), codebase-memory-mcp (cmm) | named competitor arms (`graphify_source.py`, `cmm_source.py`); positioned as the nearest **code-specific** memory baselines, distinct from general conversational memory. |
| agentic coding benchmarks | SWE-bench, SWE-bench Verified, SWE-bench Multilingual, SWE-bench Multimodal, SWE-rebench | task **source**, not task **design** — every existing member of this family scores one isolated task. BrainMark's pairing (`mine_pairs.py`) and fresh post-cutoff split (`mine_fresh.py`, plan 0.2) are additive to this family, not a competing benchmark; DATASHEET.md is explicit that no upstream content is redistributed. |
| agent-memory surveys / position papers | (cite current agent-memory survey literature at write time) | used to justify why "does memory help" has remained an assumed-rather-than-measured claim, and to situate the mechanism-primary choice (§3) against prior score-primary evaluations. |

**Novelty claim, precisely stated:** not the memory mechanism (entire-brain
is not the paper's subject), not the task source (SWE-bench is upstream),
but the **cross-session pairing + mechanism-primary measurement + sealed
same-bytes multi-competitor comparison**, together. Each piece alone has
precedent; the combination, sealed and pre-registered, does not.

## 3. Method (pointers, not restatement)

Design: `PREREGISTRATION.md` §2. Primary metric: §3, `mechmetrics.py`.
Gates: §5. Mining: `DATASHEET.md` §Collection process. Sealing: §7's own
document, `SEAL_PROTOCOL.md`. Do not re-derive these in the paper without a
pointer back to the frozen document — the pre-registration is the source of
truth; the paper's method section is a readable summary of it, not a second
copy that can drift.

## 4. Experiments

Structure follows the runbook (plan PART 1 "Runbook + cost", PART 2 Phases
1–4): null test (machinery-inert check) → pilot (harness debugging only) →
confirmatory (frozen n, per §11's power rule) → fresh-split robustness →
validity check (§12). Report headline (`report.py`'s `full_brain` vs
`no_brain`) and competitor table together, never the headline alone.

## 5. Limitations

Copied from the plan's own residual-risk list (PART 2, "Residual risks we
cannot engineer away") — stated in the paper, not left in an internal
planning doc:

1. **Contamination residue.** The fresh split is small and only
   claude-training-cutoff-clean, not clean for every model family evaluated.
   Mitigated but not eliminated by `PREREGISTRATION.md` §10.
2. **Null or tiny effect is a possible, publishable outcome.** BrainMark is
   valuable as a *measurement instrument* even if the headline effect is
   small or non-significant — that is a weaker claim than "the product
   works", and the paper states it as such rather than reframing a null
   result as a win.
3. **De-anonymization guessability.** Five *named* competitors
   (`mem0`/`graphify`/`cmm`) plus one anonymized `system_x` arm is a
   recognizable shape; a knowledgeable reviewer may guess `system_x`'s
   identity. `release/make_release.py` anonymizes what can be mechanically
   anonymized; this residual risk is disclosed, not hidden.
4. **SWE-bench-derivative fatigue.** The benchmark's originality rests on the
   pairing and the mechanism metric, not on a novel task source — reviewers
   who weight "yet another SWE-bench variant" negatively should weigh the
   pairing/metric contribution specifically (§1, items 2–3).
5. **Backend confound.** Results are reported **per-backend, never pooled**
   (extends `PREREGISTRATION.md` §7's "cross-cell comparisons are
   confounded" rule to the model-backend axis specifically).
6. **Single-vendor generality in v1.** All-GPT backends in the confirmatory
   run (plan 0.5); cross-vendor (Claude/Gemini) generality is a planned
   expansion, not claimed in v1. Reviewers may ask for it directly — the
   answer is: planned, out of scope for this submission, reported
   per-backend so a future cross-vendor addition composes cleanly rather
   than requiring a redesign.
7. **Rater affiliation.** Both seal-review raters are project-affiliated
   (the benchmark author + one teammate) — disclosed explicitly in
   `SEAL_PROTOCOL.md`'s disclosure template and repeated here, not buried.
8. **Mechanism-primary is a methodological choice, not a neutral default.**
   Some reviewers will prefer an outcome-primary (resolved-rate) metric; the
   pre-registration's own rationale (`PREREGISTRATION.md` §3, "why a
   mechanism endpoint is primary") is the defense, backed by the construct-
   validity check (§12) rather than asserted alone.

## 6. Ethics

- **Human subjects:** the two seal raters and up to two validity-labeling
  raters are project-affiliated adults performing a documented review task
  (not a study of them) — no personal data about a third party is collected.
  Rater identity is disclosed per `SEAL_PROTOCOL.md`'s template; no IRB
  applies (internal quality-review labeling of code artifacts, not human-
  subjects research on the raters).
- **Dataset content:** no PII. `DATASHEET.md` §Composition: no SWE-bench
  problem statement, patch, or test text is copied into a record — only
  instance IDs, commit SHAs, and derived sha256/score metadata; the release
  pipeline (`release/make_release.py`) additionally strips machine-local
  paths and personal names from the harness code and docs before publication.
- **Dual-use:** the benchmark measures a coding-agent capability (efficient
  code navigation with memory); no plausible harmful dual-use beyond the
  ordinary dual-use profile of coding-agent capability research generally.
- **Competitor fairness:** named competitors (`mem0`, `graphify`, `cmm`) are
  evaluated under the SAME pinned bytes, SAME prompt scaffold, and SAME
  gates as the benchmark author's own product (`full_brain`/`system_x`) —
  `PREREGISTRATION.md` §5 and `prompts.py`'s symmetry gate are the technical
  guarantee; `MAINTENANCE.md`'s leaderboard intake section extends the same
  fairness discipline to future third-party submissions.

## 7. Compute disclosure

Fill in from the actual runbook once Phases 2–3 execute; the table shape is
fixed now so no run happens without its cost being recorded in the same
place it will be reported:

| phase | arms × pairs × reps | approx cost | model tier | source |
|---|---|---|---|---|
| null test | 6 arms × 3 dev pairs × 2 reps | ~$15 | pilot | `PREREGISTRATION.md` §6 |
| pilot | 6 arms × 10 dev pairs × 2 reps | ~$60–120 | gpt-5.6-sol (Azure) | plan PART 2 Phase 2 |
| second-model smoke | 6 arms × 3 pairs | ~$20 | gpt-5.6-terra | plan PART 2 Phase 2 |
| validity labeling | ~30 sessions × 2 raters | ~8h/rater (human time, not $) | n/a | `validity/LABEL_PROTOCOL.md` |
| confirmatory primary | 6 arms × n(power rule) pairs × 2 reps | ~$400–900 | gpt-5.6-sol (Azure) | plan PART 2 Phase 3, `PREREGISTRATION.md` §11 |
| confirmatory generality | 6 arms × 60-pair subset | ~$150–350 | gpt-5.6-terra (or gpt-5) | plan PART 2 Phase 3 |
| fresh split | 6 arms × 25–40 pairs | ~$100–250 | matches primary | plan 0.2, `PREREGISTRATION.md` §10 |
| Docker grading | all of the above | $0 (self-hosted) | n/a | `grade.py` |

Report **actual** spend beside this pre-estimate once each phase completes
(`MAINTENANCE.md`'s versioning-by-seal makes each phase's manifest the
place that actual cost gets attached to).

## 8. NeurIPS checklist -> repo-artifact map

| checklist item | answer | evidence artifact |
|---|---|---|
| Claims match contributions and scope | Yes | §1 above states each contribution as a claim with an evidence pointer; `PREREGISTRATION.md` §1 states the single claim under test |
| Limitations discussed | Yes | §5 above (verbatim from the plan's residual-risk list); `DATASHEET.md` "Known limitations" |
| Theory assumptions and proofs | N/A | no formal theoretical results; the estimator's assumptions are stated in `PREREGISTRATION.md` §8 instead of proved |
| Experimental result reproducibility | Yes | `SEAL-MANIFEST.json` pins every input; `config.json` pins seeds/models/concurrency; `REPRODUCIBILITY.md` states exact toolchain/commit pins |
| Open access to data and code | Yes, anonymized for review | `release/make_release.py` (anonymized harness copy + grep-gate), hosting = HF dataset (pair metadata only) + Zenodo DOI per plan 0.10; de-anonymized on acceptance |
| Experimental setting/details specified | Yes | `PREREGISTRATION.md` §2 (design), `config.json` (all pins), `prompts.py` (exact scaffold) |
| Error bars / statistical significance | Yes | `report.py`'s 20k-draw bootstrap CI + median beside the geomean (`PREREGISTRATION.md` §3, §8); leave-one-out range also reported |
| Compute resources disclosed | Yes | §7 above, filled in with actuals per phase |
| Code of ethics followed | Yes | §6 above |
| Broader impacts discussed | Yes | §6 "Dual-use" paragraph |
| Safeguards for release of data/models with misuse potential | N/A | no model weights released; dataset is task-pair metadata pointing at an already-public benchmark |
| Licenses for existing assets respected | Yes | `DATASHEET.md` "Distribution and licensing" (SWE-bench/Multilingual MIT, per-repo licenses noted, no upstream content redistributed) |
| New assets (the pair dataset) documented | Yes | `DATASHEET.md` (full Gebru et al. datasheet), `release/croissant_gen.py` (machine-readable Croissant + RAI metadata) |
| Crowdsourcing / research with human subjects details | Yes | `SEAL_PROTOCOL.md` (seal raters), `validity/LABEL_PROTOCOL.md` (validity-labeling raters); both project-affiliated, disclosed, no third-party subjects |
| IRB approvals, if applicable | N/A, reasoned | internal review-task labeling by project-affiliated adults on code/text artifacts is not human-subjects research; reasoning stated in §6 rather than asserted |

Every "Yes" row above must resolve to a real, checked-in artifact before
submission — this table is filled in with pointers now specifically so that,
at submission time, it is an audit against existing files rather than a
last-week scramble to produce them.
