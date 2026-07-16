# Agent Brain Benchmark Harness

This directory contains a repeatable harness for comparing Codex and Claude Code
with and without Entire Brain.

For unpaid product-path latency and packet-size profiling without an agent or
model, use `profile_brief.py` with the 114-task checked-in development corpus.
See `BRIEF-PROFILE-BASELINE.md`. That profile is explicitly not confirmatory and
does not measure code quality.

For an unpaid, same-query comparison of the MCP `brain_brief` `legacy_json` and
opt-in `compact_v1` packet formats, use `packet_format_ab.py`. See
`PACKET-FORMAT-AB.md`. It checks parseability, compact-packet integrity,
independently projected semantic parity, exact bytes, and a frozen offline token
proxy. Report schema 2 also retains numeric-only, exact structural byte
attribution for future packet-format design. It does not call an agent/provider,
measure code quality, authorize a paid trial, or change either MCP or CLI
defaults.

For the independently parsed `compact_v2` design, use
`packet_format_v2_ab.py`; see `PACKET-FORMAT-V2-AB.md`. It additionally checks
the in-band `~`/`^` legend, exact keyed schemas and types, same-opcode reference
scope, and the deterministic strictly-shorter per-family wire-form choice.
Its fixed 114-task byte/token-proxy gates and privacy/authorization limits are
the same as the v1 comparison. Report schema 2 also binds the exact runner,
corpus verifier, product contract, golden, schema/allowlist, legend, and gates
through a path-free hash-only runner identity.

## Treatment-isolated tasks

Confirmatory tasks use `user_query`, an explicit `retrieval_query_source` (`user_query` or
`oracle_queries`), and a `treatments` object keyed by condition. Treatment arms are `no_memory`,
`placebo_packet`, `retrieved_memory`, and the upper-bound-only `oracle_retrieval`. Comparable arms
must use the WIP harness-owned `frozen_brief` delivery; their agent-visible instruction is identical and differs only inside the
`<frozen-memory-packet>` payload. Oracle queries are harness-owned and never written to `prompt.txt`.

Legacy `prompt` and `brain_queries` tasks remain runnable for reproducing exploratory suites, but
legacy `frozen_brief` delivery may still consume `brain_queries` harness-side. Those queries are not
agent-visible, and legacy tasks fail confirmatory panel preflight. A confirmatory panel must set
`"confirmatory": true`; every included task also needs
an `approved_symptom_only` entry in `task-review-ledger.json`. Automated task-validity lint is triage,
not human approval. The historical 8828752a7 and 4dd458656 prompts are retained under
`fixtures/task-validity/` and explicitly excluded as oracle-assisted.

Run deterministic triage and validate the review ledger without an agent/model call:

```sh
python3 run.py lint-tasks --tasks fixtures/task-validity/oracle-assisted-regressions.json
```

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

## Order, cache, resume, and timing controls

`run` and `panel` build every requested cell before execution. The default
`--order-policy counterbalanced` uses seeded cyclic Latin-square rows inside each
task/runner block (`AB`/`BA` for two arms); `--schedule-seed` selects the
deterministic schedule. `--order-policy latin_square` explicitly selects the same
Latin-square construction for preregistrations that name it that way. Unsupported
order policies fail before suite setup.

The immutable plan is written to `schedule.json` before cache setup, tool builds,
or agent calls. `actual-order.ndjson` records actual starts, finishes, and explicit
deviations, while `schedule-state.json` gives the current planned-versus-actual
view. Resume a named interrupted suite with the identical arguments plus
`--resume`. A cell with a start event but no durable record is considered
ambiguous and is not rerun; the resulting imbalance is recorded as
`interrupted_incomplete_not_retried`.

The conservative default `--cache-policy isolated_per_cell` assigns separate
`GOCACHE`, `GOMODCACHE`, and retrieval-vector cache paths to every cell and never
inherits those host paths. `--cache-policy prewarmed_shared` performs an untimed
deterministic `go mod download` prewarm and shares the suite cache paths. Cache
path identities, prewarm commands/durations, host load/concurrency/power context,
and resume invocations are retained in `runtime-controls.json`. This runtime
policy is separate from the historical Brain-prep cache flags
`--no-brain-cache`/`--refresh-brain-cache`.

The primary time field is `timing.harness_agent_interval_wall_seconds`: monotonic
harness wall time immediately around the agent CLI, including declared transient
retries/backoff but excluding setup and validation. The harness also records
`timing.agent_reported_api_seconds` when the provider CLI exposes it,
`timing.cell_setup_wall_seconds`, and `timing.cell_total_wall_seconds`; missing
provider timing remains null and is never replaced silently.

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
- `agent_info.seconds` for legacy harness wall-clock agent duration, plus the
  explicit `timing` fields defined above.
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
- `mcp_semantic`: local `entire brain mcp` semantic graph tools only
  (`brain_status`, `brain_context`, `brain_impact`, `brain_changes`,
  `brain_code`); unified `brain_query` retrieval is intentionally excluded from
  this condition.
- `mcp_history`: local `entire brain mcp` with `brain_brief` and indexed-history
  retrieval; no history excerpt file is provided as a shortcut.
  > Note: this condition uses `brain_brief` plus unified `brain_search` /
  > `brain_query` retrieval. The old dedicated `brain_history` tool was removed;
  > history is now one source within the unified retrieval verbs.

Temporal-memory ablation conditions are separate from the product-style
full-brain conditions:

- `raw_history`: indexed records derived directly from a pinned pre-cutoff
  session bundle;
- `facts_only`: durable facts distilled from that same bundle;
- `history_facts`: the exact indexed history and exact fact artifact together.

These conditions require a task-level `memory_bundle` with an exact checkpoint
ref commit, cutoff, session variant, retrieval branch, and pinned distillation
configuration. The harness builds one unisolated source cache and derives all
three deliveries from copies of it, so stochastic distillation cannot differ by
condition. Raw transcripts, checkpoint refs, semantic context, seed context,
docs, and patterns are removed before the task agent starts. See
`temporal-memory/README.md` for the Phase 0A development protocol.

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

When `hide_validation_from_agent` is true, tasks may set `leak_markers` to
high-entropy canaries that should never appear in agent output. If omitted, the
harness falls back to treating the hidden validation commands themselves as leak
markers, which is intentionally strict but can false-flag agents that
independently discover the same focused test command.

Tasks that do not exercise the benchmark harness itself may set
`agent_hidden_paths`, for example `["benchmarks/agent-brain"]`, to remove extra
local scaffolding from the disposable worktree before the agent runs.

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

The confirmatory v2 code-quality endpoint is deliberately narrower than this
exploratory utility score: it normalizes only output `outcome` and `patch_focus`
points. `validation_discipline` (whether the agent ran tests or checked its
diff), runtime efficiency, token use, and brain-use behavior remain diagnostics
and cannot improve confirmatory quality.

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
leads until a retained subset is audited and committed under
`benchmarks/agent-brain/evidence/release`. For release evidence, run:

```sh
python3 benchmarks/agent-brain/audit_codex.py \
  --results <retained-results-dir> \
  --out-dir <proof-output-dir> \
  --suite-glob '<suite-pattern>' \
  --fail-on-flags \
  --min-suites <n> \
  --min-records <n> \
  --min-proof-ready <n>
```

The repo-level release gate is:

```sh
mise run release:evidence
```

That gate reads `benchmarks/agent-brain/evidence/release/manifest.json`,
audits only suite directories matching `release-candidate-*`, rejects
`release-local-*`, and enforces the manifest thresholds: currently 2 suites, 16
records, 2 proof-ready comparisons, 4 repetitions per side, panel provenance,
zero hard flags, at least one proof-ready comparison per retained suite, and
required proof scopes `history` and generic `mcp`.

The release evidence manifest lives at
`benchmarks/agent-brain/evidence/release/manifest.json`; it records the citable
suite pattern and minimum proof thresholds. Do not cite benchmark results from
outside that retained lane.

Audit mode treats missing or malformed provenance as a hard integrity flag. A
release audit must therefore be able to show, per record, which harness revision,
source base/head commits, task config hash, runner/config fingerprint, and tool
hashes produced the result. Ignored `results/` panels remain
quarantine/reference evidence unless copied or regenerated into the
provenance-complete retained lane under
`benchmarks/agent-brain/evidence/release` and passing `mise run
release:evidence`.

For a SWE-bench-style matrix, tag tasks with `source` and `suite_tags`. The
current harness already supports the essential SWE shape: issue prompt,
regression setup patch/replacements, hidden validation, disposable worktree, and
per-run artifacts. External SWE-bench tasks should be imported as local task
JSON files with fixed base commits and cached repositories before large runs.

The harness intentionally keeps generated brain artifacts and worktrees out of
the repository. Result directories are ignored by git.

Rolling-cutoff frozen-brain tasks constrain temporal eligibility before lexical
or semantic top-K ranking. See [TEMPORAL-ELIGIBILITY-DESIGN.md](TEMPORAL-ELIGIBILITY-DESIGN.md)
for the fail-closed provenance, immutable-cache, and audit-field contract.
