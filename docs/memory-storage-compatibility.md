# Memory storage compatibility

## Supported release range

Upgrade coverage includes stable v0.2.0, v0.3.0, v0.4.0, and v0.5.0. Nightly
releases and v0.1.0 are outside this matrix.

`scripts/test-memory-storage-compat.sh` builds actual released code from pinned
commits and executes it against isolated repositories and data directories.
The pre-flat writer suite covers v0.2.0 through v0.4.0. The automatic cleanup
suite also covers v0.5.0, including directories produced by its real migration.

## Storage and migration contract

Memory payloads use one schema 3 `facts/store.json` file. Branch is metadata;
memories are visible across branches. The common repository API and hosted
per-fact wire format remain unchanged.

Normal writes and refreshes fold supported legacy layouts into this store.
Migration reads back the published store and compares complete records with the
expected merged result before cleanup. Schema 1 snapshots and schema 2 markers
remain supported migration inputs.

Local migration is one-way. Released v0.2.0 through v0.4.0 clients cannot read the
migrated directory. Hosted interoperability requires separate local stores.
Files written later by an old local client are imported, with a warning to update
that client. This recovery behavior does not make a shared mixed-version store
supported.

There is no storage rollback command. Recovery to an older version requires a
complete pre-migration backup in an isolated environment. Such a backup does not
include later writes. A post-migration bundle is not a downgrade format.

## Automatic cleanup

Customers do not need a cleanup command, script, or confirmation prompt.
Normal memory writes, foreground refreshes, and watcher maintenance remove verified
redundant legacy files. Maintenance includes unchanged watcher passes and existing
global stores. It does not create an unused global store or resume paused services.

Cleanup never imports parked records. Existing v0.5.0 copies must be proved
redundant against the active store. Missing records or unexplained differences
remain available for recovery. Cleanup must not restore a deleted memory or
reverse a retraction.

Unknown fields, unknown files, invalid records, corrupt receipts, unsafe paths,
and unsupported formats prevent removal of the affected data. Valid fact files
can be removed while unrelated files in the same directory remain intact.

Small files use a bounded pass under the existing locks. Large-file verification
runs outside the brain write lock. Removal requires the source checksum and
published store checksum to remain consistent with that verification.
A durable receipt records the source bytes verified before unlink. Recovery
rechecks remaining source bytes and resumes after interruption. A busy writer
causes maintenance to defer its work.

`memories storage status --json` reports cleanup state, pending directories,
retained reasons, and recorded reclaimed bytes. The byte count can underestimate
removal if a process exits before recording progress. It is never a deletion gate.
Status, recall, and other read surfaces remain read-only.

## Scan-cache compatibility

Compact-transcript extraction uses its extraction revision to invalidate older
scan results. Its cache wire version remains compatible with the supported older
clients. An older write without the current extraction revision causes a later
current refresh to rescan the transcripts.

## Test coverage

| Area | Checks |
| --- | --- |
| Stable upgrades | Actual v0.2.0–v0.5.0 binaries; normal writes trigger migration and cleanup without maintenance commands |
| Existing v0.5.0 stores | Real parked migration output, repeated cleanup, later edits, retractions, deletions, and provenance changes |
| Repository and global memories | Complete record preservation, cross-branch copies, lifecycle, recall, and unused global stores |
| Recovery | Receipt publication, real process exit before/after removal, changed source bytes, and stale verification |
| File safety | Unknown fields/files, malformed records, repeated IDs, symlinks, future receipts, and retained corrupt data |
| Background work | Unchanged watcher ticks, maintenance before worker launch, busy-writer deferral, and pause settings |
| Existing compatibility | Pre-flat memory commands, deterministic distillation/outline workflows, hosted sync/retractions, and released-writer recovery |

The full Go suite covers other repository behavior. Released-client tests use
isolated data and a local HTTP server for hosted wire checks. They do not establish
production service availability or live model output quality. Those require
separate packaged-release checks.

## Reproduce

```bash
bash scripts/test-memory-storage-compat.sh
mise run test:prepush
go test -race ./internal/cli/internal/factstore ./internal/cli/command/watch
go test -race ./internal/cli -run 'TestAutomaticMemoryCleanup|TestRepoFactStore|TestHosted|TestMemoriesSync'
```
