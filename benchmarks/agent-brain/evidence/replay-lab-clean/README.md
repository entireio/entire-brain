# Clean replay-lab agent-lift proof (history channel)

The retained replay-lab comparison that survives the proof-ready gate under
full physical isolation.

`mise run clean-proof:evidence` checks it; `mise run clean-proof:evidence:update`
regenerates the committed audit report.

## What this proves

A clean **correctness-axis** brain-lift on the **history channel**, audited by
`audit_codex.py` (0 hard integrity flags, 20/20 provenance-backed records):

| task | no_brain | semantic_history_brain | verdict | proof_ready |
|---|---|---|---|---|
| `entire-brain-clean-default-fact-merge-confidence` | **0/5** | **5/5** | brain_positive (stable) | **true** |
| `entire-brain-clean-contract-checkpoint-branch-reachability` | 5/5 | 5/5 | saturated/no_signal | false (retained datapoint) |

Runner: `claude:sonnet:high`, five repetitions per side (one above the proof
minimum of four valid rows, because baseline agents on the memory-only task
sometimes escalate to hard adherence violations and forfeit their row).

The fact-merge task regresses a deliberately decided default
(`defaultFactConfidenceThreshold`, the confidence gate for auto-applying a
durable-fact merge/supersede) to a plausible alternative, scrubs the value and
its rationale from everything the agent can read, and only asks it to "restore
the intended default from project history". The decided value lives **only** in
retained session history, so:

- **no_brain (claude:sonnet:high) cannot recover it — 0/5.** It cannot guess the
  exact arbitrary value; the focused go test passes at several plausible values,
  so the hidden exact-value check is the sole discriminator.
- **semantic_history_brain retrieves it with the product brain CLI and passes —
  5/5**, with >=20% improvements in both time-or-tokens and search-call volume.

This is a stable `brain_positive` comparison (pass-rate lift surviving the
drop-one robustness check), so `proof_ready=true`.

## Isolation — why a brain win can only come from retrieval

Every cell runs under the same physical isolation:

- a deny-first `sandbox-exec` profile: harness and source trees unreadable,
  only the cell's disposable worktree, frozen tools, and env-designated Go
  caches re-allowed;
- origin remotes removed from the worktree; remote git/VCS-host network
  operations are hard adherence violations in every arm;
- the answer and its rationale scrubbed from worktree history (blobs and
  commit messages), checkpoint objects repacked away and verified gone by
  `git fsck`;
- an answer-free agent binary (HEAD code with the task's
  `binary_replacements` applied) on PATH, identical in every arm, so no
  `--help` default can leak the value;
- the brain store republished through the product after sanitization, so the
  treatment loads a coherent index built over exactly the scrubbed bytes.

Adherence findings are judged by information flow: name-only existence
probes, exclusion patterns, parent-hash discovery, and diffs wholly inside
ordinary source history are not violations; reading private content, spanning
the synthetic-merge boundary, or fetching sources over the network is, in any
arm, and any such row forfeits validity.

## Scope — what this does NOT claim

This lane claims agent lift **only for the history channel, correctness axis,
with `claude:sonnet:high`.** It does **not** claim:

- MCP, Radar, semantic, or workspace agent lift, or
- codex-runner agent lift, or
- that the saturated contract-checkpoint task shows lift (Sonnet solves it
  without the brain; it is retained as an honest non-proof-ready datapoint).

Those remain `no_release_claim` in `../release`.

## Provenance / why this is a separate lane

The codex `gpt-5.5` workspace ran out of credits during the original proof
attempt, so the confirmation runs use the Claude runner. An earlier retained
bundle (June 2026) was withdrawn: its treatment arm read a harness-prepared
history excerpt rather than invoking the brain, and its baseline could reach
the unscrubbed answer through committed history. This bundle replaces it after
those defects and every subsequently observed leak were closed at root cause.
Records are redacted of host paths and contributor names; the suite directory
name matches each record's `provenance.run_config.suite` so the
panel-provenance integrity check passes.
