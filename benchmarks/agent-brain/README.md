# Historical benchmark archive

This archive records what we tested, what happened, and why some results did not support a claim.
It contains 152 retained records and 48 restored context files, totaling about 12 MB.
Every archived file matches the pre-cleanup snapshot. Positive, negative, invalid, and inconclusive records remain together.

These are historical records, not current product measurements. Old commands and instructions describe the original experiments and must not be treated as current workflows.
The archive has no Python programs, dependencies, or virtual environments. The Go integrity check reads data only.

## Experiment index

| Work | Records and conclusion | Restored context | Limits |
|---|---|---|---|
| Agent retrieval comparisons | [Clean comparison series](evidence/semantic-clean-proof/README.md), including earlier and negative comparisons | [Conditions](CONDITIONS.md) and the corresponding [task definitions](tasks/) | The narrow positive comparison measured a complete packet treatment. It did not isolate retrieval alone. |
| Temporal memory and history delivery | [Development pilot and correction](temporal-memory/phase0b-powered-pilot-memo.md), [clean history comparison](evidence/replay-lab-clean/README.md), and [delivery experiments](evidence/history-delivery-fix/README.md) | [Development protocol](temporal-memory/phase0a-development.json), [sealed-smoke protocol](temporal-memory/phase0a-sealed-smoke.json), [source identity](temporal-memory/phase0a-source-artifact.json), tasks, and [comparison configuration](panels/p01-clean-proof-claude.json) | Keep invalid delivery runs, exclusions, and small-sample limitations with the corrected results. Private checkpoint content is not bundled. |
| Distillation scheduling | [Local audit](evidence/distill-perf/distill-perf-audit-report.md) and [larger-repository audit](evidence/distill-perf-large/distill-perf-audit-report.md), with timing records and manifests | Workload and command-agent settings remain in the retained manifests and records | These measure historical local scheduling. They do not establish hosted-model latency or current performance. |
| Packet formats and brief profiling | [Compact v3 measurements](PACKET-FORMAT-V3-LOCAL-MEASUREMENT.md) and [brief profile specification](BRIEF-PROFILE-BASELINE.md) | [Earlier A/B design](PACKET-FORMAT-AB.md), [v2 design](PACKET-FORMAT-V2-AB.md), and [profile corpus identities](brief-profile-corpus-v1.json) | Development measurements do not establish agent quality. The identity snapshot does not include the complete 114-task prompt corpus. |
| Release controls and negative comparisons | [Release evidence](evidence/release/README.md), [facts evaluation](evidence/facts-eval/facts-eval-audit-report.md), [release matrix](evidence/release-matrix/README.md), [Radar tool checks](evidence/radar-tool/README.md), and [workspace checks](evidence/workspace-radar/README.md) | Matching tasks, [comparison configurations](panels/), and four historical Go [scoring fixtures](fixtures/validation/entireio-cli/) | Retain the original no-release-claim decisions. Tool-contract success does not prove agent performance. |
| Confirmatory planning and blocked decisions | [No-go decision](confirmatory/go-no-go.json), [power analysis](confirmatory/power-analysis.json), and [blocked ranking decision](analysis/WORKSTREAM-7-GATE-REPORT-2026-07-15.md) | [Preregistration](confirmatory/PREREGISTRATION.md), [engine matrix](confirmatory/engine-matrix.json), [analyzer identity](confirmatory/analyzer-lock.json), and [power calibration](confirmatory/power-calibration-exploratory-v1.json) | Plans and readiness checks are not completed confirmatory experiments. Larger development corpora and private attestations remain omitted. |
| Exploratory analysis | [Earlier-run analysis](analysis/reports/existing-winner-suites-exploratory.md) and [ranking diagnostic output](analysis/artifacts/ranking-baseline-output-v1.json) | Source identities and methodology remain in the records | Some source suites and synthetic analyzer inputs are not archived. These reports are exploratory. |

The [archive manifest](archive-manifest.json) lists every record, context file, SHA-256 digest, experiment, and selected local dependency.
Its reference lists connect retained outcomes to task definitions, comparison configurations to tasks, and tasks to their scoring fixtures.

## Source and completeness

All 200 historical files come from commit `115f271363804359a195708807435bff7a7136fe`.
This is the archive's source snapshot, not necessarily the revision used for each original run.
Per-run provenance remains authoritative for dates, model versions, source commits, configurations, and recorded hashes.
A restored task definition can differ from an earlier run's recorded configuration hash.

We retain the 24 task definitions named in the recorded outcomes, six comparison configurations, four Go scoring fixtures, and 14 methodology or configuration files.
The scoring fixtures preserve historical success criteria; the current Go tests do not execute them.
Large candidate collections, unused task pools, duplicate snapshots, synthetic analyzer fixtures, and retired runners remain deleted.

This archive supports historical inspection. It is not a complete reproduction bundle.
Historical documents can reference omitted files, retired commands, private data, or external tools.
Use the source commit to inspect omitted tracked files. Git history cannot supply private inputs that were never committed.
Do not interpret historical commands as instructions to reinstall the old tooling.

## Integrity checks

Run the archive checks with Go:

```sh
go test ./tools/releasecheck -run 'TestHistoricalBenchmarkArchive|TestArchiveIntegrity' -count=1
```

The check verifies file hashes, complete inventory, experiment entries, and the manifest's required local references.
It rejects missing, changed, unlisted, or linked files. It does not run historical commands or certify experimental validity.
Normal CI and `mise run check` include this test package.

When intentionally changing the archive, review the source and update the manifest together.
Keep historical records unchanged when adding an interpretation; put the new explanation in this index.
Use `mise run check` for current source checks and `mise run release:readiness` for current local release checks.
