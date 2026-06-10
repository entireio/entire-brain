# Radar Tool Evidence Audit

- Status: **PASS**
- Claim scope: **radar_tool_contract**
- Required tests: **15**
- Artifact: `go-test-internal-cli-radar.jsonl`
- Artifact hash: `sha256:1a242732f3c5c46657c4e590cb41d767b50dccfeef4eb2ec7453a96fe1c835a5`
- Package pass event: **True**

## Limitations
- Tool-contract proof only; this is not agent lift proof.
- Recent release-candidate Radar agent panels were saturated or noisy on no-brain baselines and are not retained as pass-rate proof.
- This artifact proves local deterministic Go/MCP behavior for the current internal/cli test fixtures.
