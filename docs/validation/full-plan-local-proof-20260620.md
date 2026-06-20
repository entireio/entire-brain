# Full Plan Local Proof - 2026-06-20

Branch: `codex/full-plan-implementation`

## Commands

```sh
go test ./internal/cli -run 'TestSemanticGraphCommandsUseSQLiteStore|TestWorkspaceGraphReportsSharedExternalContracts'
go test ./internal/cli -run TestRefreshSkipsCurrentSemanticIndex
go test ./...
```

## Results

- Focused semantic/workspace graph tests passed.
- Warm-refresh semantic no-op proof passed: after an initial refresh, a second
  refresh with the same HEAD does not call `entire sem snapshot` again.
- Full repository tests passed:
  - `github.com/ashtom/entire-brain/internal/cli`: 91.600s
  - `github.com/ashtom/entire-brain/internal/config`: cached
  - `github.com/ashtom/entire-brain/internal/tui`: cached

## Coverage

- Graph query accepts the existing filter syntax plus the Cypher-style
  `MATCH (a)-[r:TYPE]->(b) WHERE ... RETURN ... LIMIT n` subset.
- Runtime trace ingestion persists queryable `RUNTIME_TRACE` graph facts and
  folds them into graph schema/metrics, trace-path traversal, and
  `brief --json` semantic context.
- Workspace graph inspection writes a persisted `workspaces/<name>/graph.json`
  artifact with aggregate contracts and explicit `cross_edges` connecting
  symbols/resources in different repos through shared external endpoints and
  repo-key-matched unresolved import candidates.
- MCP exposes `brain_workspace_graph` for the same workspace graph contracts
  and `cross_edges` JSON.
- Current semantic indexes are reused on warm refresh instead of forcing an
  unchanged provider snapshot.

## Remaining Honesty Notes

- The graph query language is a supported Cypher-style subset, not full Cypher.
- Workspace cross-repo graph edges are explicit in `graph.json`, but they are
  still derived from shared external endpoint contracts and repo-key-matched
  unresolved import candidates, not full cross-repo compiler/type-aware symbol
  resolution.
- Runtime trace facts are now visible in `brief` and influence likely-file
  ranking there. Review-specific ranking and proof-ready agent-value evals
  remain open.
