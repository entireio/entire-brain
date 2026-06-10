# MCP/Radar Tool Evidence Audit

- Status: **PASS**
- Claim scope: **mcp_radar_tool_contract**
- Required tests: **49**
- Artifact: `go-test-internal-cli-radar.jsonl`
- Artifact hash: `sha256:c9df2ea8825991aa50661c90e8da7da58d31ffcf7fa89b49794f6a457f108311`
- Package pass event: **True**

## Limitations
- Tool-contract proof only; this is not agent lift proof.
- Radar agent-lift proof is retained separately under benchmarks/agent-brain/evidence/release and enforced by mise run radar:agent-evidence.
- This artifact proves local deterministic MCP/Radar behavior for the current internal/cli test fixtures.
