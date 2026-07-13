# Phase 0B Powered Dev Pilot — Memo

Date: 2026-07-13
Status: **diagnostic development pilot — NOT confirmatory evidence, NOT paper evidence**
Harness: `codex/brain-phase0b-fable-hardening` (post causal-validity, distiller-de-pinning,
and adherence fixes)
Data: `phase0b-powered-pilot-results-v2-harness.json` (VALID, harness-delivery lane).
Supersedes `-v1.json` (agent_tool lane — all memory arms were adherence-invalid).

## Headline finding (valid causal lane)

**Project memory substantially HELPS on this hidden-decision recovery task — in
both correctness and token efficiency.** With the harness delivering the frozen
memory packet (6 results per memory arm, adherence-clean), the null hypothesis
("no temporal-memory condition improves over no_brain") is falsified for raw
history and history+facts:

| Condition | Validation pass | Mean score | Mean tokens | Δ vs no_brain (p) |
| --- | --- | --- | --- | --- |
| no_brain | 1/4 | 50 | 6.69M | baseline |
| raw_history | 4/4 | 96.5 | 0.24M | +46.5 (p≈0.040), **~28x fewer tokens** |
| history_facts | 4/4 | 96.75 | 0.22M | +46.75 (p≈0.040), **~30x fewer tokens** |
| facts_only | 3/4 | 75.75 | 2.84M | +25.75 (p=0.24, n.s.) |

Without memory the agent fails 3/4 (recovering a deliberately hidden project
decision is hard from current code alone). With raw history (or history+facts) it
passes every time, scores ~46 points higher, and uses ~1/28th the tokens. Distilled
`facts_only` helps but is noisier (one run scored 33) and not significant at n=4.

## Why this corrects the first run

An earlier run of this same pilot in the **agent_tool** delivery lane produced the
opposite (wrong) reading — "memory hurt." That was a methodology error I made: in
agent_tool mode the agent must itself issue exactly one `entire brain search` first,
and the claude runner never did, so **all 12 memory arms failed the required
adherence audit** — the agent wasn't actually using the delivered memory. `no_brain`
(free exploration) looked fine and the memory arms looked bad. Switching to
**harness delivery** (`memory_delivery: harness`), where the harness performs the
retrieval and injects the packet, removes that confound. All 12 memory arms are now
adherence-valid, and the true signal (memory helps) appears.

## Validity caveats

- **Valid causal lane**: harness delivery; every memory arm adherence-ok with 6
  results delivered. 0 infrastructure-excluded.
- **Baseline wrinkle**: 2/4 `no_brain` runs were adherence-INVALID
  (`forbidden_memory_artifact_access` — the agent probed `.entire`/checkpoint
  artifacts it was not given). `summarize` does **not** auto-exclude adherence-
  invalid runs (only infrastructure failures), so the all-runs table above includes
  them. The conclusion holds on the adherence-valid subset (no_brain valid n=2: one
  pass, one fail, mean 64 — still far below the memory arms). See follow-up #2.
- **n=4, one task, one runner.** The task is the committed **diagnostic** dev task
  (README: "not paper evidence"). p≈0.04 is marginal at n=4; `facts_only` is n.s.
- Token totals include cache-read tokens; the ~28x is a total-token comparison, but
  the direction and magnitude are consistent across all four memory runs.

## What this pilot also validated (harness/fix correctness)

1. **Distiller de-pinning fix** — the frozen content-verified facts loaded and drove
   16 real runs despite the codex binary having moved
   (`/Applications/Codex.app` → `/Applications/ChatGPT.app`) and changed hash.
2. **F2 infrastructure-exclusion** — 0 spurious exclusions here; correctly excluded a
   pre-agent failure in an earlier single-rep attempt.
3. **The adherence audit works** — it correctly invalidated the agent_tool run (that
   is how the methodology error was caught) and flagged baseline probing here.

## Learnings / follow-ups for future runs

1. **Always run the causal lane in harness delivery mode.** agent_tool makes the
   result hostage to the agent choosing to search first; the claude runner does not.
2. **Consider excluding adherence-invalid runs from the causal comparison**, the way
   `analysis_excluded` (F2) excludes infrastructure failures — right now an
   adherence-invalid `no_brain` (agent probed forbidden artifacts) still counts in
   arm means. This is a real, small harness gap worth closing before a confirmatory
   run.
3. **Content-address facts; never pin an agent/model binary** (fixed here across
   validation, cache key, PATH, and provenance).
4. **`facts_only` vs `raw_history`**: raw history was cleanly reliable; distilled
   facts alone were noisier. Worth understanding whether distillation drops or
   distorts the decisive detail for this task family.
5. **Provenance for any comparison** (all in the results artifact): harness commit,
   source_cache_key, runner, repetitions, per-rep scores/tokens/adherence, record
   hashes. Confirmatory needs a sealed task set, preregistration, and repeats.

## Reproduction

```
# harness-delivery variant: task with memory_delivery: harness + packet.min_results
AGENT_BENCH_REPO_ROOT=/Users/thomi/Projects python3 run.py run \
  --tasks tasks/temporal-memory-default-fact-merge-confidence-harness.json \
  --agents claude:sonnet:high \
  --conditions no_brain,raw_history,facts_only,history_facts \
  --repetitions 4 --suite-name <name>
```

Requires the frozen source-cache entry symlinked into `benchmarks/agent-brain/cache/`.
Aggregate via `run.summarize`.
