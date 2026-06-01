# Agent Brain Benchmark Harness

This directory contains a repeatable harness for comparing Codex and Claude Code
with and without Entire Brain.

Each task creates a disposable git worktree, applies a known regression patch,
commits that setup state, runs an agent, validates the fix, scores the run, and
writes artifacts under `benchmarks/agent-brain/results/`.

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

Claude Code runs are supported, but use `--claude-budget` deliberately. Even a
small non-interactive Claude Code call can create a large prompt cache.

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
turns, and cost when available.

The harness intentionally keeps generated brain artifacts and worktrees out of
the repository. Result directories are ignored by git.
