# Multi-repository negative-control owner approval v1

This slice implements a local, offline, verifier-only Ed25519 SSHSIG boundary for a future owner
approval of the exact development negative-control contract E0. It does **not** contain an owner
key, authorize a run, consume an approval, execute a candidate, contact a model/provider, or enable
paid work. Its checked trust-root artifact is deliberately pending and empty.

The current verifier contract has status
`verifier_compiled_trust_root_pending_execution_forbidden`, execution status
`forbidden_missing_owner_approval_remaining_gates_and_atomic_consumption`, and
`execution_authority: false`. The current production `verify` path fails before reading the
caller-supplied approval or invoking SSHSIG because there is no approved trust root. A successful
`build` or `check` therefore proves only that the non-authorizing verifier contract and all of its
fixed dependencies are internally exact.

## Current pending boundary

The checked trust-root artifact
`negative-control-owner-approval-trust-roots-v1.json` contains:

- status `pending_owner_authorization`;
- zero roots;
- namespace `entire-brain-negative-control-approval-v1`;
- raw SHA-256 `07b515dddf74c53872c01f69cf2b1076820a10782dd9102cf235cf9691d89f1b`;
  and
- canonical self-hash
  `b3374aff490518f3f1eb142428810bb7791aeaf5a6a05cf16f63455d02908b37`.

Pending status requires an exactly empty root array. Approved status requires at least one active
root. The CLI has no trust-root path override, environment override, sign, key-generation,
authorize, consume, run, candidate, model/provider, or network command. Test fixtures may exercise
an approved in-memory contract only through the private `_verify_approval_with_dependencies` seam;
public `verify_approval` and `verify_approval_file` always load the fixed checked production
dependencies. Synthetic fixtures are not a production trust source.

The checked verifier contract
`negative-control-owner-approval-verifier-contract-v1.json` records all authority and runtime
fields as non-passing:

- no approval envelope or owner key;
- `approval_signature_verified: false`;
- `atomic_consumption: false`;
- benchmark and candidate execution forbidden;
- model/provider and paid execution forbidden; and
- null approval, run, consumption-receipt, and verification-time runtime bindings.

Its raw SHA-256 is
`d19ec3df382070c743e2b466175c661d91d03001d15c0de561d9e700df6c5569`; its canonical self-hash is
`ed22928cfe42e29ba57f3c58fab62eecebe9fd1bee010a2708fe02773ec35093`.

## Fixed E0 and implementation identities

The verifier accepts no alternate execution contract. It binds the E0 artifact produced at source
commit `f0552070921605cd9a0165ee29ca96e100675d58`:

| Identity | Fixed value |
| --- | --- |
| E0 artifact | `development-task-negative-control-execution-contract-v1.json` |
| E0 raw SHA-256 | `94422af8a5e24c6a855b66cf40868565ee3518d0c75eb5343b5d2450523b2c1e` |
| E0 canonical self-hash | `1e290b91d5e0be42f750bf0e655edee48d297f86ad3e3867b8fe9177fd5125ef` |
| E0 builder SHA-256 | `f3cfb82978b0ce98d17999ef4cb4f4089a425bb528ece4ff53b61a03a4344bcd` |
| E0 schema SHA-256 | `dbcb95119feeda4c436e90d8162ac0498577454ca9ccaab7a8254d55d90359f9` |
| Candidate projection | `4fbde9876d4c91e9cedcf994de9cecd5442beee928c1cadc505116a25c2bef60` |
| Schedule | `c818e8ad87f4d6ff5fac8839f4296fd86da13eaf95dadf8b2c789c7254bfdbcd` |
| Population | 3 repositories, 62 candidates, 248 attempts |
| E0 status | `contract_compiled_execution_forbidden` |
| E0 execution status | `forbidden_missing_all_residual_gates_and_audited_execution_adapter` |

The verifier also canonically hashes and checks E0's authority, runtime bindings, protocol, state
machine, enforcement requirements, and residual gates. A modified contract that merely refreshes
its self-hash still fails the fixed raw-hash and component-binding checks.

The current verifier implementation identities are:

| Component | SHA-256 |
| --- | --- |
| `negative_control_owner_approval_v1.py` | `569045e1c15366f93500fa030c8fcb1214c7c6a6e912a8b5499a32690e484119` |
| Pinned Draft 2020-12 validator | `8688b67468b096427f758163174432e221d8197c6b13870a6427939f6ea281eb` |
| Approval-envelope schema | `1300806efed4cedfccbbd68291676f71eabfc1296dbbf054a721e18a20a4d675` |
| Trust-roots schema | `54dd9ccd634863cfbc050221d2394b54326e03a6e465ebf807e358113f37158d` |
| Verification-report schema | `352ceeb16bc398c4241c0c3347b021a08c4ea16948f28d9d34e28965257e1594` |
| Verifier-contract schema | `e3b8e95e62cda252434e11bf190e4f248526486ba3c1f63415ce78d78f2b5707` |

Schema bytes, source bytes, E0 bytes, trust-root bytes, the system SSH verifier, and the rebuilt
verifier contract are all checked before an approval can reach SSHSIG verification. The schema
validator itself is fixed to the adjacent 18,097-byte `draft202012.py`; its fixed source path,
raw hash, and size are checked before every schema validation. On first use, the verifier compiles
and executes those exact checked source bytes in a private module namespace and validates the
module's dialect and required API; later validations reuse that exact-source module only while the
source identity continues to pass. It never imports `draft202012` through Python's timestamp-based
bytecode cache.

## Exact approval statement

The envelope profile is `agent_brain_negative_control_owner_approval_envelope_v1`; the signed
statement profile is `agent_brain_negative_control_owner_approval_statement_v1`. The statement's
action is narrowly fixed to
`approve_exact_development_negative_control_run_subject_to_all_residual_gates_and_atomic_consumption`
with scope `exact_e0_contract_one_development_run_v1`.

The statement binds:

- the complete fixed E0 identity above;
- the exact **approved** verifier-contract raw and self hashes;
- one trust-root ID, constrained signer principal, and signer public-key identity;
- one approval ID and one distinct run ID and run-nonce hash;
- the exact 3-repository, 62-candidate, 248-attempt run identity;
- candidate and schedule hashes;
- repository order `entire-brain`, `entire-db`, `entire-graph`;
- arm order `baseline`, `first_parent_source_reversal` with two repetitions per arm;
- maximum concurrency 1, 600 seconds per arm, and 28,800 seconds total;
- the full prohibition set, remaining-gate effect, and external atomic-consumption requirement; and
- canonical issuance, activation, and expiry timestamps.

The signed bytes are exactly:

```text
b"entire-brain/negative-control-execution-approval/v1\0"
+ compact sorted-key UTF-8 JSON of the statement
```

The payload-domain SHA-256 is
`15ccf62b85c5e24f02f8eb0d04183ffe6abb142284417784cce86f742faa2f37`.
The signature namespace is `entire-brain-negative-control-approval-v1`; the signature scheme is
`sshsig_ed25519_sha512_v1`.

The envelope's `approval_sha256` is the compact canonical hash of the complete envelope with only
that field set to null. `run_identity_sha256` is the compact canonical hash of the run-identity
object. `signed_payload_sha256` hashes the domain-prefixed statement bytes above. A trust root's
`public_key_sha256` is the SHA-256 of the exact canonical OpenSSH public-key line
`ssh-ed25519 <base64>\n`, not of a comment or an authorized-keys options field.

Checked contracts and trust roots use sorted, indented UTF-8 JSON plus one LF. An approval file uses
compact, sorted UTF-8 JSON plus one LF. Duplicate keys, floating-point values, non-finite values,
surrogate code points, noncanonical bytes, excessive integers, more than 32 levels, or more than
20,000 JSON nodes fail closed. Approval bytes are capped at 128 KiB and the domain-prefixed signed
payload at 64 KiB. The verifier also reconstructs the complete canonical envelope and requires it
to equal the supplied raw approval bytes before any cryptographic subprocess.

## SSHSIG, time, and filesystem threat model

Verification is local and offline. The boundary is designed against an unprivileged local attacker
who can control ambient `PATH`, supply a malformed approval, try principal or allowed-signers
injection, race mutable input paths, or replace repository artifacts without updating the reviewed
source pins.

### SSHSIG

Only `/usr/bin/ssh-keygen -Y verify` may be invoked. The current binary identity is:

- SHA-256 `bddae9c4ea46fd903574ec6ff61eda75e133f940fa538f2adca80af474767596`;
- 849,024 bytes;
- UID/GID 0/0; and
- mode `0755`.

Every component from `/` to the executable is checked as non-symlinked, root-owned, root-grouped,
and not group/world writable. The leaf must be a regular file with the exact hash, size, and mode.
Identity is checked before and after the subprocess. `PATH` is not consulted for executable
selection and there is no executable override. A changed macOS system binary fails closed and
requires a reviewed source-pin rotation.

The verifier decodes and structurally checks the SSHSIG object before subprocess invocation:

- exact `SSHSIG` magic and version 1;
- exact trusted Ed25519 public-key blob;
- exact namespace;
- empty reserved field;
- SHA-512 payload hash;
- `ssh-ed25519` signature algorithm;
- exactly 64 signature bytes; and
- no trailing wire bytes.

Armor is ASCII, at most 16 KiB, canonically Base64-encoded and wrapped at 70 columns, with exact
begin/end markers. The JSON string has no terminal newline; the private temporary signature file
gets exactly one. The principal must match
`^entire-brain-negative-control-owner-[a-z0-9][a-z0-9._-]{0,63}$`, preventing whitespace, comma,
wildcard, option-column, quote, control-character, and leading-dash injection. The generated
`allowed_signers` file contains one literal line only.

The temporary directory is mode `0700`; `allowed_signers` and signature files are exclusively
created as `0600`. The subprocess receives only `LANG=C`, `LC_ALL=C`, and `PATH=/usr/bin`, uses
`shell=False`, receives the signed payload on stdin, discards output, and is killed as a process
group after ten seconds.

### Time and replay

Times are exact UTC seconds in `YYYY-MM-DDTHH:MM:SSZ` form. The statement must satisfy
`issued_at <= not_before < expires_at`, with no more than 60 seconds from issuance to activation and
no more than 900 seconds of active lifetime. The root window is at most 366 days and must contain
the statement's complete issuance-through-expiry window.

System UTC is sampled immediately before and after SSHSIG verification. Both the root and approval
must be active at both checks; a backward clock movement fails closed. The CLI exposes no clock
override. A revoked root is never accepted based on a signer-controlled historical timestamp.

A nonce and run identity scope an approval but do not consume it. Verification reports
`unconsumed_no_atomic_replay_store`; repeated verification is not evidence of single use. A future
executor must perform an external crash-safe atomic compare-and-set over the approval and payload
identities, recheck validity before any external effect, and bind the durable consumption receipt to
the exact E0 and run identity. That mechanism is not implemented here.

### Filesystem and residual assumptions

JSON inputs are opened component-by-component with descriptor-relative `O_NOFOLLOW`, must be regular
files, are bounded before parsing, and are read from one descriptor while pre/post identity is held
constant. Parent traversal is rejected. The future approval leaf must be owned by the current user,
have exact mode `0600`, and have one hard link. Fixed trust, schema, and E0 files are additionally
protected by exact source-pinned raw hashes.

The `/usr/bin/ssh-keygen` pathname cannot be executed directly from the already-verified descriptor
on this host, so the security argument also relies on its root-owned, non-writable path. A concurrent
root attacker, a compromised owner private key, a compromised system clock, and cryptographic
breaks are out of scope. No claim is made about a future executor or replay store because neither is
present.

## Local build, check, and verify commands

Run from the repository root. These commands do not run a candidate benchmark or authorize paid
work.

Build a deterministic verifier contract into a non-authoritative temporary path:

```bash
python3 benchmarks/agent-brain/confirmatory/negative_control_owner_approval_v1.py build \
  --output /private/tmp/negative-control-owner-approval-verifier-contract-v1.json
```

Check the checked contract and every exact dependency:

```bash
python3 benchmarks/agent-brain/confirmatory/negative_control_owner_approval_v1.py check \
  benchmarks/agent-brain/confirmatory/negative-control-owner-approval-verifier-contract-v1.json
```

The current result is:

```json
{"execution_authority":false,"execution_status":"forbidden_missing_owner_approval_remaining_gates_and_atomic_consumption","profile":"agent_brain_negative_control_owner_approval_verifier_contract_v1","status":"verifier_compiled_trust_root_pending_execution_forbidden"}
```

The same checker can validate a just-built temporary artifact:

```bash
python3 benchmarks/agent-brain/confirmatory/negative_control_owner_approval_v1.py check \
  /private/tmp/negative-control-owner-approval-verifier-contract-v1.json
```

The future verification command is:

```bash
python3 benchmarks/agent-brain/confirmatory/negative_control_owner_approval_v1.py verify \
  "$OWNER_APPROVAL_ENVELOPE"
```

Today it necessarily exits nonzero with `approval verifier trust root is pending`. This occurs
before the approval path is read and before `/usr/bin/ssh-keygen` is invoked. Once a reviewed trust
root exists, the supplied envelope must be a current-user-owned, single-link, exact-mode-`0600`,
compact canonical file. There is intentionally no issuance or signing command in this module.

## Non-authorizing verification report

Even a cryptographically valid future approval produces only one compact JSON report on stdout. It
is not persisted and is not an approval-consumption receipt. The report schema fixes:

- status `owner_approval_signature_verified_execution_forbidden`;
- `signature_verification: valid`;
- `execution_authority: false`;
- `atomic_consumption: false`;
- `replay_status: unconsumed_no_atomic_replay_store`;
- `run_binding_status: signed_statement_verified_not_bound_to_executor`;
- execution status `forbidden_missing_remaining_gates_and_atomic_approval_consumption`; and
- candidate, model/provider, and paid execution all forbidden.

The report self-hashes its approval file, approval envelope, signer, trust-root, run, expiry, and
verification-time bindings, but it cannot be substituted for the missing atomic consumption or an
execution adapter. The verifier never mutates E0's null runtime bindings.

## Reviewed trust-root authorization, rotation, and revocation

Changing the pending boundary is an explicit owner and source-review operation, not a runtime CLI
operation:

1. The owner selects an Ed25519 public key, root ID, constrained principal, validity window, and
   revocation policy outside this repository. The private key is never committed, copied into this
   worktree, or passed to this verifier.
2. Encode only the canonical SSH Ed25519 public-key wire blob in `public_key_base64`. Set
   `public_key_sha256` to the hash of `ssh-ed25519 <base64>\n`. Set purpose exactly to
   `negative_control_execution_approval_v1`.
3. Use canonical UTC-second validity values. An active root has `revoked_at: null`; a revoked root
   has a `revoked_at` inside its validity window. Root lifetime cannot exceed 366 days.
4. Keep at most eight roots, with unique IDs, principals, and key identities. Set the aggregate
   status to `approved` only when at least one root is active. Pending status may never contain a
   root.
5. Recompute the trust-root self-hash over compact canonical JSON with only
   `trust_roots_sha256` set to null, then render the artifact as sorted indented UTF-8 JSON plus one
   LF and compute its raw SHA-256.
6. Update the reviewed source pins for the trust-root raw and self hashes. Do not add a CLI,
   environment, or caller-controlled path override.
7. Rebuild the verifier contract. Review the resulting approved status, root count, trust hashes,
   builder hash, contract raw hash, and contract self-hash. `execution_authority` and all runtime
   bindings must remain false/null.
8. Run the exact checker and dedicated unpaid unit suite from a clean checkout. Review that the diff
   contains public-key material only and no private key, signature, credential, owner path, or
   benchmark output.
9. Only separately reviewed owner tooling may create an approval envelope. The envelope must bind
   the exact approved verifier-contract raw/self hashes and the exact one-run E0 statement. This
   repository deliberately provides no signing or key-generation workflow.

For a rotation, add and review the new active public root before retiring the old one. Once the new
root is usable, mark the old row `revoked`, provide a valid `revoked_at`, retain at least one active
root, recompute both trust identities, refresh the reviewed source pins, and rebuild the verifier
contract. Because each envelope binds the exact approved verifier contract, a trust-root change
also requires a newly bound owner statement; an old envelope does not silently carry forward.

## Tests

The dedicated owner-approval suite is
`benchmarks/agent-brain/confirmatory/test_negative_control_owner_approval_v1.py`. Run it from the
repository root without creating bytecode:

```bash
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=benchmarks/agent-brain/confirmatory python3 -m unittest \
  benchmarks/agent-brain/confirmatory/test_negative_control_owner_approval_v1.py
```

The current 23-test suite covers deterministic build/check and the pending boundary; pending
rejection before approval reads or subprocess dispatch; the exact three-command CLI; non-authorizing
valid-fixture reports; domain-separated payload construction; semantic, nested-hash, E0, verifier,
run, signer, namespace, and signature tampering; timestamp shape, ordering, activation, lifetime,
future, and expiry failures; pending, revoked, duplicate, mismatched, and out-of-window roots;
duplicate keys, floats, non-finite values, surrogates, and JSON complexity limits; compact canonical
approval bytes, mode `0600`, and single-link enforcement; symlink, traversal, FIFO, and oversize
input rejection; strict SSHSIG armor/wire parsing; and exact subprocess binary, argv, environment,
private-file modes, timeout, process-group kill, and no-fallback behavior. An isolated regression
fixture proves that even a forged timestamp-valid `draft202012.pyc` accepted by normal import
machinery is ignored by the verifier's exact-source loader.

Synthetic keys and approvals may exist only inside isolated test fixtures. They are not owner
authorization and must not populate the checked production trust-root artifact. No test may run any
of the 248 candidate attempts.

## Next gate

The dedicated adversarial suite currently passes against the exact pending artifacts. The next
governance gate is an explicit owner review and authorization of public trust-root material through
the rotation procedure above. That permits signature verification only. It still does not permit
execution.

After a valid signature, all eight residual gates remain:

- approved offline cache seed/archive binding;
- safe archive traversal, type, link, device, and content verification;
- trusted APFS observation and atomic capacity reservation;
- clean detached-worktree execution and first-parent reversal proof;
- attested attempt producer and classifier integration;
- private-log execution, retention, and aggregate accounting;
- fail-closed interruption cleanup and no-receipt proof; and
- atomic single-use approval consumption bound to the exact execution.

Do not start the 248-attempt development run until every residual gate is independently implemented,
audited, bound into an executable contract, and atomically coupled to the still-valid one-run owner
approval.
