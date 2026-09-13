# Live reader and distillation pilot — 2026-09-13

Both evaluations ran against real model calls using `gpt-5.6-sol`, low reasoning
effort, through Codex CLI 0.154.0. Raw history matched or beat the experimental
facts-plus-sources packet on the eight synthetic cases. Actual Brain distillation
on four real sessions showed useful restraint, selective omissions, and one
unsupported policy addition. These are development findings, not coding-task
success measurements or a population estimate of distillation accuracy.

## 1. Controlled reader comparison

The existing eight-case injected-error corpus and its answer keys were unchanged.
Each request ran in a fresh ephemeral context outside the repository, with user
configuration, project documents, shell, apps, web search and agent delegation
disabled. Retained events contain no tool calls. The reader saw only its task,
choices, instruction and memory packet; it did not receive gold labels or other
requests. A JSON schema constrained the answer shape. Codex's normal base prompt
remained; there was no separate hard output-token cap available through this CLI.

Three repetitions, four arms, and two memory budgets produced 192 scored rows.
Identical request hashes share one response within each repetition, including
across budgets: **93 actual calls**, not 192 independent observations. Delivery
order was shuffled with seed 20260913. The model alias and effort were fixed;
the provider did not expose an immutable backend revision.

| Memory supplied | 4096-byte budget | 512-byte budget |
|---|---:|---:|
| None | 9/24 (37.5%) | 9/24 (37.5%) |
| Raw history | 24/24 (100%) | 24/24 (100%) |
| Distilled facts only | 9/24 (37.5%) | 9/24 (37.5%) |
| Facts plus sources and raw backfill | 24/24 (100%) | 21/24 (87.5%) |

These are answer-choice scores, including abstention. The no-memory arm abstained
on every question; three of eight answer keys call for abstention, explaining its
baseline score. Facts in this corpus were deliberately wrong, incomplete or stale:
the facts-only result does **not** estimate normal Brain fact quality.

The tight-budget failure was the later retry-policy correction in all three
repetitions. The combined packet contained the old source and stale claim, leaving
no room for the correction. It led to two abstentions and one outdated answer.
Raw retrieval fit both passages and answered correctly each time. This supports
testing a reserved budget for independently retrieved recent corrections; it does
not show that source expansion is superior to raw retrieval.

Facts-only responses cited IDs absent from the delivered packet in 7/24 scored
answers per budget. These IDs were mentioned in facts' provenance fields, rather
than wholly invented identifiers: the reader treated references to unseen sources
as available citations. Raw-history and combined responses had no invalid IDs.
All required source IDs were cited in 24/24 raw-history answers, 24/24 combined
answers at 4096 bytes, and 21/24 combined answers at 512 bytes. This is an ID
coverage check, not semantic citation verification.

The UTF-8 byte ceilings are equal; actual token counts and retrieval work are not.
The combined arm is experimental harness composition, not a shipped Brain command.
All eight tasks were already development fixtures. Repeating them three times does
not create 24 independent tasks or justify a statistical generalization.

## 2. Actual Brain distillation

The built Brain binary ran `distill --agent codex --model gpt-5.6-sol --effort low
--concurrency 1 --json` on four completed local Codex sessions: three from
`my-entire` and one from `forgemark`. Selection preceded model outputs and covered
an implementation session with revised diagnoses, routine setup/history checks,
and a server startup diagnosis later corrected in the same session.

Original transcript bytes were imported into fresh isolated Brain stores. Brain
performed its normal preprocessing, taxonomy/prompt rendering, parsing and storage.
A recording executable forwarded its exact prompt and stdin to Codex, adding only
the same ambient-document/tool restrictions used for the reader. Prompts, inputs,
provider events, source hashes, stored records and usage are retained.

This tests extraction from real conversation history. It does not test Entire's
checkpoint export, Graph, an existing seed, installed memory, or branch ownership.
The imported branch labels come from session-start metadata, even if the branch
changed during the session. All resulting facts are unsigned and unverified.

| Session type | Extraction calls | Facts retained |
|---|---:|---:|
| Deck implementation, Trail diagnosis, Linear integration | 1 | 3 |
| Entire/Graph setup and history recap | 1 | 0 |
| Cache verification and history recap | 1 | 0 |
| Server startup with revised diagnosis | 1 | 0 |

There were four extraction calls and no reconciliation calls. Across the sample,
3,577,012 raw transcript bytes became 71,213 preprocessed bytes in four chunks.

The primary assistant wrote a source rubric before execution, then manually
reviewed every emitted fact against the original conversation. This was not an
independent human adjudication or an exhaustive recall annotation.

- **Two records were supported as historical claims:** the observed Trail API/UI
  inconsistency, and the decision to connect Linear through OAuth MCP.
- **One record had a supported core plus an unsupported addition:** it accurately
  recorded rejection of the old Linear CLI for credential handling, then added
  “revisit only if the CLI gains secure OAuth or protected credential storage.”
  The source never established that exclusive condition. Brain's closed-negative
  template asks for a revisit condition, which may encourage the model to supply
  one when the conversation does not. This is an inference about the cause.
- **Selected coverage was incomplete:** of four evaluator-selected durable items,
  one was complete, two partial, and one missing. The design rationale for keeping
  the slides asset-light disappeared. The correction distinguishing web-API Trails
  from the old CLI's capabilities and the unsuccessful metadata repair survived
  only partially. These counts are selected coverage, not a measured overall recall.
- **Restraint worked in the routine sessions:** no facts froze the superseded
  detached-process diagnosis, promoted jokes into architecture, or turned the
  one-off Kannada/GitHub CLI requests into universal preferences.
- **Provenance was coarse:** all three stored anchors point to source line 6, the
  chunk-opening environment message. Supporting passages occur much later. The
  anchors identify the source chunk; they are not semantic supporting-line citations.

Source support does not prove external truth. In particular, July incident reports
do not establish present-day service behavior, and an assistant's historical claim
about reload requirements was not independently verified here.

## Interpretation and next changes to test

Preserving sources helped the reader recover from injected bad facts, but raw
history was at least as effective here. Before expanding fact-based retrieval,
test three narrow changes: reserve room for recent independent evidence; allow
“revisit condition not established” instead of requiring a new condition; and
return precise supporting spans with facts. Evaluate those changes with new cases,
including actual cross-session reconciliation, before drawing broader conclusions.

No retrieval or distillation behavior was changed during these two live runs.
The earlier opt-in `facts eval --include-context` patch remains the only product
code change. No commits, pushes, or installed-memory updates were made.

## Audit artifacts and usage

Machine-local artifacts are retained under the ignored directory
`bin/memory-integrity-live-20260913/`:

- `readers/`: frozen packets, schedule, 93 provider event logs, per-call usage and
  timing, response files, scores and `summary.json`.
- `distillation-run/`: frozen source sample, pre-output rubric, exact prompts and
  inputs, provider events, final Brain stores and `adjudication.json`.
- `scripts/`: exact Python runners and Go recording-wrapper source.
- `artifact-manifest.json`: SHA-256 hashes of the retained artifacts.

The raw real-session transcripts remain local in this ignored directory.

| Evaluation | Model calls | Input tokens | Cached input tokens (subset) | Output tokens |
|---|---:|---:|---:|---:|
| Reader | 93 | 797,518 | 13,312 | 7,564 |
| Distillation | 4 | 56,710 | 1,920 | 443 |
| Total scored evaluations | 97 | 854,228 | 15,232 | 8,007 |

Reasoning tokens are reported separately in raw logs and are not added again to
output totals. Reader latency summed across calls was 789.1 seconds with up to
three concurrent calls; this is not wall-clock duration. Distillation pipeline
time was 43.2 seconds. A successful connectivity probe used another 8,236 input
and 9 output tokens; an unavailable-model probe failed before evaluation.
Provider dollar cost was not reported by the subscription-backed CLI.

Execution followed the CLI's documented [non-interactive mode](https://learn.chatgpt.com/docs/non-interactive-mode);
the installed CLI help was also checked for its isolation and schema flags.
