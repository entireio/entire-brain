# Workspace Radar Evidence

This directory retains the workspace-Radar agent evidence status separately from
single-repo Radar proof.

`mise run workspace-radar:evidence` audits this directory without rewriting it.
The current manifest is `claim_policy: "no_release_claim"`: it passes only
because the retained workspace candidate is explicitly rejected as
`no-brain-too-easy`, with zero proof-ready or promotable workspace-Radar
comparisons.

Current retained status:

- `release-candidate-cli-workspace-radar-transcript-haiku-xhigh-20260610T154230Z`
  stopped after one no-brain run. The no-brain score was 89 with an early-stop
  threshold of 85, so the benchmark did not spend workspace-Radar repetitions
  and is not release-citable.

To promote workspace Radar later, replace this no-claim manifest with
`claim_policy: "proof_required"` only after retaining an audit-clean
`mcp_workspace_radar_location_only` suite with baseline headroom, 4 repetitions
per side, server-named and completed `brain_workspace_regressions` MCP calls,
no extra brain MCP tools, and a proof-ready comparison.
