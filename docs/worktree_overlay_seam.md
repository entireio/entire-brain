# Working-tree overlay seam: treating the semantic index as a stale baseline a coding agent patches at read time (DESIGN ONLY)

## What this is

The semantic index is a batch artifact built at a commit (`semantic/generations/<commit>-<tree>/semantic.sqlite`).
A coding agent's job is to *change code*, so the index is **always stale relative to the working tree** —
on a live branch `entire brain status` reports `freshness: unsafe` the moment the agent edits a file, and
a full reindex is too slow to run between tool calls. Today the read surfaces resolve symbols against the
index (`semanticSymbolsForFiles` → `findSemanticSymbolsForFilesSQLite` in `internal/cli/semantic.go`), so a
fact's locus, an impact traversal, or `inspect changes` can be answered against code that no longer matches
disk.

This note describes the intended fix: stop treating index freshness as a precondition and instead treat the
index as an **immutable global baseline** that a **session-scoped working-tree overlay** patches at read
time. The overlay is bounded by the diff — which the agent itself produced — not by repo size.

This is **DESIGN ONLY**; nothing below is implemented. It depends on one provider-side capability that does
not exist yet (see *Provider dependency*).

## The model: baseline ⊕ overlay

```
effective graph = index (global, as of commit C)  ⊕  fresh-parse(working-tree diff)
```

The fresh parse *overrides* the index for files in the current diff; the index supplies everything else.
The freshness gap is exactly the set of files in `git status` — which is exactly what `changedSemanticFiles`
(`internal/cli/semantic.go`) already computes for `inspect changes`. The agent does not need to discover
what is stale; it knows, because it made the edits.

## The definition/relation asymmetry (decides what a fresh parse can return)

A fresh parse of a changed file is not symmetric across record types:

- **Definition / signature / outbound edges** of a changed symbol — a single-file parse returns these
  *exactly and completely*. Cheap, high-value, no index needed.
- **Inbound edges** (who calls the changed symbol) — a single-file parse **cannot** return these; the caller
  set is global, which is what the index is for. The saving grace: a *new* inbound edge can only originate
  from another changed file, which is already in the diff. Unchanged callers' edges remain valid in the
  index.

So the overlay rule is: **fresh-parse the diff for definitions + outbound edges; keep the index for inbound
edges from unchanged files.** The union is correct for any consistent edit.

### Residual hole: rename / move

Renaming `loadFacts` → `loadAllFacts` orphans every caller edge the index stored by the old compound id
(`gh/<repo>:Go:<file>:function:loadFacts`). A *consistent* edit updates those callers, so they land in the
diff and are re-parsed; a *mid-edit inconsistent* state has a wrong graph because the **code** is wrong
(it would not compile). This is acceptable: the overlay is never more wrong than the working tree.

## Provider dependency: a scoped parse entrypoint

`--worktree` already exists — the provider can parse uncommitted content (`runSemanticSnapshot`,
`internal/cli/semantic.go:739`, emits `sem snapshot --repo <dir> --format ndjson --no-network [--ignore-file …] [--worktree]`).
What is missing is **scope**: `snapshot` is whole-repo, so a fresh read today costs a full sweep plus the
worktree-stability guard (`verifySemanticWorktreeStable`, `internal/cli/semantic.go:1690`). tree-sitter
parses a single file in low-ms; the expensive part is the repo-wide sweep + relation resolution + SQLite
write, none of which a read should pay.

The concrete ask to the `entire-sem` side is a narrow op:

```
entire sem parse --repo <dir> --paths a.go,b.go --worktree --format ndjson --no-network
```

(or a `--paths` filter on `snapshot`). It emits the same `semanticRecord` shape as `snapshot` — `file`,
`symbol`, and `relation` records for the named paths only — so the overlay can be unioned with index rows
without a schema change. Until this lands, the overlay is not cheap enough for the read path.

## Where it goes — and where it does not

- **Read surfaces** — `inspect changes` / `context` / `impact` (`runSemanticChanges`, `runSemanticImpact`).
  `inspect changes` is already scoped to `changedSemanticFiles`, so it is the drop-in target: fresh-parse
  the changed files, overlay their definitions/outbound edges, union with index relations for the rest, then
  feed the fresher symbol set into `factsRelevantToChange` (`internal/cli/facts_locus.go`). This is the
  surface where freshness matters most and the fit is exact.
- **Batch backfill / `reclassify`** — does **not** use the overlay. Resolving every fact by spawning the
  provider per fact is wrong, and freshness barely matters for maintenance metadata. Keep `reclassifyFacts`
  index-resolved (or text-only) and re-reconcile after each `brain index`. The locus-canonicalization design
  applies to the write path unchanged; only the read path goes fresh.

## Caching

- **Session overlay cache.** Key a parsed-file result on its working-tree **content hash** (the index's
  `parse_cache` table is blob-keyed, but uncommitted files have no blob — hash the bytes instead). An agent
  reads the same file many times between edits; parse once per save, not once per read.
- **Post-Edit hook (optimization).** Because the harness mediates the agent's `Edit`/`Write`, a hook can
  fresh-parse the just-changed file and patch the session overlay immediately, taking the parse off the read
  path entirely. Lazy-with-cache is the simpler default; the hook is the optimization if read latency bites.

## No-egress

The overlay preserves the Phase 1 guarantee: fresh parsing uses the same local provider binary with the same
`--no-network` flag and the same `sem doctor` egress check (`runSemanticDoctor`). No path opens the network.

## Status

**DESIGN ONLY — not implemented.** No overlay code exists; the read surfaces still resolve against the index.
Blocked on the scoped `sem parse` provider entrypoint (cross-repo, `entire-sem`). Everything downstream — the
session overlay store, the `inspect changes` integration, and read-time locus canonicalization — is
straightforward once the provider can parse N named paths from the working tree cheaply.
