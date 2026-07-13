# Phase 0B Powered Dev Pilot — Memo

Date: 2026-07-13
Status: **diagnostic development pilot — NOT confirmatory evidence, NOT paper evidence**
Harness: `codex/brain-phase0b-fable-hardening` @ `af4640a6` (post causal-validity +
distiller-de-pinning fixes)
Data artifact: `phase0b-powered-pilot-results-v1.json` (aggregates + record hashes;
no memory content). Raw records live in the (uncommitted, private) suite
`results/phase0b-frozen-pilot-powered`.

## Purpose

First end-to-end run of the **fixed** Phase 0B temporal-memory harness on the
frozen phase0a source artifact, to (a) validate the harness/fixes on live agent
runs and (b) get a preliminary read on the memory-vs-`no_brain` hypothesis. It is
a shakeout, not a claim.

## Setup

- Task: `temporal-memory-default-fact-merge-confidence` (the committed **diagnostic**
  dev task; the README states it is not paper evidence).
- Conditions: `no_brain`, `raw_history`, `facts_only`, `history_facts`; 4 reps each
  (16 runs). Runner: `claude:sonnet:high`. Delivery lane: **agent_tool** (the agent
  drives memory itself; the harness did not perform the retrieval).
- Memory inputs: the frozen, content-hash-pinned source artifact
  (`source_cache_key = 1dd2312593bd40ce7e66f748`), loaded from cache. This run only
  succeeded because the distiller-de-pinning fix let those content-verified facts
  load without the (vendor-moved) codex binary present.

## Raw numbers (per condition, n=4)

| Condition | Validation pass | Mean score | Mean total tokens | Adherence-valid |
| --- | --- | --- | --- | --- |
| no_brain | 4/4 | 91 | 7.64M | 3/4 |
| raw_history | 4/4 | 96 | 0.26M | **0/4** |
| facts_only | 1/4 | 48 | 2.06M | **0/4** |
| history_facts | 4/4 | 96 | 0.27M | **0/4** |

`summarize` deltas vs no_brain: raw_history +5.5 (p=0.0003), history_facts +4.75
(p=0.0014), facts_only −42.5 (p=0.058). 0 infrastructure-excluded.

## The load-bearing caveat: the memory arms are protocol-INVALID

**All 12 memory-arm runs failed the required protocol-adherence audit**
(`memory_search_was_not_first_tool` + `memory_search_command_mismatch`,
`required=True`). Per the lane's rule (README): in agent_tool mode a memory arm is
valid **only if its first tool action is exactly one `entire brain search`**. The
claude runner never did that. Therefore:

- The outcomes **cannot be causally attributed to the memory channel** — we do not
  know the agent used the delivered memory as prescribed (it may have solved the
  task by code exploration regardless).
- The score/token comparisons above are observations from **non-compliant** runs.
  They are not a channel-attributed result and must not be cited as one.
- Two runs (one `facts_only`, one `no_brain`) also flagged
  `forbidden_memory_artifact_access`.

The audit **correctly rejecting** these runs is itself a positive result: the
harness's causal-validity gate works.

## What this pilot DID validate (the real wins)

1. **Distiller de-pinning fix works end-to-end.** The frozen, content-verified facts
   loaded and drove real agent runs even though the pinned codex binary had moved
   (`/Applications/Codex.app` → `/Applications/ChatGPT.app`) and changed hash. No
   integrity check was weakened — facts are still content-addressed.
2. **F2 infrastructure-exclusion works on live data.** In the earlier single-rep
   attempt, a pre-agent harness failure was correctly tagged `analysis_excluded`
   and dropped from arm means (no fabricated comparison). In this clean run, 0
   exclusions.
3. **The fixed harness runs 16 real agent sessions** with valid, provenance-backed
   records and a correct `summarize`.

## Preliminary observations (NOT claims — all memory arms were invalid)

- `facts_only` correlated with task failure: 3/4 runs scored ~34 and failed
  validation (one outlier passed at 91). If it survives a valid re-test, it would
  suggest the **distillation produced misleading facts** for this fact-merge task,
  while raw history was reliable (`history_facts`, which includes raw history,
  recovered). Worth a targeted, valid investigation.
- `no_brain` mean tokens (7.64M) are anomalously high vs the memory arms (~0.26M).
  Likely a blind-exploration loop; unexplained. Do not read the ~30x "efficiency"
  as a validated win — it is entangled with the adherence failures and this
  anomaly.

## Learnings to carry into future runs

1. **Run the causal lane in HARNESS delivery mode, not agent_tool.** Set
   `memory_delivery: harness` so the harness performs the single frozen retrieval
   and delivers the packet. That removes the agent-adherence confound entirely
   (no dependence on the agent choosing to `entire brain search` first) — which is
   exactly why this pilot's agent_tool comparison is uninterpretable. This is the
   single most important change for a valid pilot.
2. **The claude agent-tool runner does not satisfy the search-first adherence
   rule** as-configured. If an agent_tool comparison is ever wanted, the adapter/
   prompt must make the first action exactly one `entire brain search`, or the
   audit will (correctly) invalidate every memory arm.
3. **Content-addressing beats binary-pinning.** Trust facts by their content hash;
   never gate on an external agent/model binary path or hash (they are vendor-
   updated). Fixed here across validation, cache key, PATH, and provenance.
4. **`facts_only` merits scrutiny.** The distillation-misleads hypothesis is the
   most interesting thread; test it in the harness lane with reps before believing
   it.
5. **Provenance for any future comparison:** pin harness commit, source_cache_key,
   fact_artifact_sha256, runner, and repetitions (all recorded in the results
   artifact), and keep `no_brain` in the SAME delivery lane as the memory arms.

## Reproduction

```
# from benchmarks/agent-brain, with the frozen source cache present in ./cache
AGENT_BENCH_REPO_ROOT=/Users/thomi/Projects python3 run.py run \
  --tasks tasks/temporal-memory-default-fact-merge-confidence.json \
  --agents claude:sonnet:high \
  --conditions no_brain,raw_history,facts_only,history_facts \
  --repetitions 4 --suite-name <name>
```

Records: `results/<name>/…/record.json`; aggregate via `run.summarize`. For a
**valid** causal pilot, first add `"memory_delivery": "harness"` to the task (or a
task variant) so the harness delivers memory and adherence is not agent-dependent.
