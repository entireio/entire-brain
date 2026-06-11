# MCP/Radar Tool Evidence Audit

- Status: **FAIL**
- Claim scope: **mcp_radar_tool_contract**
- Required tests: **52**
- Artifact: `go-test-internal-cli-radar.jsonl`
- Artifact hash: `sha256:de337055302850ac2eb75fa926489a875a1b2ba56e8aeee2ec4d8bb72f41b4b7`
- Package pass event: **True**

## Errors
- Radar tool source file hashes are stale: internal/cli/regression.go, internal/cli/mcp.go

## Limitations
- Tool-contract proof only; this is not agent lift proof.
- Radar agent-lift proof is retained separately under benchmarks/agent-brain/evidence/release and enforced by mise run radar:agent-evidence.
- This artifact proves local deterministic MCP/Radar behavior for the current internal/cli test fixtures.
