# Multi-repository negative-control run plan v2

This slice freezes the next development-only negative-control execution plan without implementing
or authorizing execution. No candidate process, candidate test, detached worktree, source reversal,
model/provider call, calibration action, or holdout action ran while producing this artifact.

The checked plan is `development-task-negative-control-run-plan-v2.json`. It is intentionally
`pending_owner_authorization` and unexecutable. `task_negative_control_plan_v2.py` has only `build`
and `check` lanes; neither lane invokes Git, Go, a candidate command, or an executor. The checker
rejects an `authorized` status even if an attacker recomputes the plan self-hash because no separate
approval receipt or trust mechanism exists.

## Exact frozen inputs

The plan binds the raw file SHA-256, canonical ledger self-hash, ordered candidate references,
first-parent positions, pinned base/head, and exact repository-specific Git/Go/native toolchains for:

| Repository | Candidates | Go toolchain |
| --- | ---: | --- |
| `github.com/entireio/entire-brain` | 18 | Go 1.26.2, darwin/arm64 |
| `github.com/entirehq/entiredb` | 25 | Go 1.26.4, darwin/arm64 |
| `github.com/entireio/entire-graph` | 19 | Go 1.24.13, darwin/arm64 |

The global order is exactly the three rows above and then increasing first-parent position within
each row: 62 unique development-only candidate references in total. All candidates remain
`structurally_ready_not_executed`. The plan also binds the repository contract, v2 scanner,
eligibility schema, global overlap registry raw/self hashes, overlap builder, and overlap schema.
The overlap binding additionally verifies the current CLI scanner and canonical-JSON helper bytes,
and requires the registry's Git binary/version identity to equal the one exact Git binding shared
by all three repository contracts. Rebuilding requires exact current bytes; the checked artifact
must also match the deterministic sorted-key, two-space-indented UTF-8/LF rendering byte for byte.
The JSON Schema contains an exact generated `const` projection of all three ordered repository rows
and all 62 candidate references/positions, so it independently rejects count, order, identity, and
toolchain drift instead of accepting a merely well-shaped replacement.

## Frozen protocol and resource ceiling

The future protocol is frozen at two repetitions per arm, baseline before first-parent source
reversal, one candidate process at a time, a 600-second process-group outer deadline per arm, and an
eight-hour total wall-clock ceiling. The Go test internal timeout must be disabled so it cannot be
misclassified as a causal reversal failure.

Execution must not begin unless a future preflight proves at least the 8 GiB free-disk reserve plus
the 8 GiB total staging ceiling. Additional exact ceilings are 1 GiB per worktree, 2 GiB per arm
cache, 2 GiB for the cache seed, 16 MiB raw output per arm, 2 GiB total private raw logs, and 16 MiB
total public receipts. These are plan limits, not claims that the current host passes preflight.
A separate v1 primitive now checks this arithmetic from explicitly injected, unattested filesystem
statistics, but it does not observe or reserve host capacity. The trusted host adapter/reservation,
bounded executor, cleanup attestation, and interruption handling do not exist yet. See
`MULTI-REPOSITORY-NEGATIVE-CONTROL-GATE-PRIMITIVES-V1.md`.

## Honest isolation and log boundary

The plan records both OS network enforcement and OS filesystem enforcement as `not_enforced` and
makes no sandbox claim. A future executor must disable network dependency resolution and start every
arm/repetition from a fresh private copy of a verified offline cache seed. Reuse of ambient host Go
caches is forbidden.

The required cache-seed manifest hash, archive hash, byte count, and source locator are all null;
none was fabricated. A schema-bound pure manifest/check primitive now exists, but no actual archive
exists and an injected identity observation is explicitly not safe archive traversal/content
verification. That primitive is pinned to this plan's exact raw/self hashes and exact dependency
rebuild; its v1 planning envelope permits at most 250,000 non-empty files and a 128 MiB canonical
manifest, with global NFC/case-fold collision and ancestor rejection. Those limits still require
validation against a real safely inspected seed before any future authorization. The future raw-log
root is likewise runtime-only and null in this host-free plan. Its
required directory/file modes are `0700`/`0600`, raw logs are retained for seven days and addressed
by exact-byte SHA-256, and public receipts may expose only each raw-log hash and byte count. Raw test
output can contain secrets, so this plan makes no redaction claim and forbids putting raw output in a
public plan or receipt. A subsequent pure content-addressed private writer/checker and a pure public
attempt classifier now exist, but neither is integrated with an executor or provides retention,
cleanup, or execution attestation. See `NEGATIVE-CONTROL-PRIVATE-RAW-LOG-V1.md` and
`MULTI-REPOSITORY-NEGATIVE-CONTROL-CLASSIFICATION-V1.md`.

## Remaining hard gates

All of the following must be implemented and independently reviewed before a separate plan version
could even represent authorization:

- an owner execution-approval receipt plus an exact trust mechanism;
- a safely traversed/content-verified actual Go cache archive and manifest bound into an authorized
  plan (the pure external-manifest primitive is not that binding);
- the clean detached-worktree executor and first-parent-only source-reversal proof;
- an attested attempt producer wired to the pure classification truth table and receipt checker;
- executor integration, retention enforcement, and aggregate accounting for the pure private
  content-addressed raw-log writer/checker; and
- a trusted filesystem observer plus atomic resource reservation, fail-closed cleanup, and
  no-receipt-on-interruption attestation.

Calibration membership, confirmatory holdout membership, population assignment, model/provider
execution, paid execution, candidate execution, and benchmark execution remain explicitly
forbidden. This plan cannot grant them.

## Deterministic reproduction

Build to a temporary path:

```bash
python3 benchmarks/agent-brain/confirmatory/task_negative_control_plan_v2.py build \
  --contract benchmarks/agent-brain/confirmatory/development-task-repositories-v2.json \
  --eligibility-schema benchmarks/agent-brain/confirmatory/schemas/development-task-eligibility-scan-v2.schema.json \
  --ledger benchmarks/agent-brain/confirmatory/development-task-eligibility-entire-brain-v2.json \
  --ledger benchmarks/agent-brain/confirmatory/development-task-eligibility-entire-db-v2.json \
  --ledger benchmarks/agent-brain/confirmatory/development-task-eligibility-entire-graph-v2.json \
  --plan-schema benchmarks/agent-brain/confirmatory/schemas/development-task-negative-control-run-plan-v2.schema.json \
  --registry benchmarks/agent-brain/confirmatory/development-task-overlap-registry-v1.json \
  --registry-schema benchmarks/agent-brain/confirmatory/schemas/development-task-overlap-registry-v1.schema.json \
  --output /tmp/development-task-negative-control-run-plan-v2.json
```

Check exact artifact bytes and all current dependency bytes:

```bash
python3 benchmarks/agent-brain/confirmatory/task_negative_control_plan_v2.py check \
  benchmarks/agent-brain/confirmatory/development-task-negative-control-run-plan-v2.json \
  --contract benchmarks/agent-brain/confirmatory/development-task-repositories-v2.json \
  --eligibility-schema benchmarks/agent-brain/confirmatory/schemas/development-task-eligibility-scan-v2.schema.json \
  --ledger benchmarks/agent-brain/confirmatory/development-task-eligibility-entire-brain-v2.json \
  --ledger benchmarks/agent-brain/confirmatory/development-task-eligibility-entire-db-v2.json \
  --ledger benchmarks/agent-brain/confirmatory/development-task-eligibility-entire-graph-v2.json \
  --plan-schema benchmarks/agent-brain/confirmatory/schemas/development-task-negative-control-run-plan-v2.schema.json \
  --registry benchmarks/agent-brain/confirmatory/development-task-overlap-registry-v1.json \
  --registry-schema benchmarks/agent-brain/confirmatory/schemas/development-task-overlap-registry-v1.schema.json
python3 -m unittest benchmarks/agent-brain/confirmatory/test_task_negative_control_plan_v2.py
python3 -m unittest benchmarks/agent-brain/confirmatory/test_task_negative_control_gate_v1.py
```
