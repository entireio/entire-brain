# Memory storage compatibility

## Supported upgrade range

This release supports upgrades from stable v0.2.0, v0.3.0, and v0.4.0.
The compatibility matrix excludes v0.1.0 and nightly releases.

**Local migration is one-way.** Older releases cannot read the migrated store.
Hosted sync compatibility does not provide local downgrade support.

| Scenario | Contract |
| --- | --- |
| Upgrade a v0.2.0, v0.3.0, or v0.4.0 store | Supported. Legacy memories are imported into the flat store. |
| Import an unreleased schema 1 snapshot or schema 2 marker | Supported. These are migration inputs, not the current storage format. |
| Repeat migration after success or interruption | Supported with the current binary. Keep the source files and retry. |
| Run an older client against a migrated local store | Unsupported. It cannot read the complete migrated corpus. |
| Exchange hosted memories between old and new clients | Supported with separate local stores and the existing Brainstore per-fact wire. |
| Downgrade through `memories storage rollback` | Unsupported. The command has been removed. |
| Return to an older release | Restore a complete pre-migration backup into a separate location. |

Upgrade every Brain CLI and watcher process that shares a local store before
restarting them. This includes the global memory store. Installing Graph does not upgrade Brain.

## Current storage contract

Each repository has one flat memory collection in `facts/store.json`, with store
schema 3. The file contains full records keyed by memory ID. Repository manifests
use schema 5. Global memories use the same flat memory format in their separate store.

Default memory visibility spans branches. Branch names remain record metadata;
`recall --branch` filters that metadata. Branch directories no longer partition
the active store. Migration merges copies with the same ID, retaining provenance
and resolving lifecycle state. It does not perform semantic deduplication of
memories with different IDs.

Readers combine the flat store with remaining legacy sources in memory. That
operation does not rewrite memory payloads. A memory write, refresh, or explicit
storage migration can publish the flat store and move legacy directories aside.
Corrupt flat stores and unsupported future schemas cause an error.

## Migration and retained files

Before any migration, stop processes that share the store and make a complete
backup. A repository backup must preserve its checkpoint refs as well as its
Brain data. Back up global memories separately if they are in use.

Explicit migration uses the current binary:

```sh
entire brain memories storage status --json
entire brain memories storage migrate --json
entire brain memories storage status --json
```

Use `--global` on each command to inspect or migrate global memories.

Migration combines legacy branch files, schema 1 snapshots, and schema 2 layouts.
It writes the flat store atomically and verifies the imported IDs before moving
legacy branch directories under `facts/.legacy-*`.

Those directories retain original bytes for recovery, including records without
usable IDs. Invalid JSON or oversized legacy lines stop migration before publication.
Investigate integrity warnings before cleanup. The retained directories are not active memory sources and are not a
supported rollback mechanism.

Exact-ID consolidation reduces active memory copies. Total disk usage can grow
initially because the originals remain. This release does not automatically delete
those retained directories. Defer disk cleanup until migration verification and
backup checks are complete.

If migration stops or fails, retain the store, legacy directories, and temporary
files. Stop conflicting writers, correct the reported cause, and repeat migration
with the current binary. Do not delete or replace the flat store to force a retry.
If the error persists, preserve the data for support.

A released client can append another legacy file before reporting a manifest
version error. Current reads include that file; migration imports it and reports
`straggler_files`. Update the older process before continuing. This recovery path
does not make mixed-version access to a migrated local directory supported.

## Status fields

`memories storage status --json` reports the combined readable memory set without
migrating its payloads.

| Field or value | Meaning |
| --- | --- |
| `format: "legacy"` | A pre-flat store or legacy branch directories still require migration. |
| `format: "repository"` | The store is flat, or empty, with no live legacy directories. |
| `format: "compatibility"` | A flat store also has live legacy directories to import. |
| `schema_version` | The detected store schema; an absent store reports the current schema. |
| `records` | Unique readable memory IDs after combining current and legacy sources. |
| `views` | Legacy branch directories still awaiting migration. It is not a branch-visibility count. |
| `legacy_compatibility` | Live legacy directories are present. It does not mean older clients can read the flat store. |

`migrate --json` also reports `folded`, `straggler_files`, and `moved_dirs`.
`folded` counts IDs contributed by legacy sources, not the number of duplicate
copies removed. `moved_dirs` lists the original directories that were moved.

## Downgrade and recovery limits

There is no supported in-place downgrade. Do not delete `facts/store.json`, lower
schema numbers, or rename retained directories back to attempt one.

To use an older binary, restore a complete backup made before migration into a
separate location. That backup does not contain later writes. Preserve the current
store separately before recovery so support can assess any newer data.

A bundle exported after migration retains current schema requirements. Including
transcripts does not convert it into an older memory format. An older binary can
read a materialized bundle only when it supports that bundle's schema.

The separate checkpoint-backed transcript migration also has compatibility limits.
Before removing cached transcripts, Brain checks that local checkpoint refs can
supply them. If that check fails, it retains the cache and reports an error.

## Claude capture requirement

Use Claude Code **2.1.78 or later** with the generated Entire hooks. Check:

```sh
claude --version
```

Claude 2.1.72 can ignore project capture hooks when settings contain `StopFailure`.
A successful Claude command does not prove that capture ran. Verify that a real
Claude turn followed by a Git commit creates an Entire checkpoint.

The packaged Brain capture test passed with Claude 2.1.78, stock hook settings,
and the unpatched installed Entire CLI. Both turns produced checkpoints and
retrievable conversations. The separate CLI version-check change is deferred;
this Brain release does not depend on it. Do not assume the installed CLI will
reject an incompatible Claude version.

## Validation scope

`scripts/test-memory-storage-compat.sh` builds actual stable releases from pinned
commits and runs released-client tests on isolated stores. CI runs that lane on
Linux and macOS with race detection. These tests use a local HTTP server for
hosted sync; they do not contact production or invoke live models.

| Area | Coverage |
| --- | --- |
| Storage upgrades | Legacy branch files, schema 1 and schema 2 inputs, repeated migration, repository and global stores. |
| Preservation | Exact-ID consolidation, provenance, lifecycle state, retirement, and recovery of later old-client writes. |
| Failure handling | Interrupted publication, retry, malformed or future schemas, unsafe paths, and retained originals. |
| Commands | Current memory reads and writes, import, retraction, GC, distillation, and outlines. Removed commands are not compatibility promises. |
| Hosted sync | Separate old/new stores, feed pagination, retractions in both directions, failed requests, and retries. |
| Transcript migration | Checkpoint-backed retrieval and materialized bundle handling under supported schemas. |

The packaged release audit on November 2, 2026 used candidate `6c4b11de` and verified
stable v0.2.0, v0.3.0, and v0.4.0 archive checksums. It also checked commands, Graph
integration, service upgrades, and authorized production hosted sync.

One committed repository-visibility fixture still uses an obsolete directory path
and skips. Eighteen additional packaged audit cases covered repository/global
visibility and migration. Those supplemental cases are audit evidence, not
permanent regression coverage; the fixture correction remains pending.

The audit supports upgrades and hosted sync through separate stores. It does not
support local downgrade, every flag combination, or every customer data layout.
Live Claude capture was verified separately. Customer-scale background writer
contention remains a separate release assessment.

## Reproduce the committed checks

```sh
bash scripts/test-memory-storage-compat.sh
go test ./...
go vet ./...
go test -race ./internal/cli/internal/factstore ./internal/cli \
  -run 'TestFlatStore|TestRepositoryStatus|TestRepoFactStore|TestHosted|TestMemoriesSync'
```

The script builds pinned stable releases; it does not replace the installed plugin
or use the user's memory directories. Separate packaged, production-sync, and
live-capture audits are not run by these commands.
