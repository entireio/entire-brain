# Compact evidence selector and multi-session diagnosis

Date: 2026-09-13. Local opt-in experiment. This follow-up reduces selector work and diagnoses the four multi-session failures while preserving the original labels and results. See the [CLI contract](../../docs/recall-evidence.md) and [original pilot](RECALL-EVIDENCE-RESULTS.md).

## Changes

- `sparse_v2` groups field metadata once, sends short request-bound aliases, and asks for selected/supporting tuples instead of a full JSON assessment for every candidate. Original citation IDs, source hashes, text and offsets remain local and unchanged. Catalogue and request digests bind the alias mapping. Unlisted blocks become unrelated/unknown for packing; this is a fallible selection judgment, not proof of irrelevance.
- Conversation retrieval uses positive lexical coverage and simple plural matches. The shared history scorer’s strict coverage requirement and long-record/code-specific adjustments could discard relevant conversational sessions. Candidate pools now interleave speakers and admit matched sessions in rounds, with at most 128 blocks and a 24 KiB charged candidate budget.
- `joint_evidence` groups records needed together for a complete answer, including counts across sessions. Whole-group packing and structural validation remain mandatory. No model-written quotes or persistent inferred relationships are introduced.
- Brain was fast-forwarded to `399ce473`, including [#254](https://github.com/entireio/entire-brain/pull/254) (MCP trace containment for an unbound server) and [#255](https://github.com/entireio/entire-brain/pull/255) (Radar evidence re-record). These do not expose the experimental evidence mode through MCP. Existing local work was retained.

## Frozen comparison

Reused the same 28 LongMemEval oracle questions, canonical sessions, published labels, `k=128`, 8,192-byte evidence-array cap and original reader/judge prompts. Selector, reader and judge used `gpt-5.6-sol`, effort low, through Codex. One attempt per stage, no retries; fresh reader and judge per arm. The four diagnosed multi-session cases also received a compact-format-only comparison with the original candidate pool. This isolates the protocol bundle from the retrieval changes, not each prompt/schema edit individually.

The questions were inspected during diagnosis and are now a **development/regression set**, not unseen validation. Before live calls, an offline audit caught two problems in the first proposed pool: history length penalties still canceled partial matches, and short assistant boilerplate outranked user accounts. That pool is retained separately; the corrected code and binary hashes were frozen before live calls. Gold labels never entered retrieval, selector or reader requests.

| Category | Original deterministic | Original selector | New deterministic | New selector |
|---|---:|---:|---:|---:|
| abstention | 4/4 | 4/4 | 4/4 | 4/4 |
| knowledge-update | 1/4 | 3/4 | 4/4 | 4/4 |
| multi-session | 0/4 | 0/4 | 2/4 | 3/4 |
| single-session-assistant | 1/4 | 4/4 | 3/4 | 4/4 |
| single-session-preference | 1/4 | 3/4 | 3/4 | 2/4 |
| single-session-user | 0/4 | 1/4 | 3/4 | 3/4 |
| temporal-reasoning | 1/4 | 4/4 | 4/4 | 4/4 |
| **All** | **8/28** | **19/28** | **23/28** | **24/28** |

New selector modes: `{"model": 26, "fallback": 2}`. New selected-arm errors: 0; deterministic-arm errors: 0. Every attempted question remains in its denominator. Compact-format-only scored 1/4 on the multi-session cases.

Aggregate accuracy improved, but **no quality loss is not established**: preference performance fell from 3/4 to 2/4. The new deterministic path reaches 23/28, only one answer below the selected path, so this run does not justify paying for selection by default. Both paths remain opt-in experimental evidence recall.

## Latency and usage

| Run | Selector median | Maximum | Calls |
|---|---:|---:|---:|
| Original 28-question pilot | 104.4 s | 1,393.4 s | 28 attempts; 26 structured results |
| Compact format, original pool | 14.56 s | 16.12 s | 4 |
| Compact format and new pool | 9.68 s | 26.45 s | 28 |

The new full-run median meets the aspirational 15-second target in this sample. New serialized selector request size: median 19,330 bytes, maximum 22,458 bytes. This excludes the provider’s own system context. Public evidence still carries full provenance and obeys the same 8,192-byte array budget.

The original latency run had abnormal wall-clock waits, timeout failures and a subsequently fixed inherited-pipe wait. This is not a controlled 104.4-to-new-median speedup estimate. Service load, cache state, model alias and one-run variation remain confounders. The compact-only comparison supports the value of the protocol bundle; it does not separately establish the effect of sparse output, aliases and prompt changes.

| Role/arm | Calls | Input tokens | Output tokens | Cached input tokens |
|---|---:|---:|---:|---:|
| compact-only | 4 | 72,743 | 991 | 13,056 |
| compact-only-reader | 4 | 36,035 | 947 | 0 |
| compact-only-judge | 4 | 34,246 | 218 | 0 |
| selected | 28 | 350,545 | 4,659 | 99,840 |
| selected-reader | 28 | 254,697 | 5,172 | 0 |
| selected-judge | 28 | 239,898 | 1,582 | 6,528 |
| deterministic-reader | 28 | 327,869 | 6,368 | 0 |
| deterministic-judge | 28 | 240,093 | 1,588 | 0 |

Selector usage missing calls: compact-only 0, new selector 0. Cached input is a subset, not additional input. No dollar cost is claimed. A model alias is recorded, not an immutable backend revision.

## Four failure diagnoses

The original candidate pool touched 9/14 labeled source turns across these cases; the new pool touches 14/14. All 28 new candidate pools touch their labeled turns. Turn coverage is only a proxy: labels can include negative/context turns, and touching a turn does not prove that every required claim survived paragraph selection.

| Case | Diagnosis and observed outcome |
|---|---|
| `gpt4_59c863d7` — model kits | Original retrieval admitted 1/4 relevant sessions. New retrieval admits 4/4 and the reader counts five kits. Compact format alone still fails because it cannot recover absent sessions. |
| `b5ef892d` — camping | Both required turns were present in the original candidate pool. The original 180-second selector timeout triggered a fallback dominated by early non-camping context. Compact format alone and the combined version return both camping records and answer eight days. This refines the initial suspicion of candidate starvation: the confirmed failure was timeout plus fallback packing. |
| `6d550036` — project leadership | Original retrieval excluded one of four labeled sessions and the selector timed out. New retrieval reaches all four. However, compact-only and combined selection produced **identical reader requests**, yet readers counted one versus two projects depending on whether a solo project counts as leading. The combined score is correct, but this difference cannot be attributed to retrieval or selection. |
| `0a995998` — clothing | All three labeled turns were in the original pool and are in the new pool. Readers count a blazer pickup and replacement boots; the published answer is three. One user turn also says the old boots need returning while saying they were exchanged, and another repeats the exchange. Treat this as unresolved event/label ambiguity, not a proven bad label. Keep the official failure; human adjudication is needed. Sparse selection also omits some labeled turns here. |

Identical-reader-request pairs in the new runs: 3; score disagreements: 1. Exact pairs and answers are retained. Reader/judge variance prevents attributing the entire score change to the selector. The deterministic project reader also sees all four labeled turns but does not pass the rubric.

## Remaining errors and regression

- `75832dbd` is the sole before-correct/after-incorrect regression. The selector returned the user’s interest in explainable AI for medical imaging, but the constrained reader declined to name publications or conferences without supplied examples. The new deterministic packet passed. This is an end-to-end selection/reader-contract regression, not a missing-session explanation.
- `0edc2aef` still fails: the selector returned no evidence for a Miami hotel recommendation despite the candidate pool containing the labeled preference turn. Preferences from earlier hotel experiences need to transfer across locations; selecting only an exact destination match is insufficient.
- `51a45a95` still fails: the reader declined to infer that a coffee-creamer coupon was redeemed at Target. The expected store is not explicit in the selected statement. Preserve the official failure; inspecting contextual associations is necessary before attributing it solely to retrieval.
- `06878be2` and `gpt4_2655b836` produced structurally invalid selector responses and fell back; both final answers passed. Neither was a timeout. The public warning records the validation stage but does not retain the raw rejected model response or the exact validation clause, so a more specific cause is not established by these artifacts.

## Verification and limits

- Reconstructed 1,804 span instances from canonical files: {"candidates": 1411, "selected": 59, "deterministic": 327, "compact-only": 7}. Full file hashes, decoded-string hashes, UTF-8 slices, IDs, counts and byte budgets matched; zero integrity issues.
- Invalid citation rows: selected 0, deterministic 0, compact-only 0.
- Focused recall, sparse-protocol, source-anchor, speaker/session coverage, privacy, distillation/pipe-timeout and persisted-decoder checks pass. `go vet ./internal/cli` passes and both comparison binaries build. The earlier full-suite Windows baseline failures remain documented in the original report; this follow-up does not claim a clean full CLI suite.
- Final review added actual metadata charging for valid RFC3339 timestamps with unusually long fractional seconds. The ordinary field allowance is retained. A regression test passes, and the final binary reproduces all 28 frozen candidate request digests and full candidate packets exactly without additional model calls. Evaluation and final binaries/source snapshots are retained separately.
- Source integrity is not semantic truth. Sparse output cannot prove all blocks were considered. Lexical candidates, source scanning and the smaller input budget can still miss relevant context. Oracle sessions do not test full-corpus retrieval.
- This is one regression run after diagnosis on public data. No general accuracy, unseen-task improvement, production latency guarantee, MCP integration or Graph integration is claimed.

Next validation: fix preference transfer and the recommendation reader contract, add safe validation-failure diagnostics, then freeze the implementation and use previously unseen cases with repeated readers/judges. Adjudicate the clothing and project-count interpretation rules before making a deployment decision.

## Reproducibility

Artifacts: `bin/memory-recall-compact-20260913/`, including protocol, source snapshots, both binaries, isolated stores, first offline pool, candidate packets, provider requests/responses, per-case scores, stage diagnosis, integrity audit, tests and a verified SHA-256 manifest. The original `bin/memory-recall-evidence-20260913/` archive is unchanged. Nothing was committed, pushed or placed in the Radar release-evidence lane.

Compact-only binary SHA-256: `d10287fbf319fc0ad96f61adab593588482f13dc6d39955a1e21ce090e6c808f`.
Combined evaluation binary SHA-256: `fcfc7fc091ded53a030e20960b87aace589857b31582a28228030a770d3da2a7`.
Final binary (`compact-diverse-final.exe`) SHA-256: `b8592abbbff71620aa6a66e116388262a77ebd426f0213eee7b117b31fb5d4a3`.
