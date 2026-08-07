# Draft Release Press Release

> Internal draft for the P0 local-brain release (`entire-graph`, `entire-brain`,
> and the GraphMark evaluation harness, all on the aligned `v0.1.0` line). Not for
> publication. Claims are scoped to what the retained evidence supports; anything
> unverified is flagged rather than asserted.

## Headline

Entire ships a local brain for software teams: durable repo memory, a
benchmark-backed semantic code graph, and replayable tests for measuring agent
outcomes, all on the developer's machine.

## Customer Promise

Agents should not start every task as if the repository has no past. Entire Brain
turns local sessions, checkpoints, docs, semantic-provider records, and curated
durable facts into an inspectable memory layer that stays on the developer's
machine.

`entire-brain` gives agents a qmd-inspired retrieval surface over the repo's
local memory. `entire-graph` is the semantic provider: it parses source locally and
gives the brain a code-structure graph (symbols, relations, boundaries, and
likely tests), with freshness and blind-spot reporting so an agent knows how far
to trust it. The local replay-lab agent-benchmark harness measures whether those
tools improve task outcomes, instead of relying on demos or anecdotes.

## What Is Shipped

- Local brain refresh over retained sessions, seed context, copied docs, history,
  and semantic records.
- Durable facts through separate distill, remember, recall, review, admin,
  verify, and eval surfaces.
- QMD-inspired retrieval verbs: `search`, `vsearch`, `query`, `get`, and
  `multi-get`.
- Semantic freshness, blind-spot reporting, and a semantic audit surface
  (`status --json`, gated in CI by `status --fail-on release`).
- Local benchmark harnesses: GraphMark for semantic-graph accuracy, and the
  replay-lab agent harness for no-brain, semantic-brain, and full-brain agent
  conditions.
- Local-only by default: stdio MCP with no network listener, deterministic
  refresh and retrieval with no hosted-model calls, and `ENTIRE_BRAIN_NO_EGRESS`
  / `ENTIRE_BRAIN_LOCAL_ONLY` enforcement on the no-agent, dry-run, and
  loopback paths.
- `entire-graph` and `entire-brain` both tagged `v0.1.0` on the aligned GA commit.

## Proven With Evidence

**Semantic usefulness is claimable, backed by GraphMark.** On the frozen
benchmark board, Entire beats the comparison baseline on 28 of 30 top languages
at exact-McNemar p<0.05 (C# at p=0.07 and PHP at p=0.18 lead on rate but sit just
under significance, reflecting fewer scored repos rather than a loss), for an
overall 1,148 vs 802 correct (91% vs 64%, p near 0), with Benjamini-Hochberg
multiple-comparisons control. This is the first broad, reproducible, in-repo-gated
proof, and it supersedes the earlier single narrow condition-level result.
GraphMark is wired as a CI regression gate: the committed significance report
must regenerate byte-identical, so a corpus or harness change has to re-commit it.

## Known Replay-Lab Outcome

The retained replay-lab history, MCP-history, and location-only Radar agent-lift
candidate suites were rerun after the B1 query-hint confound was removed. The
retained release lane is audit-clean with zero hard integrity flags, but all
three clean comparisons remain `proof_ready=false`: one was noisy and
brain-negative, and two were saturated and brain-negative. That codex release
evidence lane therefore stays in `no_release_claim` mode. See
`docs/release-blockers.md` (B1) for the clean suite names and outcomes.

A separate, narrower lane has cleared the proof-ready gate. A harder, clean
correctness-axis task (`entire-brain-clean-default-fact-merge-confidence`, a
deliberately decided default whose value lives only in retained session history)
produced a stable `proof_ready` comparison on the history channel: no_brain
`claude:sonnet:high` scores 0/4, full_brain scores 4/4 (`brain_positive_stable`),
audited by `audit_codex.py` with zero hard integrity flags. It is retained under
`benchmarks/agent-brain/evidence/replay-lab-clean/` and gated by
`mise run clean-proof:evidence`. Its claim is deliberately scoped:
history-channel correctness brain-lift with `claude:sonnet:high` only. It does
not establish MCP, Radar, semantic-localization, codex-runner, or efficiency
agent-lift, all of which remain `no_release_claim`. (The codex `gpt-5.5`
workspace ran out of credits during the proof attempt, so the n=4 confirmation
used the Claude runner after the limit reset.)

## What Must Be Proven Before Public Claims

- Frontend and hosted-model distill latency and fact quality on a large session
  repo, using a real hosted agent (not the deterministic command-agent) for the
  paired timed `--jobs 1` and `--jobs N` runs. The large-repo extraction-scheduler
  speedup itself is already retained: `distill --dry-run --json` plus paired timed
  command-agent runs on entireio/cli (822 sessions, 838 chunks) are committed
  under `benchmarks/agent-brain/evidence/distill-perf-large/` with matching
  cache and timing fields and a roughly 2.2x `--jobs 4` speedup. The hosted-model
  end-to-end latency and fact-quality claim is the part that is not yet made.
- Facts-vs-session retrieval quality, using paired `facts eval --retriever`
  comparisons over shared proof labels. Proxy-comparison opt-in remains
  smoke and calibration only and must be labeled as such. Facts ship as a
  capability with no comparative-quality claim, because there are currently zero
  active durable facts and therefore no paired proof.
- Agent-lift beyond the one proven scope. A history-channel correctness
  brain-lift now survives the proof-ready gate (see "Known Replay-Lab Outcome"),
  but it is scoped to `claude:sonnet:high` on the history channel. MCP,
  focused location-only Radar, broader Radar, workspace Radar, codex-runner, and
  efficiency agent-lift claims still need their own harder clean tasks, repeated
  benchmark scenarios, stable verdicts, and committed release and Radar audit
  reports. The deterministic Radar and MCP tool-contract evidence remains
  separate from the agent-lift question.

Note: this is distinct from the GraphMark semantic-accuracy proof above.
GraphMark measures whether the semantic graph is correct; the replay-lab
items here measure whether the tools change agent outcomes on a task.

## Blocked Or Access-Dependent

- Backend-specific audits wait for backend access. [Unverified from this repo.]
- Slack and onboarding-dependent release workflow details wait for workspace
  access. [Unverified from this repo.]
- Frontend and hosted-model distill latency, paired facts evals, and broader
  multi-task replay evidence are still pending. The large-repo command-agent
  extraction-scheduler speedup is retained (`distill-perf-large`); the
  hosted-model latency and fact-quality claim is not.
- The semantic audit surface has historical local clean evidence after refresh.
  The final release checkout must pass `mise run semantic:evidence` before citing
  current semantic health from `status`. [The retained evidence exists in-repo;
  a fresh pass on the final checkout was not run as part of this draft.]
- Full turn-level cryptographic fact verification still depends on CLI-side turn
  signing.
- Generated fact-at-checkpoint creation remains a future CLI and checkpoint
  pipeline change. Current distill still supports backfill.

## Future Claims We Should Not Make Yet

- Do not claim facts beat raw or preprocessed sessions except for the specific
  paired eval metric and relevance source being cited.
- Do not claim tree-sitter or semantic indexing covers every file or language.
  The GraphMark result names the covered languages; inventory-only filetypes
  are not semantic coverage.
- Do not claim multi-agent collaboration is complete.
- Do not claim the replay-lab proves universal agent improvement across all
  tasks. One history-channel correctness scope is proven; the rest is not.

## Release Checklist

- Shipped: local retrieval, semantic index consumption, durable facts, anchor
  verification, MCP and Regression Radar contracts, workspace-radar MCP
  harnessing, eval harness, benchmark harness. Both repos tagged `v0.1.0`,
  aligned.
- Proven with committed evidence:
  - Semantic usefulness via GraphMark (28 of 30 languages, 1,148 vs 802,
    p<0.05, Benjamini-Hochberg controlled), wired as a CI regression gate.
  - A clean history-channel correctness-axis replay-lab agent-lift proof
    (`claude:sonnet:high`, no_brain 0/4 vs full_brain 4/4,
    `brain_positive_stable`, audit-clean, `evidence/replay-lab-clean/`).
  - Distill dry-run sizing, current-repo command-agent distill scheduler speedup,
    and the large-repo entireio/cli command-agent extraction-scheduler speedup
    (822 sessions, 838 chunks, roughly 2.2x at `--jobs 4`).
  - Facts-eval baseline plumbing plus the no-claim evidence guard, the B1 clean
    replay-lab no-claim guard that stops non-proof-ready suites from becoming
    release claims, local QMD-inspired retrieval contract tests, and retained
    deterministic MCP and Radar tool-contract proofs (QMD-inspired MCP retrieval,
    branch-scoped facts, location-only Radar, deletion opt-in, workspace Radar,
    strict MCP schemas, safe tool-result logging).
- Pending proof: frontend and hosted-model distill latency and fact quality,
  paired facts evals with active durable facts and proof labels, and
  proof-ready replay-lab agent-lift on the MCP, focused and broad Radar,
  workspace Radar, codex-runner, and efficiency scopes (the history-channel
  correctness scope is proven).
- Blocked: backend and Slack access, turn signing, checkpoint-time fact
  generation.
- Future: distributed and shared brain, team artifact hydration, and multi-agent
  write coordination beyond local safety.
