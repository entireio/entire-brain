# Semantic MCP Guide

`entire brain mcp` serves local brain tools over MCP stdio. It does not open a
network listener, fetch remote data, or call hosted models.

Available tools:

- `brain_stale`
- `brain_query`
- `brain_context`
- `brain_impact`
- `brain_changes`

The tool responses wrap the existing CLI `--json` output as text content. Treat
the CLI JSON contracts as the source of truth for fields and freshness policy.

Use workspace commands through the CLI for now; the Phase 1 MCP adapter exposes
single-repo semantic tools only.
