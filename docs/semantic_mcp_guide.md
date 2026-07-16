# Semantic MCP Guide

`entire brain mcp` serves local brain tools over MCP stdio. It does not open a
network listener, fetch remote data, or call hosted models.

Available tools:

- `brain_status`
- `brain_index_status` (alias for `brain_status`)
- Project/index management: `brain_index_repository`, `brain_list_projects`,
  `brain_delete_project`
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

Tool responses wrap the existing CLI `--json` output as text content by default.
Treat the CLI JSON contracts as the source of truth for fields and freshness
policy.

`brain_brief` also has opt-in `packet_format: "compact_v1"`,
`packet_format: "compact_v2"`, and `packet_format: "agent_v1"` responses for
coding agents. Omitting `packet_format`, or setting it to `legacy_json`,
preserves the existing pretty-JSON text response. The compact formats run the
same retrieval and ranking and change only serialization. `compact_v1` is a
deterministic keyed
line packet. `compact_v2` adds a hashed in-band legend immediately after its
version marker: `~` means an absent field, while `^` reuses the value from the
previous record of the same opcode and column, even when records with other
opcodes occur between them. For each repeated record family, v2 compares the
exact canonical bytes of keyed rows against its schema declaration plus
positional rows; positional is used only when strictly smaller, and ties remain
keyed. Both packets retain safely encoded data, task-relevant status/trust
signals including stale-locus annotations, semantic relations and neighbors,
history, facts, actions, patterns,
guidance, and an end-to-end body checksum. They are experimental and are not
selected automatically; a future incompatible representation will use a new
version name.

`agent_v1` is a separately versioned bounded coding projection rather than a
compact encoding of every brief field or a claim to be the smallest possible
packet. Its packet-level `delivery_policy` is frozen to `always`. It has a 32 KiB UTF-8
byte budget and carries a SHA-256 identity for the canonical field-selection,
priority, overflow, privacy, and budget configuration. Exact ranked fact IDs,
order, and text are mandatory, together with stale-locus and pending-review
trust state. A fact's `locus_drift` marker is retained even when every unsafe
structured locus value must be omitted. Lower-priority edit/test files, actions,
symbols, test suggestions, and history excerpts are admitted as deterministic
section prefixes in exactly that frozen priority order. When a section's next
complete record cannot fit, shorter records from later sections may use the
residual bytes. If mandatory
evidence cannot fit, emission fails before writing
any partial packet. The projection omits generated timestamps, host roots,
history transcript paths/timestamps, session and checkpoint identifiers,
transcript anchors, and raw provenance. `delivery_policy: "always"` or
`delivery_policy: "shadow"` may be specified explicitly only with
`packet_format: "agent_v1"`; cross-format policy settings are rejected.
`shadow` is control-plane instrumentation only: it builds and delivers the
exact same always-bound packet bytes, never suppresses delivery, and keeps its
decision/features/reason codes in a pure internal artifact that is not
serialized into the agent packet. The current evaluator is
`diagnostic_unfrozen`: development admission thresholds are not yet authorized,
its candidate decision is unknown, and `would_silence_authorized` is always
false. Evaluator errors, unavailable signals, trust warnings, non-finite values,
unsafe structured input, unclassified warnings, and always-bound packet build
errors all fail safe to
unknown/serve. Its deterministic configuration identity binds the exact feature
schema, rule and threshold state, evaluator version, base product commit,
always-bound packet configuration, and requested/effective limits; a second
artifact hash binds the emitted feature values, decision, and ordered reason
codes. Structured path fields are filtered
to repository-relative or symbol-like values. Natural-language task, fact,
review-message, action/evidence, signature/reason, and history text is preserved
verbatim and is not a redaction boundary; do not put secrets into task or
memory prose.

`brain_query` and `brain_search` rank across facts, history, and docs;
`brain_vsearch` ranks vector-backed facts and docs (plus history when a
Gemma-class embedder is configured and `refresh` has built history vectors). All three return ids you can
pass to `brain_get`/`brain_multi_get`. (The earlier `brain_history` tool was
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

`brain_index_repository` is a local write tool for building the semantic index;
it does not publish artifacts. `brain_delete_project` removes local generated
brain data for the selected repo key.

`brain_patterns` and `brain_patterns_status` are read-only pattern-corpus
inspection tools. Skill formation is intentionally not exposed as an MCP write
tool; use the explicit CLI flow `entire brain patterns skills form` when a human
or authorized automation wants to synthesize a skill.
