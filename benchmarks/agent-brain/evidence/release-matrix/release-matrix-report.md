# Release Readiness Matrix

- Status: **PASS**
- Release fully ready: **false**
- Tracks: **10**
- Claimable tracks: **4**
- No-claim or pending tracks: **6**

## Notes
- This is a claim-hygiene gate, not a declaration that every release blocker is closed.
- release_fully_ready remains false while target large-repo distill, facts-vs-raw, semantic usefulness, workspace Radar, and broader replay proof are pending.

## Matrix

| Track | Status | Claimable | Evidence | Detail |
|---|---|---:|---|---|
| replay-lab retained agent proof | no-claim | false | benchmarks/agent-brain/evidence/release/codex-audit-report.json | B1 retained query-hint/task-hash confound is detected; clean replay-lab reruns are required before citing agent lift |
| QMD-inspired MCP/Radar tool contract | proven | true | benchmarks/agent-brain/evidence/radar-tool/radar-tool-audit-report.json | deterministic MCP/Radar Go test artifact |
| distill local scheduler/backfill mechanics | proven-local | true | benchmarks/agent-brain/evidence/distill-perf/distill-perf-audit-report.json | current-repo local command-agent distill extraction scheduling speedup |
| target large-repo/frontend distill performance | pending-target-evidence | false | benchmarks/agent-brain/evidence/distill-perf/distill-perf-audit-report.json | current retained speedup is current-repo command-agent scheduler proof, not the frontend/large-repo claim |
| facts vs raw/session retrieval quality | no-claim | false | benchmarks/agent-brain/evidence/facts-eval/facts-eval-audit-report.json | paired proof-labeled facts/history/query/raw-sessions eval still required |
| workspace Radar agent lift | no-claim | false | benchmarks/agent-brain/evidence/workspace-radar/workspace-radar-audit-report.json | latest retained candidate is no-brain-too-easy; stronger task/runner still needed |
| semantic freshness/audit health | local-gated | false | mise.toml | freshness must be verified by the live semantic:evidence gate on the final clean release checkout |
| semantic usefulness benchmark claim | pending-retained-proof | false | benchmarks/agent-brain/evidence/release/codex-audit-report.json | release evidence has no semantic proof-ready scope; semantic usefulness remains unclaimed |
| release narrative / backwards-working story | documented | true | docs/release_press_release.md | press-release doc names shipped, proven, pending, blocked, and future claims |
| single local release-readiness gate | gated | true | mise.toml | release:readiness runs check plus retained evidence gates |
