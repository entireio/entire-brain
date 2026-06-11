> **ARCHIVED (2026-06-10): complete.** All four items shipped on 2026-06-09
> (commits f44c4ba, 0037714, f3aeb08, 864207d) with regression tests in
> `internal/cli/agent_ux_test.go`. Kept for the rationale; no open work remains.
> Remaining provider-side items live in `entire_cli_semantic_provider_prompt.md`.

# Agent-UX Polish Plan

Follow-up backlog for the agent-facing brain surface. The high-value work shipped
in PR #6 (`claude/brain-agent-ux-improvements`): tokenized/IDF-ranked specialist
and symbol search, `overview`, recency ordering, `stale --blind-spots`, record-id
resolution, the path-query guard, and `[]`-vs-`null` consistency. The items below
are the remaining **plugin-side** polish — each is low-risk, well-scoped, and
independent. (Provider-side work lives in `entire-sem`, tracked separately.)

Every item should ship with a regression test in
`internal/cli/agent_ux_test.go` following the patterns already there.

## 1. Materialize caller/callee symbols in `context`  *(done)*

**Problem.** `inspect context <symbol>` returns the matched `symbols` plus
`relations`, but the relation *endpoints* are only ids (`from_id`/`to_id`). An
agent that wants "who calls this" must read the relations, extract the other id,
and issue a follow-up `context`/`search` per neighbor. The eval rated
context-shaped tasks 3/5 largely for this reason.

**Proposed change.** Resolve each relation endpoint id to its symbol record and
return them in a new additive field so existing consumers are unaffected.
- Add `Neighbors []semanticRecord json:"neighbors,omitempty"` to
  `semanticContextResult` (`internal/cli/semantic.go`).
- In `semanticContextFacts` (semantic.go:2709), after loading relations, collect
  the endpoint ids not already in `symbols` and resolve them via
  `loadSemanticSymbolsByIDSQLite` (semantic.go:3410). Populate `Neighbors`.
- Normalize `Neighbors` with `nonNilRecords` at the `runSemanticContext` marshal
  site. `brief` also calls `semanticContextFacts`; the extra field is harmless
  there (or wire it through `brainBriefSemantic` if useful).

**Risk.** Low — additive field, reuses an existing loader. Watch the relation
fan-out: cap neighbors at the existing `limit` so a hot symbol does not return
hundreds of records.

**Acceptance.** `context` on a symbol with callers returns those callers as
records with `name`/`file_path`/`start_line`, not just relation ids. Test:
fixture with `caller -> ValidateToken`, assert the caller symbol appears in
`neighbors`.

**Status.** Implemented locally in `semanticContextResult.Neighbors`, populated
by `semanticContextFacts`, surfaced by `inspect context` and `brain brief`, and
covered by `TestSemanticContextJSONIncludesRelationNeighbors`.

## 2. `inspect changes`: clear "no changes" signal  *(done)*

**Problem.** On a clean worktree `inspect changes --json` returns
`{"files": null, "symbols": null}` with no explanation, which reads like a broken
command to a fresh agent.

**Proposed change.** In `runSemanticChanges` (semantic.go:2528):
- Normalize `report.Files`/`report.Symbols` to `[]` (the changes report predates
  the array-consistency pass — `semanticChangesReport` has `Files`/`Symbols`).
- Add an explicit signal: either a top-level boolean `"clean": true` /
  `"changed": false`, or a short human note in non-JSON mode
  ("no changes since the indexed HEAD").

**Risk.** Low. Check `mcp.go` `brain_changes` and any test asserting the changes
shape.

**Acceptance.** Clean worktree → arrays are `[]` and the response unambiguously
indicates "nothing changed". Test: index a fixture, run changes on a clean tree,
assert `[]` + the clean indicator.

## 3. Dedup / disambiguate `overview` commands  *(done)*

**Problem.** In a monorepo `overview` lists duplicate-looking command names —
e.g. two `npm run build` entries (`next build` and `pnpm --filter web build`)
from different `package.json` files. They are not exact duplicates but read as
noise.

**Proposed change.** In `runBrainOverview` (`internal/cli/agent_surface.go`) when
copying `seed.Commands`:
- Drop entries that are identical on `(Name, Command)`.
- For same-`Name` different-`Command` entries, disambiguate by appending the
  source (the `seedCommand.Source` field carries provenance), or group them so
  the duplication is intentional and legible.
- Consider whether the dedup belongs upstream in seed command extraction
  (`internal/cli/seed.go`) instead, so every consumer benefits.

**Risk.** Low; cosmetic. Keep the full set available via `overview --json` even
if the text view collapses it.

**Acceptance.** A monorepo overview shows no exact-duplicate command lines, and
same-name commands are distinguishable. Test: seed with duplicate + same-name
commands, assert the rendered/serialized set is deduped/disambiguated.

## 4. Advertise `blind_spots` in the `brain_stale` MCP schema  *(done)*

**Problem.** `runSemanticStale` already accepted `blind_spots` over MCP (read from
`params.Arguments`), but the `brain_stale` `inputSchema` did not declare it, so an
MCP client could not discover it.

> The `relax`/`brain_history` half of this item is **obsolete** (qmd-retrieval
> refactor): `brain_history` and `runBrainHistoryInspect` were removed — history is
> now a source within the unified `brain_query`/`brain_search` verbs.

**Shipped.** Added a `booleanArg` helper alongside `stringArg`/`integerArg` in
`internal/cli/mcp.go` and declared `blind_spots` (boolean) on `brain_stale`.
`tools/list` now advertises it; covered by
`TestMCPToolsListAdvertisesBlindSpots`.

## Out of scope (provider-side, `entire-sem`)

Not addressable in this plugin — tracked separately:
- SQL migrations unindexed by the bundled tree-sitter grammar (verification task:
  the fix is believed to be in progress).
- Route boundaries emitted as `external:route:*` nodes without a handler
  `file_path`.
- Low-confidence / spurious `CALLS` relations (e.g. everything resolving to a
  common `sleep`).
