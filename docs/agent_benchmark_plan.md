# Agent Brain Benchmark Plan

## Summary

This benchmark measures whether Codex and Claude Code perform better on real
repository tasks when they can use Entire Brain.

The first version should optimize for a repeatable harness, then use the same
run artifacts to produce demo-quality before/after evidence. The benchmark
starts with three local repositories:

- `entire-brain` at `/Users/thomi/Projects/entire-brain`
- `entire-cli` at `/Users/thomi/Projects/cli`
- GitHub CLI at `/Users/thomi/Projects/github-cli`

The benchmark should compare agent performance across controlled context
conditions, not just tool latency. Useful outcomes include faster localization,
better test selection, fewer irrelevant file reads, fewer wrong edits, and
better recovery of implementation rationale.

## Context Conditions

Run each task under these conditions when the repo supports them:

1. **No brain**
   - The agent receives the task prompt and normal repository access.
   - The agent must not run `entire brain ...`.
   - No brain docs or MCP tools are preloaded.

2. **Semantic brain only**
   - The agent receives the task prompt plus Entire Brain semantic intake
     instructions.
   - The harness prepares seed and semantic artifacts with local-only commands.
   - The agent may use `stale`, `query`, `context`, `impact`, `changes`,
     `routes`, `tools`, `workflows`, `tests`, and workspace commands.
   - Checkpoint transcript/session history is withheld.

3. **Full brain**
   - Includes semantic brain plus exported Entire checkpoint history.
   - The agent may use seed context, semantic facts, history gaps, checkpoint
     metadata, prompts, and transcripts.
   - This condition is available for `entire-brain` and `entire-cli`.

Keep the base task prompt identical across conditions. Only the allowed context
policy changes.

## Checkpoint Availability

Checkpoint history is materially different across the starting repos.

| Repo | Checkpoint source | Checkpoints | Session metadata | Prompts | Transcripts |
|---|---|---:|---:|---:|---:|
| `entire-brain` | local `refs/heads/entire/checkpoints/v1` | 30 | 142 | 142 | 142 |
| `entire-cli` | `https://github.com/entireio/cli-checkpoints.git`, `refs/heads/entire/checkpoints/v1` | 3,945 | 5,426 | 4,656 | 5,088 |
| GitHub CLI | none found locally/configured | 0 | 0 | 0 | 0 |

The `entire-cli` checkout stores checkpoint discovery in
`.entire/settings.json`:

```json
{
  "strategy_options": {
    "checkpoint_remote": {
      "provider": "github",
      "repo": "entireio/cli-checkpoints"
    }
  }
}
```

Entire Brain export discovers checkpoints by trying local refs first, then
falling back to the configured checkpoint remote and fetching only the v1
checkpoint ref into a temporary repository. Ignore v2 checkpoints for this
benchmark.

## Harness

Add a benchmark harness under `benchmarks/agent-brain/`:

- `tasks/*.yaml` defines repo, base commit, prompt, context condition, timeout,
  expected files, validation commands, and scoring checks.
- `adapters/codex.*` and `adapters/claude.*` run the agents in disposable
  worktrees.
- `brain/prepare.*` builds isolated brain state in temporary plugin dirs.
- `runs/<timestamp>/...` stores prompts, transcripts, commands, diffs, test
  output, timing, and scoring data.
- `score.*` calculates deterministic scores from tests, patches, touched files,
  and rubric checks.

Use fixed base commits and clean disposable worktrees. Avoid mutating the source
checkouts during benchmark runs.

## Initial Scenarios

Start with nine tasks: three per repository.

### `entire-brain`

1. **Semantic command bugfix**
   - Fix a narrow issue in `stale`, `query`, or `context`.
   - Brain benefit: semantic commands should identify the relevant functions and
     tests quickly.
   - Validate with focused semantic tests and `go test ./...`.

2. **MCP and CLI contract change**
   - Add or adjust one semantic JSON field while keeping MCP wrappers compatible.
   - Brain benefit: `impact` should reveal the CLI command, JSON output path,
     MCP adapter, and contract tests.
   - Validate with semantic and MCP tests.

3. **Workspace behavior task**
   - Change workspace query or impact behavior in a bounded way.
   - Brain benefit: workspace context should locate freshness handling and
     workspace tests.
   - Validate with workspace tests.

### `entire-cli`

1. **Agent integration fix**
   - Fix a Codex or Claude hook/session edge case.
   - Brain benefit: full brain should recover prior rationale and point to
     agent integration surfaces.
   - Validate with focused unit/integration tests.

2. **External command/plugin behavior**
   - Adjust plugin dispatch, external command behavior, or env filtering.
   - Brain benefit: semantic context should connect dispatch code, plugin code,
     and integration tests.
   - Validate with external command/plugin tests.

3. **Review/resume context task**
   - Modify review or resume context assembly.
   - Brain benefit: checkpoint history should expose previous design choices and
     known edge cases.
   - Validate with review/resume smoke tests.

### GitHub CLI

1. **Command JSON/export behavior**
   - Change JSON or format flag behavior for one command family.
   - Brain benefit: semantic context should locate `cmdutil` helpers and command
     tests.
   - Validate with JSON flag and affected command tests.

2. **HTTP/API error handling**
   - Fix a mocked API error path.
   - Brain benefit: semantic query should identify API query/client code and
     `httpmock` tests.
   - Validate with package tests only; no live network.

3. **Factory/auth/repo resolution task**
   - Adjust factory, auth check, or repo override behavior.
   - Brain benefit: `impact` should identify factory setup, repo override hooks,
     auth checks, and tests.
   - Validate with focused package tests.

## Metrics

Collect hard metrics for every run:

- pass/fail and final validation output
- wall time
- number of shell/search commands
- number of files read before first relevant edit
- number of failed test iterations
- final diff size
- expected files touched and forbidden files touched
- token/cost data where available

Collect brain-specific metrics for brain-enabled runs:

- whether the agent checked `stale`
- which brain commands were used
- whether semantic results led to relevant files/tests
- whether full brain runs cited useful checkpoint history
- whether the agent avoided stale or unsafe semantic data

For demo reporting, emphasize:

- time to first relevant file
- files inspected before first correct edit
- selected validation quality
- wrong-area edits avoided
- concise side-by-side transcript excerpts.

## Scoring

Score each run out of 100:

- 50 correctness: tests pass and behavior matches task requirements
- 20 locality: expected areas touched, no unrelated rewrites
- 15 validation quality: correct focused tests and no obvious skipped checks
- 10 efficiency: fewer irrelevant inspections, retries, and failed test loops
- 5 brain hygiene: freshness checked and brain output used appropriately

Compare:

- Codex no brain vs semantic brain vs full brain
- Claude Code no brain vs semantic brain vs full brain
- semantic benefit separately from checkpoint-history benefit
- per-repo and per-scenario aggregates.

## Phases

### Phase 1: Repeatable Harness

- Implement disposable worktree runner.
- Implement brain preparation in isolated plugin dirs.
- Add Codex and Claude Code adapters.
- Add one smoke task per repo.
- Store transcripts, diffs, test outputs, timing, and command logs.

### Phase 2: Full Scenario Suite

- Expand to nine tasks.
- Run each agent and condition at least three times per task.
- Add scoring and aggregate summary generation.
- Include both semantic-only and full-brain conditions where checkpoint history
  exists.

### Phase 3: Demo Evidence

- Select three or four strongest scenarios.
- Produce a concise report from the same run artifacts.
- Show side-by-side outcomes for no brain, semantic brain, and full brain.
- Highlight concrete examples where brain context changed the agent's path to a
  correct solution.

## Assumptions

- Generated brain artifacts remain local and uncommitted.
- The benchmark may fetch configured checkpoint remotes for repositories that
  explicitly define them.
- GitHub CLI starts with semantic-only comparison because no Entire checkpoint
  source is available locally.
- The first public/demo comparison should use results from the repeatable
  harness, not a separate hand-curated run.
