# Agent Brain Benchmark Results

## Current Status

The harness is implemented and has run initial Codex suites, but the completed
runs do **not** yet provide statistical proof that brain-enabled scenarios
improve agentic coding.

The first two runnable tasks were too easy for Codex:

| Suite | Task | Condition | n | Mean score | Result |
|---|---|---|---:|---:|---|
| `codex-mcp-r3` | `entire-brain-mcp-tool-name` | no brain | 3 | 100 | baseline saturated |
| `codex-mcp-r3` | `entire-brain-mcp-tool-name` | semantic brain | 3 | 100 | no delta |
| `codex-mcp-r3` | `entire-brain-mcp-tool-name` | full brain | 3 | 100 | no delta |
| `codex-cli-env-r3` | `entire-cli-external-command-env-filter` | no brain | 3 | 90 | baseline passed |
| `codex-cli-env-full-r3` | `entire-cli-external-command-env-filter` | full brain | 3 | 90 | no delta |
| `claude-mcp-r1` | `entire-brain-mcp-tool-name` | no brain | 1 | 100 | baseline saturated |
| `claude-mcp-r1` | `entire-brain-mcp-tool-name` | semantic brain | 1 | 100 | no delta |
| `claude-mcp-r1` | `entire-brain-mcp-tool-name` | full brain | 1 | 100 | no delta |

Token and duration metrics are now included in aggregate reports. Backfilled
Codex token estimates from saved logs show a token-consumption reduction despite
no score improvement:

| Task | Comparison | Mean agent seconds | Mean tokens |
|---|---|---:|---:|
| `entire-brain-mcp-tool-name` | no brain | 65.4 | 71,744 |
| `entire-brain-mcp-tool-name` | semantic brain | 65.7 | 55,997 |
| `entire-brain-mcp-tool-name` | full brain | 90.8 | 59,459 |
| `entire-cli-external-command-env-filter` | no brain | 144.4 | 105,866 |
| `entire-cli-external-command-env-filter` | full brain | 135.6 | 75,401 |

Claude smoke metrics on the saturated MCP task:

| Task | Comparison | Mean agent seconds | Mean tokens | Mean turns |
|---|---|---:|---:|---:|
| `entire-brain-mcp-tool-name` | no brain | 38.7 | 191,706 | 7 |
| `entire-brain-mcp-tool-name` | semantic brain | 52.7 | 270,383 | 10 |
| `entire-brain-mcp-tool-name` | full brain | 55.5 | 242,388 | 12 |

Combined report:

```sh
python3 benchmarks/agent-brain/run.py report \
  codex-mcp-r3 \
  codex-cli-env-r3 \
  codex-cli-env-full-r3
```

Result: every completed comparison has `delta = 0` and approximate `p = 1`.

## Important Observations

- The benchmark harness works end to end: disposable worktrees, setup
  regressions, agent execution, validation, scoring, records, and aggregate
  reports.
- The harness now records agent duration, brain-prep duration, validation
  duration, and normalized usage fields for turns/tokens/cost when available.
  The initial Codex records were backfilled from saved `tokens used` logs.
  Turn counts were not available in those text logs; future Codex runs should
  use JSON event output if exact turn counts are required.
- `entire-brain-mcp-tool-name` is useful as a harness smoke test, but it is not
  useful for proof because no-brain Codex solves it perfectly.
- Claude Code is now authorized and runs through the harness. On the saturated
  MCP smoke task, brain conditions increased turns/tokens, which is expected for
  a tiny task where context lookup overhead cannot pay back.
- `entire-cli-external-command-env-filter` is a better large-repo task, but
  no-brain Codex still finds the correct area and passes hidden validation.
- Live semantic indexing of `../cli` is too slow to rebuild per repetition with
  the current harness. The `entire-cli` task was adjusted to compare no-brain
  against full checkpoint-history brain without semantic indexing.
- Claude Code Max authorization removes the practical cost blocker. The harness
  leaves Claude uncapped by default; pass `--claude-budget <usd>` only when a
  hard cap is desired.

## Next Iteration Needed

To get real proof, add tasks where the brain changes available information, not
just navigation speed:

- history-dependent tasks that require recovering a prior design decision from
  checkpoint transcripts;
- multi-step tasks where the correct validation command is not obvious from
  file names alone;
- cross-repo tasks where workspace or checkpoint context points to the right
  repository boundary;
- tasks scored on search breadth and time to first relevant file, not only
  final pass/fail.

Do not claim a positive result until a combined report shows a positive delta
with enough repetitions and an acceptable p-value.
