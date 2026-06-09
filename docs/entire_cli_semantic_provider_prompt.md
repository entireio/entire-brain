# Handoff prompt — entire-cli semantic provider (`entire sem`)

Paste the section below to an agent working in the **entire-cli** repo (the binary
that exposes `entire sem …`). It is the upstream root-cause work for issues found
while consuming the provider from `entire-brain`.

---

## Context

`entire-brain` builds its semantic index by shelling out to this repo's provider:

- `entire sem doctor --json` — checked for `no_egress` / `local_only`.
- `entire sem snapshot --repo . --format ndjson [--worktree] [--ignore-file …]` — the
  NDJSON snapshot that becomes the brain's symbol/relation store.

On a real TypeScript repo (entire.io: 1212 files, 1204 TS) the snapshot reported
**45 `E_PARSE_ERROR` partial failures** ("tree-sitter syntax error nodes present").
Investigating the consumer side surfaced three provider-side gaps. Fix them here.

## Problem 1 — Parse failures carry no file attribution (highest priority)

In the snapshot NDJSON, the **header** lists partial failures as bare objects:

```json
{"code":"E_PARSE_ERROR","severity":"warning","detail":"tree-sitter syntax error nodes present"}
```

There is **no `path`** on these entries, and the per-record `warning_codes` field is
empty (`[]`) on every symbol/relation record and `null` on `file` records. As a
result the consumer knows *45 files failed* but cannot say **which** ones, so the
failures are not actionable and cannot be triaged, ignored, or fixed.

**Required changes:**

1. Add a `path` field (repo-relative, forward-slashed) to every `partial_failures`
   entry in the snapshot header. Each `E_PARSE_ERROR` must name the file it came from.
2. Populate `warning_codes` on the affected `file` records (and ideally on the
   symbol records derived from those files) with `["E_PARSE_ERROR"]`, so a code
   that consumes the per-record stream can attribute failures without re-deriving.
3. Keep the existing aggregate behavior (the consumer counts `partial_failures`),
   but the entries must now be individually attributable.
4. If feasible, include a short reason beyond "syntax error nodes present" — e.g.
   the byte offset / line of the first ERROR node — to speed up triage.

Acceptance: `entire sem snapshot --format ndjson` header `partial_failures[*]` each
include a non-empty `path`, and those paths exist in the repo tree.

## Problem 2 — Grammar version trails modern syntax

The 45 failures are almost entirely TypeScript. tree-sitter is error-tolerant (it
still extracts symbols), so these are "incomplete, not missing" — but they are
likely real grammar-version gaps against modern TS (e.g. `using`/`await using`
declarations, `const` type parameters, newer decorator syntax, `satisfies`, import
attributes). Investigate and act:

1. Identify the actual failing constructs (use Problem 1's attribution to dump the
   offending spans from the 45 entire.io files as a corpus).
2. Upgrade the bundled `tree-sitter-typescript` (and any other lagging grammars) to
   a version that parses current syntax, or document why a construct is unsupported.
3. Add regression fixtures for the constructs that were failing.

Acceptance: re-running the snapshot on the corpus reduces `E_PARSE_ERROR` count;
remaining failures are documented as genuinely unsupported.

## Problem 3 — No per-file progress from `snapshot`

`entire sem snapshot` runs as a single opaque step. The consumer's `refresh` can
only show "parsing sources" with a spinner while the provider walks every file +
builds the store. Emit incremental progress so long indexing is observable:

1. Stream progress to **stderr** (keep stdout as pure NDJSON) as structured lines,
   e.g. `{"progress":{"files_done":N,"files_total":M,"phase":"parse"}}`, or expose a
   `--progress-fd` / `--progress` flag. Pick whatever fits the repo's conventions.
2. Document the format so `entire-brain` can parse and forward it into its own
   refresh progress UI.

Acceptance: a long snapshot emits monotonically increasing file progress on stderr
without corrupting the NDJSON on stdout.

## Problem 4 — No workflow boundaries (YAML / CI definitions not parsed)

`entire brain inspect workflows` is always empty because the provider emits no workflow
boundaries. On entire.io the indexed languages are only TypeScript/JavaScript/
Bash/SQL — **no YAML** — so the repo's 9+ `.github/workflows/*.yml` files are not
parsed at all, and there are no `external:workflow:*` nodes nor
`HANDLES_WORKFLOW`/`PART_OF_WORKFLOW` relations. (Routes and tools work because
they're derived from TypeScript.)

The consumer already handles the workflow boundary shape (external
`external:workflow:<name>` nodes referenced by `HANDLES_WORKFLOW`/
`PART_OF_WORKFLOW`, or kind-tagged `workflow`/`job`/`pipeline` symbols) — it just
has nothing to list. Required: detect CI/workflow definitions (GitHub Actions
`.github/workflows/*.yml`, and ideally other CI systems) and emit them as
workflow boundaries with handler relations, the same way routes/tools are
emitted. *Acceptance: `entire sem snapshot` on a repo with GitHub Actions emits
`workflow` boundary nodes and the relations that wire jobs/steps to them.*

## Notes

- Do **not** break the current stdout NDJSON schema or the `doctor` no-egress
  contract — the consumer fails closed if no-egress is unverified.
- The consumer already tolerates a small fraction of `E_PARSE_ERROR` (it no longer
  marks the whole brain "degraded" below ~10% of files), so Problem 1 (attribution)
  is more valuable than driving the count to zero — but Problem 2 still reduces noise.
