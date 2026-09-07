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
   identity until this happens.
3. Run `entire brain entities migrate` in each repository with existing entity
   history. This is distinct from `entities backfill --full`: full backfill skips
   existing delta documents and cannot repair a parser upgrade.
4. Check `entire brain entities history <current-qualified-name>`. A successful
   migration retains the original commits and their checkpoint/session join.

`entities migrate --graph-binary <entire-cli-wrapper>` selects a provider for
migration. The wrapper must support `graph version --json` and `graph diff`.
Refresh's existing `--graph-binary` option likewise selects its provider.
Normal status and history reads verify the provider installed as `entire`.

## What migration changes

The migration recomputes **every already indexed commit across all branches**,
using each commit's actual first parent and timestamp. It replaces the forward
entity deltas and the derived reverse/rename index in one git-meta commit. The
branch coverage windows remain unchanged. Corrected current names therefore
retain history from commits indexed by the older parser; erroneous old parser
spellings are retired, not retained as synthetic source-level renames.

Source Git commits, checkpoint trailers, session manifests, authored facts,
retirements, and unrelated git-meta keys are untouched. The prior git-meta state
also remains in its Git history. The derived Brain history cache rebuilds because
its git-meta tip changed.

A failed/cancelled provider call, unavailable source commit, malformed diff,
wrong diff base/head, changing provider revision, or truncated delta aborts
without publishing a partial migration. The explicit migration is capped at
20,000 stored commits and 200,000 entity changes and fails above either cap; it never silently migrates a
subset. It holds the git-meta write lock while computing the replacement.

Until migration completes, backfill refuses to mix parser revisions, and entity
history reads warn that migration is required. If Graph is unavailable, existing
history remains readable with an identity-verification warning. Revision tags on
each forward document also detect mixed history received through git-meta sync.
The revision check scans the stored entity documents; this prioritizes detecting
mixed imported history over the former constant-cost unchanged-history check.

## Verification

The normal unit suite tests successful migration, refusal before migration,
atomic provider failure, coverage preservation, retirement of obsolete reverse
keys, and preservation of unrelated memory. To run the real provider upgrade:

```sh
BRAIN_TEST_GRAPH_BEFORE=/path/to/old/entire-graph \
BRAIN_TEST_GRAPH_AFTER=/path/to/new/entire-graph \
  go test -race ./internal/cli -run '^TestGraphIdentityUpgradeLive$' -count=1
```

The live test creates an isolated two-commit JS/TS repository, indexes it with the
old provider, switches binaries without editing source, refreshes the semantic
snapshot, runs the migration command, and checks corrected IDs, relations,
history for both commits, checkpoint links, unchanged source HEAD, and reuse of
the now-current snapshot. The test is explicitly skipped if either binary is not
supplied; normal CI alone does not prove cross-version integration.
