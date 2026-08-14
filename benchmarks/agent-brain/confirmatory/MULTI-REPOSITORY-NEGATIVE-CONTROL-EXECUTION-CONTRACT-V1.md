# Multi-repository negative-control execution contract v1

This slice compiles the frozen development-only v2 plan into an exact contract for a future
negative-control executor. It does **not** implement that executor or authorize a run. Building and
checking the contract did not invoke Git, Go, a worktree, a candidate process, a model/provider,
private-log publication, host observation, resource reservation, or approval mechanism.

The checked artifact is
`development-task-negative-control-execution-contract-v1.json`. Its status is
`contract_compiled_execution_forbidden`; its execution status is
`forbidden_missing_all_residual_gates_and_audited_execution_adapter`. Every runtime binding is null,
including approval/trust, actual cache material, capacity observation/reservation, cleanup
attestation, execution host, producer key, and private-log root.

## Exact projection

`negative_control_execution_contract_v1.py` securely reads the existing checked v2 plan and verifies
the exact repository contract, eligibility scanner/schema, three ledgers, plan builder/schema,
overlap registry/builder/schema, and CLI compatibility-helper bytes bound by that plan. The plan
remains fixed at raw SHA-256
`f55b6e22a25daf8016305304adf3b7ac8d31675281d97cfc9bb78cc76b619790` and canonical self-hash
`a47311fa1f4553f8085ebea6e8ddd003a4ea5d0b2d269d91bbcdd414134a248e`.

The checked execution-contract raw SHA-256 is
`94422af8a5e24c6a855b66cf40868565ee3518d0c75eb5343b5d2450523b2c1e`; its canonical self-hash is
`1e290b91d5e0be42f750bf0e655edee48d297f86ad3e3867b8fe9177fd5125ef`. It binds builder SHA-256
`f3cfb82978b0ce98d17999ef4cb4f4089a425bb528ece4ff53b61a03a4344bcd` and schema SHA-256
`dbcb95119feeda4c436e90d8162ac0498577454ca9ccaab7a8254d55d90359f9`.

The contract projects all 62 candidates in the exact repository and first-parent order:

| Repository | Candidates |
| --- | ---: |
| `github.com/entireio/entire-brain` | 18 |
| `github.com/entirehq/entiredb` | 25 |
| `github.com/entireio/entire-graph` | 19 |

Each row binds the candidate reference, ordinal, repository, first-parent position, commit, exact
first parent, tree, parent count, unit kind, source/test/full diff identities and stable patch IDs,
production-path identity, module-binding identity, and exact test-target-binding identity. The
future private execution projection and test command remain null. This avoids inventing a command or
runtime path before a real executor can derive and attest it against the immutable Git objects.

The schedule contains exactly 248 rows. For each candidate, it serializes baseline repetitions 1 and
2 followed by first-parent source-reversal repetitions 1 and 2. Both the candidate projection and
the schedule have independent canonical SHA-256 bindings in addition to the whole-contract
self-hash: candidate projection
`4fbde9876d4c91e9cedcf994de9cecd5442beee928c1cadc505116a25c2bef60` and schedule
`c818e8ad87f4d6ff5fac8839f4296fd86da13eaf95dadf8b2c789c7254bfdbcd`.

## Bound non-executing components

The contract content-addresses the already integrated pure gate, private-log, classifier, and live
unattested Darwin capacity-observation source/schema files. These bindings record the exact
components available to a future separately audited executor. They do not turn any primitive into
an attested producer, trusted host adapter, reservation mechanism, or authority source.

The module has only `build` and `check` CLI commands. It does not import the private-log writer or
classifier and never imports a subprocess or network library. Component files are read only to bind
their bytes. JSON inputs use a bounded regular-file reader that opens every supplied path component
descriptor-relative with `O_NOFOLLOW`, rejects parent traversal, and verifies the leaf identity
before and after reading. The only write surface is atomic publication of the requested build output.

## Frozen reversal and cleanup requirements

The reversal contract requires the exact candidate `parent_oid`, first-parent selection for merge
units, and production-Go-source-only reversal. Its proof must bind the exact parent and candidate Git
tree mode, type, blob, or absence for every reversed source path; all non-production entries must
remain at the candidate tree state, with no untracked paths. Test targets and direct argument-vector
bindings must be identical across arms. A whole-commit revert and test-evidence mutation are
forbidden. A future executor must independently reverify commit, parent, tree, test targets, and
source-path identity before doing any work.

The state-machine contract has a run journal plus a nested journal for each exact schedule row. Its
cursor starts at zero and advances once only after the matching attempt cleanup commits. A cleanly
captured candidate failure or timeout is data, not an infrastructure abort. The aggregate receipt is
unreachable until cursor 248, aggregate ceilings pass, final run cleanup commits, and the capacity
reservation is released. Infrastructure failure or interruption latches the no-receipt path.
Restart recovery must hold one exclusive run lock and finish/verify orphan cleanup and reservation
release before any new run or receipt. Any restart of a started nonterminal run irreversibly selects
the no-receipt branch, including interruption during final cleanup or after reservation release but
before the atomic durable receipt commit. A latched cleanup failure blocks every future run until
independently verified remediation, and can never release the failed run's receipt. These are
requirements for a future executor, not claims that cleanup or journaling exists.

Other frozen requirements include authenticated approval, verified offline cache material, an
atomic APFS-aware reservation, fresh detached worktrees and private caches, direct argument-vector
execution with pinned binaries, process-group timeouts, private content-addressed logging, aggregate
resource ceilings, and durable cleanup journaling.

## Remaining gates

Execution remains forbidden until all of these are separately implemented, supplied, and audited:

- authenticated owner approval and trust root bound to the exact contract;
- an actually approved, safely traversed, content-verified offline cache seed/archive;
- trusted APFS observation plus atomic bounded capacity reservation;
- the clean detached-worktree executor and first-parent source-reversal proof;
- an attested attempt producer wired to the classifier;
- private-log executor integration, retention, and aggregate accounting; and
- fail-closed interruption cleanup and a proven no-receipt-before-cleanup guarantee.

No owner approval, cache archive, candidate command, host reservation, or attempt result is included
in this artifact. None of the 248 scheduled attempts ran while creating or testing it.

## Reproduction

From the repository root:

```bash
python3 benchmarks/agent-brain/confirmatory/negative_control_execution_contract_v1.py build \
  --contract benchmarks/agent-brain/confirmatory/development-task-repositories-v2.json \
  --contract-schema benchmarks/agent-brain/confirmatory/schemas/negative-control-execution-contract-v1.schema.json \
  --eligibility-schema benchmarks/agent-brain/confirmatory/schemas/development-task-eligibility-scan-v2.schema.json \
  --ledger benchmarks/agent-brain/confirmatory/development-task-eligibility-entire-brain-v2.json \
  --ledger benchmarks/agent-brain/confirmatory/development-task-eligibility-entire-db-v2.json \
  --ledger benchmarks/agent-brain/confirmatory/development-task-eligibility-entire-graph-v2.json \
  --plan benchmarks/agent-brain/confirmatory/development-task-negative-control-run-plan-v2.json \
  --plan-schema benchmarks/agent-brain/confirmatory/schemas/development-task-negative-control-run-plan-v2.schema.json \
  --registry benchmarks/agent-brain/confirmatory/development-task-overlap-registry-v1.json \
  --registry-schema benchmarks/agent-brain/confirmatory/schemas/development-task-overlap-registry-v1.schema.json \
  --output /private/tmp/development-task-negative-control-execution-contract-v1.json

python3 benchmarks/agent-brain/confirmatory/negative_control_execution_contract_v1.py check \
  benchmarks/agent-brain/confirmatory/development-task-negative-control-execution-contract-v1.json \
  --contract benchmarks/agent-brain/confirmatory/development-task-repositories-v2.json \
  --contract-schema benchmarks/agent-brain/confirmatory/schemas/negative-control-execution-contract-v1.schema.json \
  --eligibility-schema benchmarks/agent-brain/confirmatory/schemas/development-task-eligibility-scan-v2.schema.json \
  --ledger benchmarks/agent-brain/confirmatory/development-task-eligibility-entire-brain-v2.json \
  --ledger benchmarks/agent-brain/confirmatory/development-task-eligibility-entire-db-v2.json \
  --ledger benchmarks/agent-brain/confirmatory/development-task-eligibility-entire-graph-v2.json \
  --plan benchmarks/agent-brain/confirmatory/development-task-negative-control-run-plan-v2.json \
  --plan-schema benchmarks/agent-brain/confirmatory/schemas/development-task-negative-control-run-plan-v2.schema.json \
  --registry benchmarks/agent-brain/confirmatory/development-task-overlap-registry-v1.json \
  --registry-schema benchmarks/agent-brain/confirmatory/schemas/development-task-overlap-registry-v1.schema.json

PYTHONPATH=benchmarks/agent-brain/confirmatory python3 -m unittest \
  benchmarks/agent-brain/confirmatory/test_negative_control_execution_contract_v1.py
```
