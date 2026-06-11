# Semantic Agent Guide

Use the local semantic brain as task-specific context. Treat freshness as part
of the answer, not as decoration.

Recommended intake flow:

1. Run `entire brain status --json` and check `semantic.freshness.severity`.
2. Use `entire brain inspect code <symbol> --json --limit 20 --offset 0` to find
   candidate symbols.
3. Use `entire brain inspect context <symbol> --json --include-content=false` for
   relation-aware context.
4. Use `entire brain inspect boundaries --kind route --json`, `entire brain inspect boundaries --kind tool --json`, or
   `entire brain inspect boundaries --kind workflow --json` when the task is about project boundaries.
5. Use `entire brain inspect tests <symbol> --json` before choosing validation
   commands for a changed symbol.
6. Use `entire brain workspace inspect context <workspace> <query> --json` (symbols) or
   `entire brain workspace search|vsearch|query <workspace> <query> --json`
   (facts/history/docs, grouped per repo; ids are repo-qualified for
   `workspace get`) only for local workspaces that already list local repo
   path hints.
7. Use `entire brain mcp` only as a local stdio adapter when an agent needs MCP
   tool calls instead of direct CLI commands.
8. Refresh with `entire brain refresh` when `status` reports unsafe semantic
   data.
9. Use `entire brain refresh index --worktree` only when uncommitted code is
   intentionally part of the question.

Do not publish semantic artifacts or send semantic context to remote services in
phase 1.
