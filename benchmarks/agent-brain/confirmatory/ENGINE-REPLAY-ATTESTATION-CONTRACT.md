# Public-v4 and restricted replay attestation contract

Status: **implemented and fail-closed at `pending_owner_authorization`**. This document does not
authorize a production signing key, signer principal, restricted storage provider, publication,
download, upload, encryption operation, paid benchmark, or gate transition.

## Purpose and privacy boundary

The production engine gate needs two different kinds of evidence:

1. A privacy-safe public-v4 projection that can be published and independently checked without raw
   corpus, query, session, runtime, vector, stdout, stderr, host, or credential material.
2. An owner-controlled signed statement that a clean, exact-byte replay validated the restricted
   diagnostic evidence and the corresponding public projection with the frozen production inputs
   and checker sources.

Those objects are intentionally retained in separate roots. The restricted attestation is never a
member of the public archive, and the private diagnostic bundle is never copied into either root.
The legacy v1 descriptor remains byte-for-byte available only for diagnosis; its archive is
privacy-failed, not publishable, and cannot satisfy production or final freeze.

| Root | Exact content | Custody |
|---|---|---|
| `public-v4-v1/` | One schema-v4 manifest plus exactly five inventoried payload files | Candidate immutable GitHub release asset after owner approval |
| `restricted/` | `replay-attestation-v1.json` only | Owner-managed immutable restricted object, locally hydrated as owner-only mode `0600` |

## Checked-in contracts

- `engine-evidence-storage.json` is schema v2/profile
  `public_v4_restricted_replay_v1`. Its checked-in status is
  `pending_owner_authorization`, so all owner-selected/publication fields are null and all release,
  retention, replay, and trust decisions remain non-passing.
- `engine-replay-trust-roots.json` is intentionally pending with an empty root set. The repository
  contains no production private key, key-selection rule, principal choice, or approved public key.
- `engine-replay-checker-lock.json` pins the exact 12-source verification surface and its ordered
  aggregate. The storage descriptor binds both the raw lock hash and aggregate.
- `engine-evidence-storage-legacy-v1.json` and
  `schemas/engine-evidence-storage-v1.schema.json` preserve the retired diagnostic contract.
- The v2 storage, trust-root, checker-lock, and restricted-attestation schemas reject extra fields;
  runtime semantic validation additionally closes status-dependent nulls and cross-object bindings.

The private manifest is represented only by its fixed SHA-256 commitment
`b7c9baae2c3f0ed9b0773226ececf5bf129bd4dd487c02ffab9945cf0fc9c17d`. The public archive uses a
deterministic tar-zstd encoding with exact paths, modes, counts, logical byte totals, and the tree
algorithm `sha256_ordered_type_nul_path_nul_mode_nul_sha256_nul_size_newline_v1`. Links, special
files, alternate path spellings, collisions, PAX extensions, sparse members, missing files, and
surplus files are rejected.

## Signed envelope

The restricted object is canonical UTF-8 JSON plus one LF. Its envelope profile is
`restricted_engine_replay_attestation_v1`; floats and duplicate keys are prohibited. Signed bytes
are:

```text
"entire-brain/restricted-engine-replay-attestation/v1\0"
+ compact sorted-key JSON of the envelope without signed_payload_sha256 and signature
```

The signature is Ed25519 SSHSIG under namespace `entire-brain-engine-replay-v1`. Verification ignores
ambient `PATH` and invokes only the pinned Darwin system executable `/usr/bin/ssh-keygen` after
checking that the complete path is non-symlinked, root-owned, non-group/world-writable, regular, and
executable. The production verifier exposes no executable or signature-verifier injection point.
Trust-root IDs and signer principals are bounded literal ASCII tokens; whitespace, commas, wildcard
and allowed-signers metacharacters, quotes, controls, and option-column injection are rejected.
Timestamps use one exact RFC3339 profile: uppercase `T`/`Z`, timezone required, zero to six
fractional digits, and no unknown `-00:00` offset.

The signed statement binds exactly seven sections: issuance time, private diagnostic commitment,
public projection, production pin set and engine matrix, source/checker/analyzer identity, replay
result, and immutable public archive identity. Approved storage must match every signed projection,
archive, source, trust, and release field exactly.

## Verification and hydration

Production verification is local and offline. It performs this chain:

1. Validate v2 structure, status semantics, canonical pin/matrix/analyzer bytes, checker-source lock,
   trust-root bytes, and the preserved legacy descriptor binding.
2. Capture the exact six-file public tree once without following links; validate its tree and
   manifest commitments from those captured bytes.
3. Materialize that captured snapshot privately and run the public-v4 schema, semantic, inventory,
   and privacy checker against the snapshot, not a mutable original path.
4. Read the owner-only restricted attestation once; bind size, hash, canonical JSON, statement,
   trust root, principal, public key, signed payload, and real SSHSIG verification to that buffer.
5. Publish both hydrated roots together with a no-replace atomic rename only after the complete
   chain succeeds. Failure leaves no authoritative partial destination.

Public v4 alone always fails production. A legacy manifest/archive always fails production. A
pending v2 contract validates as preparatory structure but cannot hydrate authoritative evidence or
close `all_engines_machine_verified`. `check_protocol.py --freeze` therefore remains a hard no-go.

## Local unpaid commands

These commands do not authorize owner decisions:

```sh
# Validate the checked-in pending structure and all current lock bindings.
python3 benchmarks/agent-brain/confirmatory/hydrate_engine_evidence.py inspect-contract

# Deterministically package an already-created privacy-safe six-file public-v4 bundle.
python3 benchmarks/agent-brain/confirmatory/hydrate_engine_evidence.py package-public \
  --bundle "$PUBLIC_V4_OUTPUT" \
  --output "$LOCAL_PUBLIC_ARCHIVE"

# Synthetic fixture issuance only; never a production-key workflow.
python3 benchmarks/agent-brain/confirmatory/restricted_replay_attestation.py issue \
  --synthetic-fixture --statement "$FIXTURE_STATEMENT" \
  --private-key "$FIXTURE_KEY" --public-key "$FIXTURE_KEY.pub" \
  --trust-root-id fixture --principal fixture@example.invalid \
  --output "$FIXTURE_ATTESTATION"
```

After a separate owner authorization has populated and reviewed every approved field, the intended
local-only custody path is:

```sh
python3 benchmarks/agent-brain/confirmatory/hydrate_engine_evidence.py hydrate-all \
  --archive "$APPROVED_LOCAL_PUBLIC_ARCHIVE" \
  --attestation "$APPROVED_LOCAL_RESTRICTED_ATTESTATION"
python3 benchmarks/agent-brain/confirmatory/hydrate_engine_evidence.py verify-hydrated
python3 benchmarks/agent-brain/confirmatory/check_protocol.py
python3 benchmarks/agent-brain/confirmatory/check_protocol.py --freeze
```

`hydrate-public` and `hydrate-attestation` exist for isolated owner diagnostics. Neither establishes
the authoritative two-root gate. Network retrieval is deliberately absent from the v2 restricted
path; approved artifacts must be supplied as local files. The old downloader is reachable only as
`legacy-hydrate --allow-legacy-diagnostic` and remains non-production.

## Owner authorization still required

Before changing the status to `approved`, an owner must independently:

1. Select and authorize an Ed25519 public trust root, principal, validity window, and revocation
   policy without committing the private key.
2. Select restricted immutable storage and access/retention controls without placing provider URLs,
   credentials, query text, or key material in the descriptor.
3. Re-run the exact private and public validators from a clean pinned source identity; require zero
   errors; create and verify the signed statement.
4. Privacy-review the exact deterministic public archive, publish it as an immutable release asset,
   and record its API identity/digest and target commit.
5. Update all cross-bindings, trust-root raw hash, checker lock, and approved timestamps; review the
   resulting diff and rerun the unpaid suite from a clean checkout.

Until every step is explicitly authorized and evidenced, leave both checked-in contracts pending,
do not publish either legacy or restricted bytes, and do not mark any gate pass.
