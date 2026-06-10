# MCP/Radar Tool Evidence

This directory retains deterministic local evidence for the MCP tool surface and
Regression Radar.

`mise run radar:evidence` audits `manifest.json`, verifies the retained
`go test -json` artifact hash, and requires the focused `internal/cli` MCP/Radar
tests to pass. This proves the tool contract: QMD-style retrieval tools,
branch-scoped fact retrieval over MCP, detector output, location-only redaction,
deletion opt-in, missing anchored call sites, hinted assignment-deletion loci,
same-name and same-function assignment-deletion loci, invariant-scoped related
locations that distinguish same-identifier deletions with different RHS values,
per-file hinted changed signals, workspace Radar multi-locus redaction, unsafe
workspace pairing skips, strict MCP arguments, framing behavior, and safe
server-side tool-result logging.

It is not agent pass-rate proof. Agent lift for Radar remains blocked until a
fresh non-saturated, `tool_result`-backed release-candidate panel passes.
