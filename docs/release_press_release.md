# Draft Release Press Release

## Headline

Entire prepares a local brain for software teams: durable repo memory,
audit-reported semantic code context, and replayable tests for measuring agent
outcomes.

## Customer Promise

Agents should not start every task as if the repository has no past. Entire
Brain turns local sessions, checkpoints, docs, semantic provider records, and
curated durable facts into an inspectable memory layer that stays on the developer's
machine.

`entire-brain` gives agents a qmd-inspired retrieval surface over the repo's
local memory. `entire-sem` gives agents an audit-reported map of code structure,
relations, boundaries, and likely tests when provider coverage is fresh enough
for the target repo. `entire-replay-lab` measures whether those tools improve
task outcomes instead of relying on demos or anecdotes.

## What Is Shipped

- Local brain refresh over retained sessions, seed context, copied docs, history,
  and semantic records.
- Durable facts through separate distill, remember, recall, admin, verify, and
  eval surfaces.
- QMD-inspired retrieval verbs: `search`, `vsearch`, `query`, `get`, and
  `multi-get`.
- Semantic freshness, blind-spot reporting, and semantic audit output.
- Local benchmark harness for no-brain, semantic-brain, and full-brain agent
  conditions.

## Known Replay-Lab Outcome

The retained replay-lab history, MCP-history, and location-only Radar
agent-lift candidate suites were rerun after the B1 query-hint confound was
removed. The retained release lane is now audit-clean with zero hard integrity
flags, but all three clean comparisons remain `proof_ready=false`: one was
noisy/brain-negative, and two were saturated/brain-negative. That codex
release evidence lane therefore stays in `no_release_claim` mode. See
`docs/release-blockers.md` (B1) for the clean suite names and outcomes.

A separate, narrower lane has since cleared the proof-ready gate. A harder,
clean correctness-axis task (`entire-brain-clean-default-fact-merge-confidence`:
a deliberately decided default whose value lives only in retained session
history) produced a **stable `proof_ready` comparison on the history channel**:
no_brain `claude:sonnet:high` scores 0/4, full_brain scores 4/4
(`brain_positive_stable`), audited by `audit_codex.py` with zero hard integrity
flags. This is retained under `benchmarks/agent-brain/evidence/replay-lab-clean/`
and gated by `mise run clean-proof:evidence`. Its claim is deliberately scoped:
**history-channel correctness brain-lift with `claude:sonnet:high` only.** It
does not establish MCP, Radar, semantic, codex-runner, or efficiency agent lift,
all of which remain `no_release_claim`. (The codex `gpt-5.5` workspace ran out
of credits during the proof attempt, so the n=4 confirmation used the Claude
runner after the limit reset.)

## What Must Be Proven Before Public Claims

- Frontend/hosted-model distill latency and fact quality on a large session
  repo, using a real hosted agent (not the deterministic command-agent) for the
  paired timed `--jobs 1`/`--jobs N` runs. The large-repo extraction-scheduler
  speedup itself is now retained: `distill --dry-run --json` plus paired timed
  command-agent runs on entireio/cli (822 sessions, 838 chunks) are committed
  under `benchmarks/agent-brain/evidence/distill-perf-large/` with matching
  cache/timing fields and a ~2.2x `--jobs 4` speedup.
- Facts-vs-session retrieval quality, using paired `facts eval --retriever`
  comparisons over shared proof labels; proxy-comparison opt-in remains
  smoke/calibration only and must be labeled as such.
- Semantic usefulness, using the `status` semantic-audit output plus benchmark tasks where
  semantic context changes file/test localization or agent efficiency.
- Replay-lab agent-lift beyond the one proven scope. A history-channel
  correctness brain-lift now survives the proof-ready gate (see "Known
  Replay-Lab Outcome" and `evidence/replay-lab-clean/`), but it is scoped to
  `claude:sonnet:high` on the history channel. MCP, focused location-only Radar,
  broader Radar, workspace Radar, codex-runner, and efficiency agent-lift claims
  all still need their own harder clean tasks, repeated benchmark scenarios,
  stable verdicts, and committed release/Radar audit reports. The deterministic
  Radar/MCP tool-contract evidence remains separate.

## Blocked Or Access-Dependent

- Backend-specific audits wait for backend access.
- Slack/onboarding-dependent release workflow details wait for workspace access.
- Frontend/hosted-model distill latency, paired facts evals, semantic usefulness, and broader
  multi-task replay evidence are still pending. The large-repo command-agent
  extraction-scheduler speedup is retained (`distill-perf-large`); the
  hosted-model latency and fact-quality claim is not. Semantic audit coverage has
  historical local clean evidence after refresh, and the final release checkout
  must pass `mise run semantic:evidence` before citing current semantic health.
- Full turn-level cryptographic fact verification still depends on CLI-side turn
  signing.
- Generated fact-at-checkpoint creation remains a future CLI/checkpoint pipeline
  change; current distill still supports backfill.

## Future Claims We Should Not Make Yet

- Do not claim facts beat raw/preprocessed sessions except for the specific
  paired eval metric and relevance source being cited.
- Do not claim tree-sitter or semantic indexing covers every file or language.
- Do not claim multi-agent collaboration is complete.
- Do not claim replay-lab proves universal agent improvement across all tasks.

## Release Checklist

- Shipped: local retrieval, semantic index consumption, durable facts, anchor
  verification, MCP/Regression Radar contracts, workspace-radar MCP harnessing,
  eval harness, benchmark harness.
- Proven locally: historical semantic audit coverage after refresh on clean
  checkpoints, pending a fresh `mise run semantic:evidence` pass on the final
  clean release checkout,
  distill dry-run sizing, current-repo local command-agent distill scheduler
  speedup evidence, large-repo (entireio/cli) command-agent distill
  extraction-scheduler speedup evidence (822 sessions, 838 chunks, ~2.2x at
  `--jobs 4`), facts-eval baseline plumbing and no-claim evidence guard,
  local QMD-inspired retrieval contract tests, the B1 clean replay-lab no-claim
  evidence guard that prevents non-proof-ready retained suites from becoming
  release claims,
  a clean history-channel correctness-axis replay-lab agent-lift proof
  (`claude:sonnet:high`, no_brain 0/4 vs full_brain 4/4, `brain_positive_stable`,
  audit-clean — `evidence/replay-lab-clean/`),
  plus retained deterministic MCP/Radar tool-contract proof for QMD-inspired MCP
  retrieval, branch-scoped facts, location-only Radar, deletion opt-in,
  workspace Radar, strict MCP schemas, and safe tool-result logging.
- Pending proof: frontend/hosted-model distill latency and fact quality, paired facts evals
  with active durable facts and proof labels,
  proof-ready replay-lab agent-lift on the MCP, focused/broad Radar, workspace
  Radar, codex-runner, and efficiency scopes (the history-channel correctness
  scope is now proven), workspace Radar outcome proof,
  answer-assisted/broad Radar claims, semantic usefulness, and broader retained
  replay-lab benchmark evidence.
- Blocked: target large-repo evidence, backend/Slack access, turn signing,
  checkpoint-time fact generation.
- Future: distributed/shared brain, team artifact hydration, multi-agent write
  coordination beyond local safety.
