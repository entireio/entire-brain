# MCP/Radar Tool Evidence Audit

- Status: **PASS**
- Claim scope: **mcp_radar_tool_contract**
- Required tests: **52**
- Artifact: `go-test-internal-cli-radar.jsonl`
- Artifact hash: `sha256:b1f2011679a3fafb0b462912fc5287698787229dff364ac6409daf117a82f44f`
- Package pass event: **True**

## Limitations
- Tool-contract proof only; this is not agent lift proof.
- Radar agent-lift proof is retained separately under benchmarks/agent-brain/evidence/release and enforced by mise run radar:agent-evidence.
- This artifact proves local deterministic MCP/Radar behavior for the current internal/cli test fixtures.
