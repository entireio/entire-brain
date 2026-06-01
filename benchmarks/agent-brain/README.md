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

Claude Code runs are supported, but use `--claude-budget` deliberately. Even a
small non-interactive Claude Code call can create a large prompt cache.

The report command recomputes aggregate means and approximate Welch p-values:

```sh
python3 benchmarks/agent-brain/run.py report codex-mcp-smoke
```

Reports include score deltas plus mean agent seconds, total tokens, and turns
when available.

The harness intentionally keeps generated brain artifacts and worktrees out of
the repository. Result directories are ignored by git.
