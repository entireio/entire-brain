# Existing-suite analysis (exploratory)

> This recomputation uses retained raw records and the shared executed-run predicate. These legacy suites do not contain schema-v1 prompt/packet evidence manifests and are not confirmatory evidence.

Estimand: `all_executed_attempts; correctness and efficiency reported separately`

Raw records: 29 total; 29 executed; 0 non-executed.

## Inputs

- `roll-s0/records.ndjson` — `ca0eec0f9855601ae5dfdecbacd979164a77feacc880515135c98d148b97e618` (16 records)
- `roll-s2/records.ndjson` — `a3e6379777f69ef9b8b372f672748c75da6145bcdf631fad4654405677cb8dce` (11 records)
- `rollcap6-s1/records.ndjson` — `b7600c9f21eddc7a8e9d87ac31a81776352a5b89d1c142c9c4d05e14d0beb302` (2 records)

## All-executed arm metrics

| Task | Runner | Delivery | Condition | N | Passes | Pass rate | Token n | Mean tokens | Time n | Mean seconds |
|---|---|---|---|---:|---:|---:|---:|---:|---:|---:|
| entire-cli-c0701-078a63ebb | claude-opus-medium | agent_tool | full_brain | 2 | 0 | 0.000 | 2 | 1688477.0 | 2 | 464.4 |
| entire-cli-c0701-078a63ebb | claude-opus-medium | agent_tool | no_brain | 2 | 0 | 0.000 | 2 | 1018299.0 | 2 | 312.6 |
| entire-cli-c0701-21fcaf5f2 | claude-opus-medium | agent_tool | full_brain | 2 | 2 | 1.000 | 2 | 527442.0 | 2 | 118.1 |
| entire-cli-c0701-21fcaf5f2 | claude-opus-medium | agent_tool | no_brain | 2 | 2 | 1.000 | 2 | 720986.0 | 2 | 112.6 |
| entire-cli-c0701-250538b1f | claude-opus-medium | agent_tool | full_brain | 2 | 2 | 1.000 | 2 | 391753.5 | 2 | 120.1 |
| entire-cli-c0701-250538b1f | claude-opus-medium | agent_tool | no_brain | 2 | 2 | 1.000 | 2 | 520935.0 | 2 | 128.7 |
| entire-cli-c0701-338d79887 | claude-opus-medium | agent_tool | full_brain | 2 | 2 | 1.000 | 2 | 221107.5 | 2 | 53.1 |
| entire-cli-c0701-338d79887 | claude-opus-medium | agent_tool | no_brain | 2 | 2 | 1.000 | 2 | 141371.5 | 2 | 39.5 |
| entire-cli-c0701-36563bc78 | claude-opus-medium | agent_tool | full_brain | 1 | 1 | 1.000 | 1 | 723187.0 | 1 | 163.3 |
| entire-cli-c0701-36563bc78 | claude-opus-medium | agent_tool | no_brain | 2 | 2 | 1.000 | 2 | 547236.5 | 2 | 191.3 |
| entire-cli-c0701-40cb4d68a | claude-opus-medium | agent_tool | full_brain | 2 | 2 | 1.000 | 2 | 355105.5 | 2 | 110.8 |
| entire-cli-c0701-40cb4d68a | claude-opus-medium | agent_tool | no_brain | 2 | 2 | 1.000 | 2 | 294656.5 | 2 | 95.3 |
| entire-cli-c0701-4dd458656 | claude-opus-medium | agent_tool | full_brain | 2 | 2 | 1.000 | 2 | 703891.0 | 2 | 168.6 |
| entire-cli-c0701-4dd458656 | claude-opus-medium | agent_tool | no_brain | 2 | 2 | 1.000 | 2 | 471805.0 | 2 | 174.0 |
| entire-cli-c0701-6699ec40a | claude-opus-medium | agent_tool | no_brain | 2 | 0 | 0.000 | 2 | 1833538.0 | 2 | 653.7 |

No successful-attempt-only token or duration headline is computed. Failures remain in the correctness denominator, and measured efficiency values use all executed attempts with that measurement.
