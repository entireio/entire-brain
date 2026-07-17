# Negative-control gate primitives v1

This slice implements two pure, unpaid prerequisites for the pending multi-repository negative-control
plan: an exact offline Go cache-seed identity manifest and deterministic injected resource arithmetic.
It does not supply an actual cache archive, inspect an archive, observe host free space, reserve disk,
create a worktree, invoke a candidate, run Git or Go, contact a provider, or authorize execution.

`task_negative_control_gate_v1.py` has exactly two CLI lanes: `check-manifest` checks a cache manifest
against the pending plan and manifest schema, while `check-receipt` additionally checks a canonical
primitive receipt against that exact plan, manifest, and both schemas. The CLI only reads JSON and
schema/module bytes. There is deliberately no build, extract, fetch, filesystem-probe, executor, or
receipt-writing command.

## Cache identity primitive

A manifest built through the Python API binds:

- the pending plan self-hash and exact three-repository Go toolchain projection;
- the gate builder, run-plan builder, schema validator, and manifest schema bytes;
- a basename-only, NFC-normalized archive name plus its declared SHA-256 and byte count;
- a sorted file inventory under only `gocache/` or `gomodcache/`, with globally unique canonical
  archive paths, repository ownership, exact file byte counts, and file SHA-256 values; and
- the frozen 2 GiB compressed and unpacked cache-seed ceilings.

Every repository must have at least one cache entry. Host-shared cache reuse and network dependency
resolution remain `forbidden`. The checked-in pending plan still has null archive, manifest, source,
and unpacked-size locators: this slice intentionally does not fabricate or bind an actual seed.

The preflight API can compare an injected archive-identity observation with a manifest, but labels that
observation `injected_archive_identity_unattested`. Matching declared hashes and counts is not safe
archive verification. A future gate must traverse an actual archive fail-closed, reject unsafe paths,
non-regular entries, links and devices, hash every extracted file, and then bind that verified archive
into a separately authorized plan.

## Injected resource arithmetic

`evaluate_injected_resource_stats` accepts only `fragment_size_bytes` and `available_blocks`. It
recomputes available bytes, checks the frozen `8 GiB reserve + 8 GiB staging = 16 GiB minimum`, and
rejects anything below the threshold. It does not call `statvfs`, identify a filesystem, reserve
capacity, or eliminate time-of-check/time-of-use races. A synthetic 11 GiB observation therefore
fails deterministically without consuming disk, while an exact 16 GiB observation passes the pure
arithmetic primitive.

A deterministic primitive receipt binds the exact canonical plan and manifest bytes, both schemas,
all implementation dependencies, the injected observations, and its own self-hash. Its status is
`primitive_checks_passed_execution_forbidden`, its cache binding remains
`external_manifest_not_bound_by_pending_plan`, and its execution status remains
`forbidden_missing_remaining_gates`.

## Residual hard gates

These primitives do not close:

- owner execution approval and its trust mechanism;
- binding a safely verified actual cache archive and manifest into an authorized plan;
- safe archive traversal/type/link/device/content verification;
- a trusted filesystem observer plus atomic capacity reservation;
- the detached-worktree executor and first-parent source-reversal proof;
- the classification truth table and receipt checker;
- the private content-addressed log writer/checker; or
- fail-closed cleanup and no-receipt-on-interruption behavior.

Calibration membership, confirmatory holdout membership, model/provider execution, paid execution,
candidate execution, and benchmark execution remain forbidden.

## Verification

Run the focused static and synthetic checks from the confirmatory directory:

```bash
python3 -m unittest \
  test_task_negative_control_plan_v2.py \
  test_task_negative_control_gate_v1.py
pyright task_negative_control_gate_v1.py test_task_negative_control_gate_v1.py
```

The tests use fabricated hashes, file inventories, and injected block counts only. They do not create
cache archives, fill disk, create candidate worktrees, or run any of the 62 candidate tests.
