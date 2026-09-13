# Evidence recall integration and independently labeled pilot

Date: 2026-09-13. Local experimental work; not release evidence or a production
default. See the [usage and contract guide](../../docs/recall-evidence.md).

## Implementation

`recall --evidence` retrieves canonical session text with lexical ranking and
fact-provenance expansion, selects immutable source spans, and returns original
text with verifiable file/string hashes and UTF-8 ranges. Required/conflicting
blocks are kept together; corroboration is optional. Model relationships are
query-scoped and never update stored facts. A deterministic `--agent none` path
is available; provider failures/invalid responses fall back with warnings.

The path enforces branch and privacy guards and reports missing sources, input
omissions, output omissions, actual returned counts, timing and usage. It is a
CLI feature, not yet an MCP tool option or Graph retrieval integration.

Brain was fast-forwarded to `7af5ecdb`, including newly merged
[PR #249](https://github.com/entireio/entire-brain/pull/249) and
[PR #253](https://github.com/entireio/entire-brain/pull/253). Existing local
experiments were preserved. The byte budget covers the evidence array; it is
distinct from MCP's newly corrected whole-response budget.

## Frozen external-label comparison

Used the published cleaned LongMemEval **oracle** file: first four questions in
file order for each of six question types, plus four abstentions. Protocol,
question IDs, dataset hash and expected answers were frozen before live calls.
Gold answers and turn labels never enter recall or reader requests. These labels
were authored independently of this work; histories are a research benchmark,
not newly human-reviewed local coding sessions. Oracle supplies the relevant
sessions, so this measures selection and reading, not full-corpus retrieval.

Both arms used the same canonical sessions, no distilled facts, `k=128`, and an
8,192-byte evidence-array budget including provenance. Fresh reader contexts were
used for each arm. Selector, reader and judge used `gpt-5.6-sol`, effort low,
through the installed Codex CLI. One repetition; model alias, not immutable
backend revision. The judge used the upstream task-specific prompts adapted to
structured JSON, not the official fixed-model leaderboard protocol.

| Category | Deterministic blocks | Selected blocks |
|---|---:|---:|
| abstention | 4/4 | 4/4 |
| knowledge-update | 1/4 | 3/4 |
| multi-session | 0/4 | 0/4 |
| single-session-assistant | 1/4 | 4/4 |
| single-session-preference | 1/4 | 3/4 |
| single-session-user | 0/4 | 1/4 |
| temporal-reasoning | 1/4 | 4/4 |
| **All questions** | **8/28** | **19/28** |

All 28 questions stay in each denominator, including 2
selected-arm process failures. Selection modes: {"model": 21, "fallback": 5, "process_error": 2}.
This small stratified pilot is not a general accuracy estimate. In particular,
multi-session questions remain weak and update selection can still regress.

There were 5 identical reader requests
across arms; 1 received different
correctness scores. Reader/judge variance therefore contributes to the observed
gap; it cannot all be attributed causally to the selector. Published data may
also occur in model training. Independent gold labels do not make model grading
independent or infallible.

## Runtime, failures and amendments

Observed selector median: **104.4 seconds**. Maximum recorded:
1393.4 seconds. This run included abnormal long wall-clock waits and
two outer-process timeout failures; it is not a controlled latency benchmark.
The selector adds substantial latency and remains explicitly opt-in.

The provider adapter lacked the `WaitDelay` already used by Brain's general
command runner. A portable child/grandchild test reproduced a 500 ms deadline
waiting 15.07 seconds for inherited pipes. Setting the same two-second pipe-drain
backstop makes that regression test pass. It bounds the caller's wait; it does
not establish the remote provider's billing/cancellation behavior. The failed
primary attempts remain failures in this report; no post-fix rerun replaces them.

One empty-packet reader emitted `NO_EVIDENCE`, allowed by the original harness
schema but rejected by its citation checker. Its original answer was retained,
its invalid citation counted, and the missing answer grading completed without
rerunning the reader. Future empty-packet schemas require an empty citation
array. Invalid citation rows: deterministic 0,
selected 1.

After freezing the evaluation binary, implementation checks added a JSON nesting
ceiling, registered the ephemeral model decoder in the persisted-state guard's
allowlist, and fixed provider pipe draining. The selector prompt, retrieval,
stable IDs and packing were not tuned against the benchmark. Both binaries and
the evaluation-build source snapshots are retained.

| Role | Calls | Reported input tokens | Reported output tokens | Cached input tokens |
|---|---:|---:|---:|---:|
| reader | 54 | 606,851 | 9,737 | 0 |
| judge | 54 | 463,164 | 3,059 | 0 |
| selector | 28 | 681,767 | 111,442 | 45,696 |

Cached input is a subset of reported input, not additive. Selector usage is
missing for 5 attempts, so reported totals
understate actual usage. No dollar cost is claimed; provider billing data was
not available. Exact call/usage artifacts and timings are retained.

## Verification and retained artifacts

- Reconstructed **515 returned spans** independently
  from canonical source files; file hashes, decoded-string hashes, stable IDs,
  byte intervals, returned counts and byte budgets all matched. This verifies
  bytes and accounting, not semantic correctness.
- Recall, distillation, timeout, privacy-boundary and decoder-guard checks pass;
  final binary builds and `go vet ./internal/cli` passes.
- The final binary reproduces all 28 frozen candidate-request digests and
  deterministic packets exactly, with no additional model calls.
- The full CLI suite initially had 36 failing top-level tests. The one integration
  issue (strict decoder registration) was fixed. **All 35 other failures reproduced
  on a clean copy of merged main**: Windows symlink privileges, Git `NUL` handling,
  and a rooted-path assumption in the newly merged MCP test. Full-suite green is
  not claimed. Before/after logs and baseline failure sets are retained.

Artifacts: `bin/memory-recall-evidence-20260913/`, including the published input,
frozen protocol, isolated stores, original reader/judge calls, failed-attempt
records, scores, integrity audit, source snapshots, test logs and manifest.
No benchmark result was pushed, committed, or admitted to the release evidence
lane. The next useful work is to reduce selector payload/output cost and address
multi-session evidence coverage before a broader deployment.

Evaluation binary SHA-256: `56e11f790cf821ad79ab68f7c2bbd10c1421f00c7d0258e2fe8cf86355941cc4`.
Final binary SHA-256: `5ac8c2ef7c78ddfc450616dad7a32d462456e9b0fdbafa5c99cc1dd804ef18c1`.

Sources: [LongMemEval repository](https://github.com/xiaowu0162/LongMemEval),
[cleaned dataset](https://huggingface.co/datasets/xiaowu0162/longmemeval-cleaned),
[upstream grading script](https://github.com/xiaowu0162/LongMemEval/blob/main/src/evaluation/evaluate_qa.py).
