# Graph parser identity upgrades

Graph's `identity_revision` is separate from its release version and its NDJSON
schema version. It changes when a parser correction changes the IDs, names, or
kinds of already indexed symbols. A missing field denotes the legacy rules.
Ordinary provider releases need not change it.

For Graph trail 154 the revision is `js-ts-callable-scope-1`. JavaScript and
TypeScript nested helpers become local functions qualified by their enclosing
callable; real methods no longer collide with phantom members. Anonymous
callables and callable class fields also have their bodies indexed correctly.

## Upgrade order

1. Install this Brain release before installing the Graph release carrying the
   new identity revision. Keep the source repository and its Git objects available.
2. Run `entire brain refresh` in each repository. Even if the source tree is
   unchanged, a different provider identity revision rebuilds its semantic
   snapshot and SQLite generation. Normal freshness checks report stale provider
   identity until this happens. If the identity command is unavailable, refresh
   warns and continues with source/artifact checks; it does not stamp identity as
   verified or abort an otherwise usable warm refresh.
3. Run `entire brain entities migrate` in each repository with existing entity
   history. This carries the union of previously indexed commits across branches
   into the current parser revision. Normal backfill also works, but follows its
   own branch and budget; it does not automatically carry all older coverage.
4. Check `entire brain entities history <current-qualified-name>`. A successful
   migration retains the original commits and their checkpoint/session join.

`entities migrate --graph-binary <entire-cli-wrapper>` selects a provider for
migration. The wrapper must support `graph version --json` and `graph diff`.
Refresh's existing `--graph-binary` option likewise selects its provider.
Normal status and history reads verify the provider installed as `entire`.

## What migration changes

Migration adds a separate namespace for the selected parser revision. It diffs
previously indexed commits missing from that namespace, using each commit's
actual first parent and timestamp. Already present commits are skipped. A second
migration with no new input does no provider diff work and creates no commit.

Legacy forward, reverse, alias and window keys remain byte-for-byte intact.
Revisioned keys have the form
`brain:entities-revision:<hex revision>:<hex legacy key>`. No global active-revision
marker is written and no old records are tombstoned. Readers and writers choose
the namespace from their local provider; older Brain versions ignore the new
namespace. Exchanging the added records therefore does not switch an older
peer's parser or delete its history. An older peer can keep extending legacy
history; another migration carries those newly indexed commits forward later.
The current MetaStore is local-only; tests exercise serialized record exchange,
not a hosted peer-sync deployment.

Source Git commits, checkpoint trailers, session manifests, authored facts,
retirements, and unrelated git-meta keys are untouched. Corrected current names
retain the original commit/checkpoint/session join. Old parser spellings stay
available in the legacy namespace, without invented source-level rename aliases.
Source coverage windows are copied only when the destination has no window;
existing destination windows stay intact. The query cache is keyed by both
metadata tip and parser revision, so switching or rolling back the provider
cannot silently reuse the other parser's history cache.

Normal backfill and unchanged-history ticks use namespace-specific cursor and
commit-key reads. They **do not scan historical delta documents**. Queries still
materialize history when their derived cache is invalidated, as before; that
rebuild also caches a warning if other revisions contain commits missing from
the selected revision. Offline history uses the last cached revision with a
provider-verification warning; `entities show` needs a cached revision to select
history when the provider is unavailable.

## Publication and limits

Migration captures the metadata tip and computes all diffs **without the shared
write lock**. It then tries the lock without waiting, verifies the tip is still
the captured value, and publishes one atomic additive update. A concurrent fact,
backfill or metadata-import write causes publication to abort; rerun migration.
No concurrent work is overwritten and no partial migration is published.

A failed/cancelled provider call, unavailable source commit, malformed or partial
diff envelope, wrong base/head, changing provider revision, or truncated delta
also aborts. Entity mapping follows the same rules as backfill: file-only changes
and records without a resolvable name/path contribute no entity, and a missing
kind is retained according to the existing contract. These do not abort migration;
provider warnings about partial results still do.
Migration is capped at 20,000 distinct stored commits and 200,000 new entity
changes. It never silently migrates a subset above those limits.

This removes the long lock around parser subprocesses, not all publication cost:
MetaStore still materializes and serializes the final state under its write lock.
That cost scales with store size. Retaining revisions also consumes additional
storage, and contention can require recomputation on retry. No automatic pruning
or cross-peer deletion protocol is introduced by this change.

## Verification

The unit suite tests legacy documents with a global identity marker, unchanged
ticks without delta decoding, revision isolation, serialized exchange with a
legacy writer, idempotence, lock availability during parsing, concurrent-write
refusal, malformed provider output, coverage and unrelated-memory preservation.
To run the real provider upgrade:

```sh
BRAIN_TEST_GRAPH_BEFORE=/path/to/old/entire-graph \
BRAIN_TEST_GRAPH_AFTER=/path/to/new/entire-graph \
  go test -race ./internal/cli -run '^TestGraphIdentityUpgradeLive$' -count=1
```

The live test creates an isolated two-commit JS/TS repository, indexes it with the
old provider, switches binaries without editing source, refreshes the semantic
snapshot, runs the migration command, and checks corrected IDs, relations,
history for both commits, checkpoint links, unchanged source HEAD, and reuse of
the now-current snapshot. It also switches back to the old provider and then
forward again without changing metadata, verifying cache isolation and preserved
legacy history. The test is explicitly skipped if either binary is not
supplied; normal CI alone does not prove cross-version integration.
