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

This is a **building block**, per the design split between entire-brain / entire-graph / entire-replay-lab.
The intended consumers are the cli's `entire review` and `entire labs investigate`, which gain a
"diff-less mode" when the brain is installed — the same graceful upgrade entire-brain gets from
entire-graph. **Those consumers live in the `entireio/cli` repo, not here.** The cli side is a moving
target (it is being redesigned in the CLI repository), so this repo owns only the
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

Release evidence is retained in two layers. Deterministic tool-contract proof
lives under `benchmarks/agent-brain/evidence/radar-tool`: focused local tests
cover location-only redaction, deletion opt-in, anchored call deletion, hinted
changed/deleted loci across multiple files, same-name and same-function
assignment-deletion sites, unsafe workspace pairing skips, workspace Radar,
strict MCP argument validation plus schema/validator parity, branch-aware QMD
retrieval over MCP, per-file raw-history scan closing under descriptor pressure,
and safe `tool_result` logging.

The retained Radar agent-lift candidate is
`benchmarks/agent-brain/evidence/release/release-candidate-cli-radar-mcp-del-clean-20260611T0412Z`.
It compares `no_brain` against `mcp_history` with
`mcp_radar_location_only` delivery on the manual-attribution deletion task and
retains useful server-side MCP/Radar activity. It is not currently citable
agent-lift proof: the clean B1 rerun is hard-flag clean but saturated and
brain-negative, so the release evidence lane remains in `no_release_claim`
mode until a future clean task produces a proof-ready lift.

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
`pilot-radar-*` suites and promoted `release-candidate-*-radar-*` /
`release-candidate-*-workspace-radar-*` reruns. It now builds the independent
Codex audit first, so rows with retained server logs can show whether the MCP
server actually handled and completed the required named Radar tool. The screen
labels each comparison as proof-ready, promotable, audit-gapped, saturated, or incomplete;
when a pilot is stopped before Radar runs because the first no-brain score is
already too high, it emits a `no-brain-too-easy` row instead of hiding the
suite, and record-only runs now appear as `incomplete-suite` rows. Older
manual-attribution deletion reruns without embedded deletion-policy attestation
or completed server-side tool results remain diagnostic only. The retained
`release-candidate-cli-radar-mcp-del-clean-20260611T0412Z` rerun is also
diagnostic because it saturated after the B1 confound was removed.
`mise run radar:evidence` currently checks the deterministic MCP/Radar
tool-contract artifact. The stricter agent-lift proof gate remains separate
from deterministic tool proof and should be used only after a clean retained
candidate has enough baseline headroom to pass the proof-ready gate.
New MCP/Radar runs also write redacted server-side tool names (`tool:
brain_regressions`, `tool: brain_workspace_regressions`, etc.) to
`mcp-server.log`; the independent audit cross-checks those names when present,
so retained Radar proof shows which brain tool the server actually handled
rather than relying only on agent transcript activity.
For Radar proof, the audit pairs safe args and `tool_result` status on the same
server-side call, so a failed location-only call cannot be combined with a later
successful non-location-only call to manufacture proof.
The retained release gate now also counts named-tool MCP datapoints separately
from basic MCP-verified datapoints and can require named-tool proof by proof
scope. The generic MCP-history proof is call-count-backed legacy evidence.
Citable named-tool MCP proof currently comes from the deterministic local
MCP/Radar contract tests. The retained `mcp_radar_location_only` agent-lift
candidate is no-claim because its clean rerun saturated.
Radar proof records only safe MCP boolean arguments (`location_only`,
`include_deletions`) plus validated workspace names from structured tool-call
events, so the audit can reject a would-be location-only or workspace proof that
actually called the wrong mode or workspace without retaining query text. New
records also embed the deletion-policy bit in record provenance, so future
audits do not infer required Radar arguments from today's task file contents.
The live harness audit now checks deletion-shaped Radar tasks for
`include_deletions: true` too, so bad calls are visible in the run record before
the retained-evidence audit.
For non-radar MCP-history proof, the committed
`release-entire-cli-mcp-manual-attribution` panel targets a harder manual-commit
attribution invariant where the brain should provide historical localization and
the agent still has to repair the code. Its retained
`release-candidate-entire-cli-mcp-manual-attribution-clean-20260611T0412Z`
suite is also no-claim: the clean rerun removed the B1 confound but saturated
and remained proof-negative.

## Consumer 1 — `entire review` (cli; prototyped, not landed)

When entire-brain is installed and the review scope is empty (no commits unique to the branch and no
uncommitted changes), or the user passes `--diff-less`, `entire review` shells
`entire-brain review "<query>" --json`, checks `schema_version`, and folds the findings into the review
prompt — reviewing the working tree against the brain's memory instead of a diff. Query is derived from
the in-scope checkpoint summaries + branch name (there is no diff to mine). No-op when the brain is
absent, the scope is non-empty, or the brain returns nothing.

**Status:** the producer contract is implemented here; the CLI consumer remains unlanded;
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
