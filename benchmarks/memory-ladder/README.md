# Memory ladder

A small, reproducible comparison of what a coding agent can answer about a
repository's past as you add memory surfaces one step at a time:

| Cond | Agent has | Guide the agent receives |
|---|---|---|
| 1 | source tree and git history | plain note; no Entire tooling |
| 2 | + hosted session history (`entire search`, `entire checkpoint explain --full`) | plain note + session section |
| 3 | + Graph (`entire graph query`, `neighbors`, `impact`) | `entire graph agent-guide` rendered outside a repository (standalone Graph) + session section |
| 4 | + Brain, normal guidance | Brain's `agent-guide --normal` rendered inside this repository (combined Graph and Brain) + session section |
| 5 | + Brain, strict guidance | this repository's committed `.entire/agent-guide.md` + session section |

Every condition runs the same prompt against a fresh clone with the same
read-only tool allowlist. The harness directory itself (prompt, rubric, this
README) is removed from the clone's worktree and HEAD tree before the agent
starts, so it cannot read its own answer key. Once the harness has merged it still
exists in history, so the agent's `git` is also a shim (`bin/git`): read
subcommands only, any argument naming the directory refused, and a pathspec
exclusion appended to history and content readers so `git show <sha>` and
`git log -p` cannot print it even for the commits that added it. `parse.py`
records any probe for the directory and marks a run contaminated if a tool
result carries rubric or README content; `grade.py` excludes those. The clone's
HEAD is one commit past the indexed commit, and Brain's freshness label moves from
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

The agent is confined to its clone. Measured against this Claude Code build:
Bash file commands (`cat`, `find`, `ls`, `grep`) on a path outside the working
directory are blocked by the runtime; the Read tool is not, so it is granted only
as `Read(./**)`; the Grep and Glob tools are not path-scoped by permission rules,
so they are not granted and the agent uses `grep`, `rg`, `find` and `ls` through
the sandboxed shell; the Agent tool launches regardless of the allowlist and
cannot be scoped to one subagent type, so it is denied (subagents inherited the
parent's rules when measured, and no run so far launched one). Runs land in `$TMPDIR/memory-ladder-runs` by default and
`run.sh` refuses an output directory inside this repository, so the clone never
sits a relative path away from the un-stripped harness. `parse.py` records every
tool input that names a path outside the clone and marks a run contaminated if
one reaches the source repository.

Knobs: `LADDER_BRAIN_BIN` (an `entire-brain` binary to run instead of the
installed plugin for conditions 4 and 5, for example a build from `main`; it
reads the installed store and also renders condition 4's guide), `LADDER_MODEL`
(default `sonnet`), `LADDER_MAX_TURNS` (80), `LADDER_OUT`
(default `$TMPDIR/memory-ladder-runs`, must be outside this repository), `LADDER_ORIGIN` (clone origin URL; Brain and
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

## Questions

- `global-activation`: should global plugin install auto-activate Brain in every
  repository, and was that decided before? Recorded answer: tried, called a
  regression, reversed; lives in one session and one closed-negative fact.
- `test-first-checklist`: should the brief's action checklist put the exact
  failing test first? Recorded answer: tried, rejected after it cost about 31%
  more tokens and 27% more time under semantic tolerance; lives in one session
  (deep in a 16k-line transcript) and one closed-negative fact. Chosen for the
  second run because the first question had been discussed in sessions by then.

## Run 2 (2026-10-07): after the #335 fix, question `test-first-checklist`

Brain built from `main` at the merge of #337 via `LADDER_BRAIN_BIN`; three
repetitions; Sonnet.

| Cond | Recovered | Mean duration | Mean input tokens | Mean tool calls |
|---|---|---|---|---|
| 1 code + git | 0/3 | 24 s | 179k | 9 |
| 2 + sessions | 0/3 | 38 s | 213k | 8 |
| 3 + Graph | 0/3 | 52 s | 224k | 11 |
| 4 + Brain, normal | **3/3** | 26 s | 160k | 7 |
| 5 + Brain, strict | **3/3** | 115 s | 260k | 11 |

Every normal-guide Brain run called `recall` first and cited the fact with its
session, checkpoint and transcript line. Sessions-only runs found the right
session through hosted search and explained five other checkpoints from it, but
never the one carrying the measurement, and none read a transcript with
`--full`; two said so. Conditions 1 and 3 reported no evidence, correctly. The
strict guide reached the same answer at four times the wall clock, mostly
preflight and Graph impact calls.

## Run 1 (2026-10-06): before the fix, question `global-activation`

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
every run. The agent did not see them: `brief --json` led with a ~12.8 KB
status block and put `facts` and `history` last, and every agent capped the
output before reaching them. `query` returned only `doc` results for natural
phrasings while `recall` ranked the fact first. Tracked in
[#335](https://github.com/entireio/entire-brain/issues/335); the brief order and
guide text were fixed in #337, which run 2 measures. `query` ranking is still
open.

## Caveats

- One question, one repository, one model, n=3. Treat it as a probe, not a
  benchmark; the claim gate in `docs/benchmarks.md` does not apply to it.
- Conditions 2 to 5 depend on the hosted search service, so results vary with
  what has been synced. Condition 1 has no network surface.
- Sessions spent building or running this harness enter the hosted history and,
  once distilled, the Brain, and they discuss the answer. Later runs of the same
  question can find them. Check `summary.json` commands for hits on those
  sessions, or ask a fresh question whose answer has not been discussed. Run 2
  used a new question for exactly this reason; check a candidate with hosted
  `entire search` and a repo grep before using it.
- Brain scopes facts to the checked-out branch. A clone of a feature branch sees
  no facts; `run.sh` clones `main` for that reason.
- The first run executed arms in parallel; `run-all.sh` runs them sequentially
  per the recorded benchmark convention. Re-check timing before quoting it.
