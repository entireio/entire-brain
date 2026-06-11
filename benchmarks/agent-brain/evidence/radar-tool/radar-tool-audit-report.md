# MCP/Radar Tool Evidence Audit

- Status: **PASS**
- Claim scope: **mcp_radar_tool_contract**
- Required tests: **49**
- Artifact: `go-test-internal-cli-radar.jsonl`
- Artifact hash: `sha256:d5a46b2efe76006a8028b475e4496713816b529fdaa964771a7d5e440f29d50b`
- Package pass event: **True**

## Limitations
- Tool-contract proof only; this is not agent lift proof.
- Radar agent-lift proof is retained separately under benchmarks/agent-brain/evidence/release and enforced by mise run radar:agent-evidence.
- This artifact proves local deterministic MCP/Radar behavior for the current internal/cli test fixtures.
