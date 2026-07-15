# Agent-brain evidence analysis

This package is the durable replacement for the former `analyze_eff.py`,
`analyze_hidden.py`, and `analyze_scale.py` scratch scripts.

`common.is_executed_run` is the only executed-attempt predicate. An attempt is
included once the agent ran, even when validation, return code, integrity, or
protocol adherence failed. Pre-agent infrastructure failures are excluded and
reported. Primary token and duration means use every executed attempt that has
the corresponding raw measurement; successful-attempt-only efficiency is not a
primary metric.

New `run` and `panel` suites fail closed when the harness is dirty. The explicit
`--allow-dirty-harness-exploratory` override retains `harness.patch` and marks
the suite ineligible for confirmatory use. Every completed run retains its exact
`prompt.txt`, optional `packet.txt`, output, patch, raw record, and schema-v1 run
manifest. The suite manifest uses bundle-relative paths and content hashes.

Verify a copied bundle and optionally regenerate its report:

```console
python3 benchmarks/agent-brain/run.py verify-evidence /path/to/suite --write-report
```

Legacy records can be recomputed, but cannot be promoted to schema-v1 evidence
when their direct prompt/packet artifacts are missing:

```console
python3 benchmarks/agent-brain/run.py analyze-records /path/to/suite-a /path/to/suite-b
```

The JSON schemas in `schemas/` document the stable v1 wire format. Local source
or quarantine locations are resolution concerns outside evidence identity;
durable manifests contain logical names, bundle-relative artifact paths, and
hashes only.

## Confirmatory analyzer

`confirmatory.py` implements the frozen three-arm analysis without reading task
configs, holdout assignments, or relevance labels. It first verifies the
evidence bundle and exact schedule, then requires every task x treatment x
repetition cell to be present, unique, executed, and balanced. Post-agent
failures remain in the correctness denominator. Missing or nonpositive total
tokens, missing harness timing, and invalid observed values refuse analysis.

The correctness estimand is the task-paired pass-rate difference. Its one-sided
95% task-clustered bootstrap lower bound is compared with the -0.10 margin.
Only after retrieved memory clears that Holm-corrected gate does the analyzer
compute task-paired geometric-mean total-token ratios and the two independent
wall-time endpoint families. Agent-reported API duration is nullable: if any
cell lacks it, that entire secondary endpoint is withheld and harness wall time
is never substituted.

The runtime analyzer identity is an explicit ordered source/schema set, hashed
as `source_path + NUL + content_sha256 + newline`. Reports, generated artifacts,
fixtures, caches, and tests are excluded. A suite must contain the same analyzer
aggregate as the frozen `confirmatory/analyzer-lock.json` (or an explicitly
supplied hash):

```console
python3 benchmarks/agent-brain/analysis/confirmatory.py /path/to/verified-suite \
  --output confirmatory-analysis.json
```

For deterministic test fixtures, `--resamples` and `--seed` are configurable;
the frozen protocol values are 10,000 and 607152026.
