# Temporal Memory Phase 0A / 0B

This lane isolates project memory from present-code retrieval. It materializes
one authentic, temporally frozen Entire session bundle and derives three
agent-visible conditions from it:

- `raw_history`: indexed history only;
- `facts_only`: durable facts only;
- `history_facts`: indexed history plus durable facts.

Raw transcripts, checkpoint refs, semantic indexes, seed context, docs, and
patterns are absent from every delivered memory condition. `no_brain` receives
the same repository snapshot and prompt with no memory source.

The development task pins the retained source-cache key plus transcript,
history-index, and fact-artifact SHA-256 hashes. A pinned run fails instead of
redistilling when that exact private source artifact is unavailable or differs.
This keeps agent backends on byte-identical memory inputs.

Temporal rows have a hard protocol audit. In the agent-tool lane a memory arm
is invalid unless its first tool action is exactly one `entire brain search`;
in every lane all arms are invalid if they inspect raw `.entire`, `.benchmark`,
or checkpoint-ref artifacts. Claude uses stream JSON and safe mode so tool
activity is observable and user hooks, skills, auto-memory, and project
instructions are disabled.

The committed development task is diagnostic, not paper evidence. It was used
to implement and debug the adapter. Sealed tasks and repeated runs are required
before any channel-attributed claim.

## Phase 0B: Harness-Owned Causal Delivery

Phase 0A's clean confirmation showed that prompt-only tool adherence is
unstable: four memory rows violated the single-frozen-search protocol, and each
violation destroyed a causal row. Phase 0B removes the causal lane's dependence
on whether the task agent chooses to call Brain.

A temporal task selects its lane with the task-level `memory_delivery` field:

- `agent_tool` (default): the original product-adherence lane. The prompt
  mandates the single frozen `entire brain search`, and the temporal audit
  fails rows whose first tool action is not exactly that search. This lane
  measures whether the product's agent-driven delivery is followed; it is not
  the causal estimate.
- `harness`: the causal lane. Before the task agent starts, the harness itself
  executes the one frozen retrieval (the same `entire brain search
  <query> --json --limit <N> --branch <branch>` the agent-tool lane mandates;
  both lanes share one query/limit/branch builder so they cannot drift) against
  the isolated per-condition store. The response is deterministically bounded
  to `memory_bundle.packet.max_bytes` (default 65536; head-of-response, UTF-8
  safe cut) and injected verbatim into the task prompt between
  `<frozen-memory-packet>` tags. The `no_brain` arm runs through the same lane
  with no packet, so all four arms share the same prompt shape and scoring.

After harness delivery, the worktree's `.benchmark` store (Brain plugin data,
the local copy of the shared one-distillation source cache) is physically
deleted before the agent starts. Raw transcripts, checkpoint refs, and withheld
channels were already deleted during prep. The benchmark `entire` wrapper stays
on `PATH`, so a disobedient `entire brain ...` call is intercepted against the
emptied store — never a host installation — and is flagged by the temporal
audit (`brain_used_in_harness_delivery`) as an isolation probe. The harness
lane has no first-tool/search-count requirement: retrieval adherence is not
part of the causal treatment.

Delivery is fail-closed. A retrieval that exits non-zero, returns an empty
response, or returns non-JSON raises before the task agent is launched, and
the failed attempt's provenance is still persisted. Every harness-lane row
writes `memory-delivery.json` (also embedded in `record.json` as
`memory_delivery`) with: the exact argv and agent-lane-equivalent CLI command,
query, limit, branch, condition, exit status, duration, full-response SHA-256
and byte count, delivered-packet SHA-256, byte count, deterministic token
estimate (`ceil(utf8_bytes / 4)`), truncation and budget metadata, source IDs
(session IDs, transcript/history/fact hashes, prep and source cache keys), and
product identity (brain binary SHA-256 plus harness head commit). The record
holds hashes, sizes, commands, and configuration — never hidden answers; the
delivered packet text itself appears only in `prompt.txt`.

The two lanes are never pooled. `summarize()` keys every comparison by
delivery mode, so a harness-lane arm only compares against a harness-lane
`no_brain` baseline and adherence failures cannot contaminate the causal
treatment estimate. Harness-lane rows also score the soft `brain_use`
component exactly like `no_brain` in every arm, keeping the composite score
symmetric across the causal arms.

Task schemas additionally support behavioral validators: a `validation` entry
may be an object `{"command": ..., "kind": "exact" | "behavioral"}` instead of
a plain string (which stays an exact validator). The kind labels validators
that assert behavior through tests rather than one exact source expression —
the Phase 0A neutral-task over-specification repair — and every validator of
either kind must still pass, so behavioral support never weakens exact
validation. No new sealed tasks are authored or run in this change.

Prepare without launching a task agent:

```sh
python3 benchmarks/agent-brain/run.py prep \
  --tasks temporal-memory-default-fact-merge-confidence.json \
  --conditions raw_history,facts_only,history_facts \
  --suite-name temporal-memory-phase0a-prep
```

Run the four-arm development smoke with one pinned runner:

```sh
python3 benchmarks/agent-brain/run.py run \
  --tasks temporal-memory-default-fact-merge-confidence.json \
  --runners codex:gpt-5.5:high \
  --conditions no_brain,raw_history,facts_only,history_facts \
  --repetitions 1 \
  --suite-name temporal-memory-phase0a-development
```

Regenerate the development report from retained records. Repeat
`--agent-suite` for each backend:

```sh
python3 benchmarks/agent-brain/temporal-memory/generate_report.py \
  --source-suite benchmarks/agent-brain/results/temporal-memory-phase0a-source-v3 \
  --derived-suite benchmarks/agent-brain/results/temporal-memory-phase0a-derived-v1 \
  --agent-suite benchmarks/agent-brain/results/temporal-memory-phase0a-agent-smoke-v1 \
  --agent-suite benchmarks/agent-brain/results/temporal-memory-phase0a-agent-smoke-claude-v2 \
  --task benchmarks/agent-brain/tasks/temporal-memory-default-fact-merge-confidence.json \
  --expected-text 0.75 \
  --out-dir benchmarks/agent-brain/temporal-memory/generated
```

The generator re-parses `agent.stdout` with the current harness rather than
trusting activity counters stored by an older parser.

The sealed feasibility manifest freezes three additional strata before agent
runs: fact-positive, stale/conflict, and neutral. Verify the injected
regressions, prepare all delivery caches, and run each backend in a separate
suite:

```sh
python3 benchmarks/agent-brain/run.py check \
  --tasks temporal-memory-sealed-expand-opt-in.json \
          temporal-memory-sealed-semantic-default-stale-history.json \
          temporal-memory-sealed-windows-drive-neutral.json

python3 benchmarks/agent-brain/run.py prep \
  --tasks temporal-memory-sealed-expand-opt-in.json \
          temporal-memory-sealed-semantic-default-stale-history.json \
          temporal-memory-sealed-windows-drive-neutral.json \
  --conditions raw_history,facts_only,history_facts \
  --suite-name temporal-memory-phase0a-sealed-prep

python3 benchmarks/agent-brain/run.py run \
  --tasks temporal-memory-sealed-expand-opt-in.json \
          temporal-memory-sealed-semantic-default-stale-history.json \
          temporal-memory-sealed-windows-drive-neutral.json \
  --runners codex:gpt-5.3-codex-spark:low \
  --conditions no_brain,raw_history,facts_only,history_facts \
  --repetitions 1 \
  --suite-name temporal-memory-phase0a-sealed-codex
```

Generate the gate report from one or more backend suites:

```sh
python3 benchmarks/agent-brain/temporal-memory/generate_sealed_report.py \
  --manifest benchmarks/agent-brain/temporal-memory/phase0a-sealed-smoke.json \
  --source-artifact-manifest benchmarks/agent-brain/temporal-memory/phase0a-source-artifact.json \
  --source-archive /path/to/temporal-source-1dd2312593bd40ce7e66f748.tar.gz \
  --results-artifact-manifest benchmarks/agent-brain/temporal-memory/phase0a-results-artifact.json \
  --agent-suite benchmarks/agent-brain/results/temporal-memory-phase0a-sealed-codex-v1 \
  --agent-suite benchmarks/agent-brain/results/temporal-memory-phase0a-sealed-claude-v1 \
  --out-dir benchmarks/agent-brain/temporal-memory/generated \
  --report-stem sealed-smoke-pilot-report
```

Use a distinct `--report-stem` for a confirmation or repaired protocol. This
prevents a later run from overwriting the frozen pilot report while keeping all
reports reproducible through the same generator.

The sealed gate is intentionally stricter than row success: it also requires
no-Brain correctness headroom, clean run provenance, and a portable private
source artifact before authorizing a scaled correctness study.

## Protocol Repairs After the Frozen Smoke

The clean confirmation is retained separately from the first sealed run. It
showed that prompt-only tool adherence is unstable and that the frozen neutral
task's source-pattern validator rejects implementation-equivalent fixes. The
original task and records remain unchanged. The replacement
`temporal-memory-protocol-repair-windows-drive-neutral-v2.json` is explicitly a
non-sealed development task and validates lowercase drive behavior through Go
tests instead of matching one source expression.

New harness runs retain `agent.patch` plus its byte count and SHA-256 before
worktree cleanup, after a post-agent secret audit. The sealed report treats
missing patch artifacts and missing provider-reported distillation tokens as
separate failed gates; complete task-agent token totals do not mask either
evidence gap.

Generated reports are split by evidence role:

- `generated/sealed-smoke-pilot-report.*` records the first dirty-worktree
  sealed run;
- `generated/sealed-smoke-clean-confirmation-report.*` records the clean rerun
  and every protocol/validator failure;
- `generated/development-report.*` records adapter-development behavior.

The public `phase0a-results-artifact.json` manifest verifies the private raw
evidence pack. The pack is not committed because agent output can quote the
authentic session-derived memory.

Verify either private archive against its public manifest without extracting
it:

```sh
python3 benchmarks/agent-brain/temporal-memory/verify_artifact.py \
  --manifest benchmarks/agent-brain/temporal-memory/phase0a-results-artifact.json \
  --archive /path/to/phase0a-evidence-ca40c070-v1.tar.gz
```
