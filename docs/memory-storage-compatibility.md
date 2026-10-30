# Memory storage compatibility

## Supported release range

This change supports stable v0.2.0, v0.3.0, and v0.4.0. Nightly releases and
v0.1.0 are outside this compatibility matrix.

The tests run actual released CLI code against isolated repositories and data
directories. `scripts/test-memory-storage-compat.sh` builds each stable release
from its pinned commit. CI runs the script on Linux and macOS with Go's race
detector. Local validation also uses the published macOS binaries.

## Storage contract

The repository memory API remains the common interface. During support for these
older clients, memory payloads remain in `facts/<branch>/facts.ndjson`. Existing
branch visibility, record identity, provenance, lifecycle, outlines, proposals,
and usage receipts keep their formats.

Schema 2 `facts/store.json` contains only:

```json
{"schema_version":2,"storage":"legacy_views"}
```

It contains no record IDs, text, provenance, or view data. Older clients can
physically purge all memory payloads because those payloads stay in files they
already manage. Physical consolidation into a new payload file is deferred until
these older clients leave support. Brainstore's per-fact wire is unchanged.

Schema 1 snapshots remain readable. Conversion reconciles all legacy views,
removes excluded-session provenance, publishes missing legacy files, and replaces
the snapshot last. The privacy guard checks session IDs and transcript paths from
both the manifest and durable privacy transactions. Conversion retains memories
with independent, permitted provenance. A failed conversion can be retried.

For ordinary schema 2 writes, an unrelated damaged view does not block the target
write. Converting a schema 1 snapshot requires every retained view to be readable.
Corrupt state causes an error; conversion does not silently discard that state.

## Privacy repair

An older client cannot remove an existing schema 1 snapshot. Run
`memories storage migrate` before using an older client for privacy cleanup on
such a store. No new client can change this behavior inside an already released
binary.

Current `privacy verify` checks physical schema 1 representatives and overrides,
including unreferenced bodies. It also reports abandoned snapshot temporary
files. Current purge persists reconciliation even when an older purge already
removed all visible records. This removes hidden snapshot residue and temporary
files without requiring another visible memory change.

## Scan-cache compatibility

History extraction revision 11 adds compact transcript parsing. Its cache record
schema is the same as version 10, which all three stable releases understand.
New caches therefore use wire version 10 and an `extractor_version: 11` field.

Current history readers reuse a version 10 cache only when that field matches.
Older writers omit the field, so the next current refresh rescans the transcripts.
Existing version 11 files remain readable by current clients. Current privacy
inspection accepts both known formats and still rejects unsupported formats.
The extraction invalidation check remains in place.

## Test coverage

| Area | Released-client checks | Additional current-client checks |
| --- | --- | --- |
| Migration | Upgrade, schema 1 conversion, repeated migration, rollback, re-upgrade | Partial publication, retry, missing and empty views, corrupt/future schemas, symlinks, read-only stores |
| Repository memories | Remember, recall, branch visibility, promotion, retraction, GC, import | Lifecycle replay during schema 1 import, randomized mutation sequences, provenance differences |
| Global memories | Old/new reads and writes, retraction, downgrade/re-upgrade | Global storage behavior and taxonomy validation |
| Concurrent writers | Eight old/new processes sharing a global store | Concurrent views, interrupted commits, process termination at the publication boundary |
| Read surfaces | Brief, status, tree, map, review listing, vitality, Markdown export | Bundle export, manifest/integrity checks, classification and refresh |
| Agent workflows | Distill, outline generation, and proposal application using deterministic agent responses | Extraction, reconciliation, proposal decisions, cache and outline regression suites |
| Privacy | Legacy purge, old purge after conversion, current purge, old schema 1 purge followed by current detection/repair | Orphan representatives, overrides, temporary files, missing projections, transcript-only anchors, multi-source provenance |
| Hosted sync | Push/pull, multiple feed pages, shared cursor use, both retraction directions, independent repository reads, failed push/pull and retry, rollback/re-upgrade | Existing Brainstore wire, binding, permission, cursor, filtering, and background sync suites |
| History cache | Privacy operations and verification after switching clients | Old parser cache invalidation, current parser cache reuse, known-schema inspection, unsupported-schema rejection |

The full Go suite checks other repository behavior. Focused race tests cover the
changed storage, privacy, history-cache, and sync code. Tests do not use customer
repositories, production Brainstore, or live model services. The local HTTP test
server checks the Brainstore wire contract; it does not prove production service
availability or model output quality.

## Reproduce

```bash
bash scripts/test-memory-storage-compat.sh
go test ./...
go vet ./...
go test -race ./internal/cli/internal/factstore ./internal/cli \
  -run 'TestRepoFactStore|TestHistoryScanCache|TestPrivacy|TestSessionPrivacy|TestHosted|TestMemoriesSync|TestFacts'
```
