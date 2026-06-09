# Semantic MCP Guide

`entire brain mcp` serves local brain tools over MCP stdio. It does not open a
network listener, fetch remote data, or call hosted models.

Available tools:

- `brain_stale`
- `brain_brief`
- Unified retrieval (qmd-aligned): `brain_query` (hybrid lexical+vector, RRF),
  `brain_search` (lexical), `brain_vsearch` (vector), `brain_get`, `brain_multi_get`
- Symbol graph: `brain_code`, `brain_context`, `brain_impact`, `brain_changes`,
  `brain_tests`, `brain_boundaries`
- Diff-less review: `brain_regressions`, `brain_review`

The tool responses wrap the existing CLI `--json` output as text content. Treat
the CLI JSON contracts as the source of truth for fields and freshness policy.

`brain_query`/`brain_search`/`brain_vsearch` rank across the brain's facts,
history, and docs and return ids you can pass to `brain_get`/`brain_multi_get`.
(The earlier `brain_history` tool was removed — history is now one source within
these unified verbs.) Use workspace commands through the CLI for now; the MCP
adapter exposes single-repo brain tools.
