# Semantic Agent Guide

Use the local semantic brain as task-specific context. Treat freshness as part
of the answer, not as decoration.

Recommended intake flow:

1. Run `entire brain status --json` and check `semantic.freshness.severity`.
2. Use `entire brain inspect code <symbol> --json --limit 20 --offset 0` or
   `entire brain inspect search-graph <query> --json` to find candidate symbols
   and relation hits.
3. Use `entire brain inspect query-graph "type:CALLS <query>" --json` when the
   question needs a specific relation family, and `entire brain inspect
   graph-schema --json` when you need the available relation/types inventory.
   Use `entire brain inspect graph-ui semantic-graph.html` when a local visual
   graph explorer is useful.
4. Use `entire brain inspect context <symbol> --json --include-content=false` for
   relation-aware context.
5. Use `entire brain inspect snippet <symbol-or-id> --json` before editing a
   resolved symbol, and `entire brain inspect trace-path <from> <to> --json`
   when you need to verify a directed static path.
6. Use `entire brain inspect boundaries --kind route --json`, `entire brain inspect boundaries --kind tool --json`, or
   `entire brain inspect boundaries --kind workflow --json` when the task is about project boundaries.
7. Use `entire brain inspect dead-code --json` for local unused-callable
   candidates, and `entire brain inspect ingest-traces <json-or-ndjson> --json`
   to compare runtime trace edges to static graph edges. After ingest, use
   `entire brain inspect query-graph type:RUNTIME_TRACE --json` to retrieve
   persisted dynamic trace facts.
8. Use `entire brain inspect tests <symbol> --json` before choosing validation
   commands for a changed symbol.
9. Use `entire brain workspace inspect context <workspace> <query> --json` (symbols) or
   `entire brain workspace inspect graph <workspace> --json` (per-repo graph
   metrics, shared external contracts, explicit shared-endpoint `cross_edges`,
   and a persisted `graph.json` artifact) or
   `entire brain workspace search|vsearch|query <workspace> <query> --json`
   (facts/history/docs, grouped per repo; ids are repo-qualified for
   `workspace get`) only for local workspaces that already list local repo
   path hints.
10. Use `entire brain mcp` only as a local stdio adapter when an agent needs MCP
   tool calls instead of direct CLI commands.
11. Refresh with `entire brain refresh` when `status` reports unsafe semantic
   data.
12. Use `entire brain refresh index --worktree` only when uncommitted code is
   intentionally part of the question.

Do not publish semantic artifacts or send semantic context to remote services in
phase 1.
