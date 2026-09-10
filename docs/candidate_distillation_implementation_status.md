# Candidate Distillation Implementation Status

Updated 2026-09-10 UTC. Candidate mode remains opt-in. Phase 2 acceptance failed
closed after the bounded development evaluation; default promotion is blocked.
This implementation does not claim corpus recall, fact quality, or retrieval
non-inferiority.

## Delivered implementation

Commit `540d6688` contains the runtime and evaluation contracts:

- Ollama uses the strict candidate protocol. Blank, omitted, malformed, or
  invalid-taxonomy results remain unresolved; only explicit valid `NO_FACTS`
  can cache an empty result and retract a candidate-owned application. The
  Ollama extraction policy identity invalidates historical ambiguous negatives.
- Oversized authoritative turns split only at independently meaningful cue
  paragraphs. Coupled or indivisible oversized triggers still fail preflight.
- A bounded, content-free discovery cache reuses exact source and complete
  newline-delimited JSON append boundaries. Mutation, partial records, changed
  authority evidence, and unsupported formats fall back safely. Canonical source
  reads remain necessary; suffix normalization savings are not byte-read savings.
- Session-end hooks and watch accept `--pipeline candidates`, retaining the
  captured branch/session and existing policy defaults. Discovery counters are
  available in command output and are scrubbed from strict schema-v3 manifests.
- Discovery state participates in privacy purge plans, receipts, verification,
  atomic-temp cleanup, and publication exclusion.
- Quality verdict reuse preserves exhausted recovery budgets across rebases.
  `facts distill-quality resolve` validates and resolves imported digest-bound
  phase judgments. The quantitative evaluator requires complete source labels,
  paired outputs, development power calculations, and disjoint confirmation.

Commit `b4e498b4` adds `facts distill-quality snapshot`: private plugin roots for
legacy and candidate evaluation with identical canonical source and taxonomy,
empty fact stores, bounded input, and source identity that includes authority
metadata. The existing `corpus` command exports every visible turn in a selected
scope, including zero-candidate sessions, with explicitly unset labels.

See the [evaluation workflow](../benchmarks/agent-brain/candidate_distillation_eval.md)
and [phase evidence contract](candidate_distillation_gate_evidence.md).

## Accepted bounded gates

The separate [Phase 2 readiness archive](candidate_distillation_evidence/2026-09-10/phase2.json)
records two `fail` judgments and one `insufficient_evidence` judgment. The
resolver returned `fail`, with no critical dissent. Its missing-reference and
quantitative-proof requirements are not discharged by the runtime gates below.

Claude `claude-haiku-4-5`, Copilot `gpt-5.4`, and Cursor `composer-2.5`
independently passed performance, quality, and progress for both scopes below.
There was no critical dissent, so no Astra vote was requested.

| Scope | Result | Retained archive |
| --- | --- | --- |
| Phase 3 strict-negative corrective delta | Three passes | [Phase 3](candidate_distillation_evidence/2026-09-10/phase3.json) |
| Phase 4 incremental session-end runtime | Three passes | [Phase 4](candidate_distillation_evidence/2026-09-10/phase4.json) |

These judgments bind tree `feae75d4cd34c8098c5dae65e26c0c956afe3d16`, the exact
runtime commit tree of `540d6688`. They followed one manifest-compatibility
correction: discovery counters must never become unknown durable v3 fields.
Earlier judgments on the superseded tree are not the accepted artifacts.
The later snapshot command adds an evaluation-only source preparation surface;
it does not change the reviewed extraction, discovery, or hook behavior.

## Verification

- Full `go test -race ./... -timeout 20m` passed on the corrected runtime tree.
- Focused candidate, hook, discovery, privacy, manifest, corpus, and gate tests
  passed with and without the CGO build tags.
- Lint, five-target cross-builds, and the Phase 1 contract suite passed.
- All nine quantitative-evaluator tests passed. Snapshot race tests and vet
  passed after the aggregate-bound and authority-identity corrections.
- The complete `mise run check` is **not green**: its CGO suite detects
  `TestVecStoreConcurrentSavePresentMergesUnderBrainLock`. The same race was
  reproduced on the unchanged plan commit `13f60d74`; it is not fixed here.

Checks used isolated tracked-file checkouts because ignored benchmark module
caches disrupt repository-wide Go package discovery. Those artifacts were kept.
The [verification receipt](candidate_distillation_evidence/2026-09-10/verification.json)
records the check scopes and log digests.

The hook tests suppress background worker launch so the test binary cannot
recursively launch another suite. The real candidate engine is still exercised
with a fake provider: zero calls, one call after an isolated candidate append,
then zero additional calls on replay.

## Measured scope

Cold call counts below are dry-run projections using local Qwen 2.5 7B and an
explicit 65,536-byte legacy chunk limit. They are not measured provider costs.

| Scope | Legacy exports / candidate views | Legacy chunks | Candidate packs | Legacy preprocessed bytes | Candidate bytes |
| --- | ---: | ---: | ---: | ---: | ---: |
| One candidate-bearing session | 1 / 1 | 2 | 1 | 87,858 | 2,751 |
| `entire-brain` development corpus | 186 / 186 | 359 | 87 | 14,448,382 | 1,063,344 |
| `entire-graph` development corpus | 189 / 186 | 637 | 87 | 36,415,818 | 1,126,865 |

Candidate mode coalesces re-exports in the large corpus. These totals therefore
describe each planner's selected views; they are not a paired quality estimate.
The large-corpus projection reduces extraction calls by 86.34 percent. Provider
input-token reduction and full-corpus latency remain unproven.

A one-member Ollama shadow smoke made one call, reported 2,710 input and 340
output tokens, and produced four protocol-valid facts with no unresolved member
in 40.482 seconds. Its immediate replay made zero calls. Protocol validity does
not establish semantic faithfulness. A later real discovery-cache replay made
zero provider calls in both runs: first one full scan, then one exact discovery
hit and no full scan.

### Bounded development comparison

Two complete sessions were exported into isolated empty legacy and candidate
stores. Each arm ran three times with the same source and taxonomy. All 12 runs
completed with provider-reported token accounting:

| Source scope | Pipeline | Facts in each repetition | Calls in each repetition | Input tokens in each repetition |
| --- | --- | --- | --- | --- |
| Zero-candidate, 53 visible turns | Legacy | 1 / 1 / 1 | 1 / 1 / 1 | 6,943 / 6,943 / 6,943 |
| Zero-candidate, 53 visible turns | Candidates | 0 / 0 / 0 | 0 / 0 / 0 | 0 / 0 / 0 |
| Candidate-bearing, 274 visible turns | Legacy | 4 / 4 / 4 | 2 / 2 / 2 | 26,423 / 26,423 / 26,423 |
| Candidate-bearing, 274 visible turns | Candidates | 4 / 4 / 4 | 1 / 1 / 1 | 2,710 / 2,710 / 2,710 |

These are development observations, not confirmation. Legacy's four-fact
identity set varied across repetitions; candidate identities were stable. Equal
fact counts do not establish equal quality, and the legacy-only fact is not a
proven candidate miss without validated reference labels.

The independent full-source labeling requests required complete turn coverage
and exact attributable quotations. After one bounded recovery, the 53-turn
scope had only one valid judge and the 274-turn scope had none. Invalid outputs
remain missing coverage. Neither source meets the two-valid-judge floor, and
Astra cannot replace that missing coverage.

The admission diagnostic retained Claude 556 completed / 17 invalid, Copilot
671 completed, and Cursor 616 completed / 55 invalid judgments. The remaining
98 Claude packets were cancelled after the full-source failure; 16 were in
flight and their token usage is unknown. No cancelled request is a verdict or
a negative label. The [development receipt](candidate_distillation_evidence/2026-09-10/development.json)
retains these counts, artifact digests, and explicit orchestration corrections.

## Remaining acceptance work

The full-source reference-label, paired fact-quality, retrieval, power, and
untouched-confirmation gates remain unaccepted. An all-admitted plus
500-filtered admission panel is diagnostic; it cannot supply their denominators.
Private development source labels and paired extraction experiments are retained
separately and must not be relabeled as confirmation or human proof.

Default promotion still requires those provisional gates, the final Phase 5
human recomputation, and explicit human approval. No new human audit was started.
Use `--pipeline legacy` to keep or return to the existing path; candidate cache
state need not be deleted.
