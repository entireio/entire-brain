# Semantic Agent Guide

Use the local semantic brain as task-specific context. Treat freshness as part
of the answer, not as decoration.

Recommended intake flow:

1. Run `entire brain stale --json` and check `severity`.
2. Use `entire brain query <symbol> --json --limit 20 --offset 0` to find
   candidate symbols.
3. Use `entire brain context <symbol> --json --include-content=false` for
   relation-aware context.
4. Refresh with `entire brain refresh --semantic` when `stale` reports unsafe
   semantic data.
5. Use `--semantic-worktree` only when uncommitted code is intentionally part
   of the question. `--worktree` only affects seed refresh inputs.

Do not publish semantic artifacts or send semantic context to remote services in
phase 1.
