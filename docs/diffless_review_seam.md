# Diff-less review seam: the contract `entire review` and `entire labs investigate` are intended to consume (cross-repo, not yet wired)

## What this is

`entire-brain` ships a **diff-less reviewer**: instead of reviewing a branch-vs-base diff, it compares
the current working tree against what session history asserts the code used to be, and reports
suspected regressions (`file:line`, expected vs current, confidence, provenance). The detector lives in
`internal/cli/regression.go` (`detectRegressionAnomalies`) and is exposed three ways:

- `entire brain inspect regressions <query>` — raw anomalies.
- `entire brain review <query>` — the same findings in a review shape (hidden command; it is the
  machine contract, not a human verb — the human review surface is `entire review` in the cli).
- MCP tools `brain_regressions` / `brain_review` (single repo) and `brain_workspace_regressions` /
  `brain_workspace_review` (cross-repo workspace).

This is a **building block**, per the design split between entire-brain / entire-sem / entire-replay-lab.
The intended consumers are the cli's `entire review` and `entire labs investigate`, which gain a
"diff-less mode" when the brain is installed — the same graceful upgrade entire-brain gets from
entire-sem. **Those consumers live in the `entireio/cli` repo, not here.** The cli side is a moving
target (it is being redesigned by Peyton across PRs #1241 → #1370 → #1352), so this repo owns only the
*contract*; the wiring is cross-repo and is tracked below.

## The contract (stable, versioned)

`entire brain review --json` and the `brain_review` MCP tool emit a `reviewReport`
(`internal/cli/regression.go`). The shape consumers bind to:

```json
{
  "schema_version": 1,
  "mode": "diff-less (brain memory vs current tree)",
  "query": "...",
  "repo_path": "...",
  "brain_path": "...",
  "summary": "Diff-less review: N suspected regression(s) ...",
  "findings": [
    {
      "severity": "medium",          // high (reserved) | medium (changed) | low (deleted)
      "file": "cmd/.../review_context.go",
      "line": 412,
      "title": "Suspected regression: `scopeBaseRef` changed",
      "detail": "history shows `...`; current is `...` — verify (could be a rename).",
      "evidence": "sessions/.../x.jsonl:2162",
      "confidence": 0.8
    }
  ],
  "warnings": ["..."]
}
```

**Versioning rule:** `schema_version` starts at `1`. Bump it on any breaking change to `reviewReport` /
`reviewFinding` (field rename/removal or changed semantics). A consumer must check `schema_version` and
**degrade gracefully** (skip the brain context, run a normal review) on an unrecognized version rather
than risk mis-parsing. Additive, backward-compatible fields do not bump the version.

`--location-only` (CLI) / `location_only` (MCP) blanks `expected`/`current`/`reason`, handing the
suspected site but not the fix — so a fair A/B measures detection, not answer-pasting.

## Consumer 1 — `entire review` (cli; prototyped, not landed)

When entire-brain is installed and the review scope is empty (no commits unique to the branch and no
uncommitted changes), or the user passes `--diff-less`, `entire review` shells
`entire-brain review "<query>" --json`, checks `schema_version`, and folds the findings into the review
prompt — reviewing the working tree against the brain's memory instead of a diff. Query is derived from
the in-scope checkpoint summaries + branch name (there is no diff to mine). No-op when the brain is
absent, the scope is non-empty, or the brain returns nothing.

**Status:** prototyped on a held branch (`claude/diff-less-brain`) off Peyton's `review-cutover`;
**not landed** and not pushed, pending the redesign settling + review.

## Consumer 2 — `entire labs investigate` (DESIGNED, NOT WIRED)

`entire labs investigate` is multi-agent brainstorming on a topic (no diff concept). The brain hook
would, when entire-brain is installed, query `entire-brain review "<topic>" --json` once at the start of
a fresh run and fold the suspected regressions into the per-turn shared context (the loop's
`AlwaysPrompt`), so every brainstorming agent sees what history says the code used to be. Because there
is no diff, it fires whenever the brain is present (gated only on availability + a non-empty result),
and reuses the same `schema_version` contract and graceful-degrade behavior as review.

**Status: DESIGNED ONLY — not implemented.** No investigate code in this repo references the brain.
A prototype of this hook exists alongside the review hook on the held cli branch above, but it is not
landed. This repo's responsibility is the contract; wiring `investigate` is cross-repo work tracked by
the TODO in `internal/cli/regression.go`.

## No-egress

Everything is local: the detector reads on-disk sessions and the optional local semantic index; the cli
consumer shells the local `entire-brain` binary. No network calls on any path.
