# Agent Brain Benchmark Plan

## Status addendum (2026-06-10)

The per-phase "current status" notes below are dated ~2026-06-01 and predate the
most recent harness work. Current state:

- **Phase 1 (repeatable harness) and Phase 2 (discovery): done.** 39 task
  definitions exist across entire-brain, entire-cli, github-cli, swe-style, and
  ultron variants; `run.py discover` generates candidates.
- **Phase 3 (value-prop proof): in progress.** A stable benchmark panel landed
  (`panels/full.json`, commit 497218c) with pinned runners
  (claude:claude-sonnet-4-6:high, codex:gpt-5.5:high), a stability gate, and 8
  tasks × 4 repetitions. Welch t-test + Holm–Bonferroni significance is
  implemented (commits fb7ca3f, 2a4c4fe). A `prep` subcommand (cd547d5)
  validates brain caches without agent runs.
- **Phase 4 (demo evidence): not started.**
- **Reconciling panel size vs. layer targets:** Phase 2's "20+ significant
  scenarios per layer" is the *discovery* goal; the 8-task panel is the
  *committed proof subset* for reproducible Phase 3 runs. The panel grows as
  discovery retains more brain-positive scenarios; the two numbers are not in
  conflict.
- **Known open items:** real SWE-bench Lite/Verified integration (Layer C),
  the cross-repo workspace task (declared not-runnable in the panel), Codex
  model attribution (pinned via flag but not echoed in output), and expanding
  full-brain (checkpoint-history) tasks in the panel.

Checked-in artifacts under `benchmarks/agent-brain/evidence` are immutable
historical records. Product and command labels in those artifacts identify the
binaries that generated them; naming migrations must preserve the original
bytes unless a repository-owned process derives new, fingerprint-valid evidence.

## Summary

This benchmark measures whether Codex and Claude Code perform better on real
repository tasks when they can use Entire Brain.

The first version should optimize for a repeatable harness, then use the same
run artifacts to produce demo-quality before/after evidence. The benchmark
starts with three local repositories. The checked-in task fixtures use portable
repo names; runners resolve them from the benchmark workspace or explicit task
paths:

- `entire-brain`
- `entire-cli`
- GitHub CLI

The benchmark should compare agent performance across controlled context
conditions, not just tool latency. Useful outcomes include faster localization,
better test selection, fewer irrelevant file reads, fewer wrong edits, and
better recovery of implementation rationale.

The Phase 2 benchmark should optimize across three scenario layers:

- Layer A: project-native hard tasks in the two repositories with Entire session
  data: `entire-brain` and `entire-cli`.
- Layer B: project-native hard tasks in GitHub CLI.
- Layer C: SWE-bench-style tasks with issue prompts, hidden tests, fixed base
  commits, and no repo-specific hints beyond the condition policy.

The model, reasoning-effort, turn, token, duration, and cost matrix is not a
separate Phase 2 layer. It is a later overlay applied to retained scenarios in
Layers A, B, and C.

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
   - The agent may use `status`, `query`, `context`, `impact`, `changes`,
     `routes`, `tools`, `workflows`, `tests`, and workspace commands.
   - Checkpoint transcript/session history is withheld.

3. **Full brain**
   - Includes semantic brain plus exported Entire checkpoint history.
   - The agent may use seed context, semantic facts, history gaps, checkpoint
     metadata, prompts, and transcripts.
   - This full-history form is available for `entire-brain` and `entire-cli`.
   - For GitHub CLI, which has no Entire checkpoint history, the full-brain
     condition means the complete available brain package for that repo
     (`history_available=false`); do not report GitHub CLI wins as
     full-history wins.

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
   - Fix a narrow issue in `status`, `query`, or `context`.
   - Brain benefit: semantic commands should identify the relevant functions and
     tests quickly.
   - Validate with focused semantic tests and `go test ./...`.

2. **MCP and CLI contract change**
   - Add or adjust one semantic JSON field while keeping MCP wrappers compatible.
   - Brain benefit: `impact` should reveal the CLI command, JSON output path,
     MCP adapter, and contract tests.
   - Validate with semantic and MCP tests.

3. **Workspace behavior task**
   - Change workspace inspect context or impact behavior in a bounded way.
   - Brain benefit: workspace inspect context should locate freshness handling and
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

- whether the agent checked freshness (`status`)
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

Score each run with the v2 quality rubric and record the component scores in
`record.json` under `score.version = 2`. The score is still only a triage aid;
final claims should cite the underlying component metrics.

- 45 outcome: agent completed successfully and harness validation commands pass,
  with partial validation credit when only some commands pass.
- 30 patch focus: expected files touched, unexpected files avoided, forbidden
  files avoided, missing expected files penalized, and diff size kept bounded.
- 10 validation discipline: the agent ran relevant tests, inspected the final
  diff/status, and left the harness validation passing.
- 10 runtime efficiency: bounded absolute credit for agent seconds and token
  use. Cross-condition time/token means remain the primary efficiency evidence.
- 5 brain use: no-brain runs avoid brain commands; brain-enabled runs use brain
  commands, check freshness when semantic context is available, and avoid
  lock/freshness failures.

The old v1 score mostly collapsed passing runs to 90 or 100 because validation
success, efficiency, and brain hygiene were nearly constant and locality only
had two passing states. Do not mix v1 and v2 score means in final proof tables;
rerun retained tasks after rubric changes.

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

### Phase 2: Brain Product Question Discovery

Phase 2 answers the three core product questions directly. It is a discovery
phase, not a smoke-test phase: the benchmark should actively search for
scenarios where agent plus brain is measurably better than the same agent
without brain context.

Do not spend Phase 2 budget on simple coding tasks that modern models solve
reliably without brain context. Saturated tasks are useful only as harness
smoke tests and should be dropped from proof queues as soon as they show no
brain advantage.

#### Question 1: Semantic Index vs Full Session Data

The semantic index is a fresh-enough repository understanding snapshot, not
live agent memory. It is best for architecture, localization, boundaries,
impact, and validation selection. It may not include edits from the current
agent session.

Session history is best for historical rationale: prior decisions, rejected
approaches, regressions, compatibility quirks, validation recipes, repeated
failure modes, and successful tool paths.

Benchmark requirements:

- Semantic-only tasks must target architecture/navigation, impact, boundary
  discovery, validation selection, and stale/live-state hygiene.
- Full-brain tasks must target rationale-dependent behavior where session
  history changes available information.
- Hybrid tasks should test whether semantic context finds the relevant code and
  session history selects the correct invariant or validation recipe.
- `brief` should include a cheap live-state overlay: branch, HEAD, dirty file
  list, staged/unstaged state, diff stats, and changed-symbol hints when
  available. Agents should inspect full diffs or file contents only when the
  task intersects those live changes.

#### Question 2: Agent-Facing Brain Shape

Benchmark the simple agent contract, not a large specialist command menu.

Normal agent entry points:

```sh
entire brain status [repo] --json
entire brain brief "<task>" --json
entire brain search "<query>" --json
entire brain show <id> --json
entire brain refresh
entire brain guide
entire brain path [repo]
```

Specialist/debug commands live under `inspect`, for example
`inspect code`, `inspect search-graph`, `inspect query-graph`,
`inspect graph-schema`, `inspect snippet`, `inspect trace-path`,
`inspect dead-code`, `inspect ingest-traces`, `inspect context`,
`inspect impact`, `inspect changes`,
`inspect tests`, `inspect boundaries`, `inspect regressions`, and
`inspect blame`. Workspace use has its own front door:

```sh
entire brain workspace inspect context <name> "<symbol-or-query>" --json
entire brain workspace inspect impact <name> "<symbol-or-query>" --json
entire brain workspace search <name> "<query>" --json   # also: vsearch, query, get
entire brain workspace refresh <name> --json
entire brain workspace watch <name>
```

`brief` should be implemented as a context-graph query that ranks code facts,
history facts, validation recipes, and live-state overlays into one bounded
task packet.

For full-history repositories, `brief` must include ranked `history.matches`
for the task prompt. Broad reads should go through `search`, `query`, `get`, and
`multi-get`, while semantic/code reads should use the current `inspect`
subcommands. Decision-like rationale is now a fact/history retrieval concern,
not a separate `inspect decisions` surface.

History ranking must preserve identifier signal. Terms such as
`ENTIRE_REVIEW_*`, `ENTIRE_PLUGIN_ENV`, `XDG_*`, camel-case invariants, and
symbol-like names should survive normalization and rank above generic status
summaries.

#### Question 3: What Full Session Data Teaches Agents

Build a history index during `refresh sessions`/`refresh --history-index`, not during
normal read commands. Extract decisions, learnings, validation recipes, tool
paths, topic clusters, repeated failure modes, and provenance back to
sessions/checkpoints.

Also extract bounded source-derived `code_fact` records from tool outputs when
session logs contain concrete code snippets, invariants, schema contracts,
state updates, validation failures, or environment constants. These are not
decisions; they are compact evidence that lets `brief` and broad `inspect`
queries recover facts that were visible to the previous agent but not stated in
assistant narration.

Link history records to semantic files/symbols when possible. Mark links
degraded when the semantic snapshot, history index, or file mapping is stale.
For `entire-cli`, start with dense exported-history areas: agents, hooks,
checkpoints, transcripts, review/resume, env filtering, attribution,
provenance, and plugin dispatch.

Do not count a full-history scenario unless the brain can surface direct useful
evidence through `brief` or a documented `inspect` command. If the best history
match is only adjacent tool noise or a broad status summary, improve extraction
first or replace the scenario.

For semantic prep, pass the repo `.brainignore` to the semantic provider with
`--ignore-file` when it exists, and still apply Entire Brain's post-snapshot
redaction as a safety net. The provider now honors Git ignores and additional
ignore-list files in worktree mode, so retained tasks may use worktree semantic
prep after validating that generated benchmark artifacts are skipped before
walking/reading.

#### Phase 2 Discovery Targets

Phase 2 should retain statistically significant brain-positive scenarios in
each scenario layer, using Claude Code with its default model. Do not use Codex,
cheaper models, or alternate reasoning efforts to satisfy Phase 2 scenario
counts.

- Layer A, Entire-data project-native tasks: at least 20 unique scenarios across
  `entire-brain` and `entire-cli` where full brain beats no brain.
- Layer B, GitHub CLI project-native tasks: at least 20 unique scenarios where
  the full-brain condition beats no brain. For GitHub CLI this means the
  complete available brain package with `history_available=false`, not a
  full-history claim.
- Layer C, SWE-bench-style tasks: at least 20 unique scenarios where full brain
  beats no brain.

The model/effort/cost matrix is postponed until after these targets are met. It
should be overlaid later on retained scenarios from Layers A, B, and C.

Treat a retained scenario as brain-positive only when:

- the compared runs use the same agent, model, effort, task, base commit,
  hidden validation, and isolation policy;
- brain improves correctness, or correctness is equal and brain improves at
  least one operational metric by a material threshold;
- the result is statistically significant at `p < 0.05` using the appropriate
  test for the metric: Fisher exact or bootstrap/permutation for pass/fail,
  Welch or bootstrap for time, tokens, cost, and file-read counts;
- the effect is repeatable with enough repetitions to survive one obvious
  outlier.

This acceptance is **enforced in code** by the stable-panel path: a committed manifest
(`benchmarks/agent-brain/panels/full.json`) pins `agent:model:effort` runners (preflight rejects
unpinned ones), and the stability gate in `summarize` tags each comparison `brain_positive_stable`
(p<0.05 token win or pass-rate lift that survives dropping the single best/worst rep),
`saturated` (both arms pass 100%), or `noisy`, alongside a coefficient of variation. Run it with
`python3 run.py panel full`. The gate can only downgrade a result — it never manufactures significance.

A unique scenario is one task/problem shape with one prompt, base commit, hidden
validation setup, and layer assignment. Repeating the same scenario on the same
model proves or rejects that scenario; it does not create more scenarios.
Running the same scenario on another model is model-matrix evidence only.

Discovery loop:

1. Generate candidates from context-graph gaps, history hotspots, hidden
   validation-selection tasks, stale/live-state hygiene, and cross-repo
   boundaries.
2. Run Claude-default pilots only to eliminate saturated or broken tasks.
3. Promote only candidates with a plausible brain-specific signal.
4. Run proof repetitions until the scenario is significant or rejected.
5. Keep a rejected-task ledger explaining why each candidate saturated or failed
   to show a brain advantage.

Acceptance criteria for Phase 2:

- `brief` is benchmarked as the default brain entry point.
- Reports separate semantic-only wins, full-history wins, hybrid wins,
  workspace wins, stale/live-state hygiene wins, and saturated/no-signal tasks.
- Each retained layer has at least 20 statistically significant brain-positive
  unique scenarios on Claude's default model before moving to broad model/cost
  proof.

### Phase 3: Value-Prop Proof Suite

The initial harness runs showed that simple bugfix tasks can saturate: agents
may pass every condition, leaving no correctness delta. Phase 3 should add
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

### Phase 3A: Isolation Hardening

Agents should not inherit useful knowledge from previous benchmark runs.

- Codex runs with `--ephemeral`, `--ignore-user-config`, and `--ignore-rules`.
- Claude runs with `--no-session-persistence`, `--strict-mcp-config`, an empty
  MCP config, and `--disable-slash-commands`.
- The remaining intentional shared state is auth only: Codex may use host
  `CODEX_HOME` credentials and Claude may use the authorized Max/OAuth account.
- Every result record includes an `isolation` object so old and new runs are not
  mixed accidentally.

### Phase 3B: Complexity Expansion

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

Current coverage is now wide enough for pilot filtering but still short of final
repo-general evidence: 19 project-native task definitions and 6 local
SWE-bench-style task definitions. Project-native coverage is 8 `entire-brain`,
6 `entire-cli`, and 5 GitHub CLI tasks. Treat local SWE-style tasks as harness
shakedown only; they do not replace real SWE-bench Lite/Verified evidence.

Current pilot-filtering status:

- `entire-cli` semantic indexing is no longer blocked, and sandboxed semantic
  runs can read `.benchmark/plugin` stores. A read-lock issue found during the
  pilot is fixed: read-only semantic commands no longer take the exclusive index
  lock, while writer commands still do.
- Most current `entire-cli` pilots are saturated. `review-base-flag-scope` has
  a possible token/time efficiency signal at `n=1`, but `review-prompt`,
  `transcript-reresolve`, and the checkpoint-history `review-provenance-strip`
  task did not produce a retained correctness signal.
- GitHub CLI semantic pilots produced two local Codex candidate leads with five
  repetitions per condition: `github-cli-repo-name-trims-dotgit` and
  `github-cli-http-scopes-suggestion`. Treat those ignored local results as
  leads, not release evidence, until a retained subset is committed and
  `audit_codex.py --fail-on-flags` passes against it. They also need Claude and
  alternate Codex runner coverage before final claims.
- A one-run Claude Code pilot on those two GitHub CLI candidates saturated:
  Claude scored 100 in both no-brain and semantic-brain conditions, and
  semantic-brain added overhead. Treat these as Codex-retained tasks unless
  redesigned for a harder Claude setting.

Expand the task inventory to:

- `entire-cli`: 6-10 non-smoke tasks. Include review/resume context, plugin
  dispatch/env filtering, agent hook lifecycle, transcript/session adoption,
  checkpoint remote handling, and provenance propagation. Run full-brain
  checkpoint-history conditions now; run semantic conditions only after
  `entire-cli` semantic indexing completes reliably.
- GitHub CLI: 5-8 semantic-only tasks. Focus on JSON/export behavior,
  `cmdutil`/factory/auth/repo resolution, mocked HTTP/API error handling, and
  validation-selection tasks where the focused package test is not obvious from
  the prompt.
- SWE-bench Layer B: start with 10 cached SWE-bench Lite or Verified tasks,
  then scale to 25 if setup is stable. Keep local SWE-style tasks as harness
  shakedown only; they do not replace real SWE-bench evidence.

Use real SWE-bench Lite or Verified tasks once the local matrix is stable:

- cache each target repo locally;
- pin the upstream base commit;
- apply the SWE problem statement as the prompt;
- apply the SWE test patch or equivalent hidden validation after checkout;
- compare no-brain vs semantic/full only after the harness can guarantee
  isolation and repeatable setup.

### Phase 3B.1: Semantic Prep Cache

Do not rebuild deterministic semantic indexes per repetition. Fresh worktrees
and isolated agent processes are necessary; recomputing the same read-only
semantic store for every repetition is not.

Add a semantic-prep cache keyed by:

- repo identity and base commit;
- setup patch/replacement hash;
- semantic binary/version and index configuration;
- brain task condition and checkpoint export limit when relevant.

For each run, copy or hardlink the cached semantic store into the run directory
and record the cache key, source manifest, and freshness metadata in
`record.json`. For stale-context tasks, build or reuse the cache at the
pre-regression state, then apply the post-brain mutation per run. Invalidate the
cache whenever the base commit, setup variant, semantic version, or indexing
configuration changes.

Implemented cache behavior:

- cache root: `benchmarks/agent-brain/cache/`;
- bypass: `--no-brain-cache`;
- refresh: `--refresh-brain-cache`;
- each run gets a copied plugin directory, not a shared writable cache path;
- text artifacts containing the cache-populating worktree path are rewritten to
  the current disposable worktree path.

This cache removes repeated prep cost after a brain has been built. The initial
`entire-cli` semantic index build is now bounded enough for local benchmark
use, so the cache is usable for the expanded semantic task set.

### Phase 3B.2: `entire-cli` Semantic Index Blocker Resolved

The earlier `entire-cli` semantic indexing pause is resolved for local
benchmarking:

- `entire-graph snapshot --repo <entire-cli-checkout> --format ndjson
  --no-network` completes in about 17 seconds with normal project ignores;
- isolated `entire brain refresh index <entire-cli-checkout>` completes in about 29
  seconds using locally built `entire-brain` and `entire-graph`;
- the artifact records 760 files, 9,130 symbols, 179,717 stored relations, zero
  warnings, zero partial failures, and a roughly 152 MB SQLite store;
- semantic prep cache entries for four `entire-cli` tasks now produce
  `stale=ok` on both cold prep and cache-hit reuse.

Harness fixes made while resolving this:

- added `run.py prep` to verify/cache brain artifacts without launching agents;
- made benchmark-generated setup commits deterministic, so commit-addressed
  semantic snapshots stay fresh across identical disposable worktrees;
- copied per-run plugin state under ignored `.benchmark/plugin/` inside the
  disposable worktree so sandboxed agents can use semantic SQLite and lock files.

Current caveat: the first resumed Codex smoke task
`entire-cli-plugin-env-xdg-prefix` remains saturated for correctness. Both
no-brain and semantic-brain conditions passed in one repetition. Semantic brain
was faster and used fewer tokens in that single run, but `n=1` is not proof and
the task should be treated as a harness smoke unless repeated runs show a stable
operational delta.

Next work:

- run Codex-only pilots across the warmed `entire-cli` semantic tasks;
- retain only tasks with a measurable correctness, navigation, validation,
  duration, token, or cost signal;
- then resume the Codex/Claude no-brain, semantic-brain, and full-brain matrix
  on retained tasks.

### Phase 3C: Model, Effort, and Cost Matrix

Run the most discriminating tasks across:

- Codex: explicitly pinned models and at least medium/high effort for the
  primary runners, with low effort added after a task has a retained signal.
  Include at least two lower-priced alternatives to the strongest supported
  model, starting with `gpt-5.4-mini` and either `gpt-5.3-codex` or `gpt-5.2`.
- Claude Code: explicitly pinned aliases or full model names for Sonnet and
  Opus where available, with low/medium/high effort. Include at least two
  lower-priced alternatives to Opus when available through the CLI, starting
  with Sonnet and Haiku.
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
- Do not use unpinned `codex` or `claude` runners for proof claims. They are
  allowed only for smoke tests because defaults can change and may not be
  visible in the run artifacts.
- Compare each runner independently against its own no-brain baseline before
  making aggregate claims.
- Optimize for cost only after correctness is equal or better. For saturated
  tasks, prefer the runner/context pair with the lowest cost, then lower
  duration and turns.
- Treat Claude costs as reported by Claude JSON output. Treat Codex costs as
  estimates unless the CLI output includes reported cost; require a current
  pricing table in the run artifact before claiming dollar savings.
- Published pricing is per token/model, not per reasoning effort. Record
  requested effort because it can change token usage, latency, and success rate;
  compute cost from actual input, cached-input, output, and tool-use tokens.

Current local model availability snapshot, captured on 2026-06-01:

| Agent | Requestable model or alias | Observed/resolved model | Efforts | Primary benchmark use |
|---|---|---|---|---|
| Codex | `gpt-5.5` | not resolved in existing Codex artifacts | `low`, `medium`, `high`, `xhigh` | primary pinned Codex model |
| Codex | `gpt-5.3-codex` | not resolved in existing Codex artifacts | `low`, `medium`, `high`, `xhigh` | Codex-specialized comparison |
| Codex | `gpt-5.4-mini` | not resolved in existing Codex artifacts | `low`, `medium`, `high`, `xhigh` | cheaper-efficiency comparison after correctness is stable |
| Codex | `gpt-5.4`, `gpt-5.2` | not resolved in existing Codex artifacts | `low`, `medium`, `high`, `xhigh` | reserve/backfill comparisons |
| Codex | `gpt-5.3-codex-spark` | not resolved in existing Codex artifacts | `low`, `medium`, `high`, `xhigh` | exclude from primary matrix until a CLI smoke confirms support |
| Codex | `codex-auto-review` | hidden in the local cache | `low`, `medium`, `high`, `xhigh` | exclude from benchmark claims |
| Claude Code | `sonnet` | `claude-sonnet-4-6` in prior benchmark artifacts | `low`, `medium`, `high`, `xhigh`, `max` | primary cheaper Claude runner |
| Claude Code | `opus` or `claude-opus-4-8` | `claude-opus-4-8[1m]` in recent/default artifacts | `low`, `medium`, `high`, `xhigh`, `max` | stronger Claude runner and long-context comparison |
| Claude Code | not a primary runner yet | `claude-haiku-4-5-20251001` observed as auxiliary usage | n/a | record as auxiliary model usage, not as the runner model |

Pricing snapshot, researched on 2026-06-01 from official provider pages:

| Provider | Model | Efforts to benchmark | Input / 1M | Cached input / 1M | Output / 1M | Notes |
|---|---|---|---:|---:|---:|---|
| OpenAI/Codex | `gpt-5.5` | `low`, `medium`, `high`, `xhigh` | $5.00 | $0.50 | $30.00 | Primary strongest Codex baseline. |
| OpenAI/Codex | `gpt-5.4` | `low`, `medium`, `high`, `xhigh` | $2.50 | $0.25 | $15.00 | Lower-priced alternative. |
| OpenAI/Codex | `gpt-5.4-mini` | `low`, `medium`, `high`, `xhigh` | $0.75 | $0.075 | $4.50 | Lower-priced alternative. |
| OpenAI/Codex | `gpt-5.3-codex` | `low`, `medium`, `high`, `xhigh` | $1.75 | $0.175 | $14.00 | Codex-specialized lower-priced alternative. |
| OpenAI/Codex | `gpt-5.2` | `none`, `low`, `medium`, `high`, `xhigh` when supported by runner | $1.75 | $0.175 | $14.00 | Backfill/reserve lower-priced alternative. |
| Anthropic/Claude | `claude-opus-4-8` | `low`, `medium`, `high`, `xhigh`, `max` | $5.00 | $0.50 cache hit | $25.00 | Stronger Claude baseline; 5m cache write $6.25, 1h cache write $10.00. |
| Anthropic/Claude | `claude-sonnet-4-6` | `low`, `medium`, `high`, `xhigh`, `max` | $3.00 | $0.30 cache hit | $15.00 | Lower-priced alternative. |
| Anthropic/Claude | `claude-haiku-4-5` | runner support required | $1.00 | $0.10 cache hit | $5.00 | Lower-priced alternative if Claude Code exposes it as a runner. |

Pricing source links:

- OpenAI API pricing: `https://openai.com/api/pricing/`
- OpenAI GPT-5.3-Codex model pricing: `https://developers.openai.com/api/docs/models/gpt-5.3-codex`
- OpenAI GPT-5.2 model pricing: `https://developers.openai.com/api/docs/models/gpt-5.2`
- Anthropic pricing: `https://platform.claude.com/docs/en/about-claude/pricing`

Model-attribution rules:

- Codex CLI `0.135.0` has `--model`, but the *earlier* benchmark runs used
  `--ignore-user-config` and no `--model`. The user config default
  `gpt-5.5` therefore cannot be attributed to those older records.
- For the cliproof proof set, every runner is pinned `agent:model:effort`, so
  `run_agent` passes `--model gpt-5.5` / `--model gpt-5.4-mini` explicitly
  (run.py, codex branch) instead of relying on a user-config default.
- Attribution confidence differs by agent, and we disclose it:
  - Claude is OUTPUT-VERIFIABLE — its JSON exposes `modelUsage` keys
    (e.g. `claude-haiku-4-5`), captured as `agent_info.resolved_model`.
  - Codex is PINNED-BUT-NOT-OUTPUT-CONFIRMED — `codex exec --json` does not echo
    the resolved model and does not client-side validate `--model` (an invalid
    model string is accepted), so codex attribution rests on the explicit
    `--model` flag, not on the output. `resolved_model` is therefore typically
    null for codex. Do not claim per-record codex model verification.
- Claude Code `2.1.159` exposes model aliases but not a full availability
  catalog. Store the requested alias/full name, requested effort, CLI version,
  and all `modelUsage` keys from Claude JSON output.
- If a requested alias resolves to a newer full model later, split the report
  by resolved model rather than mixing records under the alias.

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
  - Later include `../entire-graph` for provider/consumer contract mismatch
    scenarios.
  - Expected value: workspace inspect context should orient the agent to the right repo
    boundary faster.

- **Validation-selection tasks**
  - Add semantic query/context, workspace freshness, bundle import validation,
    and GitHub CLI JSON flag tasks where the correct focused test is not named
    in the prompt.
  - Expected value: `entire brain inspect tests` and checkpoint history should improve
    selected validation commands.

- **Stale-context hygiene tasks**
  - Prepare a semantic brain, apply the setup regression after indexing, then
    allow brain use.
  - Expected value: brain-enabled agents should run `status`, detect unsafe or
    dirty context, refresh or fall back, and avoid blindly trusting stale data.

Iteration loop:

1. Design one pilot task for history, navigation, and validation-selection.
2. Run Codex only, one repetition per condition.
3. Drop saturated tasks where all conditions score perfectly and brain
   increases tokens/time/cost.
4. Keep tasks where brain improves correctness, tokens by at least 20%, time by
   at least 15%, cost by at least 15%, files read before relevant file by at
   least 30%, or validation quality.
5. Run retained proof tasks with the implemented proof minimum of at least four
   repetitions per condition for both Codex and Claude Code across at least two
   model/effort settings per agent.
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

Avoid disabling semantic indexing for `entire-cli` solely because indexing is
slow. Fix the underlying semantic indexing blocker first. Disable semantic
indexing only when the scenario is specifically about checkpoint history and
semantic context is not part of the claim being measured.

### Phase 4: Demo Evidence

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
