# Agent-brain evidence analysis

This package is the durable replacement for the former `analyze_eff.py`,
`analyze_hidden.py`, and `analyze_scale.py` scratch scripts.

`common.is_executed_run` is the only executed-attempt predicate. A v2 attempt is
included once the causal treatment interval starts at the harness-owned
retrieval/no-op boundary, even when retrieval fails before the model runs or
validation, return code, integrity, or protocol adherence later fails.
Pre-treatment setup failures are excluded and reported. The v2 co-primary
endpoints require complete elapsed-time, billed-cost, and code-quality
measurements for every executed attempt; an early retrieval timeout with no
billed usage invalidates the suite rather than being replaced or imputed.
Successful-attempt-only efficiency is never computed.

New `run` and `panel` suites fail closed when the harness is dirty. The explicit
`--allow-dirty-harness-exploratory` override retains `harness.patch` and marks
the suite ineligible for confirmatory use. Every completed run retains its exact
`prompt.txt`, optional `packet.txt`, output, patch, raw record, and schema-v2 run
manifest. The suite manifest uses bundle-relative paths and content hashes.

Verify a copied bundle and optionally regenerate its report:

```console
python3 benchmarks/agent-brain/run.py verify-evidence /path/to/suite --write-report
```

Legacy records can be recomputed, but cannot be promoted to schema-v2 evidence
when their direct prompt/packet artifacts are missing:

```console
python3 benchmarks/agent-brain/run.py analyze-records /path/to/suite-a /path/to/suite-b
```

The JSON schemas in `schemas/` retain the exploratory v1 format and define the
authoritative v2 run and joint-superiority formats. Local source
or quarantine locations are resolution concerns outside evidence identity;
durable manifests contain logical names, bundle-relative artifact paths, and
hashes only.

## Confirmatory analyzer

`confirmatory.py` implements the frozen three-arm analysis without reading task
configs, holdout assignments, or relevance labels. It first verifies the
evidence bundle and exact schedule, then requires every task x treatment x
repetition cell to be present, unique, executed, and balanced. Post-agent
failures remain in every endpoint. Missing elapsed time, any billed category,
the frozen quote binding, or code quality refuses analysis.

The claim-bearing contrast is `retrieved_memory` versus `no_memory`. End-to-end
time and normalized cost use paired task-level log ratios; quality uses only
normalized output outcome and patch-focus criteria, excluding process behavior,
runtime, tokens, and treatment-specific tool use. It enters as a paired
task-level score difference. One-sided task-clustered bounds must clear
the frozen practical floors for all three endpoints. This intersection-union
rule is a single joint claim. Placebo comparisons, validation pass rate, raw
tokens, harness agent-only time, and provider API time remain diagnostics.
Inclusive provider token totals are normalized into five mutually exclusive
categories, including cache-write and reasoning output, before frozen prices or
explicit aliases are applied.

Usage is retained and priced per isolated provider invocation, including failed
and retried calls. Provider adapters select one authoritative cumulative attempt
total (rather than recursively summing duplicate snapshots), then the analyzer
reconciles the sum of every attempt's raw and mutually exclusive categories
against the cell aggregate. A generic total-token number cannot populate any
category. The frozen quote binds input/output inclusion rules and an explicit
absence-means-zero decision for cache-read, cache-write, and reasoning counters.
Missing or ambiguous attempt usage, an unapproved absent counter, or multiple
Claude terminal results/model rows under a single-model quote refuses analysis.
Codex cumulative snapshots must keep optional-counter presence stable, so a
later omission cannot erase usage already observed in the invocation.

Timed-out attempts retain the measured monotonic end-to-end interval. The
frozen agent-component timeout and the component limit that fired are preserved
as metadata, but neither replaces the end-to-end observation. The final response
boundary is captured before provider-output parsing and evidence artifact I/O.

The runtime analyzer identity is an explicit ordered source/schema set, hashed
as `source_path + NUL + content_sha256 + newline`. Reports, generated artifacts,
fixtures, caches, and tests are excluded. A suite must contain the same analyzer
aggregate as the frozen `confirmatory/analyzer-lock.json` (or an explicitly
supplied hash):

```console
python3 benchmarks/agent-brain/analysis/confirmatory.py /path/to/verified-suite \
  --success-contract /path/to/frozen-success-contract.json \
  --price-quote /path/to/frozen-price-quote.json \
  --output confirmatory-analysis.json
```

For deterministic test fixtures, `--resamples` and `--seed` are configurable;
the frozen protocol values are 10,000 and 607152026.
