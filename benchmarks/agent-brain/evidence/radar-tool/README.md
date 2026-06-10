# Radar Tool Evidence

This directory retains deterministic local evidence for Regression Radar and its
MCP surface.

`mise run radar:evidence` audits `manifest.json`, verifies the retained
`go test -json` artifact hash, and requires the focused `internal/cli` Radar/MCP
tests to pass. This proves the tool contract: detector output, location-only
redaction, deletion opt-in, workspace Radar, strict MCP arguments, and safe
server-side tool logging.

It is not agent pass-rate proof. Agent lift for Radar remains blocked until a
fresh non-saturated, `tool_result`-backed release-candidate panel passes.
