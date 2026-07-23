# Semantic MCP Guide

`entire brain mcp` serves local brain tools over MCP stdio. It does not open a
network listener, fetch remote data, or call hosted models.

Available tools:

- `brain_status`
- `brain_index_status` (alias for `brain_status`)
- Project/index management: `brain_refresh`, `brain_index_repository`,
  `brain_list_projects`, `brain_delete_project`
- `brain_brief`
- Unified retrieval (qmd-inspired): `brain_query` (hybrid lexical+vector, RRF),
  `brain_search` (lexical), `brain_vsearch` (vector), `brain_get`, `brain_multi_get`
- Symbol graph: `brain_code`, `brain_search_code`, `brain_context`,
  `brain_impact`, `brain_changes`, `brain_detect_changes`, `brain_tests`,
  `brain_boundaries`, `brain_search_graph`, `brain_query_graph`,
  `brain_get_graph_schema`, `brain_get_architecture`, `brain_get_code_snippet`,
  `brain_trace_path`, `brain_dead_code`, `brain_ingest_traces`
- Diff-less review: `brain_regressions`, `brain_review`
- Cross-repo (workspace): `brain_workspace_graph`,
  `brain_workspace_regressions`, `brain_workspace_review`
- Pattern corpus: `brain_patterns`, `brain_patterns_status`

The tool responses wrap the existing CLI `--json` output as text content. Treat
the CLI JSON contracts as the source of truth for fields and freshness policy.
`brain_status` and `brain_index_status` are compact by default; pass
`details: true` only when coverage histograms, staged-file classifications, or
changed-symbol records are required.
`brain_code`, `brain_context`, `brain_impact`, and `brain_tests` also use compact
semantic records by default; pass `details: true` only for the full provider
record payload. Their default limits are 10 code results, 5 context symbols, 20
impact symbols, and 3 test suggestions.

`brain_query` and `brain_search` rank across facts, history, and docs;
`brain_vsearch` ranks vector-backed facts and docs (plus history when a
Gemma-class embedder is configured and `refresh` has built history vectors).
All three return ids you can pass to `brain_get`/`brain_multi_get` for full
records. (The earlier `brain_history` tool was
removed — history is now one source within the unified lexical/hybrid verbs.)

Workspace symbol traversal and unified retrieval currently live in the CLI
(`entire brain workspace inspect context|impact|graph|regressions` and
`entire brain workspace search|vsearch|query|get`). MCP exposes the
single-repo tools plus `brain_workspace_graph` for cross-repo graph contracts
and `cross_edges` (shared external contracts, canonical route-template
HTTP client-to-handler edges, repo-key-matched unresolved import candidates,
GraphQL operation-to-resolver/schema-field and schema-field-to-resolver edges,
event-channel producer-to-consumer edges,
package-keyed `cargo/<crate>`, `gomod/<module-path>`,
`maven/<group>/<artifact>`, `npm/<name>`, `pypi/<name>`, `nuget/<name>`,
`gem/<name>`, and `composer/<vendor>/<package>` workspace repos,
including hyphen/underscore aliases for Python packages, Rust crates, Ruby
gems, and GitHub monorepo package dirs,
Kubernetes external config workload/resource candidates, including
namespace-qualified resource endpoints with short-name fallback, Docker Compose service resource candidates, and
exact, repo-prefix-qualified, or package-prefix-qualified
`external:symbol:<qualified-name>` matches, including file-path-qualified
symbol aliases for shallow symbols in nested module files),
along with workspace review/regression helpers.

The graph tools read the local semantic SQLite store built by `refresh index`
or rebuilt by `repair`. `brain_query_graph` accepts simple filters such as
`type:CALLS`, `relation:HANDLES_ROUTE`, `from:<symbol>`, and `to:<symbol>`,
plus a small Cypher-style subset such as
`MATCH (a)-[r:CALLS]->(b) WHERE a.name = "caller" RETURN a,r,b LIMIT 10`;
relation predicates such as `WHERE r.type = "CALLS"` are accepted, and
aggregate counts are available with `RETURN count(r)` or `RETURN count(*)`.
`brain_trace_path` walks directed relation paths; `brain_ingest_traces` imports
local JSON/NDJSON runtime edges, reports which ones already match static
relations, and persists them as queryable `RUNTIME_TRACE` graph facts that are
also visible to graph schema/metrics, trace-path traversal, and `brain_brief`
semantic context.

`brain_refresh` is the deterministic local write tool for rebuilding
code-derived sources when retrieval freshness is unsafe; it never runs seed
agent synthesis and returns compact status JSON after completion. Set
`worktree: true` only to include current uncommitted content. By default it
skips checkpoint export/history so code iteration stays fast; set `sessions:
true` only when those sources and patterns also need refresh.
`brain_index_repository` is the narrower local write tool for building only the
semantic index; neither tool publishes artifacts. `brain_delete_project`
removes local generated brain data for the selected repo key.

`brain_patterns` and `brain_patterns_status` are read-only pattern-corpus
inspection tools. Skill formation is intentionally not exposed as an MCP write
tool; use the explicit CLI flow `entire brain patterns skills form` when a human
or authorized automation wants to synthesize a skill.
