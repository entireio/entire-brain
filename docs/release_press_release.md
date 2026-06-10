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

## What Must Be Proven Before Public Claims

- Distill performance on a large session repo, using `distill --dry-run --json`
  and a timed run with cache/timing fields.
- Facts-vs-session retrieval quality, using paired `facts eval --retriever`
  comparisons over shared proof labels; proxy-comparison opt-in remains
  smoke/calibration only and must be labeled as such.
- Semantic usefulness, using semantic-audit output plus benchmark tasks where
  semantic context changes file/test localization or agent efficiency.
- More replay-lab evidence beyond the first retained focused history and
  MCP/Radar proofs, using repeated benchmark scenarios, stable verdicts, and
  committed release/Radar audit reports.

## Blocked Or Access-Dependent

- Backend-specific audits wait for backend access.
- Slack/onboarding-dependent release workflow details wait for workspace access.
- Large-repo distill timing, paired facts evals, and broader multi-task replay
  evidence are still pending. The semantic audit coverage gate has passed after
  a local refresh on this checkout, but semantic usefulness still needs retained
  benchmark proof.
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
- Proven locally: semantic audit coverage after refresh on this checkout,
  distill dry-run sizing, current-repo local command-agent distill scheduler
  speedup evidence, facts-eval baseline plumbing and no-claim evidence guard,
  local QMD-inspired retrieval contract tests, one retained replay-lab history
  proof where `full_brain` improved the task score/pass-rate over `no_brain`,
  and one retained MCP-history proof where `mcp_history` improved pass rate
  1/4 -> 4/4 on a manual-attribution regression without proving efficiency
  gains, plus retained deterministic MCP/Radar tool-contract proof for
  QMD-style MCP retrieval, branch-scoped facts, location-only Radar, deletion
  opt-in, workspace Radar, strict MCP schemas, and safe tool-result logging.
- Pending proof: target large-repo/frontend distill timing, paired facts evals
  with active durable facts and proof labels,
  Radar agent-lift outcome proof, workspace Radar outcome proof,
  answer-assisted/broad Radar claims, semantic usefulness, and broader retained
  replay-lab benchmark evidence.
- Blocked: target large-repo evidence, backend/Slack access, turn signing,
  checkpoint-time fact generation.
- Future: distributed/shared brain, team artifact hydration, multi-agent write
  coordination beyond local safety.
