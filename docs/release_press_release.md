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
- Facts-vs-session retrieval quality, using paired `facts eval --retriever`
  comparisons with shared labels or explicit proxy-comparison opt-in.
- Semantic usefulness, using semantic-audit output plus benchmark tasks where
  semantic context changes file/test localization or agent efficiency.
- Replay-lab evidence, using retained benchmark scenarios with repeated runs,
  stable verdicts, and a committed `audit_codex.py --fail-on-flags` report.

## Blocked Or Access-Dependent

- Backend-specific audits wait for backend access.
- Slack/onboarding-dependent release workflow details wait for workspace access.
- Large-repo distill timing, paired facts evals, release-candidate semantic
  audit output, and retained benchmark audit artifacts are still pending.
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
  verification, eval harness, benchmark harness.
- Proven: pending; fill only from retained distill/eval/semantic-audit/benchmark
  evidence.
- Blocked: target large-repo evidence, backend/Slack access, turn signing,
  checkpoint-time fact generation.
- Future: distributed/shared brain, team artifact hydration, multi-agent write
  coordination beyond local safety.
