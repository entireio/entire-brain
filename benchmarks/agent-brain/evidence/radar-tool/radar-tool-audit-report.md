# MCP/Radar Tool Evidence Audit

- Status: **PASS**
- Claim scope: **mcp_radar_tool_contract**
- Required tests: **52**
- Artifact: `go-test-internal-cli-radar.jsonl`
- Artifact hash: `sha256:f0c308d3c2941986b7901e48e9ce0b1d8519c61c13f24941ed7797dc334cb994`
- Package pass event: **True**

## Limitations
- Tool-contract proof only; this is not agent lift proof.
- Radar agent-lift proof is retained separately under benchmarks/agent-brain/evidence/release and enforced by mise run radar:agent-evidence.
- This artifact proves local deterministic MCP/Radar behavior for the current internal/cli test fixtures.
