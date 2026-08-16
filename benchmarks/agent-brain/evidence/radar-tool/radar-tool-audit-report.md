# MCP/Radar Tool Evidence Audit

- Status: **PASS**
- Claim scope: **mcp_radar_tool_contract**
- Required tests: **52**
- Artifact: `go-test-internal-cli-radar.jsonl`
- Artifact hash: `sha256:ae387fc610d9bc763e6e7320a4faf0a9279d5b9329850d5c98e33218cb8e0999`
- Package pass event: **True**

## Limitations
- Tool-contract proof only; this is not agent lift proof.
- Radar agent-lift proof/no-claim status is retained separately under benchmarks/agent-brain/evidence/release and checked by mise run radar:agent-evidence.
- This artifact proves local deterministic MCP/Radar behavior for the current internal/cli test fixtures.
