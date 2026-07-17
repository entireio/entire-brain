# Multi-repository negative-control cache archive raw identity v1

This slice implements a local, offline, non-authorizing raw-identity boundary for a future
three-repository Go cache seed. It does **not** create a cache archive or manifest, choose or parse
an archive format, extract or stage files, bind cache material into E0 runtime state, approve a run,
consume an approval, execute a candidate, contact a model/provider, use the network, or enable paid
work.

The checked material declaration is deliberately `pending_source_review`. Every actual material
identity and count is null. Consequently, production `verify` must fail before opening either
caller-supplied locator. A successful `build` or `check` proves only that the pending verifier
contract and its exact fixed dependencies are internally consistent.

## Current pending material boundary

`negative-control-cache-archive-material-v1.json` has profile
`agent_brain_negative_control_cache_archive_material_v1` and binds the exact E0, pending run plan,
cache-manifest schema, gate primitive, complete toolchain projection, repository order, permitted
cache roots, and bounded count/size policy. Its current identities are:

| Identity | Fixed value |
| --- | --- |
| Material declaration raw SHA-256 | `7527cec7b2f1b47498845ae38710369997b7c82d2d0150761b23d2b594f68d93` |
| Material declaration canonical self-hash | `f03ae113a29585a9aba0c8ce57a41ab61eb0b2cd682474f1df2d214e299a4852` |
| Material schema SHA-256 | `1424cd22b5eb057b5794347403780435f95c0428d4f81ff42441e71abc39d5d5` |
| Verifier-contract schema SHA-256 | `c35c0de98dbdcec6744a3fe33ee363261507eb894bbddbb3af5af3642be7f914` |
| Verification-report schema SHA-256 | `5827c78df73b024df32bf2853d412df428e7362e869d5d202b33dac15bbe4e5d` |
| Verifier source SHA-256 | `2f21da09c28cf2de62f808a6362f74fbc7aead07a04cfef96c15c93584540b94` |
| Checked verifier-contract raw SHA-256 | `2138adb640dee8ab3537afc1ccb69e846a6af5d360c02da9cfed30c33dded33c` |
| Checked verifier-contract canonical self-hash | `56b5e019f65ef1afc0e4f265a705cdc1523585aad899d5c18ff7a5230463bb47` |

The checked contract status is
`verifier_compiled_material_source_review_pending_execution_forbidden`; every runtime binding is
null and every authority field remains non-passing.

The pending declaration fixes these fields to null:

- archive file, exact raw SHA-256, and byte count;
- manifest file, exact raw SHA-256, and canonical self-hash;
- content-inventory SHA-256;
- total file count and exact per-repository counts; and
- unpacked byte count.

The declaration's archive interpretation is always
`opaque_bytes_content_safety_not_verified`. Its review object keeps archive creation, extraction,
content safety, E0 runtime binding, owner-approval binding, and execution authority false. Candidate,
benchmark, model/provider, network, and paid work are forbidden.

## Fixed scope

The declaration and future verifier contract accept no alternate E0, plan, cache policy, or
toolchain projection:

| Component | Fixed identity |
| --- | --- |
| E0 artifact raw SHA-256 | `94422af8a5e24c6a855b66cf40868565ee3518d0c75eb5343b5d2450523b2c1e` |
| E0 canonical self-hash | `1e290b91d5e0be42f750bf0e655edee48d297f86ad3e3867b8fe9177fd5125ef` |
| E0 source commit | `f0552070921605cd9a0165ee29ca96e100675d58` |
| E0 builder SHA-256 | `f3cfb82978b0ce98d17999ef4cb4f4089a425bb528ece4ff53b61a03a4344bcd` |
| E0 schema SHA-256 | `dbcb95119feeda4c436e90d8162ac0498577454ca9ccaab7a8254d55d90359f9` |
| Run-plan raw SHA-256 | `f55b6e22a25daf8016305304adf3b7ac8d31675281d97cfc9bb78cc76b619790` |
| Run-plan canonical self-hash | `a47311fa1f4553f8085ebea6e8ddd003a4ea5d0b2d269d91bbcdd414134a248e` |
| Run-plan builder SHA-256 | `23d08e9d3d23935cdc86b53780b609fb35f6b3020ede6b1360af288939e34715` |
| Run-plan schema SHA-256 | `294f77f165676bec6053e9637578ca22f048a887d2a978bbf8b8e300f21a2dad` |
| Cache-manifest schema SHA-256 | `37d3839411b2e30a99fdd7e93784d56f5fd525c0472717a24cc7b92213b98999` |
| Gate-primitive source SHA-256 | `f04b8634caf619cc17bb495975d2b5358164be6eae2a124ade84efebd60c51dd` |
| Toolchain projection SHA-256 | `e8240c5af77530079643593a7484cb0062408829bab76690b7f55d12176fc87c` |
| Draft 2020-12 validator SHA-256 | `8688b67468b096427f758163174432e221d8197c6b13870a6427939f6ea281eb` |

Repository order is exactly `entire-brain`, `entire-db`, `entire-graph`; permitted manifest roots
are exactly `gocache` and `gomodcache`. The archive and declared unpacked contents are each capped at
2,147,483,648 bytes, the manifest at 134,217,728 bytes, and the file inventory at 250,000 entries.

The current gate primitive remains useful only as a source of pure manifest invariants. Its
`injected_archive_identity_unattested` receipt state is not authority, provenance, content safety,
or an E0 runtime binding. This verifier does not import that module across the verification trust
boundary.

## Verifier-only surface

`negative_control_cache_archive_v1.py` has exactly three intended CLI commands:

- `build --output CONTRACT` deterministically renders a non-authorizing verifier contract;
- `check` checks only the fixed verifier contract and every fixed dependency; and
- `verify MANIFEST ARCHIVE` verifies only the exact reviewed manifest and archive raw identities.

There is no manifest builder, archive packer, extractor, fetcher, authorizer, runner, Git/Go command,
candidate lane, provider lane, or network path. Production accepts no dependency, declaration,
schema, plan, or contract override.

Verification ordering is fail closed:

1. Verify the exact source, schema, E0, run-plan, declaration, and verifier-contract pins.
2. Require `approved_source_review_identity_only` material status.
3. Only then open the supplied manifest and archive locators.
4. Require stable bounded single-link regular files, reject symlinks and changed pre/post identity,
   and hash the same descriptors that were checked.
5. Require canonical manifest bytes, the pinned manifest schema and self-hash, complete
   inventory/count/toolchain/run-plan bindings, and exact agreement with the reviewed declaration.
6. Emit one deterministic non-authorizing report on standard output.
7. Never interpret, extract, stage, or execute archive contents.

Pending material therefore fails before either supplied locator is opened. Merely changing the
declaration to approved is insufficient: the source-pinned declaration raw/self hashes and rebuilt
verifier contract must also be reviewed together.

## Raw identity is not safe content

The archive is intentionally an opaque regular file in v1. No tar, zip, zstd, custom container, or
filesystem layout is selected by this slice. Exact raw SHA-256 and byte-count equality say only that
the reviewed bytes were supplied. They do not establish traversal safety, member types, link/device
policy, duplicate paths, modes, per-entry contents, safe extraction, or staging behavior.

A successful future verification report has status
`archive_identity_verified_content_unparsed_staging_forbidden` and identity claim
`source_reviewed_actual_cache_manifest_and_archive_raw_identity`. It also fixes archive parsing,
content safety, extraction, staging, E0 runtime binding, owner approval, atomic consumption, and
execution authority to false. Benchmark, candidate, model/provider, network, and paid execution
remain forbidden. The report is not persisted by this slice and cannot be substituted for an owner
approval, an approval-consumption receipt, a safe-content report, or an executor receipt.

## Material supply and source review

Supplying actual cache material is a separate reviewed operation:

1. Produce the private cache seed outside this verifier; do not add a producer or packer command to
   the verifier.
2. Generate and independently review a canonical manifest under the existing v1 profile.
3. Record the exact archive/manifest raw identities, manifest self/inventory hashes, file counts,
   repository counts, and unpacked byte count in the material declaration.
4. Set both declaration status and review disposition to
   `approved_source_review_identity_only`; keep every authority and safety field non-passing.
5. Recompute the declaration self-hash and reviewed raw hash, refresh the source pins, and rebuild
   the verifier contract. Review the source, three schemas, declaration, and rebuilt contract as one
   change.
6. Supply the external manifest and archive only as the two `verify` locator arguments. Retain any
   resulting report as raw-identity evidence only.
7. Build a separately reviewed safe-archive verifier that freezes the chosen container format and
   validates traversal, types, links, devices, duplicates, modes, declared members, content hashes,
   and unpacked ceilings before any extraction or staging is possible.
8. Only a later composition gate may combine raw identity, safe-content verification, real owner
   approval, atomic approval consumption, APFS reservation, and an audited executor into successor
   E0 runtime bindings.

No production archive or manifest instance currently exists in this source-review boundary. Test
fixtures may use tiny synthetic regular files only through private dependency seams; those fixtures
are not a production material source.

## Residual hard gates

This slice does not close:

- an actual approved cache archive and authorized binding;
- safe archive traversal/type/link/device/content verification;
- owner authorization plus atomic single-use approval consumption;
- a trusted APFS observer and atomic capacity reservation;
- the clean detached-worktree executor and first-parent source-reversal proof;
- attested attempt production and classifier integration;
- private-log executor integration, retention, and aggregate accounting; or
- fail-closed cleanup, interruption attestation, and no-receipt-on-failure behavior.

The checked E0 runtime bindings remain null. In particular, a raw-identity report is
`identity_verified_not_bound_to_e0_runtime` and does not satisfy E0's cache residual.

## Local build, check, and verify commands

Run from the repository root. These commands do not authorize or run a benchmark:

```bash
python3 benchmarks/agent-brain/confirmatory/negative_control_cache_archive_v1.py build \
  --output /private/tmp/negative-control-cache-archive-verifier-contract-v1.json
python3 benchmarks/agent-brain/confirmatory/negative_control_cache_archive_v1.py check
python3 benchmarks/agent-brain/confirmatory/negative_control_cache_archive_v1.py verify \
  "$CACHE_MANIFEST" "$CACHE_ARCHIVE"
```

With the checked pending declaration, `verify` must exit nonzero before either locator is read.

Run the focused unpaid static and synthetic tests from the confirmatory directory without creating
bytecode:

```bash
cd benchmarks/agent-brain/confirmatory
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest test_negative_control_cache_archive_v1.py
pyright negative_control_cache_archive_v1.py test_negative_control_cache_archive_v1.py
```

The suite may construct synthetic opaque bytes and manifests in temporary directories. It must not
create a real cache seed, extract any archive, fill disk, create candidate worktrees, run any of the
62 candidates, contact a model/provider, or use paid services.
