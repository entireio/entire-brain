# Release Readiness Matrix

- Status: **PASS**
- Release fully ready: **false**
- Tracks: **12**
- Claimable tracks: **6**
- No-claim or pending tracks: **6**

## Notes
- This is a claim-hygiene gate, not a declaration that every release blocker is closed.
- release_fully_ready remains false while frontend/hosted-model distill latency, facts-vs-raw, semantic usefulness, workspace Radar, and broader replay proof are pending.

## Matrix

| Track | Status | Claimable | Evidence | Detail |
|---|---|---:|---|---|
| replay-lab retained agent proof | no-claim | false | benchmarks/agent-brain/evidence/release/codex-audit-report.json | B1 clean reruns are retained and audit-clean, but no comparison survived the proof-ready gate |
| replay-lab clean correctness-axis agent lift (history channel) | proven | true | benchmarks/agent-brain/evidence/replay-lab-clean/codex-audit-report.json | history-channel correctness brain-lift: 1 proof-ready history comparison(s), brain_positive_stable (per-arm counts in the lane codex-audit-report); mcp/radar/codex scopes remain no_release_claim |
| QMD-inspired MCP/Radar tool contract | proven | true | benchmarks/agent-brain/evidence/radar-tool/radar-tool-audit-report.json | deterministic MCP/Radar Go test artifact |
| distill local scheduler/backfill mechanics | proven-local | true | benchmarks/agent-brain/evidence/distill-perf/distill-perf-audit-report.json | current-repo local command-agent distill extraction scheduling speedup |
| large-repo distill extraction-scheduler speedup | proven-local | true | benchmarks/agent-brain/evidence/distill-perf-large/distill-perf-audit-report.json | large-repo (entireio/cli) local command-agent distill extraction scheduling speedup |
| frontend/hosted-model distill latency and fact quality | pending-target-evidence | false | benchmarks/agent-brain/evidence/distill-perf-large/distill-perf-audit-report.json | retained large-repo speedup uses a deterministic command-agent for scheduler mechanics; hosted-model end-to-end latency and fact quality still need their own retained artifacts |
| facts vs raw/session retrieval quality | no-claim | false | benchmarks/agent-brain/evidence/facts-eval/facts-eval-audit-report.json | paired proof-labeled facts/history/query/raw-sessions eval still required |
| workspace Radar agent lift | no-claim | false | benchmarks/agent-brain/evidence/workspace-radar/workspace-radar-audit-report.json | latest retained candidate is no-brain-too-easy; stronger task/runner still needed |
| semantic freshness/audit health | local-gated | false | mise.toml | freshness must be verified by the live semantic:evidence gate on the final clean release checkout |
| semantic usefulness benchmark claim | pending-retained-proof | false | benchmarks/agent-brain/evidence/release/codex-audit-report.json | release evidence has no semantic proof-ready scope; semantic usefulness remains unclaimed |
| release narrative / backwards-working story | documented | true | docs/release_press_release.md | press-release doc names shipped, proven, pending, blocked, and future claims |
| single local release-readiness gate | gated | true | mise.toml | release:readiness runs check plus retained evidence gates |
