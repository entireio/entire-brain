# Evidence-selector development experiment — 2026-09-13

The model-assisted selector repairs the synthetic recency regressions but is not ready to replace either control. Real messages expose both brittle quote reproduction and a semantic dependency error. Keep this module experimental; no production retrieval, distillation or fact status was changed.

## Fresh reader comparison

| Cohort | Memory bytes | Raw history | Combined baseline | 50% reserve | Selector with fallback |
|---|---:|---:|---:|---:|---:|
| correction | 512 | 3/3 | 0/3 | 3/3 | 3/3 |
| correction | 4096 | 3/3 | 3/3 | 3/3 | 3/3 |
| adverse | 512 | 9/18 | 12/18 | 9/18 | 18/18 |
| adverse | 4096 | 18/18 | 18/18 | 18/18 | 18/18 |
| real | 2048 | 3/12 | 3/12 | 9/12 | 0/12 |
| real | 8192 | 12/12 | 12/12 | 12/12 | 12/12 |

Three reader repetitions per question. These are 11 development questions: one original correction, six adverse synthetic questions, and four correlated questions from full historical messages in one repository. Counts are repeated responses, not independent tasks. Labels were authored by the primary assistant and have not received independent adjudication.

The real cohort reuses full messages from four historical sessions in one repository. Earlier verbatim excerpts simulate stored facts; they are not new distiller outputs. Their common original branch is represented as logical `main`. Answers concern recorded conversation claims, not a fresh code audit. Synthetic source IDs retain descriptive names and are not an adversarially anonymized corpus. Original transcript provenance remains in the preceding local adverse-evaluation artifacts.

## What was implemented

`memory_evidence_selector.py` takes the question plus the union of retrieved raw passages and source passages backing selected facts. It sees neither answer choices nor gold labels. A fixed model classifies relevance and applicability and proposes quoted replacement, addition or conflict links. Same-branch checks, chronological ordering, complete ID coverage and exact substring checks reject invalid responses.

The deterministic packer prioritizes relevant applicable evidence, orders explicit replacements, and keeps additive/conflicting groups whole. Fact backfill must retain those same additive dependencies. The reader receives original text, never selector classifications or generated summaries. Stored facts and production supersession relationships are not modified. These proposed raw-source links are fallible model judgments, not existing verified Brain graph edges.

The selector call is shared across both budgets and all three reader repetitions for each question. Consequently the experiment measures reader variation, not selector reliability across repeated classifications. Any invalid selector output falls back to the combined baseline without retry.

## Failure analysis

All seven synthetic selector responses passed structural and exact-quote checks. At 512 bytes, the selector answered all 18 adverse-case repetitions correctly, but complete required evidence was present and cited in only 15/18. For the future-effective-date case, the current 10-second policy fits but the future announcement does not. A correct answer there does not satisfy the fixture’s conservative requirement to include both passages.

Three of four real-session responses were rejected. The logo and height links omitted Markdown bold delimiters in copied quotes; the Marvin six-mood quote did the same. Their broad interpretations were plausible, but they did not provide exact source spans. All results retain the specified full-baseline fallback; no quotes were silently repaired.

The language response passed quote checks but proposed an excessive dependency: it linked the explicit language-cycling request to a much longer later implementation note as an addition. The later note corroborates implementation rather than adding a language requirement. Atomic packing therefore excluded the short request at 2048 bytes while older Kannada evidence fit. This is a semantic relationship error, invisible to exact-quote validation. The primary assistant reviewed all eleven responses; the qualitative findings are retained in `semantic-audit.json`.

Both full Marvin messages exceed the 2048-byte budget. Reordering alone cannot solve that case. Source-span retrieval with stable context and identifiers needs a separate evaluation.

## Controls and limits

Inputs reuse archived product retrieval at k=4; retrieval rankings were not rerun or changed. Every raw hit was matched to its entire fixture passage, allowing newline differences only. This run packs the canonical fixture text (including its original Markdown), whereas earlier runs packed raw-export text with extra trailing/newline bytes. Thus this table uses fresh controls and must not be treated as byte-identical to earlier report packets. All four current conditions use the same source representation and exact UTF-8 JSON byte budgets.

Prompts, code hashes, input hashes and fallback policy were frozen before selector calls. Reader packets and the shuffled schedule were frozen before reader calls. Identical reader requests share responses only within each repetition. No tuning or retries occurred after viewing outputs. The selector has the same available source universe as the combined controls, but receives it before compression: extra model work is part of the treatment.

The Codex CLI ran gpt-5.6-sol with low effort, fresh temporary directories, ephemeral sessions and shell/apps/web/multi-agent tools disabled. Event audits checked that no tools or provider errors occurred. The model is a fixed alias, not an immutable backend snapshot. Normal CLI base instructions remain; the CLI provides no hard output-token cap. This is component development evidence, not release proof or an end-to-end coding/distiller evaluation.

## Cost and verification

- Selector: 11 calls; 105,430 input tokens (0 cached, included in input); 3,409 output tokens; 126.4 summed call-seconds.
- Reader: 180 calls; 1,618,216 input tokens (66,560 cached, included in input); 12,829 output tokens; 1359.5 summed call-seconds.

Calls ran concurrently; summed latency is not wall time. No provider dollar cost was reported. Selector compute is additional to the reader and is not a free retrieval improvement.

Thirty-one focused tests pass (21 existing harness tests and 10 selector tests). A separate deterministic audit recomputed all 88 packets and 264 score rows from retained inputs and answers, checked budgets, source/request/event hashes, fallbacks, citations and schedule coverage. Exact source availability and citation IDs do not prove semantic entailment.

Local artifacts are under ignored `bin/memory-evidence-selector-20260913/`, including frozen protocols, input copies, selector and reader calls, costs, semantic review, scores, integrity audit and script snapshots. The report and experimental module contain no private session transcripts. No commits or pushes were made.

## Next experiment

Use preassigned span IDs so the model selects existing evidence instead of reproducing Markdown. Separate corroboration from a strict must-include dependency, and test that distinction with the language failure retained. Then evaluate on new, independently labeled sessions before considering a default policy. Existing quote checks should remain strict; the next candidate should change the representation rather than silently accept altered quotes.
