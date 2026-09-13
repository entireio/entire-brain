# Source-span selector experiment — 2026-09-13

This experiment replaces generated quotes with IDs bound to exact source blocks and separates corroboration from required dependencies. It uses fresh retrieval from merged Brain main `23101e06`, includes the prior failures as regressions, and adds four questions from two previously unused CLI sessions. This remains local development evidence, not a production default or release-quality benchmark.

## Fresh reader results

| Cohort | Memory bytes | Whole-message baseline | Whole-message 50% reserve | Deterministic blocks | Model-selected blocks |
|---|---:|---:|---:|---:|---:|
| correction | 512 | 0/2 | 2/2 | 2/2 | 2/2 |
| correction | 4096 | 2/2 | 2/2 | 2/2 | 2/2 |
| adverse | 512 | 8/12 | 6/12 | 6/12 | 12/12 |
| adverse | 4096 | 12/12 | 12/12 | 12/12 | 12/12 |
| real_regression | 2048 | 2/8 | 6/8 | 4/8 | 8/8 |
| real_regression | 8192 | 8/8 | 8/8 | 8/8 | 8/8 |
| new_sessions | 2048 | 0/8 | 0/8 | 4/8 | 8/8 |
| new_sessions | 8192 | 8/8 | 6/8 | 8/8 | 8/8 |

Each question has two independently sampled selectors and two fresh reader repetitions. A selector result is shared across its two budgets within a repetition. Identical reader requests share one response only within a repetition. Counts are repeated responses, not independent questions. There are 15 questions total.

## Complete required evidence cited

| Cohort | Memory bytes | Whole-message baseline | Whole-message 50% reserve | Deterministic blocks | Model-selected blocks |
|---|---:|---:|---:|---:|---:|
| correction | 512 | 0/2 | 2/2 | 2/2 | 2/2 |
| adverse | 512 | 6/12 | 6/12 | 6/12 | 10/12 |
| real_regression | 2048 | 2/8 | 6/8 | 4/8 | 8/8 |
| new_sessions | 2048 | 0/8 | 0/8 | 2/8 | 8/8 |

Coverage now checks exact, pre-labeled source paragraphs against the original source bytes. A span never gets credit merely for sharing a parent message ID. Labels and paragraph requirements were frozen before retrieval and model calls. Correct multiple-choice answers can still lack required evidence, especially the future-effective-date case whose two source blocks exceed the tight budget.

At 8192 bytes on the new sessions, the fixed reserve dropped the checkpoint-shard evidence in both runs and the reader abstained; the baseline retained it. The selector answered all eight correctly, but the strict required-paragraph citation metric was 7/8. In the remaining Trail-correction answer, the reader cited the explicit correction and webhook event inventory rather than the particular pre-labeled explanatory paragraph. Those citations support the answer; the conservative metric is retained unchanged.

## Implementation and controls

`memory_span_selector.py` deterministically splits already-retrieved full messages at blank lines, keeping fenced code and contiguous tables intact. It preserves Markdown, Unicode and line endings. IDs bind branch, source identity, whole-source hash and UTF-8 byte offsets. They are stable across retrieval ordering for the same source version; changing the source creates new IDs. The archived catalogue resolves every reader citation to its exact bytes.

The model sees all available blocks grouped by source, the task and timestamps. It sees no answer choices, labels or expected output. It assigns relevance/status and optional typed links. `corroborates` never makes its neighbor mandatory. `adds_requirement` requires both endpoints to be direct and applicable; these pairs and unresolved conflicts are packed together. Replaced and superseded blocks are excluded as standalone choices; required groups can retain their linked companions. These judgments are still fallible. Exact anchors establish bytes, not entailment.

The selected condition serves original source blocks only. Retrieved facts provide source expansion but are not themselves served in that condition. The deterministic-block control uses exactly the same source universe and representation with retrieval order and no model judgments. This separates block granularity from selection. The two whole-message controls retain the existing combined and fixed-reserve algorithms. All conditions obey the same per-cohort memory byte budgets; selector input and computation are additional.

Packing metadata records actual returned IDs/counts, omitted IDs and supplied-source availability separately from source records. Its scope is the supplied candidate set, never complete knowledge of the repository. The production MCP/Graph adapters are not modified.

All 30/30 selector responses passed binding, ID-set and structural validation; 0 used the predeclared deterministic-block fallback. Normalized judgments differed across repetitions for 8/15 questions. This is not an error rate: both judgments can retain the evidence. Per-response assessments, links and original endpoints are retained for review. No prompt tuning or retries occurred after freezing.

The primary-assistant source review found remaining semantic overreach: a broad earlier CLI-inspection caveat was labeled replaced by a narrower statement about review webhooks, and a multi-claim client/CI paragraph was linked as replaced by a webhook description even though some client-scoped statements can remain true. These packets retain decisive evidence, but the relationships must not become persistent, global supersession facts. The full qualitative review is retained as `semantic-review.md`.

## New session pilot and limits

The new cases cover a corrected discussion of Trail creation, the boundary around review-approval synchronization, checkpoint-shard derivation, and a recorded agent handoff. Four complete assistant messages come from two CLI conversations; original transcript hashes, lines, timestamps and message UUIDs are retained locally. Earlier verbatim excerpts simulate stored memory. Answers concern what those historical messages recorded, not present-day server behavior or an independently verified implementation.

The primary assistant selected the cases and authored the labels. The two new sessions were previously unused in this experiment series, but this is not independently adjudicated or held-out confirmation. The four earlier real-session questions are correlated and intentionally retained regressions. Synthetic source IDs retain descriptive names. The run measures retrieval/selection/reading components, not actual distiller error rate or end-to-end coding quality.

## Latest PRs considered

See [the PR review and applied constraints](SPAN-SELECTOR-PR-NOTES.md). In particular, merged Brain #251 informed branch-scoped identity checks, #252 required rerunning retrieval on the new identity resolver, and open Brain #253 informed count/truncation checks. Graph #248 reinforced the separate recall-versus-selection control. Open Graph #233/#250 informed the limits placed on evidence-state and completeness claims. CLI Unicode/case-folding proposals informed anchor tests and source-discovery limitations. Open proposals were not treated as merged contracts.

## Verification and cost

The updated Brain binary builds, and focused Go retrieval-context, fact-integrity and declared-index tests pass. The 48 Python tests comprise 21 prior integrity tests, 10 first-selector tests and 17 new span-selector tests. A separate audit rebuilt all 240 packet/score rows and checked byte ceilings, immutable source anchors, source/request/event hashes, actual ID counts, schedule coverage and citation coverage.

The provider audit found 0 provider errors, 0 failed reader rows and 0 rows with unknown citation IDs. Tools were disabled and no unexpected tool events were recorded. codex-cli 0.154.0 ran gpt-5.6-sol with low effort, ephemeral contexts and fresh temporary working directories. The model alias is fixed, not an immutable backend snapshot; normal CLI base instructions remain, and the CLI exposes no hard output-token cap.

- Selector: 30 calls; 339,694 input tokens (0 cached, included in input); 23,526 output tokens; 636.0 summed call-seconds.
- Reader: 195 calls; 1,795,672 input tokens (93,184 cached, included in input); 15,666 output tokens; 1471.1 summed call-seconds.

Concurrent calls make summed latency different from wall time. No provider dollar cost was reported. The selector is additional model work, so equal memory bytes do not imply equal cost.

Artifacts are local under ignored `bin/memory-span-selector-20260913/`: frozen labels and protocols, fresh retrieval, immutable catalogues, source provenance, model requests/events, scores, costs, PR snapshots, semantic review and scripts. Private session transcripts remain in ignored artifacts. Work is uncommitted and unpushed.
