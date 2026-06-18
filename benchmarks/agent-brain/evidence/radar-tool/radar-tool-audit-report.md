# MCP/Radar Tool Evidence Audit

- Status: **PASS**
- Claim scope: **mcp_radar_tool_contract**
- Required tests: **52**
- Artifact: `go-test-internal-cli-radar.jsonl`
- Artifact hash: `sha256:bd98f91c28fb891530c3fa2f96713ad99a709bb54439f1c90b688cc8d83f6804`
- Package pass event: **True**

## Limitations
- Tool-contract proof only; this is not agent lift proof.
- Radar agent-lift proof/no-claim status is retained separately under benchmarks/agent-brain/evidence/release and checked by mise run radar:agent-evidence.
- This artifact proves local deterministic MCP/Radar behavior for the current internal/cli test fixtures.
