# Bugfix Handoff — entire-brain (branch: agent-brain-benchmark-proof)

Context for another agent picking this up. Two buckets: (A) the bug **I fixed this
session that is still uncommitted in the working tree**, and (B) bugs already
**committed** (I reviewed/verified them, didn't author them).

## A. UNCOMMITTED — fix is live in working tree, needs commit

### A1. mcp_history audit contradicted the Opus/compact prompt → wrongly failed correct brain runs
- **Files:** `benchmarks/agent-brain/run.py`, `benchmarks/agent-brain/run_test.py` (`git diff --stat`: +29/-3)
- **Bug:** `record.ok = validation.ok AND returncode==0 AND leak_audit.ok AND mcp_audit.ok` (run.py ~2280). For Opus 4.8 (and gpt-5/5.5), the `mcp_history` prompt (run.py ~1528) explicitly tells the agent: *"call `brain_brief` ONCE … do NOT call `brain_history`."* But `mcp_condition_audit` (run.py ~820) **unconditionally required both `brain_brief` AND `brain_history`**, so every compliant Opus run was flagged `missing_required_mcp_history_tool: brain_history` → `mcp_audit.ok=False` → `record.ok=False`.
- **Impact:** `summarize()` counts `record.ok` for `success_rate_condition` (run.py ~2630), so 6/6 *correct* Opus `mcp_history` runs (validation passed, real one-line fixes) were scored as **failures**. This **under-reports** the brain (opposite of overclaiming) and would make a genuine win fail the `proof_ready` gate.
- **Fix:** new helper `mcp_history_required_tools(runner)` (run.py ~809) returns `("brain_brief",)` for models in `OPUS_COMPACT_MODELS | COMPACT_STRICT_MODELS`, else `("brain_brief","brain_history")`. Audit signature now `mcp_condition_audit(condition, agent_info, runner)`; call site (run.py ~2273) passes `runner`.
- **Test:** `run_test.py::test_mcp_history_audit_matches_compact_prompt_history_tool_requirement` — Opus brief-only passes, Sonnet brief-only still flagged. **33/33 harness tests pass.**
- **Verified on real data:** re-deriving the 6 Opus `mcp_history` records with the fixed audit flips all `record.ok` False→True (mcp_audit False→True). Re-running not required (fix is deterministic on captured tool-call data).
- **TODO for next agent:** commit this (Opus-only behavior change; other models unchanged). Consider re-running `report` on `opus-ab-review` / `opus-ab-transcript` so summaries reflect corrected success.

## B. ALREADY COMMITTED (reviewed + verified this session, not authored by me)

- **`f8d9271`** — (1) `historyRawLineExcerpt` sliced raw text with a *normalized* offset → misaligned excerpts; fixed to index `lower` (`agent_surface.go ~1723`). (2) `truncateString`/excerpt split multi-byte UTF-8 runes → rune-safe now (`agent_surface.go ~1858`). (3) MCP NDJSON frame had no size cap + treated malformed line as fatal → bounded + `-32700`-and-continue (`mcp.go ~307`). (4) export checkpoint cache clobbered with empty map when `git ls-tree` OID-resolution failed → skip save when inactive (`export.go ~1421`).
- **`c08a096`** — Welch p-value was `erfc` normal approx (absurd at n=3) → proper Welch t-test via regularized incomplete beta (`run.py welch_p_value`); verified matches scipy to 6dp. Plus `resolved_model` capture + honest model-attribution + README "no network" claim corrected (opt-in `checkpoint_remote git fetch` disclosed).
- **`979eb20`** — Holm-Bonferroni multiple-comparison correction (verified matches statsmodels); soft-free headline metrics `pass_rate` + `score_core` (excludes `brain_use` & `runtime_efficiency`); checkpoint-ref SHA added to brain cache key; memory-bounded MCP read; explicit `cache.active` flag; tests for MCP-recoverable / cache-clobber-guard / welch / extract_resolved_model.

## Known NON-bugs / gotchas (don't "fix")
- `go test ./...` from repo root **fails while a benchmark is live** — Go walks live worktrees under gitignored `results/`. Use `go test ./internal/... ./cmd/...`.
- `entire brain` is **not installed as a plugin** on this machine; only the harness (which builds + invokes the `entire-brain` binary directly) works.
- Raw `total_tokens` are **cache-read-inflated** (~10× cheaper than fresh input). Report **cost or time**, not raw tokens, for fairness.
- Untracked `run_opus_matrix.sh` present (not mine).

## Current Opus 4.8 result (medium effort), after the A1 fix
- HARD task (review-base-flag-scope, big file): brain `mcp_history` vs no_brain → **−54% tokens, −45% time, −41% cost, 3/3 correct both**.
- EASY task (transcript-reresolve, greppable): Opus saturated → brain ~wash (mcp_history +44% time / +11% cost). Honest boundary: brain wins when *locating* the fix is the cost.
