# Draft Release Press Release

## Headline

Entire prepares a local brain for software teams: durable repo memory, semantic
code understanding, and replayable tests for whether agents get better with
both.

## Customer Promise

Agents should not start every task as if the repository has no past. Entire
Brain turns local sessions, checkpoints, docs, semantic code facts, and curated
durable facts into an inspectable memory layer that stays on the developer's
machine.

`entire-brain` gives agents a qmd-inspired retrieval surface over the repo's
local memory. `entire-sem` gives agents a semantic map of code structure, relations,
boundaries, and likely tests. `entire-replay-lab` measures whether those tools
actually improve task outcomes instead of relying on demos or anecdotes.

## What Is Shipped

- Local brain refresh over retained sessions, seed context, docs, semantic facts,
  history, and durable facts.
- QMD-inspired retrieval verbs: `search`, `vsearch`, `query`, `get`, and
  `multi-get`.
- Durable facts with distill, remember, recall, review, promote, retract, gc,
  verify, and eval surfaces.
- Semantic freshness, blind-spot reporting, and semantic audit output.
- Local benchmark harness for no-brain, semantic-brain, and full-brain agent
  conditions.

## What Must Be Proven Before Public Claims

- Distill performance on a large session repo, using `distill --dry-run --json`
  and a timed run with cache/timing fields.
- Facts-vs-raw recall quality, using paired `facts eval --retriever` comparisons.
- Semantic usefulness, using semantic-audit output plus benchmark tasks where
  semantic context changes file/test localization or agent efficiency.
- Replay-lab evidence, using retained benchmark scenarios with repeated runs,
  stable verdicts, and a committed `audit_codex.py --fail-on-flags` report.

## Blocked Or Access-Dependent

- Backend-specific audits wait for backend access.
- Slack/onboarding-dependent release workflow details wait for workspace access.
- Full turn-level cryptographic fact verification still depends on CLI-side turn
  signing.
- Generated fact-at-checkpoint creation remains a future CLI/checkpoint pipeline
  change; current distill still supports backfill.

## Future Claims We Should Not Make Yet

- Do not claim facts are always better than raw sessions.
- Do not claim tree-sitter understands every file.
- Do not claim multi-agent collaboration is complete.
- Do not claim replay-lab proves universal agent improvement across all tasks.

## Release Checklist

- Shipped: local retrieval, semantic index consumption, durable facts, fact
  verification, eval harness, benchmark harness.
- Proven: fill from audit evidence after green distill/eval/benchmark runs.
- Blocked: backend/Slack access, turn signing, checkpoint-time fact generation.
- Future: distributed/shared brain, team artifact hydration, multi-agent write
  coordination beyond local safety.
