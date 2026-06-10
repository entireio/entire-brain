# MCP/Radar Tool Evidence Audit

- Status: **PASS**
- Claim scope: **mcp_radar_tool_contract**
- Required tests: **49**
- Artifact: `go-test-internal-cli-radar.jsonl`
- Artifact hash: `sha256:32085adc90a6d7a66fb21e08aafa1517ca88522086c4ab46fe93244f95b91991`
- Package pass event: **True**

## Limitations
- Tool-contract proof only; this is not agent lift proof.
- Radar agent-lift proof is retained separately under benchmarks/agent-brain/evidence/release and enforced by mise run radar:agent-evidence.
- This artifact proves local deterministic MCP/Radar behavior for the current internal/cli test fixtures.
