# Phase 0B Powered Dev Pilot — Memo

Date: 2026-07-13
Status: **diagnostic development pilot — NOT confirmatory evidence, NOT paper evidence**
Harness: `codex/brain-phase0b-fable-hardening` (post causal-validity, distiller-de-pinning,
and adherence fixes)
Data: `phase0b-powered-pilot-results-v2-harness.json` (VALID, harness-delivery lane).
Supersedes `-v1.json` (agent_tool lane — all memory arms were adherence-invalid).

## Headline finding (valid causal lane, adherence-invalid runs excluded)

**Project memory clearly HELPS on this hidden-decision recovery task in direction
— correctness and token efficiency — but n is too small for significance once
invalid runs are excluded.** With the harness delivering the frozen memory packet
(6 results per memory arm, all adherence-clean), and `summarize` now excluding
adherence-invalid runs from the comparison:

| Condition | Pass (all / adh-valid) | Mean score | Mean tokens | Δ vs valid no_brain (p) |
| --- | --- | --- | --- | --- |
| no_brain | 1/4 / **1/2 valid** | 50 (valid 64) | 6.69M | baseline (n=2 valid) |
| raw_history | 4/4 / 4/4 | 96.5 | 0.24M | +32.5 (p=0.43, **n.s.**), ~28x fewer tokens |
| history_facts | 4/4 / 4/4 | 96.75 | 0.22M | +32.75 (p=0.43, n.s.), ~30x fewer tokens |
| facts_only | 3/4 / 3/4 | 75.75 | 2.84M | +11.75 (p=0.74, n.s.) |

Without memory the agent fails most of the time (recovering a deliberately hidden
project decision is hard from current code alone); with raw history (or history+
facts) it passes every time, scores ~32 points higher, and uses ~1/28th the tokens.
**But the p-values are NOT significant**: excluding the two adherence-invalid
no_brain runs leaves a valid baseline of only n=2 with high variance ([90, 38]).
An earlier read of this same run reported p≈0.04 — that significance was partly
spurious, driven by counting the invalid no_brain probes; the adherence-exclusion
fix (below) removes it. The DIRECTION (pass-rate + large token reduction) is
consistent across all four memory runs; the magnitude needs more reps and a
cleaner baseline to claim significance.

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
2. **[DONE] Exclude adherence-invalid runs from the causal comparison.** `summarize`
   now drops runs whose required `temporal_memory_condition_audit` failed (like it
   drops infrastructure failures) and surfaces `n_adherence_excluded_{condition,
   baseline}`. This is what removed the spurious p≈0.04 above.
2b. **Get enough VALID no_brain reps.** Here 2/4 baseline runs were adherence-invalid
   (claude probed forbidden artifacts), leaving n=2. A confirmatory run needs more
   no_brain reps — and ideally an understanding of why the baseline agent probes.
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
AGENT_BENCH_REPO_ROOT=/path/to/repos python3 run.py run \
  --tasks tasks/temporal-memory-default-fact-merge-confidence-harness.json \
  --agents claude:sonnet:high \
  --conditions no_brain,raw_history,facts_only,history_facts \
  --repetitions 4 --suite-name <name>
```

Requires the frozen source-cache entry symlinked into `benchmarks/agent-brain/cache/`.
Aggregate via `run.summarize`.
