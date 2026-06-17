# MCP/Radar Tool Evidence Audit

- Status: **PASS**
- Claim scope: **mcp_radar_tool_contract**
- Required tests: **52**
- Artifact: `go-test-internal-cli-radar.jsonl`
- Artifact hash: `sha256:1b566b8cb6e7e0de04c937e37dc5a28142639c7144aabdb2597639f8afba9b4a`
- Package pass event: **True**

## Limitations
- Tool-contract proof only; this is not agent lift proof.
- Radar agent-lift proof/no-claim status is retained separately under benchmarks/agent-brain/evidence/release and checked by mise run radar:agent-evidence.
- This artifact proves local deterministic MCP/Radar behavior for the current internal/cli test fixtures.
