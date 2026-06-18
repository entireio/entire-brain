# Clean replay-lab agent-lift proof (history channel)

The first retained replay-lab comparison that survives the proof-ready gate.

`mise run clean-proof:evidence` checks it; `mise run clean-proof:evidence:update`
regenerates the committed audit report.

## What this proves

A clean **correctness-axis** brain-lift on the **history channel**, audited by
`audit_codex.py` (0 hard integrity flags, 16/16 provenance-backed records):

| task | no_brain | full_brain | verdict | proof_ready |
|---|---|---|---|---|
| `entire-brain-clean-default-fact-merge-confidence` | **0/4** | **4/4** | brain_positive (stable) | **true** |
| `entire-brain-clean-contract-checkpoint-branch-reachability` | 4/4 | 4/4 | brain_negative (saturated) | false (retained datapoint) |

Runner: `claude:sonnet:high`, four repetitions per side.

The fact-merge task regresses a deliberately decided default
(`defaultFactConfidenceThreshold`, the confidence gate for auto-applying a
durable-fact merge/supersede) to a plausible alternative, scrubs the value and
its rationale from everything the agent can read, and only asks it to "restore
the intended default from project history". The decided value lives **only** in
retained session history, so:

- **no_brain (claude:sonnet:high) cannot recover it — 0/4.** It cannot guess the
  exact arbitrary value; the focused go test passes at several plausible values,
  so the hidden exact-value check is the sole discriminator.
- **full_brain recovers it from history and passes — 4/4.** The history channel
  surfaces the decided value with its rationale; the agent restores it exactly.

This is a stable `brain_positive` comparison (pass-rate lift surviving the
drop-one robustness check at n=4), so `proof_ready=true`.

## Scope — what this does NOT claim

This lane claims agent lift **only for the history channel, correctness axis,
with `claude:sonnet:high`.** It does **not** claim:

- MCP, Radar, semantic, or workspace agent lift, or
- codex-runner agent lift, or
- a token/efficiency win.

Those remain `no_release_claim` in `../release` (the B1 clean reruns are
audit-clean but not proof-ready). The co-located contract-checkpoint comparison
**saturated** (Sonnet solves it without the brain) and is retained as an honest
non-proof-ready datapoint.

## Provenance / why this is a separate lane

The codex `gpt-5.5` workspace ran out of credits during the proof attempt, so
the n=4 confirmation was run with the Claude runner (post-limit-reset). The
release lane (`../release`) is codex-based and stays `no_release_claim` for the
mcp/radar scopes it still cannot prove; this lane is the honest home for the one
scope that is now proven. Records are redacted of host paths and contributor
names; the suite directory name matches each record's
`provenance.run_config.suite` so the panel-provenance integrity check passes.
