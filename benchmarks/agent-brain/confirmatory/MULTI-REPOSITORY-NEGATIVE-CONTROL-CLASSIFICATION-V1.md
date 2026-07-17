# Multi-repository negative-control classification v1

This slice implements a pure, fail-closed classifier for the frozen 62-candidate permanently
development-only negative-control plan. It accepts only injected public attempt metadata and derives
a canonical classification receipt. It does not execute a candidate, Git, Go, a detached worktree, a network
operation, a model/provider, or the private-log writer. No candidate or benchmark execution was
performed while building or testing this slice.

The classifier deliberately preserves the plan's pending authority state. Its attempt manifest is
`injected_attempt_observations_unattested_execution_forbidden`; its receipt is
`classification_applied_to_injected_unattested_attempts`; and its execution status is
`forbidden_missing_attested_executor_and_remaining_gates`. A valid receipt therefore proves only
that the checked truth table was applied to a complete, canonical, injected fixture bound to the
current frozen public inputs. It is not evidence that any attempt actually ran.

## Exact schedule and observation contract

The schedule is derived directly from the checked run plan, in repository order and then increasing
first-parent position. Each of the 62 candidates has the following four adjacent attempts:

1. `baseline`, repetition 1;
2. `baseline`, repetition 2;
3. `first_parent_source_reversal`, repetition 1; and
4. `first_parent_source_reversal`, repetition 2.

That is exactly 62 candidates x 2 arms x 2 repetitions = 248 attempts. Missing, extra, duplicated,
or reordered attempts fail closed. Every row binds the repository key and ID, candidate reference,
first-parent position, arm, repetition, and one-based attempt ordinal. All four rows for a candidate
must carry the same nonzero SHA-256 of the test command; command text is never public.

An injected attempt can have only one of these result forms:

- `status: completed` with an exact built-in integer exit code from 0 through 255; exit code zero is
  `passed`, and a nonzero code is `failed`; or
- `status: timeout` with a null exit code.

There is no `interrupted`, `cancelled`, `error`, or partial status. Such input, or an incomplete set
of 248 rows, is rejected without publishing or replacing a final receipt. Each row is explicitly labeled
`injected_attempt_result_unattested` so synthetic fixtures cannot be confused with executor
attestation.

## Frozen seven-class truth table

Baseline outcomes have precedence. The reversed-source pair is considered only when both baseline
repetitions passed.

| Baseline pair | Reversed-source pair | Classification |
| --- | --- | --- |
| contains a timeout | any | `baseline_invalid_timeout` |
| failed, failed | any | `baseline_invalid_failure` |
| one passed and one failed | any | `baseline_inconsistent` |
| passed, passed | contains a timeout | `reversed_invalid_timeout` |
| passed, passed | failed, failed | `eligible_for_symptom_review` |
| passed, passed | one passed and one failed | `reversed_inconsistent` |
| passed, passed | passed, passed | `negative_control_survived` |

The unit tests exhaust all 3^4 = 81 combinations of `passed`, `failed`, and `timeout` across the four
attempts and verify compatibility with the five homogeneous single-repetition outcomes in the
legacy development classifier.

## Public/private log boundary

The classifier never receives a raw log or a private path. Each public attempt carries only:

- `raw_log_sha256`;
- `raw_log_byte_count`; and
- `receipt_file_sha256`, the SHA-256 of the exact canonical two-field receipt produced by
  `negative_control_private_log.RawLogReceipt`.

The raw-log digest may be any 64-character lowercase hexadecimal SHA-256, including the all-zero
value for schema compatibility; unrelated identity and file hashes reject the all-zero sentinel.
The byte count is bounded to 16 MiB per attempt. The sum over all 248 references is checked against
the arithmetic ceiling of 248 x 16 MiB, while content-addressed unique bytes are deduplicated by
digest and must remain within the plan's 2 GiB private-log ceiling. A repeated digest with a
different byte count fails closed. The exact schedule permits at most 248 unique digests and the
checker also binds the private writer's 4,096-file ceiling.

Neither public artifact contains raw output, a raw-output path, a test command, environment values,
or private-log-root metadata. Actual retention, cleanup, safe storage, and executor-to-writer
integration remain separate gates.

## Exact bindings and canonical form

Both public artifacts bind and revalidate the canonical raw bytes and self-hashes of the checked run
plan, injected gate manifest, and injected gate receipt. They also bind the ordered three-repository
projection, repository ledgers, candidate order, exact Git/Go/native toolchains, current gate
primitive, run-plan builder, private-log writer, gate and private-log receipt schemas, classifier
source, Python executable, Python runtime identity, Unicode data version, and both new schema files.

The schemas are Draft 2020-12 and contain exact `prefixItems` projections for all 248 attempts, all
62 result rows, and all three repository bindings. The checked schema file hashes are:

- attempt observations: `d5a036e22ded76cc485fc62742a4f85d93ff6d228063ffc2c014fa13725b53ca`;
- classification receipt: `c025b1fde9dba21b1b139aa7f9e5ae5f9bc1912aa61bf33921fe62c07aefcca0`.

The runtime does not silently fall back to an older JSON Schema dialect. It pins these exact schema
bytes and checks the generated projections itself, in addition to full semantic validation. Input
JSON rejects duplicate object keys, floating-point values, oversized integers, excessive nesting,
noncanonical bytes, and artifacts above the applicable raw-byte ceiling. Self-hashes use the shared
sorted-key compact UTF-8 canonical profile with the self-hash field null; published files use the
separate sorted-key, two-space-indented UTF-8/LF artifact-render profile.

## CLI and publication behavior

`task_negative_control_classification_v1.py` exposes only two operations:

- `classify`: validates a complete canonical injected attempt manifest and every exact dependency,
  applies the truth table, and atomically writes one receipt; and
- `check-receipt`: revalidates a canonical receipt against the same attempt manifest and exact
  dependencies.

Example shape, using injected gate and attempt artifacts from an approved external producer:

```bash
python3 benchmarks/agent-brain/confirmatory/task_negative_control_classification_v1.py classify \
  --plan benchmarks/agent-brain/confirmatory/development-task-negative-control-run-plan-v2.json \
  --gate-manifest /path/to/injected-gate-manifest.json \
  --gate-receipt /path/to/injected-gate-receipt.json \
  --gate-manifest-schema benchmarks/agent-brain/confirmatory/schemas/offline-go-cache-seed-manifest-v1.schema.json \
  --gate-receipt-schema benchmarks/agent-brain/confirmatory/schemas/negative-control-gate-primitive-receipt-v1.schema.json \
  --attempt-schema benchmarks/agent-brain/confirmatory/schemas/negative-control-attempt-observations-v1.schema.json \
  --receipt-schema benchmarks/agent-brain/confirmatory/schemas/negative-control-classification-receipt-v1.schema.json \
  --private-log-schema benchmarks/agent-brain/confirmatory/schemas/negative-control-private-log-receipt-v1.schema.json \
  --attempt-manifest /path/to/injected-attempt-observations.json \
  --output /path/to/classification-receipt.json
```

The output file is created only after all validation and classification succeed. Publication uses a
same-directory temporary file, flush and file synchronization, then atomic replacement. Validation
errors and interruptions before the atomic replacement do not publish or replace a final receipt,
and temporary-file cleanup is attempted. The atomic replacement is the publication commit point;
an interrupt delivered after that commit can coexist with the committed receipt. A stronger
interruption/cleanup attestation remains an explicit residual gate below.

## Remaining hard gates

This pure classifier closes only the deterministic truth-table and public-receipt primitive. The
following remain explicit in every receipt:

- `owner_execution_approval_receipt_and_trust_mechanism`;
- `authorized_plan_binding_for_actual_cache_seed`;
- `safe_archive_traversal_type_link_device_and_content_verifier`;
- `trusted_filesystem_observer_and_atomic_resource_reservation`;
- `negative_control_executor_and_reversal_proof`;
- `attested_attempt_producer_and_classifier_integration`;
- `private_log_executor_integration_retention_and_aggregate_accounting`; and
- `fail_closed_cleanup_and_no_receipt_on_interruption`.

Until those gates are implemented and independently reviewed, candidate execution, paid execution,
model/provider execution, calibration membership, confirmatory membership, and benchmark execution
remain forbidden.

## Non-paid verification

```bash
cd benchmarks/agent-brain/confirmatory
python3 -m unittest test_task_negative_control_classification_v1.py
```

The tests use only synthetic in-memory and temporary-file observations. They do not run any frozen
candidate command or scheduled Go test.
