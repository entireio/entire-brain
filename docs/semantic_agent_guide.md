# Semantic Agent Guide

Use the local semantic brain as task-specific context. Treat freshness as part
of the answer, not as decoration.

Recommended intake flow:

1. Run `entire brain stale --json` and check `severity`.
2. Use `entire brain query <symbol> --json --limit 20 --offset 0` to find
   candidate symbols.
3. Use `entire brain context <symbol> --json --include-content=false` for
   relation-aware context.
4. Use `entire brain routes --json`, `entire brain tools --json`, or
   `entire brain workflows --json` when the task is about project boundaries.
5. Use `entire brain tests <symbol> --json` before choosing validation
   commands for a changed symbol.
6. Use `entire brain workspace query <workspace> <query> --json` only for
   local workspaces that already list local repo path hints.
7. Use `entire brain mcp` only as a local stdio adapter when an agent needs MCP
   tool calls instead of direct CLI commands.
8. Refresh with `entire brain refresh --semantic` when `stale` reports unsafe
   semantic data.
9. Use `--semantic-worktree` only when uncommitted code is intentionally part
   of the question. `--worktree` only affects seed refresh inputs.

Do not publish semantic artifacts or send semantic context to remote services in
phase 1.
