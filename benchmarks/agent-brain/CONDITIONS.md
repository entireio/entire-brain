# Entire Brain Benchmark Conditions

This document is the normative contract for Agent Brain benchmarks. It defines
what each agent may see and use, how implementation comparisons must be run,
and what evidence makes a result valid. If another benchmark document is
ambiguous, follow this one and update the drift.

The central comparison is simple:

- **no-Brain** is a normal coding agent with its normal tools, but without
  `entire`, `entire-graph`, `entire-brain`, or any Entire-managed memory.
- **Brain** is the same agent with the same normal tools plus the Brain surface
  selected by the condition.
- **Neither variant may use information from a previous benchmark run.**

Git history is a normal coding tool. `git log`, `git show`, `git blame`,
`git diff`, and other ordinary Git commands are allowed in both variants.
The harness represents its prepared workspace as a synthetic merge whose first
parent has the same tree. Agents must not explicitly diff the synthetic second
parent or request merge-parent patches: that reconstructs harness setup rather
than inspecting ordinary source history.

## Non-negotiable rules

1. Use real coding agents on real repository tasks. Do not replace an agent
   session with a synthetic caller, an internal retriever invocation, or a
   benchmark-specific answer generator.
2. Give both variants the same task, repository state, normal tools, runner,
   model, effort, timeout, and validation.
3. Keep prior benchmark prompts, patches, answers, logs, summaries, worktrees,
   and agent sessions out of both variants.
4. Require the Brain variant to use the configured Brain surface. A row where
   the agent ignores Brain is invalid, even if the patch passes.
5. Keep no-Brain free of every Entire-family tool and every Entire-managed
   memory source.
6. Use current mainline. Entire Brain has no released product baseline, so an
   old checkout or old Brain binary is never an implementation control.
7. Audit the raw agent protocol log. A passing validator or aggregate score
   cannot prove condition compliance.
8. Treat validation pass rate and measured token use as the headline metrics.
   Report wall-clock agent time and the component score, but do not let the
   composite score conceal a validation failure.

## Condition access matrix

| Capability | no-Brain | Brain |
|---|---:|---:|
| Read and edit the task repository | Yes | Yes |
| Shell, source search, builds, tests, and language tools | Yes | Yes |
| Ordinary Git history, including `log`, `show`, and `blame` | Yes | Yes |
| Other normal tools exposed to the agent, including web access | Yes | Yes |
| `entire`, `entire-graph`, or `entire-brain` in any form | **No** | Yes |
| Brain MCP tools | **No** | When selected by the condition |
| Entire checkpoint refs, `.entire`, plugin stores, or derived memory | **No** | Only through the selected Brain surface |
| Files or information from previous benchmark runs | **No** | **No** |
| Host agent conversation/session memory | **No** | **No** |

“Ordinary Git history” means the task repository's source history. It does not
include mining Entire-managed checkpoint refs or reconstructing the Brain
treatment through `.entire` data in a no-Brain run.

## no-Brain

The no-Brain agent should behave like an ordinary agent joining the repository
without Entire memory. It may inspect source, search broadly, read documentation,
run tests, use Git history, and use any other normal tool available to the
runner.

It must not:

- invoke `entire`, any `entire brain` or `entire graph` subcommand, the
  `entire-brain` or `entire-graph` binaries, aliases, wrappers, or equivalent
  MCP tools;
- read `.entire`, Entire checkpoint refs, Brain plugin data, a copied Brain
  store, or files staged for Brain intake;
- read `benchmarks/agent-brain`, benchmark task definitions, hidden validators,
  result directories, caches, prior prompts, patches, records, reports, or
  agent logs;
- recover prior benchmark answers from host agent history, rules, skills,
  project instructions, shell history, or persisted sessions.

The harness removes benchmark scaffolding and visible Entire history from the
agent worktree, disables MCP, and runs the agent in an ephemeral session. The
raw log must still be checked because isolation is an enforced boundary, not an
assumption.

## Brain

The Brain agent starts with everything allowed to no-Brain and adds exactly the
Brain surface named by its condition. It does not lose Git history, source
search, tests, or any other normal coding tool.

For direct-CLI product conditions, the agent uses the documented JSON surface:

- `semantic_brain` / `semantic_cli`: begin with `entire brain brief ... --json`
  and use semantic follow-ups only when the task needs them;
- `semantic_history_brain`, `full_cli_original`, and `full_cli_compact`: begin
  with `entire brain brief ... --json`; these conditions contain semantic/seed
  context plus indexed session history, but intentionally contain no distilled
  facts;
- `full_brain` is reserved for a prepared Brain that also contains at least one
  distilled durable fact; the harness fails closed if that fact source is absent;
- ask the smallest task-driven follow-up rather than touring the command
  surface.

The required initial `brief` receives the task prompt exactly as the agent
received it. Task `brain_queries` are optional follow-up hints only; the harness
must not append them to the brief query, mandate one as an exact search, or
otherwise turn normal Brain use into a synthetic caller expansion.

For MCP conditions, use the configured local `entire brain mcp` tools:

- `mcp_semantic` exposes semantic graph tools;
- `mcp_history` exposes brief plus unified history retrieval;
- `mcp_workspace_radar` exposes the task's configured workspace/review surface.

Temporal-memory conditions (`raw_history`, `facts_only`, `history_facts`) use
only their preregistered memory bundle and delivery lane. They must not acquire
semantic, transcript, checkpoint, document, or fact sources outside that
treatment.

Every Brain row must contain structured protocol evidence of a real Brain call.
Prompt text, a prepared Brain store, or a soft score bonus is not proof of use.

Brain runs remain forbidden from reading previous benchmark runs. The Brain
store for the current row may be built from allowed repository inputs or
restored from a deterministic prep cache keyed by the exact source, setup,
condition, and tool hashes. That cache must never contain a prior task agent's
prompt, patch, answer, transcript, or result.

## Real-agent and task-design requirements

Benchmark tasks must exercise the same product surface an agent would use in
normal development. Focused unit tests may protect a product fix, but they are
not substitutes for the agent session.

Task authors must:

- use a real regression in a real repository;
- apply the same setup mutation in every compared arm;
- keep the prompt task-shaped and avoid revealing the answer;
- hide expected values, exact files, exact symbols, and validators when those
  are what Brain is supposed to recover;
- use validation that works unchanged on every compared implementation;
- avoid branch-only constants, files, APIs, or test names;
- avoid benchmark-specific answer paths in product code;
- keep task IDs and source commits disjoint across development and holdout
  splits;
- declare any stale-context mutation explicitly with
  `post_brain_replacements` or `post_brain_commands`.

A task that cannot be applied and validated identically on current main and the
candidate is not a branch-versus-main benchmark.

## Isolation from previous runs

Before every agent session, the harness must:

- create a new disposable worktree;
- remove `benchmarks/agent-brain` and any other `agent_hidden_paths`;
- retain ordinary source Git history after filtering benchmark-private and
  Entire-managed paths from every visible revision;
- start a fresh, non-persistent agent session with user rules and unrelated MCP
  servers disabled;
- remove or sanitize captured benchmark sessions and history records;
- expose only the current row's prompt and condition treatment;
- keep result directories and prior worktrees outside the agent-visible tree.

The prohibition is semantic as well as path-based. Renaming or copying a
previous `record.json`, summary, patch, or transcript does not make it valid
input.

## Fair branch-versus-main implementation comparisons

There are three different comparisons:

1. **Condition ablation within one implementation.** no-Brain and Brain use the
   same freshly built Brain implementation; only Brain availability changes.
2. **Candidate versus main.** The candidate and current main use the same
   harness revision, task JSON, source-root snapshot, setup mutation, runner,
   model, effort, condition, repetition count, timeout, and validation. The
   Brain implementation is the only intended difference.
3. **Feature ablation within one implementation.** Both arms use the same
   Brain binary, Brain condition, prepared index, source snapshot, task,
   runner, model, effort, timeout, and validation. Exactly one recorded feature
   flag differs. For the action-checklist ablation, set
   `ENTIRE_BRAIN_ACTION_CHECKLIST=1` or `0` explicitly in both arms and require
   that value in `provenance.run_config.env_flags`. The product default is off;
   an ablation must never infer an arm from the unset default.

For candidate-versus-main runs:

- fetch `origin/main` immediately before the run;
- require the candidate `HEAD` to contain that fetched main;
- build the candidate Brain from the candidate checkout;
- build the control Brain from an exact detached checkout of fetched main;
- use one shared, frozen task-source root for both implementations;
- use the candidate's benchmark harness for both implementations so parser,
  scoring, isolation, and audit fixes do not drift;
- verify identical task-config hashes, runner fingerprints, source commits, and
  setup commits in the resulting records;
- never use an older WIP checkout, an old benchmark binary, or the last
  previously benchmarked commit as “main.”

If the benchmark harness itself changed, overlay that exact harness revision on
the detached main product checkout and record both the harness commit and the
main product/source commit. Do not silently compare two harness versions.

## Required raw-session audit

Inspect `agent.stdout`, `agent.stderr`, and `record.json` for every row. A row is
accepted only when all applicable checks pass:

- the agent ran successfully and every validator passed;
- source, harness, task, runner, and tool provenance is present and matches the
  intended comparison;
- the harness and source snapshots were clean;
- no benchmark-private or previous-run artifact was accessed;
- no-Brain invoked no Entire-family CLI, binary, wrapper, or MCP tool;
- Brain made a real, structured Brain call through the required surface;
- a preregistered temporal-memory adherence lane used its frozen command first;
- changed files are focused and contain no benchmark or test tampering;
- the agent inspected its diff and ran proportionate tests;
- token and duration metrics came from the agent protocol record;
- infrastructure failures are excluded rather than scored as agent failures.

Report per-task rows. Do not hide a regression inside an aggregate.

## Run tiers and claims

Use the smallest tier that answers the question:

1. **Implementation canary:** one or a few real tasks while the design is still
   changing. Do not make broad benchmark claims.
2. **Full one-pass comparison:** every retained task once with a pinned runner.
   This finds coverage regressions and gives directional quality, token, and
   speed evidence. It is not a stability claim.
3. **Stability panel:** the committed panel, pinned runners, at least four
   repetitions, CV, significance, and drop-one checks. Use this for
   repetition-robust claims.

The canonical nine-task manifest is `panels/full.json`. A one-pass run should
state explicitly that it overrides the panel's proof-level repetitions. A
proof run uses:

```sh
python3 benchmarks/agent-brain/run.py panel full
```

Before publishing any result, state:

- the candidate and main commits;
- harness and task-config provenance;
- runner, model, effort, conditions, and repetition count;
- validation outcome, quality score, tokens, and agent seconds per task;
- whether every raw session passed this condition audit;
- whether the evidence is a one-pass direction or a stability-panel result.
