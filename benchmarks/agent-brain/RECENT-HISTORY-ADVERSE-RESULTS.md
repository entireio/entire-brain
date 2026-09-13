# Adverse recency and real-session evaluation — 2026-09-13

The 50% reserve is a tradeoff, not a general improvement. It helps with corrections
but can crowd out established older policy in favor of recent irrelevant or
speculative passages. Keep it opt-in pending a relevance-aware packing policy.
No packing, retrieval ranking or distillation algorithm was changed during these
evaluations; the only harness adjustment labels real-session fixtures accurately.

## Frozen adverse cases

Six new cases were written and labeled before retrieval or model calls: cosmetic
distractions, unconfirmed proposals, another branch's rules, repeated corrections,
an additive requirement needing old and new evidence, and a future-effective rule.
The reserve stayed at 50%, k at 4, and the reader at gpt-5.6-sol/low. Three
repetitions used fresh ephemeral contexts, no tools, and shuffled blinded inputs.
Identical requests shared responses only within each repetition.

| Memory budget | Raw history | Baseline combined | 50% reserve |
|---|---:|---:|---:|
| 512 bytes | 9/18 | 12/18 | 9/18 |
| 4096 bytes | 18/18 | 18/18 | 18/18 |

| Adverse case, 512 bytes | Baseline | Reserve |
|---|---:|---:|
| corrected-again | 0/3 | 3/3 |
| future-effective-date | 3/3 | 0/3 |
| old-and-new-required | 0/3 | 3/3 |
| other-branch | 3/3 | 3/3 |
| recent-distraction | 3/3 | 0/3 |
| recent-speculation | 3/3 | 0/3 |

At 512 bytes, the reserve fixed the repeated-correction and additive-requirement
cases, but regressed distraction, speculation and future-effective-date cases.
Every reserve regression was an abstention after older evidence was omitted,
not a confident adoption of the speculative or future policy. The baseline had
six confident wrong answers in its two failing cases; the reserve had nine
abstentions and no confident wrong answers. Binary accuracy and the cost of a
wrong action versus an abstention therefore tell different stories.

## Real sessions: original excerpt pilot

Four questions use four completed sessions from one repository, `my-entire`:
the changed conference logo, its changed HUD height, the move from Kannada to
cycling major Indian languages, and six original Marvin moods plus four added
later. They are correlated questions from a convenience sample, not four projects.

Ten exact paragraphs were selected before outcomes, with original timestamps,
file hashes, line numbers, message UUIDs and character offsets. All source
records belonged to the same actual branch; the isolated fixture calls that
branch `main`. Earlier verbatim excerpts simulate stored memory. This is not a
new distiller evaluation, whole-session export test or production branch-ownership
test. Answers are supported by historical conversation claims, not a fresh code
or external-service audit. The primary assistant authored the labels; they have
not received independent human adjudication.

| Memory budget | Raw history | Baseline combined | 50% reserve |
|---|---:|---:|---:|
| 512 bytes | 3/12 | 3/12 | 9/12 |
| 4096 bytes | 8/12 | 6/12 | 7/12 |

This excerpt experiment exposed a corpus-construction problem. The logo paragraph
called it a wordmark rather than a conference logo; the isolated mood lists did
not say Marvin. With k=4, the required latest logo and mood evidence were not
returned even at the larger budget. Some correct choices were inferred despite
missing required passages: 512-byte reserve accuracy was 9/12 while complete
evidence availability was only 6/12. Correct multiple-choice selection is not
proof of grounding. No nonexistent citation IDs were returned.

## Full-message follow-up (post hoc)

A separately retained retrieval-only diagnostic indexed the full original
messages, preserving their surrounding context and deduplicating paragraphs
from the same message. The same queries and k=4 then recovered the required
quotes for all four questions. The excerpt misses must not be generalized to
Brain's full-message retrieval.

We consequently ran a separately labeled reader follow-up over six full original
messages from the same four sessions. Queries, answers, earlier memory claims,
model, effort and reserve stayed fixed. Budgets of 2048 and 8192 bytes were frozen
before this follow-up's model outputs. The changed message boundaries and budgets
make this a post-hoc sensitivity check, not a held-out confirmation or a direct
accuracy comparison with the excerpt cohort.

| Memory budget | Raw history | Baseline combined | 50% reserve |
|---|---:|---:|---:|
| 2048 bytes | 3/12 | 3/12 | 9/12 |
| 8192 bytes | 12/12 | 12/12 | 12/12 |

At 2048 bytes, the reserve retains the latest conference message for both logo
and height; the baseline combined packet and raw-history packing retain older
messages. The two full Marvin messages do not fit this budget, so every condition
lacks their evidence. At 8192 bytes every condition retains all required messages.
Messages are kept whole, never truncated to manufacture evidence coverage.

This shows why both relevance and source boundaries matter. A blind recency
reserve can help versioned decisions yet harm stable older constraints. The
next policy should combine relevance, recency and explicit supersession signals,
and be evaluated against raw history with these adverse cases retained as
regressions. These runs do not support making a fixed 50% reserve the default.

## Reproducibility and limitations

The first pilot contains 141 actual calls and 180 scored rows. The full-message
follow-up contains 66 actual calls and 72 scored rows. All 207 calls used Codex CLI
0.154.0, gpt-5.6-sol and low reasoning effort. The model alias was fixed but the
provider did not expose an immutable backend revision. Normal CLI base instructions
remained. Output used the same strict JSON shape; the CLI offered no hard output
token cap. UTF-8 byte parity is not token or retrieval-compute parity.

Twenty-one focused tests pass. Every scored row was recomputed from retained
answers; request, fixture, event and original source hashes and packet byte ceilings
were verified. Provider events contained no tool use or errors. Citation metrics
validate IDs and source availability, not semantic entailment.

Artifacts are local under ignored `bin/memory-reserve-adverse-20260913/`: frozen
corpora and protocols, product retrieval outputs, blinded requests, provider
events, usage, scores, full-message follow-up, original source snapshots, exact
scripts and `artifact-manifest.json`. The synthetic corpus is also checked into
the working tree as `fixtures/memory-recency-adverse.json`. Real transcripts and
fixtures remain in ignored local artifacts. No commits or pushes were made.

Combined reported usage: 1,862,689 input tokens (33,280 cached, included in input), 15,049 output tokens. Summed call latency was 1790.5 seconds; concurrent calls make this different from wall time. No provider dollar cost was reported.
