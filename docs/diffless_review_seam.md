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

## Evidence Status

The contract is implemented and covered locally: CLI and MCP tests exercise
`brain_regressions`, `brain_review`, `brain_workspace_regressions`, and
`brain_workspace_review`, including `location_only` checks that preserve the
file/line while not leaking expected/current fix values.

Release evidence is partially retained. The current citable Radar proof is
`release-candidate-entire-cli-radar-mcp-manual-attribution-deletions-all-loci-rerun-20260610Tprogress`:
it runs `no_brain` vs `mcp_history` with location-only
`brain_regressions(include_deletions)`, no-brain passes 1/4, Radar passes 4/4,
and the release/Radar audits require matching MCP-verified condition records,
server-side `tool: brain_regressions` log lines, and summary-vs-record pass-rate
agreement. This is a narrow single-repo deletion-Radar pass-rate proof, not
workspace Radar, answer-assisted Radar, or efficiency proof.

The committed `release-entire-cli-radar-mcp-review-base-scope` panel remains a
reproducible Radar candidate lane: it sets `BENCH_RADAR_LOCATION_ONLY=1`, runs
`no_brain` vs `mcp_history`, and is intended to graduate only if the independent
audit sees zero hard flags, MCP-verified records, and a proof-ready
`mcp_radar_location_only` comparison. The committed
`release-entire-cli-workspace-radar-mcp-transcript-reresolve` panel exercises
the workspace MCP delivery (`mcp_workspace_radar`) and audits proof-ready runs as
`mcp_workspace_radar_location_only`, but it remains a calibration lane until a
non-saturated regression task is found.
The committed `release-entire-cli-radar-mcp-attribution-realign` panel is a
true-Radar calibration lane: it uses a detector-shaped deleted state assignment,
keeps the Radar arm location-only, and validates the state invariant without
exposing expected/current values. Its clean 1x pilot saturated, so it is not
retained release proof.
The committed `release-entire-cli-radar-mcp-review-file-count` panel is the next
Radar proof candidate: it targets explicit-base review file counts with a
detector-shaped `baseRef+"...HEAD"` regression and a hidden behavioral
validation that visible tests do not already cover. Its calibrated clean 1x
pilot still saturated on pass rate, so it is directional efficiency calibration
until a future task/runner revision creates baseline headroom.
`mise run radar:screen` runs the dedicated Radar evidence audit over local
`pilot-radar-*` suites and labels each comparison as proof-ready, promotable,
or saturated. `mise run radar:evidence` is intentionally stricter: it fails
until retained `release-candidate-*` evidence contains at least one stable
proof-ready location-only Radar comparison whose own condition records are
MCP-verified by the independent Codex audit. That keeps generic MCP-history
proof separate from the stronger Radar claim.
New MCP/Radar runs also write redacted server-side tool names (`tool:
brain_regressions`, `tool: brain_workspace_regressions`, etc.) to
`mcp-server.log`; the independent audit cross-checks those names when present,
so future retained Radar proof can show which brain tool the server actually
handled rather than relying only on agent transcript activity.
Radar proof also records only safe MCP boolean arguments (`location_only`,
`include_deletions`) from structured tool-call events, so the audit can reject a
would-be location-only proof that actually called the wrong mode without
retaining query text.
For non-radar MCP-history proof, the committed
`release-entire-cli-mcp-manual-attribution` panel targets a harder manual-commit
attribution invariant where the brain should provide historical localization and
the agent still has to repair the code. Its retained
`release-candidate-entire-cli-mcp-manual-attribution-20260610Tprogress` suite is
now citable as MCP-history correctness/pass-rate evidence, not Radar or
efficiency evidence.

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
