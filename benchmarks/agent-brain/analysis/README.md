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
