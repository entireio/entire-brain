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

Both lanes are pinned to the checked plan self-hash
`a47311fa1f4553f8085ebea6e8ddd003a4ea5d0b2d269d91bbcdd414134a248e` and exact rendered-file
SHA-256 `f55b6e22a25daf8016305304adf3b7ac8d31675281d97cfc9bb78cc76b619790`. The API also rebuilds the
plan through `verify_plan_dependencies` from the checked repository contract, eligibility schema,
three v2 ledgers, run-plan schema, overlap registry, and registry schema. A self-resealed plan with a
different candidate, ledger, registry, eligibility, builder, schema, or Go/native/Git toolchain
identity is therefore rejected.

## Cache identity primitive

A manifest built through the Python API binds:

- the exact checked plan and each repository's complete Go, native compiler, and Git toolchain
  projection;
- the gate builder, run-plan builder, schema validator, all transitively used local verifier source
  files, and manifest schema bytes;
- the Python implementation, Python version, resolved interpreter executable SHA-256, and
  `unicodedata.unidata_version` that define parsing and case-fold behavior;
- a basename-only, NFC-normalized archive name plus its declared SHA-256 and byte count;
- a sorted file inventory under only `gocache/` or `gomodcache/`, with globally unique canonical
  archive paths, repository ownership, exact file byte counts, and file SHA-256 values; and
- the frozen 2 GiB compressed and unpacked cache-seed ceilings.

Every repository must have at least one cache entry and every entry must declare at least one byte.
Exact NFC paths must also be unique under the per-component portable key
`NFC(casefold(NFC(component)))`; duplicates and ancestor prefixes are rejected globally regardless
of repository or input order. This closes ASCII case, Unicode fold (including `ß`/`ss`), and
canonical-composition aliases before a future extractor chooses host paths.

The v1 profile freezes a maximum of 250,000 entries and 128 MiB of canonical rendered manifest
bytes. The JSON schema has the same `maxItems`/count/byte bounds and intentionally does not use
`uniqueItems`; the verifier uses bounded O(n) sets for exact and portable collision checks. These are
conservative planning ceilings intended to leave room for three substantial Go caches while bounding
memory and parser work. They are not a measurement of an actual seed. The future archive-preparation
step must prove the real inventory fits—including the non-zero-file rule—or version the profile and
schema rather than silently raising a ceiling.

Host-shared cache reuse and network dependency resolution remain `forbidden`. The checked-in pending
plan still has null archive, manifest, source, and unpacked-size locators: this slice intentionally
does not fabricate or bind an actual seed.

The manifest and receipt schema arguments are not caller-defined policy. Their raw bytes must match
the pinned checked-in v1 schema SHA-256 values (`37d3839411b2e30a99fdd7e93784d56f5fd525c0472717a24cc7b92213b98999`
and `a6ea9aee520a11afd5a17339661dd4e67e503aa89593f69696a4306670201e81`, respectively); an alternate
path is accepted only when its bytes are identical. Manual fail-closed invariants remain authoritative
and schema validation is additionally executed.

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

## Bounded input profile

Before JSON parsing, the checker opens only stable regular files with mandatory close-on-exec,
no-follow, and nonblocking flags, checks size with `fstat`, reads no more than the frozen ceiling, and
rejects any metadata change during the read. Symlinks, FIFOs, same-size mutation races, duplicate
keys, floats/non-finite numbers, integers longer than 64 digits, and nesting deeper than 64 levels
fail closed with CLI exit 2 and no traceback. Raw limits are 1 MiB for the checked plan, 128 MiB for
the manifest, and 4 MiB each for schemas and the primitive receipt. Validators recheck canonical
rendered byte counts after manual shape/count/scalar bounds, so a constructed Python object cannot
bypass the file parser or force an unbounded render before basic rejection.

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
