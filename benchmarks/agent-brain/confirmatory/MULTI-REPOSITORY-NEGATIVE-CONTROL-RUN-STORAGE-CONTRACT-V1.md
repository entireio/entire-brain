# Multi-repository negative-control run-storage contract v1

This slice freezes a storage, journal, recovery, and cleanup contract for the exact development
negative-control execution contract E0. It is build/check-only and permanently non-executing. It
does not create a control root, consume an approval, persist a journal, observe or reserve capacity,
stage a cache, create a worktree, run a candidate, write a private log, clean a run root, publish an
aggregate receipt, contact a network or model/provider, or authorize paid work.

The checked contract status is
`run_storage_contract_compiled_runtime_unbound_execution_forbidden`; its execution status is
`forbidden_contract_only_no_atomic_consumption_no_storage_adapter`. Its authority block freezes all
operational capabilities off:

- `atomic_consumption: false` and `owner_approval: false`;
- execution authority false;
- benchmark, candidate, model/provider, paid, and network execution forbidden; and
- filesystem mutation, journal persistence, reservation, staging, worktree creation, cleanup,
  recovery, and run-storage mutation forbidden.

The exact authority object is:

```json
{
  "atomic_consumption": false,
  "benchmark_execution": "forbidden",
  "cache_staging": "forbidden",
  "candidate_execution": "forbidden",
  "capacity_reservation": "forbidden",
  "cleanup": "forbidden",
  "execution_authority": false,
  "filesystem_mutation": "forbidden",
  "journal_persistence": "forbidden",
  "model_provider_execution": "forbidden",
  "network_access": "forbidden",
  "owner_approval": false,
  "paid_execution": "forbidden",
  "recovery": "forbidden",
  "run_storage_mutation": "forbidden",
  "worktree_creation": "forbidden"
}
```

Every runtime identity and locator remains null. The three adjacent runtime-artifact schemas describe
or constrain future evidence shapes but supply no adapter, receipt, attestation, trusted observation,
or authority. No runtime instance is created by this slice.

## Exact E0 and predecessor bindings

The contract accepts no alternate E0. It binds:

| E0 identity | SHA-256 |
| --- | --- |
| `development-task-negative-control-execution-contract-v1.json` raw bytes | `94422af8a5e24c6a855b66cf40868565ee3518d0c75eb5343b5d2450523b2c1e` |
| canonical E0 self-hash | `1e290b91d5e0be42f750bf0e655edee48d297f86ad3e3867b8fe9177fd5125ef` |
| builder source | `f3cfb82978b0ce98d17999ef4cb4f4089a425bb528ece4ff53b61a03a4344bcd` |
| schema | `dbcb95119feeda4c436e90d8162ac0498577454ca9ccaab7a8254d55d90359f9` |
| candidate projection | `4fbde9876d4c91e9cedcf994de9cecd5442beee928c1cadc505116a25c2bef60` |
| schedule | `c818e8ad87f4d6ff5fac8839f4296fd86da13eaf95dadf8b2c789c7254bfdbcd` |
| protocol | `3e5cef5c19836b61f5739347f4d0db25564b859cd26fc6c5761f81072b2f3f01` |
| state machine | `31015d9150afaf6c73cbea84dc647039449cc5bfc498ac0efedddfabad271207` |
| enforcement requirements | `c23f32b102077254ef4eae322faa12101e3e24d688808ebf6bca31cf128185e2` |
| residual gates | `650f570c5fe096626381171dd6d9d2f0ca77c4a6c13e6994c783ee3a82d5cb91` |
| authority | `422d6742432af499f9aec5e6b7ecc9d86ac0cf901e2accd0fedb611c580176de` |
| runtime bindings | `938f7a76faf1d0a3490db06fbb5ac5b1100f2967a24b0bc846ce40c3c762b634` |

The source run plan remains `pending_owner_authorization`. Its raw/self hashes are
`f55b6e22a25daf8016305304adf3b7ac8d31675281d97cfc9bb78cc76b619790` and
`a47311fa1f4553f8085ebea6e8ddd003a4ea5d0b2d269d91bbcdd414134a248e`.
The plan schema is
`294f77f165676bec6053e9637578ca22f048a887d2a978bbf8b8e300f21a2dad`.

The checked owner-verifier contract remains
`verifier_compiled_trust_root_pending_execution_forbidden`: its trust-root artifact is pending and
contains zero roots. A synthetically valid signature report would still have
`atomic_consumption: false`, `execution_authority: false`, and status
`owner_approval_signature_verified_execution_forbidden`. Signature verification is not consumption.
The contract distinguishes the canonical `approval_self_sha256`, exact approval-file raw hash, and
signed-payload hash; a later atomic claim must bind all three rather than collapsing them into one
ambiguous approval hash.

The cache-bundle verifier remains
`verifier_compiled_raw_material_source_review_pending_bundle_access_forbidden`. The v1 material
declaration is `pending_source_review`, so no actual manifest, bundle, raw-identity report, or
content-verification report exists. The format and verifier bindings do not become cache material or
an E0 runtime binding.

## Approval consumption is journal genesis

For a new run, fixed dependencies, the approval signature and validity window, input identities, and
the stable control-root metadata are checked without persistent mutation. The first persistent run
effect must be one atomic compare-and-set claim. That claim record itself is journal record zero and
carries the exact E0 transition:

```text
contract_checked_execution_forbidden -> authority_and_inputs_verified
```

There is no separate “consume approval, then create genesis” write. An adapter that cannot atomically
persist the single-use claim and complete genesis payload remains forbidden. The claim must bind the
approval-file raw hash, approval self-hash, signed-payload hash, approval-verification report, owner
trust identity, run ID and nonce-derived run identity, exact E0 raw/self hashes, and exact
run-storage-contract self-hash. The approval is rechecked as current immediately before the claim.

E0 names `contract_checked_execution_forbidden` as the run-journal initial state and permits an edge
from it to `run_aborting`. Persisting a standalone initial record before consumption would violate
approval-before-effect. This contract therefore treats that state as logical/read-only before a run
starts. A pre-claim validation failure creates no run. The atomic claim/genesis record is the first
durable record; after it commits, interruption recovery observes
`authority_and_inputs_verified` and irreversibly selects the abort/no-receipt path.

Consumption is never rolled back. A crash, failed reservation, failed staging, candidate
infrastructure error, cleanup failure, or missing aggregate receipt leaves the approval spent. It
cannot authorize a retry, replacement attempt, altered schedule, or second run.

## Durable hash chain and exact E0 transitions

The future stable control root is outside the disposable per-run root. The control root and lock must
be separately provisioned and reviewed before a run; this contract does not create them. Under one
exclusive run lock, future journal records are immutable, canonical, exclusively created files with
contiguous ordinals and a `previous_record_sha256` chain. Each ordinary external effect uses:

1. a complete `PREPARED` record durably published before the effect;
2. the external effect;
3. exact post-effect verification; and
4. a `COMMITTED` record binding the prepared record and verification evidence.

The journal is bounded to at most 20,000 records, at most 65,536 canonical bytes per record, and at
most 67,108,864 canonical bytes in total. Ordinals are contiguous and zero-based, with no gaps or
duplicates. Only the canonical record bytes count and hash; immutable exclusive-create publication
never replaces an existing ordinal.

The atomic claim/genesis record is the sole exception: its compare-and-set publication is both the
single-use effect and record zero. Publication requires file synchronization followed by directory
synchronization; unsupported durability primitives fail closed. Replacement publication is
forbidden.

The run transition graph remains exactly E0's graph:

```text
contract_checked_execution_forbidden -> authority_and_inputs_verified | run_aborting
authority_and_inputs_verified         -> capacity_reserved | run_aborting
capacity_reserved                     -> attempts_active | run_aborting
attempts_active                       -> attempts_active | run_cleaning | run_aborting
run_cleaning                          -> reservation_released | run_cleaning_no_receipt | cleanup_failed_latched
reservation_released                  -> aggregate_receipt_committed | reservation_released_no_receipt
run_aborting                          -> run_cleaning_no_receipt | cleanup_failed_latched
run_cleaning_no_receipt               -> reservation_released_no_receipt | cleanup_failed_latched
reservation_released_no_receipt       -> cleaned_no_receipt
```

The per-attempt graph also remains exact:

```text
attempt_bound          -> attempt_staged | attempt_aborting
attempt_staged         -> attempt_running | attempt_aborting
attempt_running        -> attempt_result_private | attempt_aborting
attempt_result_private -> attempt_cleaning | attempt_aborting
attempt_cleaning       -> attempt_cleanup_committed | cleanup_failed_latched
attempt_aborting       -> attempt_cleaning | cleanup_failed_latched
```

The schedule cursor begins at zero and advances by exactly one only after the matching attempt's
cleanup commits and aggregate counters are durably updated. A captured candidate exit or timeout is
data; infrastructure, journal, attestation, staging, logging, cleanup, or external interruption is an
abort. Aggregate receipt publication is reachable only after cursor 248, all ceilings pass, final
cleanup commits, the reservation is released, and the irreversible receipt-forbidden latch remains
false. `aggregate_receipt_committed` has no outgoing transition.

## Restart recovery

Before any new run, one exclusive control-root lock protects full-chain validation and recovery. An
unknown record kind, malformed canonical bytes, missing or duplicate ordinal, broken predecessor or
prepared-record hash, invalid E0 transition, partial publication, unexpected root entry, or
unverifiable effect evidence fails closed to `cleanup_failed_latched`.

Recovery of any consumed nonterminal run first durably changes the receipt-forbidden latch from false
to true. It never resumes or reruns an attempt and can never publish an aggregate receipt. It may
only verify prior effects, clean disposable state, release a reservation, and reach
`cleaned_no_receipt` or `cleanup_failed_latched`. These recovery effects derive authority only from
the already durable consumption/genesis record; they are not a new run and do not consume a second
approval.

## Capacity arithmetic and unresolved APFS mechanism

The exact frozen planning rule is:

```text
17,179,869,184 bytes minimum before staging
= 8,589,934,592 bytes maximum staging envelope
+ 8,589,934,592 bytes residual free-space requirement
```

This is `16 GiB = 8 GiB + 8 GiB`; it is not evidence that 16 GiB, or even 8 GiB, has been physically
reserved. `df`, Foundation important-use capacity, a logical file length, sparse allocation, and
`ftruncate` do not reserve blocks.

The APFS allocation mechanism is deliberately unresolved in v1. In particular, holding an
independent 8 GiB reservation while also allocating 8 GiB of staging from a 16 GiB preflight would
leave no 8 GiB residual reserve. A later reviewed design must either:

- prove a spendable conversion invariant in which
  `reservation_remaining_bytes + staging_actual_bytes == 8 GiB` with no release-before-allocation
  race and at least 8 GiB still free after every effect; or
- raise the preflight to 24 GiB for an independent 8 GiB hold plus 8 GiB staging plus 8 GiB reserve.

Consequently the v1 capacity-receipt schema freezes mechanism, adapter, observation, device,
reservation, and accounting identities as null and status
`capacity_reservation_receipt_shape_frozen_mechanism_unresolved_execution_forbidden`. It cannot
validate a successful live reservation receipt. A later reviewed mechanism requires a successor
contract/schema.

## Descriptor-relative cleanup and path policy

Every control, journal, staging, cache, worktree, reversal, cleanup, and derived runtime component is
validated separately. Paths are descriptor-relative; absolute paths, parent traversal, dot and
dot-dot components, backslashes, NUL, Unicode category-C controls, ancestor conflicts, and portable
collisions are forbidden. Every component rejects
`casefold(NFC(component)) == "..namedfork"`. It is not enough for the bundle verifier alone to apply
this rule.

Future disposable-root cleanup is same-device, descriptor-relative, no-follow postorder traversal.
The runtime adapter must freeze and enforce reviewed entry, depth, component-byte, and total-path-byte
ceilings before it can satisfy this contract; this build/check-only slice does not claim those runtime
bounds exist. Symlinks, hard links where a single link is required, mount/device crossings, FIFOs,
sockets, devices, resource-fork pseudo-paths, unexpected names, mutation races, or failed directory
synchronization fail closed. The stable control root, permanent consumption records, and journal
evidence are not children of the disposable root and cannot be removed by disposable cleanup.

The cleanup-attestation schema records evidence only and has no execution, recovery, or aggregate
receipt authority. Cleanup failure requires `cleanup_failed_latched`; successful cleanup evidence is
still only one input to a separate final receipt commit.

## Runtime schema boundary

- `negative-control-run-journal-event-v1.schema.json` describes immutable GENESIS, PREPARED, and
  COMMITTED evidence records. Its authority block states that an event is never standalone execution
  or receipt authority. A runtime checker must additionally enforce contiguous chaining, exact E0
  transitions, PREPARED/COMMITTED pairing, counter monotonicity, and canonical self-hashes.
- `negative-control-capacity-reservation-receipt-v1.schema.json` is pending-only because no reviewed
  APFS mechanism exists. All live observation, adapter, reservation, run, and receipt identities are
  null.
- `negative-control-cleanup-attestation-v1.schema.json` defines a non-authorizing future evidence
  shape for successful cleanup or a latched failure. No attestation is created here.

Schemas distinguish structural validation from trusted runtime production. Passing a schema never
authenticates its producer, proves an external effect happened, closes an E0 residual gate, or grants
authority.

The pinned attempt-observation and classification-receipt schemas use Draft 2020-12
`prefixItems`, which the checked local validator does not implement. Their raw files and hashes stay
exactly bound, but this slice does not silently claim to have meta-audited them. Execution remains
forbidden until a checked validator covering `prefixItems` audits both schemas.

## Checked identities and reproduction

| Checked file or identity | SHA-256 |
| --- | --- |
| `negative-control-run-storage-contract-v1.json` raw bytes | `a1effd9ea44f5e13bffc516d1854cdad52936d2afbcbad7534b28b7ed81da671` |
| contract canonical self-hash | `a563d6f7ed1deaeb40676dd567f2d23fbd7f827ca2719aeb83f7e4973af4f852` |
| `negative_control_run_storage_contract_v1.py` | `1d96885d0e9a7a2ce675fd29657fb300abf43b44e4c41d1e168d564ebd83aed6` |
| run-storage contract schema | `0036e470d0dcf5563877db9bed68a7d679e88dcc9169c016692b1f1d756f975e` |
| journal-event schema | `8eceb818bb1bb6fedb7a3d0bc8f9911d1305c5f43dad5b02ea3b18f6d7f64d5f` |
| capacity-reservation-receipt schema | `fb0c5cba1f72184f1192bf17b115636259940fe573e02676b4d2e4598e6b9645` |
| cleanup-attestation schema | `81ae0a8aa8ceb91d664c25117507150442bc4f9288bf17419286d49ac58f812a` |

From `benchmarks/agent-brain/confirmatory`:

```sh
python3 negative_control_run_storage_contract_v1.py build --output /tmp/negative-control-run-storage-contract-v1.json
cmp /tmp/negative-control-run-storage-contract-v1.json negative-control-run-storage-contract-v1.json
python3 negative_control_run_storage_contract_v1.py check
python3 test_negative_control_run_storage_contract_v1.py
pyright negative_control_run_storage_contract_v1.py test_negative_control_run_storage_contract_v1.py
```

## Remaining gates

Execution remains forbidden pending all of the following:

- separately established owner trust roots and an exact current approval;
- source-reviewed actual manifest/bundle material and successful v2 same-descriptor verification;
- an audited atomic single-use consumption/genesis adapter;
- the durable journal/recovery/secure-cleanup implementation and attestation producer;
- a reviewed APFS observation, spendable reservation or revised-budget mechanism, release, and crash
  recovery;
- a separately audited third-pass cache stager and complete staged-tree verification;
- the detached-worktree executor and exact first-parent source-reversal proof;
- attested attempt production and classifier integration;
- private-log retention, accounting, and executor integration;
- fail-closed interruption cleanup and the no-receipt guarantee; and
- a checked `prefixItems` schema auditor for the pinned attempt and classification schemas.

None of the 248 scheduled attempts ran while compiling this contract.
