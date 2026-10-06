# Memory ladder

A small, reproducible comparison of what a coding agent can answer about a
repository's past as you add memory surfaces one step at a time:

| Cond | Agent has | Guide the agent receives |
|---|---|---|
| 1 | source tree and git history | plain note; no Entire tooling |
| 2 | + hosted session history (`entire search`, `entire checkpoint explain --full`) | plain note + session section |
| 3 | + Graph (`entire graph query`, `neighbors`, `impact`) | `entire graph agent-guide` rendered outside a repository (standalone Graph) + session section |
| 4 | + Brain, normal guidance | `entire graph agent-guide` rendered inside this repository (combined Graph and Brain) + session section |
| 5 | + Brain, strict guidance | this repository's committed `.entire/agent-guide.md` + session section |

Every condition runs the same prompt against a fresh clone with the same
read-only tool allowlist. The harness directory itself (prompt, rubric, this
README) is removed from the clone's worktree and HEAD tree before the agent
starts, so it cannot read its own answer key. The clone's HEAD is therefore one
commit past the indexed commit, and Brain's freshness label moves from
"degraded" to "unsafe"; its facts and history sections are unchanged by that
(checked: 3 facts and 3 history matches before and after). A shim first on `PATH` decides which `entire`
subcommands exist, so an agent cannot reach a surface its condition excludes,
and lifecycle hooks are swallowed so nested runs never capture checkpoints. The
guides are generated from the product at run time, not checked in.

The question is the kind a product manager asks, not an engineer: "should we
auto-activate Brain on global install, and was this decided before?" Its
recorded answer exists only in agent session history and as a Brain
closed-negative fact, not in code, docs or commit messages.

## Run it

Requirements: `claude` (Claude Code CLI), `entire` logged in (hosted search needs
it), `entire graph` and `entire brain` plugins installed, a clean checkout.

```sh
benchmarks/memory-ladder/run-all.sh            # 3 reps x 5 conditions, sequential, then grade
benchmarks/memory-ladder/run.sh 2 1            # one condition, one rep
python3 benchmarks/memory-ladder/grade.py      # table of runs and per-condition means
```

Knobs: `LADDER_MODEL` (default `sonnet`), `LADDER_MAX_TURNS` (80), `LADDER_OUT`
(default `runs/`, gitignored), `LADDER_ORIGIN` (clone origin URL; Brain and
Graph derive the repository key from it, so keep the GitHub URL), `LADDER_BRANCH`
(default `main`; Brain scopes facts to the checked-out branch, so a clone of a
feature branch sees no facts at all). Graph prewarm
for conditions 3 and above is excluded from timing and recorded in `prewarm.txt`.

Each run leaves `stream.jsonl` (the full agent transcript), `summary.json`
(metrics, completion state, every shell command, the final answer, the clone's
HEAD) and the clone it ran in. `grade.py` averages completed runs only and lists
failed or capped runs separately.
Add a question by dropping `questions/<name>.txt` and
`questions/<name>.rubric.json`; the rubric is named regexes over the final answer
and its `recovered` key is a first pass that should be hand-checked.

## What the first run found (2026-10-06)

Three repetitions per condition, Sonnet, this repository at `main`. "Recovered"
means the answer stated the recorded decision and its reason, not an inference
from the resulting rule.

| Cond | Recovered | Mean input tokens |
|---|---|---|
| 1 code + git | 0/3 | 303k |
| 2 + sessions | 3/3 | 203k |
| 3 + Graph | 0/3 (1 partial) | 224k |
| 4 + Brain, normal | 0/3 (2 partial) | 219k |
| 5 + Brain, strict | 1/3 | 487k |

Brain retrieved the decisive fact and the decisive session line in the brief on
every run. The agent did not see them: `brief --json` leads with a ~12.8 KB
status block and puts `facts` and `history` last, and every agent capped the
output before reaching them. `query` returned only `doc` results for natural
phrasings while `recall` ranked the fact first. Tracked in
[#335](https://github.com/entireio/entire-brain/issues/335). Re-run this ladder
after that fix before claiming Brain lift over plain session access.

## Caveats

- One question, one repository, one model, n=3. Treat it as a probe, not a
  benchmark; the claim gate in `docs/benchmarks.md` does not apply to it.
- Conditions 2 to 5 depend on the hosted search service, so results vary with
  what has been synced. Condition 1 has no network surface.
- Sessions spent building or running this harness enter the hosted history and,
  once distilled, the Brain, and they discuss the answer. Later runs of the same
  question can find them. Check `summary.json` commands for hits on those
  sessions, or ask a fresh question whose answer has not been discussed.
- The first run executed arms in parallel; `run-all.sh` runs them sequentially
  per the recorded benchmark convention. Re-check timing before quoting it.
