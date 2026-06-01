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

The expanded benchmark should optimize across three layers:

- Layer A: project-native hard tasks in `entire-brain`, `entire-cli`, and
  GitHub CLI.
- Layer B: SWE-bench-style tasks with issue prompts, hidden tests, fixed base
  commits, and no repo-specific hints beyond the condition policy.
- Layer C: model, reasoning-effort, turn, token, duration, and cost matrixes
  for Codex and Claude Code.

## Context Conditions

Run each task under these conditions when the repo supports them:

1. **No brain**
   - The agent receives the task prompt and normal repository access.
   - The agent must not run `entire brain ...`.
   - No brain docs or MCP tools are preloaded.
   - Codex and Claude still use their host auth state, but the harness disables
     persistent sessions, user config/rules, slash commands, and MCP config.

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

For stale-context tasks, prepare the brain first, then apply and commit the
regression. These tasks measure whether the agent treats prepared context as a
cache with a freshness contract rather than as ground truth.

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

### Phase 2: Value-Prop Discovery Suite

The initial harness runs showed that simple bugfix tasks can saturate: agents
may pass every condition, leaving no correctness delta. Phase 2 should add
scenarios that test where the brain has unique value, then iterate until the
data supports a specific claim.

Add metrics beyond final score:

- `time_to_first_relevant_file`
- `files_read_before_first_relevant_file`
- `search_commands_count`
- `brain_commands_count`
- `validation_command_quality`
- `agent_turns`
- `total_tokens`
- `agent_seconds`
- `brain_prep_seconds`
- `cost_usd`
- `cost_source` (`reported` from agent JSON or `estimated` from a supplied
  pricing table)
- `isolation` metadata describing disabled persistent state/config surfaces

Parse Codex JSON logs and Claude JSON output to extract shell commands, file
reads, first relevant-file access, first edit time, validation commands, turns,
tokens, and duration. Keep correctness and operational metrics separate so the
report can honestly say whether the brain improves correctness, efficiency,
validation quality, or cost-normalized outcomes.

### Phase 2A: Isolation Hardening

Agents should not inherit useful knowledge from previous benchmark runs.

- Codex runs with `--ephemeral`, `--ignore-user-config`, and `--ignore-rules`.
- Claude runs with `--no-session-persistence`, `--strict-mcp-config`, an empty
  MCP config, and `--disable-slash-commands`.
- The remaining intentional shared state is auth only: Codex may use host
  `CODEX_HOME` credentials and Claude may use the authorized Max/OAuth account.
- Every result record includes an `isolation` object so old and new runs are not
  mixed accidentally.

### Phase 2B: Complexity Expansion

Add tasks that are hard because of missing rationale, stale context, or hidden
cross-file contracts:

- `entire-brain-stale-query-default-limit`: semantic/full-brain runs receive a
  prepared semantic index, then the regression is committed afterward. The
  expected behavior is that brain-enabled agents check freshness before using
  semantic results.
- `entire-cli-review-provenance-strip`: a large-repo checkpoint-history task
  where the fix depends on the review/investigate provenance design contract,
  not only on a local unit test.
- `swe-style-*`: local SWE-bench-style tasks using issue-only prompts, hidden
  validation, fixed base commits, and no explicit expected file hints in the
  prompt.

Use real SWE-bench Lite or Verified tasks once the local matrix is stable:

- cache each target repo locally;
- pin the upstream base commit;
- apply the SWE problem statement as the prompt;
- apply the SWE test patch or equivalent hidden validation after checkout;
- compare no-brain vs semantic/full only after the harness can guarantee
  isolation and repeatable setup.

### Phase 2C: Model, Effort, and Cost Matrix

Run the most discriminating tasks across:

- Codex: at least low, medium, and high reasoning effort for the active model;
- Claude Code: Sonnet and Opus where available, with low/medium/high effort;
- conditions: no brain, semantic brain, full brain when checkpoint history
  exists;
- metrics: success, score, turns, tokens, duration, reported/estimated cost,
  and cost per successful run.

Optimize for the cheapest configuration that preserves the brain advantage. A
brain condition is a win if it improves correctness, or if correctness is equal
and it significantly reduces tokens, turns, duration, or cost.

Run a model and effort matrix in addition to the agent/context matrix:

- Codex runners: `codex:<model>:<reasoning_effort>`, using the local Codex
  `--model` flag and `model_reasoning_effort` config override.
- Claude Code runners: `claude:<model>:<effort>`, using Claude `--model` and
  `--effort`.
- Compare each runner independently against its own no-brain baseline before
  making aggregate claims.
- Optimize for cost only after correctness is equal or better. For saturated
  tasks, prefer the runner/context pair with the lowest cost, then lower
  duration and turns.
- Treat Claude costs as reported by Claude JSON output. Treat Codex costs as
  estimates unless the CLI output includes reported cost; require a current
  pricing table in the run artifact before claiming dollar savings.

Add these scenario families:

- **History-dependent rationale tasks**
  - Restore the required SHA-256 contract for `bundle import` after a setup
    regression makes missing checksums valid again.
  - Restore bundle audit-log exclusion/rejection after a setup regression leaks
    local-only data.
  - Restore `.github` visibility after an over-broad `.git` ignore regression.
  - Expected value: full brain should recover prior checkpoint rationale that
    is not obvious from local code alone.

- **Large-repo navigation tasks**
  - Add harder `../cli` review/resume context, plugin env, and agent hook
    lifecycle regressions with hidden expected files and hidden validation.
  - Expected value: semantic or full brain should reduce search breadth, token
    use, and time even when no-brain can eventually pass.

- **Cross-repo workspace tasks**
  - Break a plugin dispatch/env assumption where the fix could plausibly live
    in `entire-brain` or `../cli`.
  - Later include `../entire-sem` for provider/consumer contract mismatch
    scenarios.
  - Expected value: workspace context should orient the agent to the right repo
    boundary faster.

- **Validation-selection tasks**
  - Add semantic query/context, workspace freshness, bundle import validation,
    and GitHub CLI JSON flag tasks where the correct focused test is not named
    in the prompt.
  - Expected value: `entire brain tests` and checkpoint history should improve
    selected validation commands.

- **Stale-context hygiene tasks**
  - Prepare a semantic brain, apply the setup regression after indexing, then
    allow brain use.
  - Expected value: brain-enabled agents should run `stale`, detect unsafe or
    dirty context, refresh or fall back, and avoid blindly trusting stale data.

Iteration loop:

1. Design one pilot task for history, navigation, and validation-selection.
2. Run Codex only, one repetition per condition.
3. Drop saturated tasks where all conditions score perfectly and brain
   increases tokens/time/cost.
4. Keep tasks where brain improves correctness, tokens by at least 20%, time by
   at least 15%, cost by at least 15%, files read before relevant file by at
   least 30%, or validation quality.
5. Run retained proof tasks with at least five repetitions per condition for
   both Codex and Claude Code across at least two model/effort settings per
   agent.
6. If no metric improves materially, redesign around more brain-specific
   information.

Acceptance criteria:

- At least nine non-smoke tasks:
  - three history-dependent
  - three large-repo navigation
  - two validation-selection
  - one stale-context hygiene
- At least five repetitions per retained proof task and condition.
- Final report separates correctness improvement, efficiency improvement,
  cost improvement, validation-quality improvement, and no-signal saturated
  tasks.
- Claims must match the data: correctness only when correctness improves,
  efficiency only when operational metrics improve, cost only when reported or
  current-price-estimated cost improves, and checkpoint-history value only when
  full brain beats semantic brain.

Avoid rebuilding expensive semantic indexes per repetition for large repos.
Reuse bundles or disable semantic indexing when the scenario is specifically
about checkpoint history.

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
