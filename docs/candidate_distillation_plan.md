# Candidate-First Distillation Plan

## Session handover — 2026-09-12

**Implementation is shipped; rollout acceptance is incomplete. Legacy remains
the default.** Start here in the next session, then read the
[current implementation and evidence](candidate_distillation_implementation_status.md).
The dated phase narratives below are historical evidence, not a statement that
all prerequisites have passed.

### Repository and delivered work

- Checkout: `/Users/thomi/Projects/entire-brain`.
- Branch: `feat/candidate-distillation-quality-gate`.
- Last pushed handover baseline: `87fb9241358d483a8629de73db901d66a0478161`.
  The checkout was clean and equal to its upstream before this handover edit;
  code and Entire checkpoints were pushed to `entireio`.
- `13f60d74`: plan review corrections, pushed before implementation.
- `540d6688`: strict candidate negatives, conservative splitting, discovery
  reuse, hook/watch opt-in, privacy integration, recovery-budget preservation,
  corpus export, phase resolver, and quantitative evaluator.
- `b4e498b4`: isolated paired source snapshots with bounded input and complete
  source authority identity.
- `87fb9241`: retained phase decisions, verification, and development receipts.

The user's review instruction was to fix the findings except 4 and 5. The
human-audit blinding concern and historical 48/64 KiB baseline inconsistency
were intentionally left outside that correction. New development measurements
explicitly use 65,536-byte legacy chunks.

### Accepted scope and current blocker

- Phase 3 strict-negative corrective delta and Phase 4 incremental session-end
  runtime each received three independent passes. These judgments bind runtime
  tree `feae75d4cd34c8098c5dae65e26c0c956afe3d16` from `540d6688`; they do not
  establish full-corpus quality or accept later changes automatically.
- Phase 2 readiness failed closed: Claude and Copilot voted `fail`, Cursor
  `insufficient_evidence`. There was no critical dissent and no Astra vote.
  Read the retained [Phase 2 archive](candidate_distillation_evidence/2026-09-10/phase2.json).
- Complete-source development labeling failed the two-valid-judge floor:
  one valid judge for the 53-turn zero-candidate session, none for the 274-turn
  candidate-bearing session, after bounded recovery. Invalid labels are missing
  coverage, not negatives. This does not establish a semantic extraction error
  rate. Admission-panel labels cannot replace full-source reference labels.
- All twelve paired development extractions completed (two sessions, two arms,
  three repetitions). Counts and token savings are diagnostic; reference
  quality, retrieval non-inferiority, power, and untouched confirmation remain
  unproven. No new human audit was started.

### Next session: resume at the reference-evidence blocker

1. Verify branch, HEAD, and working-tree changes before editing. Read the status
   document, [evaluation workflow](../benchmarks/agent-brain/candidate_distillation_eval.md),
   and [phase evidence contract](candidate_distillation_gate_evidence.md).
2. Inspect the retained source-label validation failures and existing recovery
   ledger. Declare a bounded corrective evaluation slice before changing the
   labeling orchestration. Preserve complete source coverage and exact quote
   attribution; do not weaken validators, reset exhausted recovery budgets, or
   repeatedly optimize prompts against the same feedback. Record which inputs
   change and why a new request is eligible under the gate contract.
3. Establish valid independent full-source reference coverage, including
   zero-candidate sessions and authority classes. Reuse only unchanged valid,
   digest-verified evidence. Astra cannot substitute for fewer than two valid
   primary judgments. If coverage remains insufficient, retain that result.
4. Once development references are valid, complete paired fact-quality and
   retrieval evidence and compute the prescribed development-only sample size.
   Inventory every known prior development source and related session family;
   the exporter's identity checks cannot discover every alias or contamination.
5. Freeze implementation, providers/repetitions, metrics, source roster, split,
   and power contract before viewing a fresh disjoint confirmation set. Use
   fresh isolated snapshot arms and verify each dry-run's `brain_dir`. The two
   sessions already inspected here are development forever. Score the complete
   quantitative artifact, then obtain the independent phase decision; a scorer
   `pass` is not phase acceptance.
6. Proceed to Phase 5 human recomputation and explicit default-promotion approval
   only after its prerequisite gates pass. Keep candidate mode opt-in until
   then. Do not restart the cancelled admission run merely to fill its counters.

### Evidence locations and verification limits

Committed content-free receipts live in
[`docs/candidate_distillation_evidence/2026-09-10/`](candidate_distillation_evidence/2026-09-10/).
The local private evidence root, verified present at handover, is
`/tmp/candidate-distill-implementation.tjw1ob`. Temporary files may not survive
another machine or cleanup; verify availability and digests before reuse. Keep
raw transcripts, prompts, facts, and source maps outside Git.

Useful private paths relative to that root:

- `phase2-readiness`, `phase3-corrected`, `phase4-corrected`: retained gate inputs
  and verdicts. Earlier `phase3-review` / `phase4-review` archives are superseded.
- `source-labels-development/summary.json` and `run.py`: full-source failures
  and labeling orchestration; `source-corpus-development` and
  `source-corpus-candidate`: complete source exports with unset label templates.
- `paired-extraction-development`: twelve extraction runs and supplemental
  `declaration-correction.json`, `fact-output-artifacts.json`, and
  `source-manifest-binding.json`. Preserve these corrections with the original
  receipts; do not treat the prototype outputs as untouched confirmation.
- `quality-delta-budgeted`, `admission-report.json`, and
  `admission-cancellation.json`: retained admission state. Claude has 556
  completed / 17 invalid / 98 cancelled-unjudged; Copilot 671 completed; Cursor
  616 completed / 55 invalid. Sixteen Claude requests were in flight when
  cancelled; their usage is unknown, not zero. No provider jobs were left running
  at the end of that implementation session.

The full non-CGO race suite passed on the accepted runtime tree. Focused CGO,
privacy, hook, discovery, manifest, corpus, and gate tests passed; the snapshot
addition passed its race tests, vet, lint, and five-target cross-builds. Nine
quantitative-evaluator tests passed. The complete `mise run check` is **not
green**: `TestVecStoreConcurrentSavePresentMergesUnderBrainLock` races in the
CGO suite and was reproduced on unchanged `13f60d74`. Baseline evidence is in
`/tmp/entire-brain-baseline-race-13f60d74.log`. This unrelated race remains
unfixed. Use isolated tracked-file checkouts for broad checks because ignored
benchmark module caches disrupt Go package discovery; preserve those artifacts.
Do not claim these checks validate a later changed tree without rerunning the
checks appropriate to that change.

## Historical plan and evidence

Historical status (2026-09-10): Phase 3 conservative-write exit gate complete.
Phase 2 had a sealed three-judge agentic evidence package and one completed
historical 20-item human selector calibration. The final Phase 5 audit remains
the only human approval gate. Current acceptance is governed by the handover
above and the retained phase decisions, not by the presence of an evidence
package alone.

### Delivery and acceptance model

The main Codex agent owns scope, decomposition, evidence contracts,
orchestration, integration judgment, and phase acceptance. It does not perform
the primary implementation work. Each bounded implementation or test task is
delegated to a cheaper Sol, Terra, or Luna subagent with explicit file/module
ownership. The main agent integrates compatible work, verifies the retained
evidence, reports performance, quality, and progress against this plan at every
phase judgment, and rejects or re-scopes changes that exceed the admitted task;
it does not turn reviewer suggestions into an unbounded implementation loop.

Every Phase 0–4 acceptance gate is independently scored from the same sealed,
redacted evidence digest by Claude, Copilot, and Cursor. Each judge reports
performance, quality, and progress against the phase contract. Each is blind
to the other verdicts and prior acceptance. Three matching valid verdicts, a
valid two-to-one majority, or two matching valid verdicts when the third judge
is unavailable are decisive unless a dissent marks a critical safety, privacy,
authority, or source-faithfulness concern. A split without a majority or a
critical dissent invokes Codex Astra: Astra receives the sealed evidence,
rubric, all available primary verdicts, and their bounded rationales, then
records one digest-bound tie-break judgment of pass, fail, or
insufficient-evidence. Fewer than two valid primary verdicts fails closed.
Astra is never a fourth routine vote or a substitute for missing coverage.

Each gate produces a content-free evidence digest that pins revisions, corpus,
packet, rubric, judge-model, prompt, payload, telemetry, and verdict digests.
Only changed packets or changed evaluation inputs are rejudged; unchanged valid
verdicts are reused after digest verification. One bounded recovery request per
invalid/unavailable judge is allowed. If the resulting panel plus Astra cannot
produce a decisive valid result, the gate fails closed and records the missing
coverage. A failed gate permits one predeclared corrective implementation
slice, followed by one delta-only rejudgment; it does not authorize iterative
local prompt, selector, or rubric optimization against the same judge feedback.
All quantitative thresholds below are provisional agentic gates until the
final Phase 5 human recomputation and approval.

The first implementation slice on `feat/candidate-first-distillation` adds the
opt-in `--pipeline candidates` path: typed/role-aware normalization, bounded
direct-user candidate cards, one card per extraction call, conservative exact
upserts with no reconciliation-agent call, separate rollback-safe session-cache
keys, dry-run accounting, redaction, and privacy-linearized egress/publication.
Legacy remains the default.

The Phase 2 branch adds candidate-id-framed multi-card output, deterministic
cross-session packing under a fixed 32 KiB/32-member provider-neutral policy,
a bounded success-only member cache, one-level failure bisection, provider usage
accounting, and `--shadow` execution that leaves active facts, proposals,
taxonomy, the legacy cache, and the manifest unchanged. Exclude/purge resets the
derived member cache; privacy verification, atomic-temp cleanup, and publish
bundle exclusion cover the new artifact. Assistant claims gain one deliberately
narrow repository-evidence route: an allowlisted successful checkpoint outcome
plus an exact full-path mention from that session's touched-file list. Review
sessions, status-only claims, basename matches, and unverified checkpoints do
not qualify.

The Phase 3A branch routes opt-in non-shadow candidate runs through that same
framed, packed, member-cached extraction path. It materializes only exact fact
identities, preserves authored and legacy provenance, and records bounded,
content-free application receipts for branch/session/candidate ownership.
Successful `NO_FACTS`, relocation, removal, force, and crash retry remove only
the v2-owned application slot. Distinct owned/shared anchor markers preserve
anchorless legacy exact matches while still making facts-before-receipt crash
recovery deterministic. Exclude/purge resets receipts wholesale;
privacy verification, atomic-temp cleanup, and publish exclusion cover the new
artifact. Different fact ids remain separate with no automatic merge or
supersession.

Phase 3B adds that evidence-bearing shape without changing reconciliation. A
different-id pair may produce a local `possible_same_subject` observation only
when both active facts have the same branch, kind, taxonomy top-level, and an
exact shared strong code locus. Bare normalized loci require matching
backticked, CamelCase, snake_case, qualified, or path evidence in the fact
text; ordinary words and flags are rejected. At least one fact must carry a
current candidate-v2 application owner. The state lives separately at
`facts/distill-v2/relationships.ndjson` and is exposed only through the
read-only `facts relationships list` command. It has no action, confidence,
status, apply/reject operation, fact crosslink, factsync transport, or hosted
publication path. Executable merge/supersede proposals remain unchanged and
unused by candidate mode.

Real local-model acceptance showed that Qwen 2.5 7B does not reliably complete
larger framed packs even at temperature zero. The Ollama adapter therefore caps
candidate packs at two members while other providers retain the 32-member
ceiling. The retained implementation converts normally stopped malformed Ollama
singletons, omitted members, and invalid taxonomy output to `NO_FACTS`. This is
an outstanding correctness gap, not an accepted negative-result contract.
Before further write-mode acceptance, those cases must become explicit failed
members: no successful extraction-cache entry, no retraction of an existing
application, and no credit as a quality-gate rejection. A normal transport stop
does not prove a semantic negative. Only an explicit, protocol-valid `NO_FACTS`
may be cached as empty and retract a v2-owned application. Invalid or
unattributable text is never persisted as a fact; truncated output remains a
failure. Historical Phase 3 evidence remains scoped to the implementation tested
and does not discharge this corrective slice.

Relationship discovery is advisory and cannot make primary fact application
fail merely because a subject is broad. It indexes exact subject blocks, sorts
them deterministically, and admits a block only when its complete pair set fits
the remaining 4,096-entry and 4 MiB store budgets. A 91-member block (4,095
pairs) is admissible; a 92-member block (4,186 pairs) is skipped before pair
enumeration. Byte- or capacity-limited blocks are likewise skipped whole,
as are blocks whose pair evidence would exceed the 128-owner record bound.
They are never truncated, and the command result plus local listing report the
skipped block count. Byte accounting advances only with the newly admitted
block rather than rescanning the accumulated store. This is a bounded
inspection foundation, not evidence that different-id facts should merge.

The opt-in slice keeps each complete redacted authoritative trigger turn. It
shrinks adjacent context first and refuses preflight when the trigger itself
cannot fit the fixed 32 KiB candidate-v2 member/pack policy; it never silently
truncates a later rule or qualifier from the same turn. A small number of
normal sessions contain cue-bearing pasted documents far above that limit, so
cue-span/paragraph splitting is required before this pipeline is corpus-general.

The final Phase 1 real-corpus dry-run against `entire-graph` (2026-08-23)
completed without an unsupported-dialect fallback. The legacy path sees 154
exports; candidate mode deterministically coalesces same-branch re-exports to
152 current session views covering 697,289,133 raw bytes. It reduced 33,663,145
legacy-preprocessed bytes to 1,417,956 rendered candidate bytes (a 95.8 percent
reduction) and removed all
reconciliation calls. The intentionally unbatched attribution contract produced
368 extraction calls versus 731 legacy extraction calls (or up to 1,462
extraction-plus-reconciliation calls). This is evidence that deterministic
filtering works, not that the latency goal is met: candidate-id-framed packing
remained necessary before a 30-60-call claim.

### Phase 2 evidence (2026-08-23)

Using the final authority filter, the current `entire-graph` dry run reports
152 current session views, 355 admitted cards, 221 unique members after replay
collapse, and 1,357,663 candidate bytes versus 33,663,145
legacy-preprocessed bytes (95.97 percent less). The provider-neutral 32-member
packer remains the Phase 2 protocol ceiling. The proven local Ollama policy
plans 111 two-member packs when cold: 84.8 percent fewer extraction calls than
the 731-call legacy baseline, or 92.4 percent fewer calls than the 1,462-call
legacy extraction-plus-reconciliation ceiling.

The retained command for that projection is:

```text
cd /path/to/entire-graph
entire brain distill --dry-run --pipeline candidates --shadow \
  --agent ollama --model entire-brain-distill:qwen2.5-7b
```

A real local-model smoke on branch `docs/chicago-style` exposed two issues that
fake-provider tests did not: the legacy output instructions conflicted with the
framed protocol, and the model appended a redundant `NO_FACTS` after a valid
fact. Phase 2 now uses a dedicated non-conflicting prompt and accepts only that
content-free trailing sentinel form (a leading sentinel followed by facts still
fails closed). The final run admitted one source-backed Chicago-style rule,
completed one packed call in 10.66 seconds, reported 2,497 input and 150 output
tokens, and produced one faithful shadow fact. The immediate rerun was a
zero-call member-cache hit in 37 milliseconds. A task-local “do not change any
files or code” request found during the same audit is now deterministically
suppressed rather than promoted as a standing rule.

This passes the implementation, isolation, fixed-policy call-count, and small
real-provider protocol checks. It does **not** pass Phase 2's full exit gate:
one manually inspected fact is not a sealed agentic evaluation. Promotion
beyond shadow still requires the agentically scored span-recall/precision set,
filtered-out negative sample, repeated paired legacy/candidate provider runs,
source-anchor-faithfulness review, and observed provider token/latency totals
described below. The final human audit occurs only after every such gate passes.

#### Sealed independent agentic panel (2026-08-25)

Phase 2 uses reproducible agentic gate evidence, not an interim human
checkpoint. `facts distill-quality prepare` seals one redacted local run
containing every unique admitted candidate card plus a deterministic, stratified
sample of 500 filtered exchanges. The retained `entire-graph` run contains 152
current session views, 219 admitted packets, 500 filtered packets, and 719
total packets. Its manifest pins evaluator and target revisions, dirty-state
bit, corpus and packet digests, rubric/prompt digest, redaction version, item
counts, and a separately protected private source-map digest. Public judge
packets contain redacted evidence and opaque ids only; source paths, branches,
session ids, checkpoints, cache keys, credentials, and prior human expectations
are not sent to judges.

`facts distill-quality judge` submits that identical sealed packet set to
Copilot, Cursor, and Claude independently. It blinds every judge to the other
verdicts and to any future human audit, validates exact packet and payload
digests on return, and records every dimension score and bounded rationale.
Provider work uses deterministic batches, bounded parallelism, atomic
parent-owned persistence, and one schema/transport retry plus one recovery
request. Missing or malformed provider output is `invalid`, never a negative
label or a silent abstention.

The planned resolution workflow must emit a concise gate summary, exhaustive
machine-readable evidence archive, and aggregate digest; it does not imply an
existing `facts distill-quality resolve` command or named artifact. It applies
the delivery-and-acceptance policy: matching valid verdicts or a non-critical
valid two-to-one result are decisive. A split without a majority or a critical
dissent invokes Codex Astra once. Fewer than two valid primary verdicts fails
closed without asking Astra to replace missing coverage. Astra receives the
sealed evidence, rubric, all available primary verdicts, and their bounded
rationales, then records one digest-bound pass, fail, or insufficient-evidence
judgment. Insufficient evidence leaves the gate open; it cannot fabricate a
label or compensate for missing coverage.

The historic `facts distill-quality adjudicate` terminal workspace is reserved
for the final Phase 5 human audit. It is not run in Phases 0–4 and no additional
calibration or bulk human queue is created before that final gate. The final UI
shows every available Claude, Copilot, Cursor, and Astra score with its concise
rationale, omits unavailable judges, and works through bounded resumable
batches. A second human review is required only for ambiguous or critical
disagreements. Private reviewer records remain bound to packet and aggregate
digests and are reused only after exact digest verification; software, rather
than a reviewer, recomputes the final metrics from the human labels.

Agentic scores and rationales are the provisional proof inputs for Phases 0–4.
They may pass their named gate only through the independent-panel resolution
above. The final Phase 5 human audit recomputes the same metrics and is the
only approval that can promote the default.

The completed sealed run is
`quality-run-v1:33808625a097f0e5ab411a5cddb92c337257a7b4c62c7fad7c53852dee919868`.
It binds target revision `6e9ef28b8ca0f148c259b30eabb5076de896b836`,
evaluator base revision `55d432eba74e8437a65284513c470b0658f36136`
with an explicit modified-worktree bit, packet digest
`sha256:ee410013b6f4bf973b28af61ebeb2aca85f452be059e0bbdacd470b8d5e7fd8a`,
corpus digest
`sha256:f935941697ec1f8b485c1fb68d6c3151c352f26e364d4d445d26567a0be94bd3`,
bundle digest
`sha256:db9eee649f97ef742d77d0edcb6e4ecca62dd8e84645dba16b12460b58e8040e`,
and rubric/prompt digest
`sha256:00448fdb651aed162f484e94936bcb23db186704b801c507853e065d919e9690`.
The sealed artifacts, rather than a mutable branch name, are the evaluated
input contract.

The independent judge result totals are shown as `pass / concern / critical`:

| Judge | Coverage | Admission | Authority | Safety |
| --- | ---: | ---: | ---: | ---: |
| Copilot `gpt-5.4` | 719 complete | 549 / 12 / 158 | 462 / 217 / 40 | 571 / 3 / 145 |
| Cursor `composer-2.5` | 719 complete | 565 / 13 / 141 | 635 / 45 / 39 | 627 / 57 / 35 |
| Claude `claude-haiku-4-5` | 687 complete, 32 invalid | 576 / 24 / 87 | 552 / 111 / 24 | 620 / 27 / 40 |

The agentic report flags 221 packets with at least one critical score, 456 with
judge disagreement, and 32 with an invalid Claude verdict. Split by the
deterministic candidate decision, that is 190 of 219 admitted packets and 31 of
500 filtered packets with at least one critical signal; 196 admitted and 260
filtered packets have disagreement. These are attention-routing counts, not
error rates; they require policy resolution before they can pass a provisional
phase gate.

Provider execution retained all call attempts. Copilot used 34 calls (29
complete, five schema failures, three retry attempts, and eight one-time
recovery calls) and did not expose token usage. Cursor used 28 calls (23
complete and five schema failures) and reported 1,248,315 input plus 434,581
output tokens. Claude used 158 calls (83 complete, 11 schema failures, and 64
transport failures), including 40 recovery calls; the bounded recovery lane
recovered 224 of 256 primary invalids and left 32 explicit invalid verdicts.
Claude reported 1,806 input plus 1,454,994 output tokens, but its CLI's cached
input accounting is not a complete prompt-volume measure. Summed provider-call
durations are 3,701,209 ms, 4,476,095 ms, and 27,475,944 ms respectively;
Claude used bounded concurrency during the primary pass, so that sum is not
wall-clock duration. A Claude Sonnet 5 diagnostic exceeded the ten-minute
single-call ceiling and was excluded before the sealed Claude Haiku run.

#### Historical human calibration and selector rebase (2026-09-10)

The simplified one-statement reviewer was completed for its first 20-item
attention-weighted calibration. The reviewer agreed with the available judge
majority on 17 items, disagreed on one, and directly decided two items whose
judges had no majority. The resulting historical labels judged the filter
correct on 14 items and identified six false-positive admissions. This batch
was deliberately sampled for attention, not prevalence, and contained no
human-labeled filtered-out miss; it is historical selector-calibration
evidence, not a Phase 2 recall or precision estimate and not an interim gate.

The six errors were current-work instructions: implement/test a plan, a long
benchmark campaign runbook, review named trails, prepare a current Sonnet plan,
accept a one-off bot rerun with bare `Yes`, and implement/audit/open a PR while
temporarily skipping benchmarks. Candidate schema v3 suppresses those bounded
shapes before egress. Regression cases retain explicit lasting rules, durable
project preferences, closed negatives, and genuine accepted technical
decisions. The one historical panel disagreement remains recorded: a direct
request to choose the best clean-slate project approach is still admitted for
extraction inspection even though two judges treated it as a one-off task.
Admission does not itself authorize a durable write; the extraction protocol may
still return `NO_FACTS`.

`distill-quality prepare --reuse-from <sealed-run>` makes evaluation
incremental. It copies no provider-call telemetry and reuses a judge verdict
only when packet id, packet digest, item digest, prompt digest, and payload
digest all match. Changed packets lose their old verdicts and are the only
members of the next agentic delta queue. A content-free `reuse-delta.json`
sidecar records the exact partition and counts. Historical sealed
candidate-schema versions remain readable, while future schemas and changed
redaction versions fail closed. Historical human records are retained but are
not consulted or expanded until the final Phase 5 audit.

The current Brain had grown from 152 to 186 candidate session views, so the
first live rebase separated 87 new/corpus-drift packets from 589 exact packet
matches and reused 1,767 verdicts. Copilot and Claude completed all 87 new
judgments. Cursor was unavailable at the transport layer; its 87 delta verdicts
remain explicitly invalid after the one bounded recovery pass. Under this plan
they require Astra escalation or fail-closed treatment before a gate can pass;
they are not silently omitted. The final selector rebase contains 171 admitted
and 500 filtered packets. All 671 surviving packets and all 2,013 verdicts were
exact matches to the immediately preceding sealed run, leaving zero unresolved
delta packets; 18 unchanged historical human records migrated. No additional
human review is created from corpus growth before Phase 5.

On that 186-view corpus, the final candidate dry run has 267 cards, 173 unique
members, 1,119,642 candidate bytes, and zero reconciliation calls. The
provider-neutral 32-member policy plans 25 extraction calls; the proven local
Ollama two-member policy plans 87. The current legacy control has 36,415,818
preprocessed bytes, 801 extraction calls, and a 1,602-call
extraction-plus-reconciliation ceiling. Candidate mode therefore sends 96.93
percent fewer preprocessed bytes and, under provider-neutral packing, schedules
96.88 percent fewer extraction calls (98.44 percent below the legacy total-call
ceiling). Ollama's conservative policy remains 89.14 percent below legacy
extraction calls and 94.57 percent below the total-call ceiling.

Same-branch re-exports are coalesced only inside the active branch/session
selection. A later timestamp wins; at an equal timestamp, one normalized turn
stream must be a strict extension of the other or candidate preflight refuses
the ambiguous views. Checkpoint ids are never treated as chronological.

### Phase 3A evidence (2026-08-23)

At the Phase 3A checkpoint, source dry runs against the retained
`entire-graph` Brain showed exact planning parity between
`--pipeline candidates` write mode and `--shadow`:
after removing only `generated_at` and the intentional `shadow` flag, their
JSON reports are byte-identical. Both report 152 current session views,
697,289,133 raw bytes, 367 admitted cards, 231 unique members, 1,417,956
candidate bytes, one member-cache hit, 230 misses, 29 scheduled packs/calls,
and zero reconciliation calls. The cache-matched legacy control reports 154
exports, 33,663,145 preprocessed bytes, 730 scheduled extraction calls, and a
1,460-call extraction-plus-reconciliation ceiling.

On that retained cache state, Phase 3A therefore plans 95.8 percent fewer input
bytes, 96.0 percent fewer extraction calls, and 98.0 percent fewer calls than
the legacy extraction-plus-reconciliation ceiling. These are deterministic
dry-run measurements, not hosted-provider latency or quality claims. Focused
integration tests additionally prove one packed call for two candidates, a
zero-provider-call byte-stable no-op replay, a zero-call mechanics-only anchor
relocation, exact legacy-provenance preservation, `NO_FACTS` retraction, and
facts-before-receipt crash convergence.

Phase 3A's synthetic evidence was necessary but not sufficient for the Phase 3
exit. The final live-corpus and controlled-result acceptance is recorded below.
The Phase 2 agentic recall and precision audit, repeated paired provider runs,
filtered-negative audit, and retrieval evaluation remain required before
promotion or any default change; the human recomputation follows them only in
the final Phase 5 approval gate.

### Phase 3B evidence (2026-08-23)

The first Phase 3B slice adds a 20-case sanitized admission fixture spanning
Codex, Claude, Pi, and OpenCode positive and negative authority shapes. It
locks deterministic candidate IDs, trigger anchors, role boundaries,
checkpoint corroboration, accepted decisions, injected mechanics, task/review
sessions, and one-off-request suppression. This is a regression fixture, not
the full agentically scored Phase 2 corpus gate.

Synthetic bad-reconciliation coverage retains the two known failure shapes:
`--no-network` and repository-key/symbol-ID facts may share a taxonomy path but
remain distinct, with neither an executable proposal nor a neutral
relationship. Exact-write replay is byte-stable, force output matches a clean
rebuild, `NO_FACTS` removes only its v2-owned fact, and sorting the fact-store
generation digest to match durable fact order fixed a receipt miss found by
this parity gate. A separate crash-window regression clears relationship state
after facts and receipts commit, then proves an unchanged retry rebuilds it
with zero provider calls.

The bounded runtime regression materializes 92 same-subject candidates, keeps
all 92 facts, skips the entire 4,186-pair advisory block with a counted warning,
and produces identical normalized facts and bounded relationship state on a
force replay. Relationship state is reset wholesale by exclude/purge, checked
fail-closed by privacy verification, covered by atomic-temp cleanup, and
excluded from publish bundles. These tests establish the local-only neutral
foundation; they do not satisfy the live-corpus or pairwise-precision exit
gates.

### Phase 3 exit evidence (2026-08-23)

The final acceptance used the retained 152-view `entire-graph` corpus and local
`entire-brain-distill:qwen2.5-7b` model at temperature zero. A cold force run
processed 355 cards / 221 unique members in 111 parent packs plus 16 bounded
split calls. Eight malformed or length-limited parents were isolated before
materialization. The run completed all 111 packs in 127 actual provider calls,
with 524,125 input tokens, 36,417 output tokens, zero reconcile calls, and zero
executable proposals. Against the retained legacy baseline, that is 82.63
percent fewer extraction calls and 91.31 percent fewer calls than the legacy
extraction-plus-reconciliation ceiling. Candidate bytes are 95.97 percent
below legacy-preprocessed bytes. The immediate replay used 221 member-cache
hits, made zero provider calls, and left every file under `facts/` byte-stable.

Two independent local-model extractions produced 419 versus 356 total facts.
That variance is an observed Phase 2 provider-quality failure, not evidence of
Phase 3 parity, and it remains a release/default blocker. Phase 3 application
parity was therefore tested with the exact same 221 protocol-valid member
results on a clean force store and a clean incremental store. After removing
only timestamps and the intentionally generation-derived receipt ids, both
stores had identical 356 facts and all 355 application receipts; relationship
state was byte-identical with five observations. A subsequent replay was again
zero-call and byte-stable.

All 30 pre-existing live facts retained their text, paths, kind, locus, origin,
status, confidence, supersession, and related-id fields. Candidate-owned facts
had zero status, supersession, or related-id mutations. The one pre-existing
superseded fact remained the only superseded fact, and executable proposals
remained empty. Sanitized regressions retain the historical `--no-network`
versus repository-key/symbol-ID corruption cases.

Every live neutral relationship was joined back to its fact pair for manual
inspection. This historical inspection is supplemental evidence under the
revised agentic-gate policy, not a claim that Phase 3 was agentically scored.
One false observation used the generic product token `entire` to connect
unrelated graph-index and semantic-snapshot gotchas; the strong-locus policy now
rejects that token and retains the exact pair as a regression. The remaining
seven observations across the two provider runs were narrow same-subject pairs
(hook event mappings, graph-plugin release steps, and
`internal/sem` boundaries). None altered facts or entered the executable
proposal path. Relationship stores invalidated by a stricter policy now rebuild
from current v2-owned facts with zero provider calls instead of blocking fact
application; privacy verification remains fail-closed.

These results satisfy the Phase 3 exit gate: the retained bad-reconciliation
fixtures pass, force/incremental lifecycle parity holds for identical extracted
results, and no wrong automatic supersession is observed. They do not satisfy
Phase 2's fact-quality, repeated-provider, filtered-negative, or retrieval
gates, so candidate mode remains opt-in and legacy remains the default.

This plan replaces full-conversation fact extraction with a candidate-first
pipeline. The deterministic path reads retained sessions, normalizes their
visible turns, identifies small evidence-backed candidate windows, and sends
only those windows to the selected fact agent. Fact quality, taxonomy,
branch-scoping, provenance, review, and recall remain durable-facts contracts;
the unit of model work and cache invalidation changes from a whole session to a
content-addressed candidate.

The intended steady state is:

```text
canonical session
  -> typed visible turns                    deterministic, local
  -> authority-aware candidate cards        deterministic, local
  -> compact candidate packs                deterministic, local
  -> durable-fact quality/classification    fact agent, candidate text only
  -> conservative reconciliation            deterministic, local
  -> branch fact store + provenance          deterministic, local
```

The raw transcript remains the retained source of truth. Candidate cards and
facts are derived views. Nothing in this plan permits deleting or replacing
canonical Entire session capture.

## Why This Work Is Needed

The current pipeline already removes tool calls, tool output, reasoning, and
session metadata before distillation. That was a large and necessary reduction,
but every remaining user and assistant line is still sent to a fact agent in
fixed-size chunks. A transcript with one durable decision and megabytes of
ordinary discussion therefore pays to classify all of that discussion. A
candidate-bearing chunk may then cause a second agent call to reconcile its
facts against existing facts sharing a taxonomy path.

The current preprocessor also removes role information. The extraction agent
cannot distinguish a human standing rule from an assistant's tentative review
finding, a pasted response from another agent, or a slash-command expansion
injected as a user message. That is both a cost problem and a fact-authority
problem.

### Measured `entire-graph` baseline

The following measurements were taken against the local `entire-graph` brain
on 2026-08-22. They are a design baseline, not a release performance claim.

| Measure | Observed value |
| --- | ---: |
| Exported sessions | 154 |
| Distinct session ids | 127 |
| Raw transcript bytes | 706,098,307 |
| Current preprocessed bytes | 33,663,145 |
| Extraction calls at the default 48 KiB chunk size | 731 |
| Total call upper bound at 48 KiB, including reconcile | 1,462 |
| Uncached chunks at the measured 64 KiB Ollama configuration | 580 |
| Scheduled extraction calls at 64 KiB | 579 |
| Total scheduled-call upper bound at 64 KiB | 1,158 |
| Sessions cached under that exact configuration | 1 of 154 |

The latest measured one-session run sent 13,054 preprocessed bytes. It made one
extraction call and one reconcile call, reported 7,179 provider tokens, and took
121.96 seconds: 93.61 seconds extracting and 27.89 seconds reconciling.

The corpus also demonstrates two kinds of reusable work:

- The 154 exports contain 146 exact transcript contents. The eight redundant
  copies account for 115,491,190 raw bytes, or 16.4 percent of the corpus.
- Twelve repeated session ids cover 39 exports. Many are not exact duplicates;
  they are later or branch-specific views of an evolving session. A turn-level
  cache can reuse their unchanged prefixes even when a whole-file digest cannot.

The existing deterministic history projection gives a useful feasibility
bound for candidate generation:

| Projection | Source loci / exchanges | Bytes | Session paths |
| --- | ---: | ---: | ---: |
| Decision or learning summaries | 1,351 loci | 725,339 | 91 |
| Role-aware exchanges containing those loci | 856 exchanges | 1,660,135 | 98 |
| Same exchange set, excluding `agent_review` assistant claims | 815 exchanges | 1,576,942 | 57 normal-session paths |

The 725 KiB summary set is 97.8 percent smaller than the 33.66 MiB current
preprocessed input. The 1.66 MiB exchange projection is 95.1 percent smaller.
Candidate packets will include more context than those bounded projections, so
these figures are an opportunity bound, not the promised token reduction.

### Quality findings from the live fact store

The live corpus had 29 active facts from eight source sessions. Twenty were
classified as gotchas. Manual inspection found failure modes that a
candidate-first design must make into regression fixtures:

- review findings were promoted as durable project truth before acceptance or
  implementation;
- a slash-command-expanded reviewer instruction became a supposed user
  preference about review format;
- task-local review scope became a standing workflow convention;
- near-duplicate `--no-network` facts remained active under different wording;
- an unrelated symbol-id/repository-key candidate superseded a `--no-network`
  fact because both shared the broad `constraints.invariants.general` path.

These examples show that taxonomy-path overlap is not a safe semantic subject
key and that assistant text cannot inherit human authority merely because it is
visible conversation text.

## Goals

- Avoid sending conversation text that has no deterministic durable-fact
  signal to an agent.
- Preserve at least 98 percent candidate recall for explicit user standing
  rules, preferences, and closed negatives, and at least 95 percent candidate
  recall across all provisionally agentically scored durable facts before the
  extraction agent runs; confirm both in the final Phase 5 human audit.
- Preserve the existing silent quality gate: most candidate cards should still
  produce no fact.
- Keep facts source-backed, branch-aware, content-addressed, locally
  inspectable, and re-derivable from retained sessions.
- Preserve exact source ranges and improve provenance from a chunk-start line
  to the turn or trigger span that earned the candidate.
- Make no-op refreshes and session appends proportional to new turns, not to the
  size of the accumulated session.
- Remove routine agent-based reconciliation. Exact duplicates and safe obvious
  paraphrases should merge deterministically; uncertain relationships should
  remain visible for review.
- Keep deterministic stages usable with `--agent none` for planning,
  observability, and cache preparation. Fact extraction remains agent-gated.
- Reduce input tokens by 85-95 percent and total agent calls by at least 90
  percent on the measured `entire-graph` corpus without a material durable-fact
  recall regression.

## Non-Goals

- Do not turn regex matches directly into durable facts. Deterministic triggers
  admit candidates; the existing durable-fact quality and classification gate
  still decides whether a fact deserves storage.
- Do not summarize every session with an agent as a replacement for chunking.
  An unconditional session-summary call has the same wrong default at a smaller
  scale: it still spends on sessions with no durable fact.
- Do not treat assistant review findings, plans, or status reports as human
  assertions.
- Do not infer contradictions from taxonomy path alone.
- Do not auto-supersede an existing fact merely because a classifier reports a
  high confidence score.
- Do not store a second plaintext transcript archive in candidate caches.
- Do not make embeddings, hosted providers, or network access mandatory.
- Do not change authored-fact semantics, fact ids, taxonomy path syntax, recall
  ranking, or the canonical session-capture format in the first implementation.
- Do not delete legacy distilled facts during an experimental or shadow run.

## Existing Contracts To Preserve

Implementation must build on the current distill and conversation seams rather
than creating a parallel transcript parser.

| Contract | Existing seam | Requirement |
| --- | --- | --- |
| Canonical safe reads | `readCanonicalHistoryTranscript` and distill preflight | Complete-or-refused input validation still occurs before egress or publication. |
| Visible conversation parsing | `internal/cli/conversation.go` | Reuse its dialect routing, wrapper filtering, visible-assistant allow-list, exact exchange ranges, and source digest. |
| Document transcripts | `parseDocumentConversation` | Preserve line anchors and parity with JSONL dialects. |
| Privacy lifecycle | session tombstones, exclude/include/purge | Excluded or purged sessions cannot produce candidates, cache hits, facts, or published metadata. |
| Quality gate | `templates/entire-brain-distill.md` | Silence remains the default and the six fact kinds remain unchanged. |
| Fact identity | `factmerge.RecordID` and `factmerge.Upsert` | Existing normalized text + sorted-path ids and provenance union remain valid. |
| Branch scoping | resolved session branch and branch fact stores | Candidate extraction may be shared, but materialized facts remain branch-scoped. |
| Deterministic chronology | current session/chunk consumer order | Parallel model work must still apply results in session/turn order. |
| Cache-on-success | current session cache | A failed pack is not marked extracted; successful empty output is cacheable. |
| Authored facts | `origin=authored` | Force or migration work never deletes or rewrites authored facts. |
| Agent safety | current Codex, Claude Code, command, and loopback Ollama runners | Candidate packs use the same runner, timeout, no-egress, redirect, output-bound, and usage-accounting rules. |

The current history classifier can seed trigger vocabulary, but its records are
not themselves durable-fact candidates. History deliberately favors retrieval
recall and can label the same assistant line as decision, learning,
architecture, and validation. Candidate generation needs a stricter authority
and evidence contract.

## Pipeline Data Model

The new internal records are derived and versioned independently. Names below
are illustrative Go types; JSON names are the on-disk contract where a stage is
persisted.

### Typed turn

```go
type distillTurn struct {
    ID            string
    SessionID     string
    TurnOrdinal   int
    PromptID      string
    Role          string // user | assistant | summary
    Origin        string // direct_user | slash_args | assistant_narrative | compaction | injected
    Authority     string // human | assistant_claim | context_only | rejected
    Text          string
    StartLine     int
    EndLine       int
    SourceDigest  string
    ContentDigest string
    ToolNames     []string
}
```

`Text` exists in memory only. Persisted turn state contains ids, ranges, source
identity, and content digests, not plaintext turn bodies.

The turn id must be stable across transcript relocation and a branch replay.
Prefer a source-provided turn or prompt id. Otherwise derive it from session id,
role, prompt id when present, normalized visible text, and the turn's stable
ordinal. Transcript path, branch, checkpoint, model, and agent configuration
must not be part of turn content identity.

### Candidate card

```json
{
  "id": "candidate:...",
  "version": 1,
  "session_id": "...",
  "turn_ids": ["turn:..."],
  "authority": "human",
  "trigger_classes": ["standing_rule", "preference"],
  "score": 9,
  "anchor": {
    "transcript": "sessions/main/...jsonl",
    "line": 418,
    "end_line": 424,
    "turn_id": "..."
  },
  "evidence": {
    "user_acceptance": true,
    "checkpoint_success": true,
    "same_locus_commit": false,
    "validation": false,
    "independent_recurrence": 0,
    "session_kind": "normal"
  },
  "locus_hints": ["internal/sem/provider.go", "repoKey"]
}
```

The in-memory card also holds a role-tagged window. The persisted candidate
index does not. Its id is a digest of candidate-rule version, trigger class,
authority, ordered turn-content digests, trigger-span digest, and deterministic
evidence features. It does not include branch, transcript path, checkpoint,
agent, model, effort, taxonomy, or confidence threshold.

### Extraction result

An extraction result is keyed by candidate-pack member rather than only by the
pack. It records one of:

- `empty`: the quality gate emitted no fact for this candidate;
- `facts`: validated fact output lines plus the candidate id they cite;
- `failed`: never persisted as a cache hit.

A failed member preserves its prior application and remains unresolved in run
accounting. Bounded failure metadata may quarantine it from automatic retry
storms, but cannot masquerade as an empty result or a completed run. Report its
opaque id and reason, and require an explicit retry or changed extraction input
to retry after the bounded recovery allowance is exhausted.

Caching individual member results means a retry or a later packing policy does
not repeat successful candidates merely because their neighbors changed.

## Stage 1: Typed Conversation Normalization

Normalization must be deterministic, role-preserving, and shared across dry
run and real execution.

### Dialect routing

Reuse the conversation parser's supported dialects:

- Codex `event_msg` and `response_item` records;
- Claude Code `user` and `assistant` records;
- pi `message` records;
- document-form transcripts handled by `parseDocumentConversation`.

Only visible user and assistant narrative is eligible text. Thinking,
redacted-thinking, tool calls, tool output, patches, command output, API error
envelopes, permission records, hook messages, and session metadata do not enter
candidate text.

Tool names, validation outcomes, files touched, and checkpoint results may be
represented as bounded structured evidence. They must not be copied wholesale
into the role window.

### Canonical user turns

The normalizer must distinguish actual human language from messages encoded
with a user role by an agent harness.

- Continue filtering environment envelopes, system reminders, local-command
  caveats, tool-result pseudo-user messages, Stop-hook feedback, and
  session-hook announcements.
- Correlate records with the same `prompt_id`. Claude slash commands commonly
  produce both a command envelope and an expanded skill prompt. Keep the
  command's human arguments and suppress the generated skill body.
- Parse the human portion of `<command-args>` when it exists. Do not retain
  `<command-message>`, `<command-name>`, or the installed command template as a
  user assertion.
- Mark compaction or continuation summaries as `context_only`. They may support
  a later claim, but text inside them is not a fresh human preference.
- Mark pasted agent responses and quoted historical material as
  `context_only` when the wrapper or record metadata proves that origin. When
  origin is ambiguous, retain the turn as a low-authority candidate input and
  let the quality gate reject it; do not assign human authority by default.
- Deduplicate dialect-level repeats of the same logical turn before trigger
  scanning.

### Canonical assistant turns

Assistant narrative is always an `assistant_claim`, never a repository fact by
itself. Preserve the request/response relationship and collect the following
bounded metadata:

- preceding substantive user turn;
- next substantive user turn, when present;
- whether the response is final narrative or intermediate commentary;
- session kind, especially `agent_review`;
- touched files and latest checkpoint from the session manifest;
- deterministic episode reinforcement and history validation signals.

### Exact anchors

Each normalized turn retains its source start and end line. Trigger spans retain
the narrowest source line that contains the trigger when the transcript format
provides one. A produced fact cites that span's first line and source turn id,
not the first line of an arbitrary size chunk.

If a transcript dialect cannot prove a complete turn range, the candidate is
marked range-incomplete. It can still be considered, but verification and
observability must report the degraded anchor.

## Stage 2: Deterministic Candidate Generation

Candidate generation is a high-recall admission filter. A match means "worth
asking the quality gate," not "write a fact."

### Trigger classes

Triggers should be token-aware and boundary-aware rather than raw substring
matches. Each class has positive cues, negative cues, and authority priors.

| Class | Representative cues | Default authority |
| --- | --- | --- |
| Standing rule | `from now on`, `going forward`, `always`, `never`, `whenever`, `every time`, `make sure` | high for direct human text |
| Preference | `I prefer`, `we prefer`, `use X over Y`, `I want`, durable corrective feedback | high for direct human text |
| Resolved decision | `decided`, `chose`, `agreed`, `instead of`, `rationale`, `tradeoff`, choice plus `because` | medium; requires resolution evidence for assistant text |
| Invariant | `must`, `must not`, `cannot`, `source of truth`, `invariant`, `contract requires` | medium; imperative task instructions score lower |
| Closed negative | `tried`, `rejected`, `rolled back`, `did not work`, `no effect`, `within noise`, `dead end`, `revisit only` | high only when failure evidence and revisit condition are present |
| Gotcha or discovery | `root cause`, `turns out`, `only when`, `unless`, `otherwise`, `silent`, `hang`, `race`, `stale` | medium; assistant claims require corroboration |
| Explicit revision | `not X; use Y`, `ignore the previous rule`, `we no longer`, `replace X with Y` | high for direct human text; carries a subject key |

Broad words such as `must`, `should`, `because`, `failed`, and `fixed` are not
sufficient alone. They are common in one-off tasks, generated command prompts,
and implementation status. Score them with role, origin, grammatical context,
resolution, and corroboration.

### Negative admission signals

Hard reject text proven to be:

- a system, developer, skill, hook, environment, or tool-result injection;
- hidden reasoning or a raw tool result;
- a duplicated dialect representation of an already normalized turn;
- an API error or empty response;
- a tombstoned or excluded session.

Lower, but do not necessarily hard reject, candidates that are:

- generic implementation status (`implemented`, `fixed`, `tests pass`) with no
  rationale or non-obvious constraint;
- a one-off task scope or deadline;
- an assistant plan that has not executed;
- a review finding that has not been accepted or fixed;
- copied historical or compaction context;
- an exact or near-exact statement already present in current code, docs, or a
  commit message.

The final item preserves the existing "not already known" quality contract. In
the first phase, only exact/high-coverage deterministic matches may suppress a
candidate. Fuzzy code/doc/git matching is advisory evidence until its false
negative rate is measured.

### Candidate scoring

Use an inspectable integer score. Do not hide admission behind a learned model.
An initial rubric should include:

| Signal | Suggested weight |
| --- | ---: |
| Direct human standing-rule or preference phrase | +6 |
| Direct human explicit revision | +6 |
| Complete closed negative: attempt, evidence, revisit condition | +5 |
| User explicitly accepts the immediately preceding proposal | +4 |
| Assistant decision/discovery with same-locus successful checkpoint | +3 |
| Assistant claim with matching validation evidence | +2 |
| Independent recurrence in another session | +2 |
| Session final narrative | +1 |
| `agent_review` assistant claim without later acceptance/fix | -8 |
| Generated/injected origin | hard reject |
| Implementation status without rationale | -4 |
| Task-local scope or temporary state | -4 |
| Exact current code/docs/git restatement | -5 or suppress |

The threshold and weights are versioned candidate rules, not user-facing fact
confidence. Dry run reports the score and reason histogram. Evaluation, not the
round numbers above, sets the shipped threshold.

### Role-aware windows

Build the smallest window that lets the quality gate judge resolution and
authority:

- For a user trigger, include that user turn and its following assistant
  response.
- For an assistant trigger, include the preceding user request, the triggering
  assistant span, and the next user acceptance, correction, or rejection.
- For an explicit user acceptance, include the immediately preceding assistant
  proposal and the acceptance turn.
- For a closed negative, include the result/evidence sentence and a bounded
  adjacent span carrying the attempted approach and revisit condition.
- Merge overlapping cards from the same exchange or adjacent turn pair.

Each rendered turn begins with an explicit role and anchor, for example:

```text
CANDIDATE candidate:abc123
AUTHORITY human
EVIDENCE user_acceptance, checkpoint_success
[USER session:... turn:12 line:418]
Going forward, keep JSON output on stdout and progress on stderr.
[ASSISTANT session:... turn:12 line:419]
Understood. The command will preserve that split.
END CANDIDATE
```

Default card size is 4 KiB with a hard maximum of 8 KiB. If necessary, retain
the trigger span, acceptance/correction, and structured evidence before generic
adjacent prose. Never split one card across packs.

## Candidate Authority And Corroboration

Authority and truth are separate. A human can authoritatively express a
preference, but a human statement about current code can still be stale. An
assistant can accurately discover a gotcha, but the claim needs evidence before
it becomes durable memory.

### Human assertions

Direct human standing rules, preferences, and explicit revisions receive the
highest authority prior. They still pass the durable-fact quality gate for
self-containment, repository relevance, durability, and duplication.

Imperatives in a task request do not automatically become conventions. "Run
the Windows tests for this patch" is task-local. "Always run the Windows tests
before release" is a standing rule. Candidate features must expose that
distinction to the agent.

### Assistant claims

An assistant claim is eligible only when at least one corroboration route is
present:

1. The next human turn explicitly accepts, corrects, or restates it.
2. The session produces a successful checkpoint or attributed commit touching
   the same file or symbol locus.
3. A deterministic validation record supports the same claim or locus.
4. The same normalized claim recurs in an independent session with different
   provenance.
5. A later human turn cites the claim as established context.

Corroboration admits the card; it does not prove the final wording. The fact
agent must still avoid claims already obvious from current code, docs, or git.

`agent_review` is a special trust boundary. Its findings are hypotheses by
default. A review claim can become eligible only after a later accepted fix,
human confirmation, or matching committed change. The original review remains
provenance, but review prose alone cannot create an active gotcha.

### Deterministic evidence envelope

Candidate generation may attach a compact evidence envelope assembled from
existing local sources:

- session manifest: session kind, agent, branch, checkpoint, files touched;
- episode layer: intent and reinforcement (`success`, `corrected`, `neutral`);
- history projection: bounded decision and validation summaries;
- checkpoint/commit join: attributed subject/body excerpts and changed files;
- current docs/code/git: exact-known or possible-known markers;
- existing fact store: exact id, subject/locus neighbors, and prior status.

Evidence is capped per candidate and rendered as structured fields, not as a
second transcript. Commit bodies and summaries are corroboration or
known-source evidence; their existence alone does not earn a fact.

## Stage 3: Candidate Packing And Fact Extraction

The existing distill prompt remains the source of the durable-fact gate and
taxonomy instructions. It needs an additive candidate-card contract:

- input contains one or more delimited cards with stable ids;
- every output line begins with the candidate id it came from;
- a candidate may emit zero to the existing per-source fact cap;
- facts may cite only candidate ids present in the pack;
- every member emits either fact lines or an explicit no-facts sentinel; blank
  output is a protocol failure, never implicit success.

The parser strips the candidate id before passing the current
`kind<TAB>path<TAB>fact` fields to fact construction. Unknown or missing candidate
ids and conflicting completions fail the pack before bounded isolation. Multiple
valid fact lines may repeat their member id. These rules apply to every provider;
neither malformed framing nor invalid taxonomy is an implicit negative result.

### Pack construction

- Sort cards by session creation time, session id, and source turn.
- Pack 8-32 cards while keeping rendered input at or below 16-32 KiB.
- Keep a session's adjacent cards together when capacity allows, but permit
  cross-session packing because every card carries complete identity.
- Never mix repositories in one pack.
- Do not add existing facts to every extraction pack. Existing-fact comparison
  belongs to deterministic reconciliation after extraction.
- Dispatch packs concurrently under the existing `--concurrency` limit.
- Consume and apply member results in canonical chronological order regardless
  of completion order.

### Failure isolation

A provider or parser failure for a pack is not a success for any member.
Retry once by bisecting the pack so one malformed or oversized card cannot
starve its neighbors. Preserve the existing fast abort for repeated provider
misconfiguration.

Successful empty output is persisted for each candidate member. Successful
facts are also persisted per candidate member. This makes repacking,
concurrency changes, and an appended neighbor free on the next run.

Here, empty means an explicit validated `NO_FACTS`, never blank or discarded
output. Force must stage replacement results before removing prior ownership:
failed selected members retain their existing applications. Invalidate historical
Ollama extraction-cache entries whose empty results cannot be distinguished from
this fallback, using a versioned extraction policy; do not replay them as valid
negatives. Regression tests must cover malformed, omitted, truncated, and invalid
taxonomy output in cold, cached, incremental, and force runs, including a prior
valid fact, bounded recovery exhaustion, and a later explicit negative result.

### Expected call shape

The simplest MVP may use one pack per candidate-bearing session. On the
measured corpus, the decision/learning-derived normal-session bound is 57
extraction calls instead of 579 at 64 KiB, about 90 percent fewer. Cross-session
packing should reduce that further to approximately 30-60 extraction calls.

The candidate pipeline makes no routine reconcile calls. Against the current
upper bound of 1,158 extraction-plus-reconcile calls, 30-60 total calls is a
94.8-97.4 percent reduction. These figures must be remeasured with the final
trigger rules and card context before becoming a product claim.

## Stage 4: Stage-Separated Content-Addressed Cache

The current cache answers one coarse question: "was this whole session
distilled under this exact prompt, reconcile prompt, threshold, agent, model,
effort, branch, checkpoint, and preprocessed content?" Candidate-first cache
state separates independent invalidation domains.

### Cache layers

| Layer | Key includes | Key excludes | Cached value |
| --- | --- | --- | --- |
| Normalization | normalizer version, session id, source digest/turn content digests | path, branch, checkpoint, model | line ranges, stable turn ids/digests, append cursor; no text |
| Candidate generation | candidate-rule version, authority, trigger class, ordered turn digests, evidence-feature digest | path, branch, checkpoint, taxonomy, model, effort | candidate id, anchors, score/reasons, card input digest; no card text |
| Redacted extraction | candidate-card digest after redaction, extraction-prompt version, taxonomy digest, agent command identity, model, effort | branch, transcript path, confidence threshold, packing policy | empty or parsed fact lines per candidate |
| Reconciliation | reconciliation-rule version, candidate fact ids, active-neighbor identity | model, effort, packing policy | deterministic action or proposal identity |
| Materialization | extraction result id, branch, provenance identity, fact-store generation | model, card layout | application receipt |

Changing a model or prompt may invalidate compact extraction results. It must
not invalidate normalization or candidate discovery. Changing the confidence
threshold only replays deterministic application. Moving a transcript path or
replaying one session onto another branch only updates provenance or branch
materialization.

### Append behavior

For an append-only transcript:

1. Revalidate canonical source identity and the last persisted complete-turn
   boundary.
2. Parse from that boundary, including one prior turn so a new acceptance can
   complete the previous assistant candidate.
3. Preserve unchanged turn and candidate ids.
4. Extract only newly admitted or boundary-changed cards.
5. Materialize only new branch/provenance receipts.

If the source changed before the saved boundary, fall back to digest comparison
of all typed turns. Recompute only cards whose ordered turn-digest set changed.
Whole-session reprocessing remains the safe fallback for an unsupported or
malformed dialect, never a silent partial read.

### Persistence and interruption

Persist stage state under a versioned directory:

```text
facts/
  distill-cache.json                 # legacy v1, unchanged
  distill-v2/
    state.json                       # schema and rule versions
    sessions.ndjson                  # source identity, turn digests, cursors
    candidates.ndjson                # content-free candidate metadata
    extractions.ndjson               # per-candidate empty/fact results
    applications.ndjson              # branch/provenance receipts
```

All files use existing brain-relative safe readers, `0600` writes, atomic
replacement, and the brain write lock. Compact or shard them if measured size
requires it; do not introduce one file per candidate.

The pipeline may prepare and call agents outside the global write lock, as it
does today. Periodic flushes persist only complete stage records. On resume,
content-derived ids make reapplication idempotent.

## Stage 5: Conservative Deterministic Reconciliation

The candidate pipeline removes the default reconcile-agent call. It uses a
precision-first deterministic ladder.

### Step 1: Exact identity

If candidate text and normalized paths produce an existing fact id, union its
provenance through `factmerge.Upsert`. This is the existing safe path.

### Step 2: Safe paraphrase block

Only compare different ids when they share a semantic subject block:

- same fact kind;
- at least one strong locus or identifier token in common; and
- compatible top-level taxonomy categories.

A taxonomy path alone is not a block key. Broad paths such as
`constraints.invariants.general` routinely contain unrelated subjects.

Within the block, deterministic normalization may merge only obvious
paraphrases at a deliberately high threshold, for example:

- normalized punctuation/whitespace/code quoting;
- identical polarity;
- identical normalized subject identifiers;
- token-shingle or weighted-Jaccard similarity above a measured threshold;
- no distinct numeric, flag, path, or quoted-value conflict.

The earlier fact text remains canonical and provenance is unioned. Every
automatic paraphrase merge reports its rule and score.

### Step 3: Supersession

The MVP performs no automatic supersession across different fact ids. It keeps
both facts active and separate. A different-id pair may create only the
optional, local `possible_same_subject` relationship when its conservative
branch/kind/taxonomy/strong-locus block qualifies. It never creates an
executable merge or supersede proposal by default.

A later phase may auto-supersede only when all of the following hold:

- the newer source has direct human authority or an accepted assistant claim;
- an explicit revision marker identifies replacement or reversal;
- old and new facts have the same normalized subject key and strong locus;
- source chronology is unambiguous;
- polarity/value analysis identifies the exact changed predicate;
- the rule has 100 percent precision on the retained supersession sealed
  evaluation corpus.

Any uncertainty keeps the facts separate and may create only the qualifying
local neutral relationship; its list view is non-mutating. One wrong
auto-supersede disables the future rule until reviewed.

### Step 4: New fact

If no exact or safe paraphrase relationship exists, add the candidate as new.
Distinct facts are preferable to silently collapsing unrelated knowledge.

### Relationship behavior

Neutral-relationship listing is read-only. Repeated qualifying evidence may add
bounded local relationship ownership without changing either fact, creating an
executable proposal, or requiring a review action.

## Branches, Checkpoints, And Provenance

Extraction content is branch-independent; fact materialization is not.

- One candidate extracted from an identical session turn can be reused across
  branch exports.
- Each branch application creates or unions the provenance appropriate to that
  branch's session/checkpoint view.
- The fact record keeps the resolved branch used by the current fact store.
- Branch- and session-limited distill runs carry untouched cache and fact state
  through exactly as the current pipeline does.
- Entity provenance may still sharpen the candidate's checkpoint to the
  checkpoint that changed its code locus.
- A checkpoint change with identical candidate content updates provenance; it
  does not repeat extraction.

Chronological application order remains source session creation time, then turn
ordinal, then candidate id. A force rebuild and an incremental build must
produce the same fact/proposal state for the same candidate and fact inputs.

## Storage And Privacy

Candidate-first distillation reduces egress but does not weaken privacy
requirements.

### No second transcript store

- Typed turn bodies and rendered role windows live in memory only.
- Persist content digests, ids, anchors, scores, reason codes, and parsed fact
  output, not raw candidate cards.
- Extraction cache fact text is equivalent in sensitivity to the durable fact
  store and receives the same permissions and lifecycle treatment.
- Logs, progress output, and manifests contain counts and candidate ids, never
  candidate excerpts.

### Egress

- Candidate packs pass through the same redaction path used for current
  transcript chunks before an external agent sees them.
- Redaction version and redacted-card digest participate in the extraction key.
- Global no-egress/local-only policy, loopback-only Ollama enforcement, Codex
  and Claude tool disabling, timeouts, and output limits remain unchanged.
- Dry run and candidate planning make no agent or network call.

### Exclude, tombstone, and purge

Before planning or cache lookup, filter tombstoned sessions. A cache hit is
derived output from that session and therefore cannot bypass exclusion.

`privacy purge` removes:

- normalized session state for the purged source;
- candidate metadata and extraction results whose only provenance is purged;
- application receipts for the purged session;
- distilled facts or provenance anchors under the existing purge rules;
- related FTS/vector state under existing lifecycle behavior.

Candidate entries with additional non-purged provenance may survive only after
their source union is recomputed without the purged anchor. A content digest is
still derived session state and must be removed even though it is not plaintext.

Bundles and publish output exclude all `distill-v2` caches by default. Durable
facts follow their existing bundle policy; transcripts and candidate windows do
not enter bundles.

### Canonical-read safety

Keep complete-or-refused preflight before provider calls or any incremental
publication. Incremental cursor optimization may reduce parsing work only after
descriptor-rooted source identity and the saved boundary are revalidated.

## CLI And Manifest Contract

Rollout uses an explicit pipeline selector:

```text
entire brain distill --pipeline legacy
entire brain distill --pipeline candidates
entire brain distill --pipeline candidates --dry-run --json
```

During experimentation, `legacy` remains the default. After all gates pass,
`candidate` becomes the default and `legacy` remains a compatibility/debugging
escape hatch for at least one release cycle.

The dry-run JSON contract gains additive fields:

```json
{
  "pipeline": "candidates",
  "normalizer_version": 1,
  "candidate_rules_version": 1,
  "sessions": 154,
  "unique_session_contents": 146,
  "turns": 3077,
  "candidate_cards": 856,
  "candidate_sessions": 98,
  "candidate_bytes": 1660135,
  "packs": 57,
  "cache": {
    "normalization_hits": 0,
    "candidate_hits": 0,
    "extraction_hits": 0,
    "application_hits": 0
  },
  "authority": {
    "human": 12,
    "assistant_corroborated": 803,
    "assistant_suppressed": 41
  },
  "drop_reasons": {
    "injected": 120,
    "review_unaccepted": 41,
    "below_score": 900
  },
  "estimated_extraction_calls": 57,
  "estimated_reconcile_calls": 0
}
```

Field values above illustrate the target contract. Legacy fields remain present
where their meaning survives. The initial candidate implementation does not run
the allocation-heavy legacy preprocessor merely to compute a comparison metric:
it reports `preprocessed_bytes: 0`,
`preprocessed_bytes_available: false`, and the independently measured
`candidate_bytes`. Run a separate legacy dry-run for the legacy-preprocessed
baseline. Candidate bytes must never be relabeled as `preprocessed_bytes`.

A future separately versioned distill-run source gains additive fields:

- `pipeline`;
- normalizer, candidate-rule, extraction-prompt, and reconciliation versions;
- typed-turn, candidate-card, candidate-session, candidate-byte, and pack
  counts;
- stage cache hits and misses;
- candidate authority and drop-reason histograms;
- exact/paraphrase merges, supersession proposals, and automatic
  supersessions;
- per-stage wall time and existing provider token usage.

Once that leaf exists, `facts status --json` reports the latest pipeline and
whether candidate cache state is current, stale, partially reusable, or
corrupt. Human-readable progress should show sessions scanned, candidate cards
admitted, packs completed, facts found, and cache hits without printing
excerpts.

## Migration And Compatibility

### Fact store

Existing fact records require no schema migration. Candidate extraction still
produces the existing six kinds, taxonomy paths, branch, origin, status,
confidence, locus, and provenance. Exact ids continue to collapse with existing
facts.

An opt-in candidate run is additive:

- preserve authored facts;
- preserve existing legacy-distilled facts;
- exact candidate matches union provenance;
- uncertain duplicates or contradictions remain separate; only qualifying local
  neutral relationships may be recorded, never executable proposals;
- do not retract legacy facts merely because no v2 candidate regenerated them.

`--force --pipeline candidates` refreshes candidate extraction and rebuilds only
v2-owned applications in the selected scope. It preserves authored facts, legacy
facts and anchors, and existing executable proposals. Successful replacements,
explicit negatives, and removed candidates may retire only their v2 ownership;
failed members preserve prior applications. Dry run must display this scope.
An unscoped force discovers stale v2 ownership through receipts and application
anchors; it does not authorize cleanup of unrelated fact or proposal stores.
A branch- or session-scoped force never expands into unrelated stores. A rebuild
that deletes legacy-distilled knowledge requires a separate migration contract
and is outside this plan.

### Cache

Keep `facts/distill-cache.json` for the legacy pipeline. Do not rewrite v1
entries into v2 keys: the two caches prove different work. The first v2 run
builds typed and candidate metadata locally, then reuses existing facts through
normal fact identity.

A corrupt or unsupported v2 cache falls back to deterministic reconstruction.
It never causes legacy cache deletion or fact-store truncation. New v2 schema
versions may lazily migrate content-free metadata only when the old key formula
is exactly reproducible; otherwise rebuild the affected stage.

### Flags

Preserve current `--branch`, `--session`, `--force`, `--agent`, `--model`,
`--effort`, `--confidence`, `--concurrency`/`--jobs`, timeout, dry-run, and JSON
semantics.

- `--max-chunk-bytes` remains a legacy option. Candidate mode exposes an
  additive pack-byte limit only if measurement shows the default cannot be
  fixed safely.
- `--force` refreshes v2 stages in selected scope and rebuilds v2-owned
  applications only; authored and legacy facts survive, as do prior applications
  for members whose replacement extraction fails.
- A future stage-specific repair command may invalidate normalize, candidate,
  extract, or apply independently. It is not required for the MVP CLI.
- Changing `--confidence` replays application without agent calls.
- Changing model or effort invalidates extraction results only.

### Rollback

Before candidate mode becomes default, a user can select `--pipeline legacy`
without deleting candidate cache state. The schema-v3 manifest is decoded
strictly by released binaries, so the initial slice does not persist
candidate-only fields there; command and dry-run output carry those metrics. A
later durable metrics leaf must be separately versioned before it is added. No
migration step rewrites canonical sessions.

## Observability And Cost Accounting

Every run, including dry run, must make the funnel inspectable:

```text
raw sessions
  -> safe parsed sessions
  -> visible typed turns
  -> triggered turns
  -> admitted candidate cards
  -> packed candidates
  -> extraction outputs
  -> exact/paraphrase/new/proposal actions
  -> active facts
```

Required counters:

- sessions selected, skipped, tombstoned, duplicated, append-resumed, and fully
  rescanned;
- raw, current-preprocessed, visible-turn, candidate-card, redacted-pack, and
  provider input bytes;
- turns by role, origin, and authority;
- trigger hits and admitted cards by class;
- hard rejects and below-threshold cards by reason;
- assistant claims corroborated, suppressed, and review-gated;
- packs planned, succeeded, split, retried, failed, and empty;
- cache hit/miss counts and bytes avoided for every stage;
- extraction calls, token usage, wait time, and facts emitted;
- exact merges, deterministic paraphrase merges, new facts, proposals,
  automatic supersessions, and rejected actions;
- persistence time, total time, and interruption recovery point.

Warnings remain bounded by the existing warning cap. A candidate id, rule id,
anchor, and reason code are sufficient diagnostics; plaintext excerpts are not.

Provider token accounting remains provider-reported and keeps completeness
metadata. Estimated tokens in dry run must be labeled estimates. Release
performance evidence retains the repository identity, pipeline version, model,
prompt/taxonomy digests, cache state, and claim scope.

## Evaluation Design And Admission Gates

No speed result can compensate for silently missing durable knowledge. Evaluate
candidate generation separately from agent extraction so a model cannot hide a
bad deterministic filter.

### Sealed provisional evaluation corpus

Build exact-span labels across:

- the largest multi-chunk sessions;
- ordinary one-chunk sessions;
- direct human preferences and standing rules;
- resolved architectural decisions;
- closed negatives with evidence and revisit conditions;
- assistant discoveries later accepted or implemented;
- rejected, corrected, and unresolved assistant claims;
- `agent_review` sessions;
- duplicate and continued sessions across branches;
- Codex, Claude Code, pi, and document-form dialect fixtures;
- sessions expected to produce no facts.

Audit all current `entire-graph` facts as an initial regression set. Include the
false reviewer preference, task-local review conventions, duplicate
`--no-network` claims, and unrelated repoKey supersession as explicit
should-not-emit or should-not-reconcile cases.

Labels record:

- should emit or should remain silent;
- exact supporting source span;
- authority and required corroboration;
- canonical fact text or acceptable semantic variants;
- kind, path, and locus expectations;
- whether the fact is already available in code/docs/git;
- expected relationship to existing facts.

For Phases 0–4, label and score this corpus through the sealed independent
Claude/Copilot/Cursor panel and the escalation policy above. Retain source
disagreements rather than forcing consensus. In Phase 5 it becomes the final
human-labeled evaluation corpus: human reviewers label the retained packets and
software recomputes the provisional metrics from those labels and evidence.

### Quantitative acceptance contract

Before scoring, seal a development corpus and a disjoint confirmation corpus.
Keep re-exports, continued sessions, and duplicate source content in the same
partition. Historical calibration items and any items used to change selectors,
prompts, or rubrics belong to development. Pin the split seed, source cutoff,
strata, corpus sizes, provider repetitions, metric implementation, and numerical
sample-size calculation in the evidence manifest before confirmation outputs or
labels are inspected. An incomplete contract is insufficient evidence.

Use fully labeled source sessions for the primary span and fact-recall gates:
enumerate durable facts and their supporting spans from all eligible source
turns, independently of whether the selector admitted them. The denominator is
the complete labeled durable-fact set in that sealed corpus. Candidate span
recall counts a fact only if an admitted card preserves its required supporting
span and qualifiers. Report the 98 percent authority-class subset separately
from the 95 percent all-fact denominator. Extraction recall counts each reference
fact once if a faithful output expresses it; precision divides supported durable
outputs by all emitted outputs. Report duplicates separately. Missing provider
results remain missing coverage and cannot be excluded to improve a score.

The all-admitted plus 500-filtered packet set is a diagnostic negative audit,
not a corpus-recall estimator. Keep its critical-miss veto. If a supplementary
sample-based population estimate is reported, retain each stratum's population
size and inclusion probability and use inverse-probability-weighted totals;
unweighted packet rates must not be described as corpus rates. The primary
fully labeled corpus results describe that fixed corpus, not unseen repositories.

Compute paired candidate-minus-legacy differences on identical source sessions
and retrieval tasks. Retain the existing retrieval evaluator's metric definitions
and pin its version; useful-per-1k is supported useful facts per 1,000 returned
tokens. For stochastic provider comparisons, use a predeclared paired stratified
cluster bootstrap with 10,000 resamples and a fixed seed: resample independent
session families (task families for retrieval), keeping both pipeline arms and
their paired provider repetitions together. Seal the repetition count, at least
three for nondeterministic providers, before scoring. Use development-only
variance estimates to choose enough independent families for at least 80 percent
power at one-sided alpha 0.05 for the two-point recall non-inferiority margin;
retain the calculation and resulting numerical counts. Do not stop early after
a favorable run or add samples after inspecting confirmation scores.

The one-sided 95 percent lower bound must be at least -0.02 for fact and retrieval
recall differences, and at least zero for durable-precision and useful-per-1k
differences. This defines the retrieval no-regression gate; failure to detect a
significant loss is not a pass. Fixed-corpus span recall must meet its stated
thresholds exactly. Zero denominators, inadequate independent families, missing
coverage, or an uncertainty method unsupported by the observed data yield
insufficient evidence. Safety requirements stated as 100 percent mean zero
observed violations across the retained fixtures and labeled corpus, not a claim
of a zero population error rate.

Delta-only rejudgment is for development and exact artifact reuse. Freeze the
implementation before evaluating confirmation; withhold confirmation judgments
from corrective work until its verdict is recorded. A failed confirmation set
becomes development evidence. Any permitted corrective slice must then pass a
new disjoint, predeclared confirmation set. Phase 5 recomputes the same frozen
metrics from human labels; a correction after human rejection likewise requires
fresh confirmation, even when unchanged development labels remain reusable.

### Agentic scoring protocol

The main agent seals each evaluation input before judging and accepts no score
without the corresponding evidence digest. Claude, Copilot, and Cursor score
candidate admission, fact quality, relationships, and retrieval independently
against the same rubric. Codex Astra is called only for the defined escalation
cases, with no access to an implementation diff beyond the sealed evidence.
Judge availability is itself measured coverage: one bounded recovery is
permitted, then the planned resolution workflow either records Astra's
digest-bound escalation judgment or fails closed. A change to code, corpus,
prompt, taxonomy, rubric, redaction, or provider configuration invalidates only
its affected digest partition; all unchanged valid verdicts remain reusable.
This bounded delta-only process limits development review cycles. It does not
replace the untouched confirmation requirement above.

### Candidate-generation gates

- At least 98 percent span recall for explicit human standing rules,
  preferences, explicit revisions, and closed negatives.
- At least 95 percent span recall across all sealed-evaluation durable facts.
- 100 percent rejection of proven system/developer/tool/hook/skill injections.
- No direct-human turn may be reclassified as generated solely because it uses
  imperative language.
- Candidate output and ids are byte-for-byte deterministic across repeated
  runs.
- The sealed, stratified 500-exchange filtered-out agentic audit finds no
  critical missed standing rule or closed negative. Any miss updates the
  provisional evaluation corpus and blocks the provisional gate until the one
  permitted corrective slice and delta-only rejudgment complete.

Because lexical cues can miss implicit decisions, retain a sampled shadow lane:
for a fixed, deterministic fraction of below-threshold turns, run the legacy
quality gate offline during evaluation. Have the independent panel score any
fact it emits that candidate mode missed. This sampling lane is evaluation-only
and budgeted; it is not a steady-state hidden cost.

### Fact-quality gates

Run paired legacy and candidate extraction with the same prompt, taxonomy,
model, effort, and source cutoff. Repeat provider runs at least three times when
the provider is nondeterministic. The independent panel scores the paired,
sealed outputs before any human review.

Measure:

- durable-fact precision and recall;
- source faithfulness and exact-anchor correctness;
- human-authority and corroboration correctness;
- resolved-versus-in-flight accuracy;
- known-from-code/docs/git exclusion;
- kind, path, and locus accuracy;
- active duplicate rate;
- merge and supersession precision;
- facts emitted per 1,000 input tokens.

Required gates:

- Candidate mode is non-inferior to legacy fact recall within a predeclared
  two-percentage-point margin and has higher or equal durable precision.
- Every emitted fact is supported by its cited candidate span.
- Automatic exact merges have 100 percent identity correctness.
- Deterministic paraphrase merge precision is at least 99 percent on labeled
  pairs; otherwise ship exact-only.
- Automatic supersession precision is 100 percent. The MVP reaches this
  vacuously: it performs no automatic different-id supersession or proposal.
- Review-origin hypotheses never become active solely from review prose.

### Retrieval gates

Run the existing facts evaluation arms on the resulting stores. The independent
panel compares useful facts per 1,000 tokens, precision, recall, and answer
support. Candidate mode must not claim a retrieval win unless the sealed agentic
paired evaluation supports it; the final Phase 5 human recomputation must then
confirm that result under the existing release evidence policy.

At minimum:

- no useful-per-1k regression under the paired lower-bound test above;
- no more than a two-percentage-point absolute recall loss against legacy
  facts;
- regression tasks for every current live-corpus quality failure;
- source-session baselines retained so fewer facts cannot be mistaken for
  better retrieval merely because output is shorter.

### Operational gates

On the measured `entire-graph` corpus, target:

- at least 80 percent fewer extraction calls than 64 KiB legacy mode;
- at least 90 percent fewer total agent calls than the extraction-plus-reconcile
  upper bound;
- at least 85 percent fewer provider input tokens;
- no reconcile-agent calls in candidate mode;
- a no-op rerun makes zero provider calls;
- appending a no-candidate turn makes zero provider calls;
- appending one isolated candidate makes at most one provider pack call;
- transcript relocation and branch replay make zero extraction calls;
- a confidence-threshold change makes zero provider calls;
- model/prompt/taxonomy changes reprocess candidate cards, not full transcripts;
- interruption loses no more than the configured completed-pack flush window.

Retain exact duplicate, continued-session, model-change, prompt-change,
taxonomy-change, branch-filter, session-filter, force, tombstone, purge, and
corrupt-cache scenarios as integration tests.

All candidate-generation, fact-quality, retrieval, and operational thresholds
in this section are provisional agentic gates in Phases 0–4. They require the
sealed independent-panel resolution and evidence digest, not an interim human
score. Phase 5 repeats the calculations from a final human-labeled corpus and
requires explicit human approval before changing the default.

## Rollout Plan

### Phase 0: Measurement-only candidate planner

- Delegate typed normalization and candidate planning behind candidate-mode dry
  run to a Sol, Terra, or Luna implementation subagent.
- Emit the full funnel and projected packs without making an agent call or
  writing v2 cache/facts.
- Reproduce the measured `entire-graph` baseline in a retained, auditable
  artifact.
- Build the sealed provisional span-label and negative-audit packet sets; do not
  request human labels.

Exit gate: canonical read safety and dry-run determinism pass; candidate recall
meets the provisional agentic gates on fixtures and the first sealed corpus
slice.

### Phase 1: Typed turns and content-addressed discovery cache

- Delegate reusable visible-turn normalization, prompt-id/slash-command
  collapse, authority/origin tags, content-free state, append-boundary reuse,
  and purge integration to bounded Sol, Terra, or Luna work items.

Likely files:

- new `internal/cli/distill_candidates.go`;
- new `internal/cli/distill_cache_v2.go`;
- focused additions to `internal/cli/conversation.go` only where parsing must be
  shared;
- privacy lifecycle integration and tests.

Exit gate: no plaintext card cache, no privacy regression, and exact duplicate
and continued-session reuse are proven by tests and a sealed agentic gate.

### Phase 2: Candidate-pack extraction, shadow only

- Delegate candidate-id framing, deterministic pack construction, member-level
  results, split retry, and usage accounting to bounded Sol, Terra, or Luna
  work items.
- Run candidate extraction in shadow against selected sessions; do not mutate
  the active fact store.
- Compare outputs with legacy extraction through the independent agentic panel.

Exit gate: candidate and fact-quality provisional agentic gates pass on the
full sealed corpus; token/call accounting is complete. Human scoring remains
deferred to Phase 5.

### Phase 3: Conservative reconciliation and opt-in writes

- Delegate `--pipeline candidates` fact writes and lifecycle tests to bounded
  Sol, Terra, or Luna work items.
- Phase 3A ships exact-only reconciliation first and keeps all different ids
  separate. It does not overload merge/supersede proposals as neutral links.
- Keep the Phase 3B evidence-bearing neutral relationship store local,
  list-only, capacity-bounded, and separate from executable proposals.
- Validate neutral relationship precision and lifecycle parity through sealed
  agentic scoring before considering any downstream consumer.
- Treat neutral-relationship consumers as optional research: the required
  default path remains exact-only writes with no different-id automation.
- Keep safe paraphrase merging as optional research, only after its pairwise
  precision gate passes; exact-only is the required shipping path.
- Preserve legacy facts on incremental candidate runs.

Exit gate (met 2026-08-23): all live-corpus bad-reconcile fixtures pass,
force/incremental lifecycle parity holds for identical extraction results, and
no wrong automatic supersession is observed. This is historical Phase 3 exit
evidence, not a retroactive acceptance through the new sealed agentic panel;
any new Phase 3 delta follows the current agentic policy. Provider-output
repeatability is tracked separately by the still-open Phase 2 quality gate.

### Phase 4: Session-end incremental candidate distill

- Delegate the session-end hook, append-only parsing, and cross-export result
  reuse to bounded Sol, Terra, or Luna work items.
- Keep watch budget, interval, and no-egress policy unchanged.

Exit gate: no-candidate session end is zero-call and one-candidate session end is
at most one pack call under normal cache state, verified by tests and a sealed
agentic gate.

### Phase 5: Default rollout

- Retain candidate dry-run and paired release evidence on a small, medium, and
  large corpus.
- After all implementation and provisional agentic gates pass, run the only
  human scoring/adjudication process: independently label the final sealed
  evaluation corpus; software recomputes every quantitative quality and
  retrieval threshold from those labels, then humans explicitly approve or
  reject default promotion. It includes the representative
  span corpus, the 500 filtered-out exchanges, paired legacy/candidate outputs,
  source anchors, authority, taxonomy, exclusions, and retrieval support. The
  review adjudicates disagreements but does not start ad hoc implementation
  tuning inside the scoring session.
- A human rejection blocks promotion and produces one consolidated,
  evidence-bound remediation scope. After that bounded slice, rerun the
  affected agentic gates and changed human-evaluation delta, then evaluate a
  fresh disjoint confirmation corpus under the sealed quantitative contract.
  Reuse unchanged development labels, not a failed confirmation set as release
  proof; do not enter an unbounded tuning or review loop.
- Make candidate mode the default only after that final human approval confirms
  all quality, retrieval, operational, privacy, and migration gates.
- Keep `--pipeline legacy` for at least one release cycle and document rollback.
- Do not describe distillation as fast, cheaper, or recall-preserving beyond the
  exact retained evidence scope.

## Implementation Order And Test Map

The main agent decomposes this order into bounded, non-overlapping Sol, Terra,
or Luna implementation tasks and owns only orchestration, integration judgment,
and phase acceptance. Each task returns its patch and deterministic test
evidence to the main agent; independent Claude/Copilot/Cursor scoring is then
run on the sealed phase evidence, rather than asking implementation subagents
to self-accept their work.

1. Extract a typed visible-turn iterator with parity tests against existing
   conversation exchanges.
2. Add origin/authority classification and command-expansion regression
   fixtures.
3. Implement deterministic trigger scoring and role windows.
4. Add candidate planning JSON and measured-corpus benchmark tooling.
5. Add v2 content-addressed cache and append/purge tests.
6. Version the extraction prompt and parser with candidate ids.
7. Add deterministic packing, member cache, split retry, and concurrency tests.
8. Add exact-only reconciliation and application receipts.
9. Keep optional safe paraphrase rules behind precision fixtures, separate from
   the required exact-only release path.
10. Wire opt-in CLI, manifest/status fields, watch/session-end path, and
    release evidence.

Required test families:

- dialect parity and exact line anchors;
- slash-command, prompt-id, wrapper, compaction, pasted-agent, and hook
  authority;
- candidate trigger positive/negative tables;
- overlapping-window coalescing and card bounds;
- content-address stability across path, branch, checkpoint, packing, and
  concurrency changes;
- invalidation isolation for rules, prompt, taxonomy, model, effort, and
  confidence;
- append-boundary, mid-file mutation, and malformed-transcript fallback;
- cache-on-success, empty-result caching, split retry, crash/resume, and corrupt
  cache;
- exact dedup, unrelated same-path facts, polarity/value conflicts,
  paraphrase proposals, and force/incremental parity;
- tombstone/exclude/purge, bundle exclusion, file permissions, symlink/path
  traversal, and no-egress behavior;
- dry-run/manifest JSON compatibility and bounded warning output;
- retained `entire-graph` cost and quality regression fixtures.

## Decisions Locked By This Plan

- Candidate generation is deterministic and model-free.
- Fact extraction remains agent-gated and silent by default.
- Human and assistant text carry different authority.
- Review findings are hypotheses until accepted or implemented.
- Candidate identity and extraction identity are separate cache layers.
- Candidate text is not persisted as a second transcript corpus.
- Branch materialization does not force re-extraction.
- Taxonomy path alone never defines a reconciliation subject.
- The MVP makes no automatic different-id supersession.
- Legacy mode remains available until candidate mode has retained quality and
  operational evidence and final human default-promotion approval.
- Claude, Copilot, and Cursor are independent provisional gate judges; Codex
  Astra is escalation-only, and human adjudication is Phase 5 only.
- Exact-only writes are required default blockers; safe paraphrase merging and
  downstream relationship consumers are optional research, not rollout
  prerequisites.

## Open Questions To Resolve With Evidence

Questions about implicit-decision recall, packing, known-from-code suppression,
turn identity, context sufficiency, and cross-repository sharing are required
default blockers when they prevent a sealed Phase 0–4 gate or final Phase 5
human recomputation. Safe paraphrase and downstream relationship research are
optional unless deliberately promoted into a future release scope.

- Which implicit-decision forms lack reliable lexical cues, and what sampled
  shadow rate is needed to estimate their miss rate?
- Does one pack per candidate-bearing session outperform cross-session packing
  after prompt-prefix caching and provider latency are included?
- What deterministic known-from-code/docs/git threshold suppresses obvious
  restatements without hiding rationale?
- Can a safe paraphrase rule clear 99 percent precision across repositories, or
  should v1 remain exact-only indefinitely? This is optional: v1 ships
  exact-only if it does not clear the threshold.
- Which source-provided turn ids are stable across every supported exporter,
  and where must normalized-content fallback identity remain explicit?
- How much adjacent context is needed for closed negatives and human acceptance
  without erasing the measured token reduction?
- Should candidate extraction results be shared across repositories when text
  is identical? The initial answer is no: repository context and taxonomy are
  part of extraction semantics even when a sentence matches.

These questions do not block Phase 0. Required blockers gate progressively
broader automation and the final default switch; optional relationship and
paraphrase research never delays exact-only promotion once the required gates
and final human approval pass.
