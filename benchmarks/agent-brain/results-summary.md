# Agent Brain Benchmark Results

## Current Status

The harness now supports Phase 2 history-dependent tasks, runner matrixes,
stale-context setup, stricter agent isolation, and metric-level cost/efficiency
p-values. Isolated Phase 2 runs found a strong Codex correctness result and a
strong Claude Code cost/efficiency result on the same history-dependent task.

Isolation note: results produced before the isolation hardening should not be
mixed with new runs for final proof. New Codex runs ignore user config/rules and
use ephemeral sessions. New Claude runs disable session persistence, slash
commands, and non-empty MCP config. Host auth is still intentionally reused.

Scoring note: the score tables below were produced with the legacy v1 rubric,
which compressed most passing runs to 90 or 100. The harness now writes
`score.version = 2` with separate outcome, patch-focus, validation-discipline,
runtime-efficiency, and brain-use components. Rerun retained tasks before making
new score-based claims; existing pass/fail, time, token, cost, and changed-file
metrics remain useful.

Current isolated proof candidates:

| Suite(s) | Runner | Task | Condition | n | Success | Mean score | Mean seconds | Mean tokens | Mean turns | Mean cost | Key p-value |
|---|---|---|---|---:|---:|---:|---:|---:|---:|---:|---:|
| `phase2-isolated-schema-claude-codex-r3`, `phase2-isolated-schema-claude-codex-r2b` | `codex-medium` | `entire-brain-history-codex-schema-contract` | no brain | 5 | 0% | 35 | 93.5 | 398,583 | n/a | n/a | score p=0.000 |
| same | `codex-medium` | same | full brain | 5 | 100% | 100 | 97.5 | 521,472 | n/a | n/a | score p=0.000 |
| same | `claude-sonnet-low` | same | no brain | 5 | 60% | 74 | 109.6 | 837,649 | 14.2 | $0.357 | cost p=0.007 |
| same | `claude-sonnet-low` | same | full brain | 5 | 100% | 95 | 63.2 | 280,855 | 9.6 | $0.205 | tokens p=0.00012 |
| `codex-github-cli-semantic-pilot-20260601`, `codex-github-cli-repo-name-retained-r2-r5-20260601` | `codex` | `github-cli-repo-name-trims-dotgit` | no brain | 5 | 100% | 92 | 162.6 | 1,170,419 | n/a | n/a | retained n=4 score p=0.0027 |
| same | `codex` | same | semantic brain | 5 | 100% | 100 | 81.6 | 368,972 | n/a | n/a | retained n=4 tokens p=0.0010 |
| `codex-github-cli-semantic-pilot-20260601`, `codex-github-cli-http-scopes-retained-r2-r5-20260601` | `codex` | `github-cli-http-scopes-suggestion` | no brain | 5 | 100% | 90 | 129.2 | 789,505 | n/a | n/a | retained n=4 score p=0.0027 |
| same | `codex` | same | semantic brain | 5 | 100% | 98 | 96.3 | 558,858 | n/a | n/a | retained n=4 seconds p=0.045 |

Interpretation: with isolation enabled, Codex default-medium failed all five
no-brain runs and passed all five full-brain runs on the history-dependent
schema-contract task. Claude Sonnet low also improved success rate from 60% to
100%, but the score p-value is not yet significant at n=5 because three
baseline runs passed. Its efficiency signal is statistically strong: full brain
cut mean cost by 42%, tokens by 66%, and duration by 42%.

The GitHub CLI semantic candidates are the first repeated large-repo semantic
signals after the `entire-cli` indexing blocker was cleared. For
`repo-name-trims-dotgit`, semantic-brain made the one-file shared-normalizer fix
in every run, while no-brain often expanded into `repo/create` tests and code.
For `http-scopes-suggestion`, no-brain consistently added an extra API test file
and scored 90, while semantic-brain usually made the one-line production fix.
Both still need Claude and alternate Codex runner coverage before final claims.
A first Claude Code pilot did not retain the signal: Claude scored 100 in both
conditions on both GitHub CLI tasks, and semantic-brain increased time, tokens,
and reported cost at `n=1`.

Phase 2 proof candidate:

| Suite(s) | Runner | Task | Condition | n | Success | Mean score | Mean seconds | Mean tokens | Mean turns | Mean cost |
|---|---|---|---|---:|---:|---:|---:|---:|---:|---:|
| `phase2-claude-sonnet-low-schema*` | `claude-low` | `entire-brain-history-codex-schema-contract` | no brain | 5 | 100% | 100 | 121.9 | 603,864 | 21.0 | $0.348 |
| `phase2-claude-sonnet-low-schema*` | `claude-low` | `entire-brain-history-codex-schema-contract` | full brain | 5 | 100% | 95 | 55.9 | 261,855 | 8.4 | $0.205 |

Interpretation: Claude Sonnet low solved the task in both conditions, but the
full-brain condition cut mean cost by 41%, tokens by 57%, turns by 60%, and
agent time by 54%. The score delta is -5 because the full-brain runs touched a
minor extra expected-adjacent surface often enough to lose locality points; the
actual validation success rate stayed equal at 100%.

Codex candidate:

| Suite(s) | Runner | Task | Condition | n | Success | Mean score | Mean seconds | Mean tokens |
|---|---|---|---|---:|---:|---:|---:|---:|
| `phase2-codex-schema-rationale-pilot`, `phase2-codex-schema-r4` | `codex` | `entire-brain-history-codex-schema-contract` | no brain | 5 | 60% | 74 | 114.8 | 562,458 |
| `phase2-codex-schema-rationale-pilot`, `phase2-codex-schema-r4` | `codex` | `entire-brain-history-codex-schema-contract` | full brain | 5 | 100% | 100 | 82.1 | 474,187 |

Interpretation: Codex full-brain improved success rate, score, time, and tokens
on this task, but the approximate Welch p-value is 0.10 at n=5, so this is a
promising signal rather than statistical proof.

Earlier Phase 1 smoke status follows.

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
- Live semantic indexing of `../cli` is no longer the blocker for local
  benchmark runs. With `entire-sem` at commit `b3839c7`, `entire-sem snapshot
  --repo /Users/thomi/Projects/cli --format ndjson --no-network` completes in
  about 17 seconds, and full isolated `entire-brain index` completes in about
  29 seconds. The current artifact records 760 files, 9,130 symbols, 179,717
  stored relations, zero warnings, zero partial failures, and a roughly 152 MB
  SQLite store.
- The harness now has a `prep` command for brain-only verification/cache
  population, deterministic benchmark setup commits, and per-run plugin state
  under ignored `.benchmark/plugin/` inside the disposable worktree. This keeps
  cache-hit semantic freshness at `stale=ok` and lets sandboxed Codex runs open
  the semantic store.
- Semantic read commands no longer take the exclusive index lock. The old
  behavior made parallel agent calls to `query`, `context`, or `impact` fail
  with `index_locked`. A focused regression now verifies that read commands work
  while a lock file exists, while index/repair/changes/GC/bundle writers still
  keep the lock.
- `entire-cli` semantic pilots are mostly saturated. `review-base-flag-scope`
  retained a possible efficiency signal after the read-lock fix (score 90,
  1.05M semantic tokens vs 1.92M no-brain tokens at `n=1`), but
  `review-prompt-uncommitted-scope` and `transcript-reresolve-updates-state`
  were worse or saturated. `review-provenance-strip` also saturated for
  full-brain history: no-brain and full-brain both scored 90, with full-brain
  slower and more token-heavy.
- GitHub CLI semantic prep is reliable locally: all five semantic preps passed
  with `stale=ok`, 855 files, 6,401 symbols, 194,152 stored relations, zero
  warnings, and zero partial failures.
- Claude Code one-run pilots on the retained GitHub CLI tasks are saturated:
  no-brain and semantic-brain both scored 100 on `repo-name-trims-dotgit` and
  `http-scopes-suggestion`. For Claude, semantic context added overhead in
  these pilots rather than improving score.
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

## Phase 2 Expansion Queue

Superseded planning note: this section predates the 2026-06-02 Phase 2 reset.
Under the current map, Layer A is Entire-data project-native, Layer B is GitHub
CLI project-native, Layer C is SWE-bench-style, and model/effort/cost is a later
overlay rather than a layer.

New tasks added for the next run:

| Task | Layer | Target signal |
|---|---|---|
| `entire-brain-stale-query-default-limit` | project-native stale context | Brain-enabled agents should check freshness and avoid stale semantic answers. |
| `entire-cli-review-provenance-strip` | project-native checkpoint history | Full brain should recover the review/investigate provenance contract in a large repo. |
| `swe-style-entire-brain-stale-query-default-limit` | SWE-bench style local | Issue-only prompt with hidden validation and stale context setup. |

Expanded task inventory now includes 25 task definitions: 19 project-native
tasks and 6 local SWE-bench-style tasks. Project-native coverage is 8
`entire-brain`, 6 `entire-cli`, and 5 GitHub CLI tasks. The local SWE-style
tasks are still harness shakedown tasks; real SWE-bench Lite/Verified import is
the remaining Layer C gap under the corrected layer map.

The harness now caches brain prep artifacts under
`benchmarks/agent-brain/cache/`. This helps repeated semantic/full-brain runs
after prep completes. Four semantic `entire-cli` task preps now pass with
`stale=ok`, including cache-hit reuse.

The first resumed Codex smoke on `entire-cli-plugin-env-xdg-prefix` is still
saturated for correctness: no-brain and semantic-brain both passed with score
90 in one repetition. The retained repeated semantic candidates are currently
the GitHub CLI `repo-name-trims-dotgit` and `http-scopes-suggestion` tasks.
Next, run them across Claude Code and alternate Codex runner settings, then add
more validation-selection and stale-context tasks before importing real
SWE-bench Lite/Verified cases.
