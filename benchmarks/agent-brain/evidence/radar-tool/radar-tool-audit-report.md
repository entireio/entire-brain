# MCP/Radar Tool Evidence Audit

- Status: **PASS**
- Claim scope: **mcp_radar_tool_contract**
- Required tests: **51**
- Artifact: `go-test-internal-cli-radar.jsonl`
- Artifact hash: `sha256:b3571ff76968bd1ae9224f72086753e6ea94c8bf298d01c87fc5b51ed2dc8142`
- Package pass event: **True**

## Limitations
- Tool-contract proof only; this is not agent lift proof.
- Radar agent-lift proof is retained separately under benchmarks/agent-brain/evidence/release and enforced by mise run radar:agent-evidence.
- This artifact proves local deterministic MCP/Radar behavior for the current internal/cli test fixtures.
