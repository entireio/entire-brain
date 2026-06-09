# Agent Brain Benchmark Harness

This directory contains a repeatable harness for comparing Codex and Claude Code
with and without Entire Brain.

## Stable panel (`panel`) + the stability gate

"Is benchmarking stable?" is answered with a **committed panel manifest** plus a **printed
per-task verdict**, not ad-hoc flags:

```sh
python3 run.py panel full          # runs panels/full.json, prints a stability verdict per task/condition
```

A panel manifest (`panels/full.json`) is the run config as data: the retained task list, **pinned**
`agent:model:effort` runners (unpinned runners are rejected in preflight — defaults drift and aren't
visible in artifacts), the conditions, and `repetitions` (>=4). `panel` preflights the manifest, then
expands it into the existing `run` code path — no new run mechanics.

The **stability gate** (in `summarize`, so `run`/`panel`/`report` all show it) tags each
(task, condition vs `no_brain`) comparison:

- `brain_positive_stable` — a real, repetition-robust win: a token reduction significant at p<0.05
  **and** surviving the drop of the single best/worst rep, **or** a pass-rate lift that survives the
  same drop-one test.
- `saturated` — both arms already pass 100% (no quality headroom; only efficiency can differ).
- `noisy` — a delta exists but isn't significant / doesn't survive drop-one.

Each comparison also carries the coefficient of variation for tokens and score, for **both** the
condition and the baseline arm (`..._condition` / `..._baseline`). **Honesty note:** agent
sampling is inherently non-deterministic; the harness-controllable variance (base commit, setup commit,
semantic cache, parentless baseline) is already pinned, so stability comes from **repetitions + CV +
drop-one**, not a fake seed. The gate can only *downgrade* a result to saturated/noisy — it never
manufactures significance — and the headline stays validation pass-rate + measured tokens.

## Reproducing on another machine (portable paths)

Task `repo_path` values are resolved portably so the suite runs without editing
hard-coded home paths:

- `~` and `$VARS`/`${VARS}` are expanded.
- A **relative** `repo_path` is resolved against `$AGENT_BENCH_REPO_ROOT`
  (default: the parent directory of this repo). The bundled tasks assume a
  sibling layout: e.g. `repo_path: "cli"` → `<repos>/cli`, `repo_path: "../Ultron"`
  → `<repos>/../Ultron`. Set `AGENT_BENCH_REPO_ROOT=/path/to/your/repos` to point
  elsewhere, or use an absolute `repo_path`.
- `path_prefix: "auto"` resolves to the directory of the host `node` (so the
  tsx-based validations work without a hard-coded node path).
- `setup_commands` get `$BENCH_SOURCE_REPO` = the resolved source repo path.

Caveat: the Ultron **session-history** scenarios (`mcp_history`, `full_*`) need a
repo that actually has Entire `.entire` session data; that data is machine-local
and not committed. The semantic scenarios (`semantic_brain`, `mcp_semantic`) only
need the source code and reproduce anywhere (e.g. `entireio/cli`).

Each task creates a disposable git worktree, applies a known regression patch,
commits that setup state, runs an agent, validates the fix, scores the run, and
writes artifacts under `benchmarks/agent-brain/results/`.

Tasks may also define `post_brain_replacements` or `post_brain_commands`. Those
mutations are applied and committed after brain preparation, which creates a
stale-context scenario for semantic and full-brain runs. Use these tasks to
measure whether agents check brain freshness before relying on prepared context.

Brain prep artifacts are cached under `benchmarks/agent-brain/cache/` by
repo/base/setup/condition/tool hash. Each run receives its own copy of the
cached plugin directory under the disposable worktree's ignored
`.benchmark/plugin/` path, with text artifacts rewritten to the current
disposable worktree path. Use `--refresh-brain-cache` to overwrite a cache entry
or `--no-brain-cache` to force per-run rebuilds. The cache avoids repeated prep
after a brain has been built successfully; it does not fix slow or incomplete
initial semantic indexing.

Use `prep` to verify and cache brain artifacts without launching an agent:

```sh
python3 benchmarks/agent-brain/run.py prep \
  --tasks entire-cli-plugin-env-xdg-prefix.json \
  --conditions semantic_brain \
  --suite-name entire-cli-semantic-prep
```

Prep records include semantic manifest counts, artifact sizes, stale status,
generation metrics, command durations, and cache hit/miss metadata.

Each `record.json` includes:

- `provenance` with the harness HEAD, source repo HEAD, source base commit used
  for the archived worktree, task config hash, runner/config fingerprint, and
  tool binary hashes. If a task omits `base_commit`, the recorded source base
  must match the recorded source HEAD. If a task pins `base_commit`, the base
  commit is recorded separately from source HEAD.
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

MCP-specific conditions are separate from CLI/context-file delivery:

- `semantic_cli`: CLI-delivered semantic brain, equivalent to the original
  `semantic_brain` condition.
- `full_cli_original`: original full-brain delivery with the generated
  `.benchmark/brain-history-excerpt.md` file.
- `full_cli_compact`: full Brain prep delivered through `brain brief` only:
  compact history hits, likely files/tests, and action checklist; no raw
  history excerpt file.
- `mcp_semantic`: local `entire brain mcp` semantic tools only.
- `mcp_history`: local `entire brain mcp` with `brain_brief` and indexed-history
  retrieval; no history excerpt file is provided as a shortcut.
  > Note: this condition uses `brain_brief` plus unified `brain_search` /
  > `brain_query` retrieval. The old dedicated `brain_history` tool was removed;
  > history is now one source within the unified retrieval verbs.

For a fast MCP session-history smoke on the local Ultron repo:

```sh
python3 benchmarks/agent-brain/run.py run \
  --tasks ultron-history-youtube-media-verification.json \
  --runners claude:sonnet:max \
  --conditions no_brain,mcp_semantic,mcp_history \
  --repetitions 1 \
  --suite-name ultron-mcp-history-smoke
```

For Phase 2 candidate screening, do not spend repetitions on tasks that are
already easy without the brain. Run one no-brain pilot first and stop the task
when that score is above the retention threshold:

```sh
python3 benchmarks/agent-brain/run.py run \
  --tasks entire-brain-history-claude-seed-agent.json \
  --runners claude \
  --conditions no_brain,full_brain \
  --repetitions 1 \
  --stop-after-no-brain-score 90 \
  --suite-name phase2-layer-a-pilot
```

Retain a scenario for proof only when no-brain is not already above 90, or when
the brain condition is demonstrably cheaper or faster at equivalent correctness.
Scenario prompts should withhold exact file names, test names, and prior
rationale from no-brain runs when those facts are supposed to come from
checkpoint/session history.

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

Reports include utility-score deltas, success rates, mean agent seconds, total
tokens, turns, cost when available, and approximate Welch p-values for score,
duration, tokens, turns, and cost. The score is not a pure correctness or model
quality metric: pass/fail validation is primary, while `score.version = 2`
combines separate `outcome`, `patch_focus`, `validation_discipline`,
`runtime_efficiency`, and `brain_use` components. A lower-effort runner can
therefore score above a higher-effort runner when both solve the task but the
lower-effort run is faster or cheaper. Do not compare v1 and v2 score means
directly; rerun retained tasks after a scoring change.

Phase 2 scenario discovery is generated with `discover`:

```sh
python3 benchmarks/agent-brain/run.py discover \
  --minimum-per-layer 21 \
  --suite-name phase2-discovery
```

Discovery writes a committed scenario ledger under
`benchmarks/agent-brain/discovery/<suite>/` with more than 20 brain-positive
candidates for each benchmark layer. The current Phase 2 layer map is:

- Layer A: project-native tasks in repos with Entire session data:
  `entire-brain` and `entire-cli`.
- Layer B: project-native GitHub CLI tasks.
- Layer C: SWE-bench-style issue tasks.

Older discovery ledgers used the obsolete map where Layer C was
model/effort/cost. Treat those ledgers as superseded until regenerated under
the current layer map. Model/effort/cost runs are a later overlay on retained
A/B/C scenarios, not a Phase 2 layer.

The discovery ledger is not the same as statistical proof. It records why each
scenario should favor the brain, which brain source should matter, which metric
should retain the scenario, and which repetitions are needed. Local
`results/*/summary.json` files are ignored working artifacts; treat them as
leads until a retained subset is audited and committed. For release evidence,
run:

```sh
python3 benchmarks/agent-brain/audit_codex.py \
  --results <retained-results-dir> \
  --out-dir <proof-output-dir> \
  --suite-glob '<suite-pattern>' \
  --fail-on-flags
```

Audit mode treats missing or malformed provenance as a hard integrity flag. A
release audit must therefore be able to show, per record, which harness revision,
source base/head commits, task config hash, runner/config fingerprint, and tool
hashes produced the result.

For a SWE-bench-style matrix, tag tasks with `source` and `suite_tags`. The
current harness already supports the essential SWE shape: issue prompt,
regression setup patch/replacements, hidden validation, disposable worktree, and
per-run artifacts. External SWE-bench tasks should be imported as local task
JSON files with fixed base commits and cached repositories before large runs.

The harness intentionally keeps generated brain artifacts and worktrees out of
the repository. Result directories are ignored by git.
