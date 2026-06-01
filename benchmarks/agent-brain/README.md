# Agent Brain Benchmark Harness

This directory contains a repeatable harness for comparing Codex and Claude Code
with and without Entire Brain.

Each task creates a disposable git worktree, applies a known regression patch,
commits that setup state, runs an agent, validates the fix, scores the run, and
writes artifacts under `benchmarks/agent-brain/results/`.

Tasks may also define `post_brain_replacements` or `post_brain_commands`. Those
mutations are applied and committed after brain preparation, which creates a
stale-context scenario for semantic and full-brain runs. Use these tasks to
measure whether agents check brain freshness before relying on prepared context.

Brain prep artifacts are cached under `benchmarks/agent-brain/cache/` by
repo/base/setup/condition/tool hash. Each run receives its own copy of the
cached plugin directory, with text artifacts rewritten to the current disposable
worktree path. Use `--refresh-brain-cache` to overwrite a cache entry or
`--no-brain-cache` to force per-run rebuilds. The cache avoids repeated prep
after a brain has been built successfully; it does not fix slow or incomplete
initial semantic indexing.

Each `record.json` includes:

- `agent_info.seconds` for wall-clock agent duration.
- `brain_prep.commands[].seconds` for seed/export/index setup cost.
- `validation.results[].seconds` for validation command duration.
- `agent_info.usage` for turns, tokens, cache tokens, and cost when the agent
  output exposes those fields.

Example:

```sh
python3 benchmarks/agent-brain/run.py run \
  --tasks entire-brain-mcp-tool-name.json \
  --agents codex \
  --conditions no_brain,semantic_brain,full_brain \
  --repetitions 3 \
  --suite-name codex-mcp-smoke
```

Runner matrixes are supported with `--runners`. Specs are
`agent[:model[:effort]]`, optionally prefixed by a stable id:

```sh
python3 benchmarks/agent-brain/run.py run \
  --tasks entire-brain-history-codex-schema-contract.json \
  --runners codex:gpt-5:medium,codex:gpt-5:high,claude:sonnet:medium,claude:opus:max \
  --conditions no_brain,full_brain \
  --repetitions 3 \
  --suite-name phase2-model-matrix
```

Codex runner efforts are passed as
`--config model_reasoning_effort="<effort>"`. Claude runner efforts are passed
as `--effort <effort>`.

Agent process isolation is explicit in each result record. Codex runs use
`--ephemeral`, `--ignore-user-config`, and `--ignore-rules`. Claude runs use
`--no-session-persistence`, `--strict-mcp-config`, an empty MCP config, and
`--disable-slash-commands`. Authentication still comes from the host account
state (`CODEX_HOME` for Codex and the configured Claude Max/OAuth state for
Claude), so the isolation claim is about run memory, MCP/tools, and project
instructions, not about auth credentials.

Claude Code runs are supported, but use `--claude-budget` deliberately when you
need a hard cap. Even a small non-interactive Claude Code call can create a
large prompt cache.

Cost is recorded in `agent_info.usage.cost_usd`. Claude reports exact cost in
JSON output. Codex cost is estimated only when a price map is supplied:

```sh
python3 benchmarks/agent-brain/run.py run \
  --tasks entire-brain-history-codex-schema-contract.json \
  --runners codex:gpt-5:medium \
  --pricing-json '{"gpt-5":{"input_per_million":0,"output_per_million":0}}'
```

Use current pricing before making cost claims. Reports label estimated vs
reported cost with `agent_info.usage.cost_source`.

The report command recomputes aggregate means and approximate Welch p-values:

```sh
python3 benchmarks/agent-brain/run.py report codex-mcp-smoke
```

Reports include score deltas, success rates, mean agent seconds, total tokens,
turns, cost when available, and approximate Welch p-values for score, duration,
tokens, turns, and cost.

For a SWE-bench-style matrix, tag tasks with `source` and `suite_tags`. The
current harness already supports the essential SWE shape: issue prompt,
regression setup patch/replacements, hidden validation, disposable worktree, and
per-run artifacts. External SWE-bench tasks should be imported as local task
JSON files with fixed base commits and cached repositories before large runs.

The harness intentionally keeps generated brain artifacts and worktrees out of
the repository. Result directories are ignored by git.
