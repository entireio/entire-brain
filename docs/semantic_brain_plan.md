# Semantic Brain Plan

This plan describes how Entire Brain can grow from a session-history and seed
exporter into a modular agent-context system with semantic code intelligence,
impact analysis, branch-aware freshness, and multi-repo workspace support.

The core constraint is that Entire Brain should remain the orchestration,
durable memory, and agent contract layer. Tree-sitter parsing and semantic
extraction should live behind a provider boundary, with `entire-sem` as the
expected provider.

## Goals

- Keep prior development history, seed context, and semantic code facts in one
  agent-readable brain.
- Make every generated artifact inspectable on disk.
- Separate historical rationale, current-code facts, and inferred impact.
- Support local development across many branches without treating one mutable
  snapshot as the whole truth.
- Scale to large repositories with incremental indexing and partial failure.
- Support multi-repo work through workspace brains layered over per-repo brains.
- Keep command-line and JSON contracts stable before adding additional
  transports.

## Non-Goals

- Do not make Entire Brain a monolithic parser.
- Do not require generated brain artifacts to be committed to the repository.
- Do not make embeddings mandatory for useful local behavior.
- Do not build browser or web UI surfaces as part of this plan.

## Phase Boundary

Phase 1 is local-only. It may read local repositories, local git history, local
Entire sessions, and locally installed provider output. It may run local tests in
CI. It must not publish generated brain artifacts, fetch generated brain
artifacts from remote services, send semantic content to hosted model APIs, or
expose a brain service over the network.

Phase 1 commands must not perform implicit network operations. In particular:

- `entire brain index`, `refresh`, and `stale` must not run `git fetch`,
  `git pull`, or remote discovery; they may only inspect local refs.
- The semantic provider must run with no network egress during Phase 1 indexing.
- Local MCP means stdio transport only. The Phase 1 adapter must not open TCP or
  externally reachable Unix socket listeners, start network sidecars, perform
  auth callbacks, upload telemetry, or call hosted models.

The following features are Phase 2 unless explicitly implemented with a
local-only backend:

- Publishing or hydrating shared brain baselines.
- CI uploading generated brain artifacts for reuse.
- Remote embeddings or any embedding provider that receives source, symbols,
  docs, summaries, or semantic records.
- Hosted-model semantic summaries, seed enrichments, architecture summaries,
  risk summaries, or wiki-like generated docs.
- Remote MCP, HTTP, or daemon serving of brain data beyond local stdio/localhost
  development.
- Telemetry or metrics upload.
- Remote artifact registries or baseline discovery services.
- Cross-machine workspace membership sync.
- Remote repo fetch/index-by-URL flows that clone or fetch code as part of brain
  generation.
- Cloud review or eval integrations that package semantic context for external
  agents.

Local filesystem bundle import/export is allowed in Phase 1 when both source and
destination are explicit local paths. It must not contact a registry, object
store, remote URL, CI artifact service, or discovery service.

## System Shape

The system should be layered.

### Entire Brain

Owns:

- Brain directory layout.
- Manifest format.
- Session export.
- Seed export.
- Refresh policy.
- Staleness checks.
- Agent-facing intake templates.
- Query, context, impact, and review commands.
- Workspace coordination.

### Semantic Provider

`entire-sem` should own:

- Tree-sitter parsing.
- Entity extraction.
- Language-specific symbol models.
- Semantic diffs.
- Import, call, inheritance, field access, route, and tool relation extraction.
- Parser capability reporting.
- Partial failure reporting.

Entire Brain should shell out to the provider or consume provider artifacts
through a stable JSON contract. It should not import provider internals.

### Semantic Store

A new storage/query layer should ingest provider artifacts and expose:

- Symbol search.
- Relation traversal.
- Impact analysis.
- Changed-symbol reports.
- Route/tool/workflow maps.
- Staleness metadata.

The first implementation can use JSONL plus SQLite. The rest of the code should
depend on a narrow interface so the backing store can evolve.

### Agent Surface

Agents should use a small command set first. `brief` is the front door for
coding agents; specialist semantic and history tools live under `inspect` so
agents do not have to choose from a wide top-level command surface.

```sh
entire brain status [repo] --json
entire brain brief "<task>" --json
entire brain search "<query>" --json
entire brain show <id> --json
entire brain refresh [repo] --json
entire brain guide
entire brain path [repo]
```

The normal agent loop should start with one command:

```sh
entire brain brief "<task>" --json
```

`brief` should combine brain freshness, semantic code facts, seeded baseline
context, history-derived decisions, likely files, likely tests, known pitfalls,
provenance, and a cheap live-state overlay into one bounded packet. The
live-state overlay should include branch, HEAD, dirty file list, staged vs
unstaged state, diff stats, and changed-symbol hints when available. It must
make clear that the brain is an indexed snapshot and may not include edits the
same agent made minutes ago. Agents should inspect full diffs or file contents
only when the task intersects those live changes or when `brief` returns low
confidence.

Specialist/debug surfaces should move under `inspect`:

```sh
entire brain inspect code "<query>" --json
entire brain inspect context <symbol-or-id> --json
entire brain inspect impact <symbol-or-file> --json
entire brain inspect changes --base main --head HEAD --json
entire brain inspect tests "<query>" --json
entire brain inspect decisions "<query>" --json
entire brain inspect history "<query>" --json
entire brain inspect sessions "<query>" --json
entire brain inspect validation "<query>" --json
entire brain inspect tool-paths "<query>" --json
entire brain inspect architecture "<area-or-query>" --json
entire brain inspect boundaries --kind route|tool|workflow --json
```

Workspace use should keep the same front door:

```sh
entire brain brief "<task>" --workspace <name> --json
entire brain status --workspace <name> --json
```

Each command should provide concise human output and a stable `--json` mode.

## Semantic Artifact Contract

Define a versioned JSON schema emitted by the semantic provider and consumed by
Entire Brain. The provider is an artifact emitter: it parses source and emits
versioned semantic facts. Entire Brain owns persistence, indexing, query
behavior, freshness policy, and agent presentation.

Initial provider commands:

```sh
entire sem snapshot --repo . --format ndjson
entire sem symbols --repo . --format ndjson
entire sem edges --repo . --format ndjson
entire sem diff --base main --head HEAD --json
entire sem doctor --json
```

Provider output should support newline-delimited JSON from the first release.
Large repositories can produce hundreds of megabytes of semantic facts, so a
single whole-repo JSON document should be a compatibility/debug mode, not the
primary integration format.

Initial snapshot header:

```json
{
  "schema_version": "1.0",
  "repo_root": "/path/to/repo",
  "commit": "abc123",
  "tree": "tree789",
  "languages": ["Go", "Python"],
  "capabilities": [],
  "warnings": [],
  "partial_failures": []
}
```

Following records should be typed:

```json
{"record_type":"file","path":"internal/auth/token.go","blob":"..."}
{"record_type":"symbol","id":"...","kind":"function","name":"ValidateToken"}
{"record_type":"relation","from_id":"...","to_id":"...","type":"CALLS"}
```

Schema compatibility policy:

- `schema_version` uses `major.minor`.
- Entire Brain refuses unknown major versions.
- Entire Brain ignores unknown fields within a supported major version.
- Entire Brain records provider version, schema version, and capabilities in the
  brain manifest.
- If the provider emits a newer supported-major minor version, Entire Brain may
  continue by ignoring unknown fields, but it must record a visible warning that
  some facts may have been skipped.
- Relation types are owned by this contract, not by an individual provider
  release.
- Provider-specific extension relation types must use an extension namespace,
  such as `X-provider-name:RELATION`.

Symbols should include:

- `id`
- `stable_id_version`
- `kind`
- `name`
- `qualified_name`
- `file_path`
- `start_line`
- `end_line`
- `signature`
- `body_hash`
- `language`
- `container_id`

The symbol ID scheme is load-bearing. The first version should use a documented
compound identity:

```text
<repo-key>:<language>:<file-path>:<kind>:<qualified-name>
```

This is stable across content edits but breaks across file moves and some
renames. Rename reconciliation can map old IDs to new IDs later using body hash,
signature similarity, and provider diff records. The manifest must record the
symbol ID version so future schemes can coexist.

Relations should include:

- `from_id`
- `to_id`
- `type`
- `confidence`
- `reason`
- `warning_codes`

Initial relation vocabulary:

- `DEFINES`
- `CONTAINS`
- `IMPORTS`
- `CALLS`
- `IMPLEMENTS`
- `EXTENDS`
- `OVERRIDES`
- `ACCESSES`
- `HANDLES_ROUTE`
- `HANDLES_TOOL`

Warnings and partial failures must be machine-readable. Free-form strings are
allowed as human detail, but every warning needs a stable code, severity, file
path when applicable, and effect on semantic completeness.

## Brain Layout

The brain should become a repo memory store with commit-addressed snapshots and
branch overlays.

Recommended per-repo layout:

```text
repos/<repo-key>/
  current.json
  manifest.json
  README.md
  seed/
  sessions/
  semantic/
    manifest.json
    semantic.sqlite
    parse-cache/
      ab/cd/<file-hash>.json
    snapshots/
      <commit>/
    overlays/
      <base>..<head>.json
  branches/
    main/
    feature-x/
  commits/
    <sha>/
  locks/
```

The stable repo brain path remains easy for agents to discover, while internal
state is commit- and branch-aware.

`semantic.sqlite` should contain all semantic tables for the current index
generation. Keeping files, symbols, relations, search, reverse edges, and
snapshots in one database avoids cross-file transactional consistency problems.
If multiple physical files are introduced later, the manifest must point to an
immutable index generation directory so half-written refreshes are detectable.

Indexing commands must take an advisory lock under `locks/`. Concurrent refresh
attempts should fail fast with a clear `index_locked` error by default. A later
`--wait-lock` option can wait with a timeout. Commands must never write the same
index generation concurrently.

## Manifest Additions

`manifest.json` should record semantic source metadata alongside seed and
session sources.

Example:

```json
{
  "schema_version": 3,
  "repo_key": "gh/org/repo",
  "repo_commit": "abc123",
  "branch": "feature/x",
  "default_branch": "main",
  "default_branch_commit": "def456",
  "indexed_tree": "tree789",
  "dirty_worktree": false,
  "generated_at": "2026-05-31T00:00:00Z",
  "sources": {
    "seed": {},
    "sessions": {},
    "semantic": {
      "generated_at": "2026-05-31T00:00:00Z",
      "commit": "abc123",
      "provider": "entire-sem",
      "provider_version": "0.1.0",
      "snapshot_path": "semantic/snapshots/abc123/snapshot.json",
      "symbols": 1234,
      "relations": 4567,
      "warnings": []
    }
  }
}
```

## Freshness Model

Keeping the brain current requires more than checking whether one export exists.
The system should distinguish:

- Local branch brain: generated from the current worktree and branch.
- Shared default-branch brain: generated from the latest mainline commit.
- Branch overlays: small semantic/session deltas for feature branches.
- Commit snapshots: immutable semantic snapshots for specific commits.
- Dirty worktree snapshots: optional content-addressed overlays for uncommitted
  files.

Freshness commands:

```sh
entire brain stale
entire brain refresh
entire brain refresh --base main --head HEAD
entire brain refresh --all-branches
```

Recommended behavior:

- `entire brain refresh` updates the current branch snapshot.
- `entire brain refresh --base main --head HEAD` creates or updates a branch
  overlay.
- `entire brain stale` emits a typed staleness report, not a single boolean.
- `entire brain path .` may materialize a missing brain, but should surface
  stale semantic data rather than hiding it.
- Agent intake should warn clearly when semantic data is stale, branch-mismatched,
  dirty-worktree-specific, or incomplete.

Freshness axes:

- `worktree`: clean, dirty-indexed, dirty-unindexed, or ignored.
- `head`: indexed commit matches `HEAD`.
- `branch_tip`: indexed commit matches the local branch tip.
- `default_branch`: default branch baseline is present and current.
- `provider`: provider version and schema version are compatible.
- `semantic_completeness`: no parse failures, partial failures, or skipped
  critical paths.
- `workspace`: all workspace member repos and contracts are current.

The JSON report should include both per-axis state and an aggregate severity:

- `ok`: current enough for normal use.
- `degraded`: usable, but answers need warnings.
- `unsafe`: semantic answers should not be used without refresh or narrower
  inspection.

Commands such as `query`, `context`, and `impact` should surface the aggregate
severity and relevant per-axis warnings before results.

Dirty worktree policy should be explicit. The default should be conservative:
semantic answers are based on committed `HEAD` unless the user runs:

```sh
entire brain refresh --worktree
```

When worktree indexing is enabled, uncommitted file content is hashed and stored
as a dirty overlay. The manifest must clearly identify that the semantic index
does not correspond to a git commit.

Branch overlays should be addressed by `(base_sha, head_sha)`, not just branch
name, so rebases and force-pushes do not silently change meaning. A garbage
collector should prune overlays whose branch no longer exists or whose head/base
pair has aged past a configurable retention window:

```sh
entire brain gc
entire brain gc --older-than 30d
```

`refresh --all-branches` should be bounded by default, for example to local
branches updated within a recent retention window. A full sweep of every local
branch should require an explicit `--force-all-branches`.

Generated brain state should be local cache/state in the first phase. Shared
baseline publishing and hydration are useful later, but they move generated
brain artifacts off the local machine and are intentionally out of scope for the
initial implementation.

## Large Repository Scaling

Whole-repo snapshots are acceptable for bootstrap, but steady-state refresh must
be incremental.

Required mechanisms:

- File-level content hashes or git blob IDs.
- Per-file parse artifacts.
- Stable symbol IDs across runs.
- Reverse-edge and dependent indexes for incremental invalidation.
- Dependency-aware invalidation.
- Chunked writes.
- Atomic index replacement.
- Worker pool parsing.
- Pagination in every query command.
- Partial failure reporting.
- Ignore rules for vendored, generated, binary, secret, and irrelevant paths.
- Build metrics for parse time, cache hit rate, relation count, index size, and
  provider failures.

Refresh algorithm:

1. Resolve tracked files from git.
2. Compute git blob IDs or file hashes.
3. Reuse cached parse artifacts for unchanged files.
4. Parse changed files in bounded worker pools.
5. Use the reverse-edge index to find affected dependents.
6. Rebuild relations for changed files and affected dependents.
7. Update FTS/search indexes incrementally.
8. Write new indexes to a temporary generation.
9. Atomically promote the generation pointer in the manifest.
10. Keep the previous usable index if the refresh fails.

Large repo degradation policy:

- Symbol/name search should work first.
- Relation data may be partial.
- Impact reports must include index completeness and confidence warnings.
- Embeddings should be optional and backgroundable.
- Agent summaries should be capped to relevant files, symbols, or changed areas.
- Impact reports should define parse-failure thresholds. Below the threshold,
  reports are `degraded`; above it, reports are `unsafe` and should tell agents
  to inspect files manually or refresh with a narrower scope.
- If a change report spans a file rename or move that cannot be reconciled to
  stable symbols, the report must say so explicitly rather than silently dropping
  edges.

Initial performance budgets should be written down before implementation. The
numbers can change, but the system needs targets such as:

- Maximum cold index time per 100k source lines.
- Maximum warm refresh time for a one-file edit.
- Maximum query/context/impact latency at default limits.
- Maximum index bytes per 100k symbols.
- Maximum tolerated parse failure rate before semantic completeness becomes
  degraded.

Ignore behavior should be configurable through defaults plus a repo-local file:

```text
.brainignore
```

The default deny list should exclude `.git`, dependency directories, build
outputs, generated lockstep artifacts, common binary/media files, and secret-like
paths. Publish flows must treat semantic artifacts as potentially sensitive,
because symbols, signatures, route names, and configuration references can leak
private information.

Commands must include bounded controls:

```sh
entire brain query "checkout" --limit 20
entire brain impact ValidatePayment --depth 2 --limit 100
entire brain context UserService --include-content=false
```

## Semantic Store Interface

Internal code should depend on an interface similar to:

```go
type SemanticStore interface {
    PutSnapshot(snapshot Snapshot) error
    FindSymbols(query SymbolQuery) ([]Symbol, error)
    Relations(id string, direction Direction, depth int) ([]RelationPath, error)
    Impact(id string, opts ImpactOptions) (ImpactReport, error)
    ChangedSymbols(base, head string) (ChangeReport, error)
}
```

Initial storage can be SQLite-backed:

- `files`
- `symbols`
- `relations`
- `reverse_relations`
- `symbol_fts`
- `relation_index`
- `snapshots`
- `parse_cache`
- `warnings`
- `metrics`

JSONL files should remain available for direct inspection and debugging.

The store should expose pagination cursors and structured error codes from the
first release. The command-line JSON surface should be designed as the future
transport contract, so later adapters can wrap it without inventing new
semantics.

## Commands

### Indexing

```sh
entire brain index .
entire brain index --force
entire brain index --sem-binary entire
entire brain index --skip-sem
entire brain index --worktree
```

`index` should:

1. Resolve the persistent brain path.
2. Run provider diagnostics.
3. Run a snapshot or incremental update.
4. Store raw provider artifacts.
5. Build or update local indexes.
6. Merge semantic metadata into `manifest.json`.

Indexing should respect `.brainignore`, take a repo brain lock, emit build
metrics, and leave the previous semantic generation active on failure.

### Doctor, Repair, And Reset

```sh
entire brain doctor
entire brain repair
entire brain reset .
entire brain reset . --semantic-only
```

`doctor` should validate provider availability, schema compatibility, brain
manifest consistency, SQLite integrity, lock state, freshness axes, and workspace
membership. `repair` can rebuild secondary indexes from raw artifacts when
possible. `reset` deletes local generated semantic state for a repo after
confirmation while leaving source files untouched. Recovery flows should prefer
`--semantic-only` so users do not accidentally delete seed or session history
when only the semantic index is corrupt.

### Local Bundle Import And Export

```sh
entire brain bundle export --output /tmp/repo-brain.tar.zst
entire brain bundle import /tmp/repo-brain.tar.zst --sha256 <bundle-sha256>
```

Bundle import/export is Phase 1 only when it reads and writes explicit local
filesystem paths. It is useful for debugging, backup, migration between local
machines by manual copy, and reproducing CI failures. It must verify checksums
and schema compatibility before import, and it must not imply remote publishing,
remote hydration, registry discovery, or artifact upload.

Bundle arguments must reject non-local schemes such as `http://`, `https://`,
`s3://`, `git+ssh://`, and `file://` URLs with hostnames. Network filesystems
and FUSE mounts are treated as local paths but should receive a warning because
data may leave the machine below Entire Brain's visibility.

Bundle contents should be explicit. The default bundle should include semantic
manifests, semantic SQLite generations, raw provider artifacts needed for
repair, seed summaries, and non-sensitive brain README files. It should exclude
dirty worktree overlays and session transcripts by default unless explicitly
requested. Export should normalize or omit producer absolute paths such as
`repo_root`.

Import must defend against archive attacks: no absolute paths, no `..`
traversal, no symlinks escaping the import root, per-entry size caps, total size
caps, checksum verification, schema compatibility checks, provider version
policy checks, repo-key checks, and symbol ID version checks. Local bundle import
and export should write a local audit log entry for debugging and provenance.

Bundle redaction should reuse `.brainignore` plus bundle-specific deny rules for
paths, symbols, generated summaries, and metadata that should not leave the
current local brain, even via manual file copy.

### Query

```sh
entire brain query "auth validation"
```

Search order:

1. Exact symbol/name match.
2. Full-text search over symbols, signatures, docs, and seed summaries.
3. Relation-aware ranking.
4. Optional local embeddings if configured.

Output should include file paths, line numbers, match reasons, and next useful
commands.

### Context

```sh
entire brain context validateToken
```

Context should show:

- Symbol definition.
- File and line range.
- Signature.
- Direct callers.
- Direct callees.
- Imports.
- Implementations/overrides.
- Routes/tools/workflows that reach the symbol.
- Relevant seed risks or conventions.
- Relevant historical sessions when available.

### Impact

```sh
entire brain impact validateToken
entire brain impact --file internal/auth/token.go
entire brain impact --changed
```

Impact reports should group facts by confidence:

- High confidence: direct graph edges such as `CALLS`, `IMPORTS`, or
  `HANDLES_ROUTE`.
- Medium confidence: same workflow/process/module paths.
- Low confidence: name-only or unresolved dynamic references.

Reports should include:

- Direct callers.
- Downstream callees.
- Imported dependents.
- Entry points affected.
- Tests likely relevant.
- Unknowns and missing index coverage.

### Changes

```sh
entire brain changes --base main --head HEAD
entire brain changes --checkpoint <id>
```

Change reports should combine:

- Semantic diffs.
- Impact report.
- Relevant session transcripts.
- Seed risks and conventions.
- Commands/tests likely to run.

Persist reports under:

```text
semantic/changes/
  <base>..<head>.json
```

### Routes, Tools, And Workflows

```sh
entire brain routes
entire brain tools
entire brain workflows
```

These commands should expose boundary-level context:

- HTTP routes.
- CLI commands.
- MCP/tool handlers.
- Test entrypoints.
- Package/module boundaries.

## Agent Intake

Update intake templates to use semantic context without reading everything.

Recommended order:

1. Run `entire brain path "$PWD"`.
2. Read `README.md`.
3. Read `manifest.json`.
4. Read seed summaries.
5. Run `entire brain stale`.
6. Use `query`, `context`, `impact`, or `changes` for task-specific semantic
   context.
7. Read relevant transcripts only when historical rationale matters.

Generated agent docs:

```text
agent/
  semantic-guide.md
  common-queries.md
  stale-index-policy.md
```

Phase 1 generated agent docs must be deterministic or produced by local-only
tools. Any enrichment that sends seed, session, source, symbol, or semantic
context to a hosted model belongs to Phase 2.

Interpretation rules:

- Seed artifacts are current repository-derived context.
- Semantic artifacts are current-code facts only when fresh for the relevant
  commit/branch.
- Session transcripts are historical rationale.
- Impact reports are partly inferred and must show confidence.
- History gaps and semantic warnings are uncertainty, not facts.

## Multi-Repo Workspaces

Each repository should have its own brain. A separate workspace brain should
coordinate cross-repo work.

Recommended layout:

```text
brain/
  repos/
    gh/org/api/
    gh/org/web/
    gh/org/mobile/
  workspaces/
    payments-platform/
      workspace.json
      workspace.sqlite
      README.md
```

A repo brain owns:

- Session history.
- Seed docs.
- Semantic index.
- Branch overlays.
- Repo-specific commands and risks.

A workspace brain owns:

- Repo membership by repo key, with local paths as machine-specific hints.
- Service names.
- API/contracts between repos.
- Cross-repo search aliases.
- Cross-repo impact links.
- Shared conventions.

Workspace commands:

```sh
entire brain workspace create payments-platform
entire brain workspace add payments-platform ../api --name api
entire brain workspace add payments-platform ../web --name web
entire brain workspace refresh payments-platform
entire brain workspace query payments-platform "checkout flow"
entire brain workspace impact payments-platform api.ValidatePayment
```

Workspace membership should resolve through deterministic repo keys such as
`gh/org/api`. Local paths differ across developer machines and CI runners, so
workspace config should store paths only as optional machine-local hints:

```json
{
  "name": "payments-platform",
  "repos": [
    {
      "repo_key": "gh/org/api",
      "name": "api",
      "local_path_hint": "../api"
    }
  ]
}
```

`local_path_hint` should not be treated as portable workspace identity. If a
workspace config is exported or committed, the CLI should strip local path hints
or clearly mark them as local-only metadata.

Cross-repo symbol references should be namespaced:

```text
<repo-key>:<symbol-id>
```

Workspace freshness is degraded when any member repo is stale for the requested
operation, when a member brain is missing, or when extracted contracts are older
than either side of the provider/consumer pair. Workspace commands should return
per-repo freshness and contract freshness, not only a workspace-level summary.

Initial cross-repo links should come from explicit contracts:

- HTTP route consumers/providers.
- OpenAPI documents.
- Protobuf/gRPC definitions.
- Package exports/imports.
- CLI/tool declarations.
- Environment/config references.

Inferred cross-repo links can be added after explicit contracts are reliable.
Explicit contract extraction is useful for single-repo route and client analysis,
so it should start with boundary extraction before the first workspace release.

## Provider Requirements

Detailed `entire-sem` provider requirements live in the sibling repository:

```text
../entire-sem/docs/semantic_provider_requirements.md
```

Entire Brain should treat `entire-sem` as an artifact-emitting provider. This
plan keeps the provider boundary and consumer contract visible, but the provider
implementation roadmap belongs with `entire-sem`.

## Roadmap

The roadmap is split into two phases. Phase 1 builds the local semantic brain
and verifies it thoroughly in CI without publishing generated brain artifacts.
Phase 2 adds collaborative and distributed features, including anything that
moves generated brain data off the local machine.

## Phase 1: Local Semantic Brain

Phase 1 builds the useful local system first. It should work for a single
developer in one or more local checkouts, across branches, with all generated
state stored under local Entire plugin directories.

### 1. Fresh Semantic Index

- Add semantic manifest source.
- Add `entire brain index`.
- Add provider diagnostics.
- Add `entire brain doctor`.
- Store raw semantic artifacts.
- Add `entire brain stale`.
- Define typed staleness axes.
- Add advisory locking.
- Add `.brainignore`.
- Add schema compatibility checks.
- Add exact symbol search.
- Add local filesystem bundle import/export with checksum validation.
- Add local audit logs for bundle import/export.
- Add provider no-egress checks.
- Add `brain gc` for local overlay retention.

### 2. Incremental Refresh

- Add SQLite-backed semantic store.
- Add file hash/blob cache.
- Add per-file parse artifact cache.
- Add reverse-edge indexes.
- Add atomic index promotion.
- Add partial failure reporting.
- Add branch overlays.
- Add dirty worktree overlays.
- Add build metrics.
- Add bounded `refresh --all-branches` behavior with explicit force for full
  sweeps.

### 3. Query And Context

- Add `entire brain query`.
- Add `entire brain context`.
- Add pagination and structured error codes.
- Update intake templates.
- Generate semantic agent guide.

### 4. Impact And Changes

- Add relation traversal.
- Add `entire brain impact`.
- Add `entire brain changes`.
- Add checkpoint semantic change reports.
- Combine semantic changes with relevant history and seed risks.

### 5. Local Boundaries

- Add route extraction.
- Add tool/CLI handler extraction.
- Add explicit contract extraction.
- Add workflow maps.
- Add relevant-test suggestions.

### 6. Local Workspaces

Local workspaces coordinate multiple repositories that already exist on the same
machine. They do not publish, hydrate, or sync generated brain data.

- Add workspace brain layout.
- Add workspace membership commands.
- Add repo-key-based workspace identity.
- Add workspace freshness reports.
- Add cross-repo contract freshness.
- Add workspace query and impact commands.

### 7. Local Agent Transport

This is still local-only: the transport wraps existing local CLI/JSON services
and reads the local brain store. MCP over stdio is allowed. Network serving of
brain data is Phase 2.

- Add an MCP adapter over stable CLI/JSON services.
- Keep CLI and JSON contracts as the source of truth.
- Add generated MCP usage docs for agents.

### 8. Comprehensive CI Testing

CI for Phase 1 should be broad and deterministic. It may build indexes and
semantic artifacts inside the CI job, but it must not publish those generated
artifacts for reuse outside the job.

- Add provider contract tests with pinned fixture repositories.
- Add golden NDJSON tests for files, symbols, relations, warnings, and partial
  failures.
- Add schema compatibility tests for supported and unsupported provider schema
  versions.
- Add semantic store migration and SQLite integrity tests.
- Add incremental refresh tests for no-op refresh, one-file edits, file moves,
  renames, deletes, and branch overlays.
- Add dirty worktree overlay tests for staged, unstaged, and untracked selected
  files.
- Add reverse-edge invalidation tests to prove dependents are refreshed without
  full reindexing.
- Add `.brainignore` tests for vendored, generated, binary, secret-like, and
  explicitly included paths.
- Add advisory lock/concurrent index tests.
- Add stale report tests for every freshness axis.
- Add query/context/impact JSON contract tests with pagination and structured
  error codes.
- Add symbol ID move/rename tests that document expected continuity or known
  breakage.
- Add provider-absent, provider-version-mismatch, unsupported-major-schema, and
  malformed-NDJSON tests.
- Add crash-mid-promotion tests that prove the previous index generation remains
  usable.
- Add read-only and permission-denied filesystem tests.
- Add symlink, path traversal, archive traversal, and very-long-path tests.
- Add `brain gc` and aged-overlay tests.
- Add staleness severity aggregation tests, including end-to-end warnings from
  `context` and `impact`.
- Add local workspace tests with multiple fixture repos and repo-key identity.
- Add command-level integration tests for `index`, `stale`, `doctor`, `repair`,
  `reset`, `reset --semantic-only`, `gc`, `bundle export`, `bundle import`,
  `query`, `context`, `impact`, `changes`, and workspace commands.
- Add bundle round-trip tests for checksum validation, schema compatibility, and
  local path-only enforcement.
- Add bundle security tests for excluded dirty overlays, excluded sessions by
  default, absolute path normalization, archive size caps, and redaction rules.
- Add performance smoke tests with budget thresholds for cold index, warm
  refresh, query latency, memory use, and index size.
- Add race tests where supported by the Go toolchain.
- Add deny-by-default egress enforcement in the CI harness, not only assertions
  after the fact. Phase 1 tests must not use network, hosted model APIs, remote
  embedding providers, publish commands, hydrate commands, or telemetry upload.

## Phase 2: Shared And Distributed Brain

Phase 2 is for collaboration features that move generated brain artifacts beyond
one machine. These should wait until the Phase 1 local contracts, storage, and
freshness model are stable.

### 1. Shared Baselines

- Add publish/hydrate commands for commit-addressed generated artifacts.
- Support CI-produced default-branch baselines.
- Add artifact integrity checks.
- Document security and retention policy.
- Define retention and garbage collection for remote artifacts.
- Require explicit opt-in before publishing semantic artifacts.

### 2. Remote Workspace Hydration

- Allow workspace members to hydrate repo brains from approved shared baselines.
- Keep local overlays local by default.
- Validate remote artifact schema, provider version, repo key, commit, and
  checksums before installation.
- Never overwrite newer local generated state without explicit `--force`.

### 3. Team Policy And Security

- Add policy files for which repos may publish semantic artifacts.
- Add redaction/deny rules for paths, symbols, and generated summaries.
- Add audit metadata for publish and hydrate operations.
- Document that semantic artifacts may contain private symbol names, route names,
  configuration references, and other sensitive metadata.

### 4. Hosted Enrichment And Remote Evaluation

- Add opt-in hosted embedding providers.
- Add opt-in hosted-model summaries and wiki-like generated docs.
- Add cloud review/eval integrations that package semantic context for external
  agents.
- Require explicit user/team policy before sending semantic context off-machine.

### 5. Remote Serving And Discovery

- Add remote MCP/HTTP serving only after local stdio transport is stable.
- Add artifact registry or baseline discovery services.
- Add cross-machine workspace membership sync.
- Add remote repo fetch/index-by-URL flows with explicit consent and security
  policy.

## Key Design Decisions

- Make brain state commit-addressed and branch-aware.
- Keep generated artifacts local by default.
- Use an artifact-emitting provider boundary for parsing.
- Treat provider schemas, relation vocabulary, and warning codes as explicit
  contracts.
- Keep storage behind an interface.
- Prefer one semantic database per active index generation.
- Make the default experience useful without embeddings.
- Prefer exact facts first, inferred context second.
- Always expose freshness, confidence, and completeness.
- Model dirty worktree state explicitly.
- Fail fast on index lock contention by default and support explicit wait later.
- Support repair/reset workflows, with semantic-only reset as the preferred
  recovery path.
- Let each repo own its own brain; use workspace brains for cross-repo work.
- Identify workspace members by repo key, not local path.
- Revisit the single-SQLite design only when measured index size, query latency,
  or write contention crosses documented thresholds.
