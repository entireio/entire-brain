# Semantic MCP Guide

`entire brain mcp` serves local brain tools over MCP stdio. It does not open a
network listener, fetch remote data, or call hosted models.

Available tools:

- `brain_stale`
- `brain_brief`
- `brain_query`
- `brain_context`
- `brain_impact`
- `brain_changes`
- `brain_history`

The tool responses wrap the existing CLI `--json` output as text content. Treat
the CLI JSON contracts as the source of truth for fields and freshness policy.

`brain_history` searches the indexed Entire session/history source for
`history`, `decisions`, `sessions`, `validation`, `tool-paths`, or
`architecture` records. Use workspace commands through the CLI for now; the MCP
adapter exposes single-repo brain tools.
