# Semantic MCP Guide

`entire brain mcp` serves local brain tools over MCP stdio. It does not open a
network listener, fetch remote data, or call hosted models.

Available tools:

- `brain_stale`
- `brain_brief`
- Unified retrieval (qmd-inspired): `brain_query` (hybrid lexical+vector, RRF),
  `brain_search` (lexical), `brain_vsearch` (vector), `brain_get`, `brain_multi_get`
- Symbol graph: `brain_code`, `brain_context`, `brain_impact`, `brain_changes`,
  `brain_tests`, `brain_boundaries`
- Diff-less review: `brain_regressions`, `brain_review`
- Cross-repo (workspace): `brain_workspace_regressions`, `brain_workspace_review`

The tool responses wrap the existing CLI `--json` output as text content. Treat
the CLI JSON contracts as the source of truth for fields and freshness policy.

`brain_query` and `brain_search` rank across facts, history, and docs;
`brain_vsearch` ranks vector-backed facts and docs. All three return ids you can
pass to `brain_get`/`brain_multi_get`. (The earlier `brain_history` tool was
removed — history is now one source within the unified lexical/hybrid verbs.)

Workspace symbol traversal and unified retrieval currently live in the CLI
(`entire brain workspace inspect context|impact|regressions` and
`entire brain workspace search|vsearch|query|get`). MCP exposes
the single-repo tools plus workspace review/regression helpers.
