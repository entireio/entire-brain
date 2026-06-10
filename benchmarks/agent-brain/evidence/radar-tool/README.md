# MCP/Radar Tool Evidence

This directory retains deterministic local evidence for the MCP tool surface and
Regression Radar.

`mise run radar:evidence` audits `manifest.json`, verifies the retained
`go test -json` artifact hash, and requires the focused `internal/cli` MCP/Radar
tests to pass. This proves the tool contract: QMD-style retrieval tools,
branch-scoped fact retrieval over MCP, detector output, location-only redaction,
deletion opt-in, missing anchored call sites, hinted assignment-deletion loci,
symbol-scoped same-file masking protection, workspace Radar, strict MCP
arguments, framing behavior, and safe server-side tool logging.

It is not agent pass-rate proof. Agent lift for Radar remains blocked until a
fresh non-saturated, `tool_result`-backed release-candidate panel passes.
