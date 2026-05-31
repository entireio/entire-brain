# Phase 1 Semantic Brain Progress

## 2026-05-31

Branch: `phase-1-semantic-brain`

Initial release 1 commit: `7f7f659` (`Implement semantic brain fresh index`),
pushed to `origin/phase-1-semantic-brain`. Follow-up release-1 hardening commit:
`f244a1f` (`Harden semantic brain release one`), also pushed to
`origin/phase-1-semantic-brain`.

### Release 1: Fresh Semantic Index

Implemented in Entire Brain:

- Added semantic manifest source metadata under `sources.semantic`.
- Added local semantic `index` command with provider diagnostics, raw NDJSON
  snapshot storage, schema-major compatibility checks, `.brainignore` support,
  advisory lock handling, and provider no-egress status recording.
- Added typed semantic `stale` reports with JSON output and freshness axes for
  head, branch tip, worktree, provider, and semantic completeness.
- Extended `doctor` to report semantic brain freshness when a repo root is
  available.
- Added exact symbol `query` over the raw provider snapshot.
- Added local-only `bundle export` and `bundle import` with path scheme
  rejection, archive traversal defenses, checksum reporting, and local audit log
  entries.
- Added `gc` for pruning old local semantic snapshots while preserving the
  active snapshot.

Coordination with `../entire-sem`:

- Read `../entire-sem/docs/semantic_provider_requirements.md`.
- The Entire Brain integration expects `entire sem doctor --json` and
  `entire sem snapshot --repo <path> --format ndjson`.
- The provider-side no-egress field names are accepted conservatively:
  `no_egress`, `no_egress_verified`, or `local_only` verify local-only status;
  `network_egress`, `requires_network`, or `network_required` degrade the
  report as a Phase 1 violation.

Verification:

- `go test ./...` passes.
- `mise run check` passes.
- `entire review` found release-1 safety issues; fixed no-egress hard failure,
  persisted `.brainignore` redaction, dirty-worktree default rejection, private
  bundle archive permissions, bundle import manifest validation, and `--force`
  enforcement.
- Follow-up review found additional local-only hardening gaps; fixed fail-closed
  provider no-egress diagnostics, dirty worktree snapshot fingerprinting, and
  bundle import total size / entry count limits.
- Later review found worktree overlay edge cases and Codex seed-agent schema
  drift; fixed clean-after-worktree-overlay freshness, staged diff
  fingerprinting, and restored Codex `--output-schema` enforcement.
- Final release-1 hardening pass fixed Codex output-schema file handling,
  `.brainignore` redaction for provider header warnings and partial failures,
  and semantic bundle import merging so seed/session sources are preserved.
- Additional review pass fixed semantic query freshness reporting, streaming
  bundle import limit enforcement, and imported semantic schema validation.
- Latest review pass changed `--worktree` indexing to fail explicitly until the
  provider exposes worktree snapshot support, required expected SHA-256 input
  for bundle import, and validates every imported NDJSON snapshot record.
- Bundle hardening follow-up fixed export-overwrite permissions, excludes local
  audit logs from bundles, and requires/cross-checks repo keys between bundle
  manifests and semantic snapshot headers.
- Latest bundle review fixed recursive/self-including export paths by rejecting
  outputs inside the active brain directory and rejects imported audit logs as
  local-only data.
- Provider-boundary review added `--no-network` to live provider snapshot
  invocation and validates live snapshot repo key, commit, and tree before
  storing artifacts.
- Freshness/checksum review now requires live provider snapshots to include
  commit and tree, hashes the full bundle file before import to catch trailing
  bytes, and fixes `.git` default ignore matching so `.github` remains visible.
- Bundle state review now requires snapshot header repo keys, restricts bundle
  contents to `semantic/snapshots/**` plus manifest, and rejects symlinked
  outputs that resolve inside the active brain directory.
- Export safety review now rejects symlinks inside exported snapshot trees and
  stores live snapshots under commit-plus-content generation directories to
  avoid same-commit in-place overwrite during forced reindex.
- Bundle privacy/import review now exports sanitized semantic-only manifests,
  persists only the manifest-referenced validated snapshot on import, and drops
  relation records whose endpoint IDs mention ignored paths.
- README/failure-path review added semantic-only README rendering, validates
  semantic bundle inputs before truncating export outputs, and removes Codex
  seed-agent schema temp files after each phase.
- GC/ignore review now fails closed when `manifest.json` cannot be parsed before
  pruning snapshots and fixes default ignored directory matching to use path
  segments instead of broad string prefixes.
- Bundle consistency review now fails export when the active snapshot is missing
  and rejects non-canonical imported `snapshot_path` values.
- Export/import race review moved active snapshot validation before output
  truncation and hashes/imports bundles through the same opened file descriptor.
- Bundle export privacy review now validates active snapshot paths with the
  import policy before opening outputs, including canonical path, snapshot
  directory, basename, non-symlink file, and snapshot header/schema checks.
- Latest review pass marks `--skip-sem` indexes unsafe for semantic completeness,
  validates seed-agent output schema/status before writing artifacts, and
  changes semantic NDJSON filtering to a two-pass scanner with an explicit
  record-size cap instead of retaining every parsed record line.
- Scanner consistency review now uses the shared semantic record-size policy for
  query and bundle snapshot validation as well as indexing.
- Seed-agent hardening review added Codex `--ignore-user-config` /
  `--ignore-rules` and Claude `--bare` to keep synthesis isolated from local or
  project agent configuration.
- Cleanup/path review now removes the actual Codex schema file by locating
  `--output-schema` dynamically and validates manifest snapshot paths before
  semantic query reads.
- Checkpoint-context review reverted Codex `--output-schema` usage to avoid the
  known local CLI schema-dialect failure; seed-agent JSON is enforced by prompt
  plus local `schema_version` and `status` validation. Bundle count validation
  now treats omitted zero counts as unknown instead of mismatches.
- Latest review pass rejects imported semantic worktree overlay metadata while
  worktree indexing is unsupported and rejects extra seed-agent artifact files
  beyond the phase's required set.
- Final hardening passes fixed Claude Code seed-agent isolation without
  `--bare`, semantic snapshot symlink and hardlink defenses across index,
  stale, query, GC, bundle import, and bundle export, bundle import count and
  provenance backfilling, blank-line-tolerant query, and fail-closed worktree
  status checks.
- Final `entire review` pass reported no actionable findings. `mise run check`
  passed after the final changes.

Notes for the next release:

### Release 2: Incremental Refresh

Implemented in Entire Brain:

- Added a SQLite-backed semantic generation under
  `semantic/generations/<generation>/semantic.sqlite`.
- Added generation metrics, file/blob cache metadata, parse-cache artifact
  metadata, symbol indexes, relation rows, and reverse-edge rows.
- Added atomic generation build-and-promote behavior using a temporary
  generation directory and rename.
- Updated semantic query to prefer the SQLite store and fall back to raw NDJSON
  when importing older bundles.
- Added dirty worktree overlay indexing through an explicit provider
  `--worktree` snapshot mode and worktree fingerprint metadata.
- Added branch overlay metadata for feature-branch snapshots and a bounded
  local `refresh --semantic --all-branches` flow that writes bounded branch
  overlay metadata from local refs only.
- Extended local bundles to include semantic generation files in addition to
  the manifest and raw snapshot.
- Hardened release-2 generated artifacts after review: provider paths are
  repo-relative, generated SQLite stores are validated for integrity/schema and
  counts, bundle imports replace generation directories as a unit, unreferenced
  generation entries are rejected, audit paths are preflighted, worktree
  fingerprints fail closed, clean `--worktree` indexes remain HEAD indexes,
  query treats SQLite search text literally, and stale reports validate declared
  stores.

Verification:

- `go test ./...` passed after release-2 implementation and hardening.
- `mise run check` passed after release-2 implementation and hardening.
- `entire review` reported no actionable findings for release 2.

### Release 3: Query And Context

Implemented in Entire Brain:

- Added pagination metadata and `--offset` support to `entire brain query`.
- Added `entire brain context` with JSON/text output, symbol matches,
  relation context, pagination, and optional bounded source snippets.
- Added declared-store validation before query/context open SQLite stores, so
  read-only commands do not recreate missing stores.
- Added context/query hardening for symlinked source paths, snippet size caps,
  lock handling, blank-line snapshot fallback, and literal SQLite text search.
- Added `docs/semantic_agent_guide.md` and updated generated README intake text
  to point agents at `query` and `context`.

Verification:

- `go test ./...` passed after release-3 implementation and hardening.
- `mise run check` passed after release-3 implementation and hardening.
- `entire review` reported no actionable findings for release 3.

### Release 4: Impact And Changes

Implemented in Entire Brain:

- Added `entire brain impact` to traverse semantic relations from matching
  symbols with JSON/text output and stale-data reporting.
- Added `entire brain changes` to map changed, renamed, copied, and untracked
  files to indexed semantic symbols and write `semantic/changes/latest.json`.
- Added snapshot fallback for impact traversal so imported raw semantic bundles
  without a SQLite generation can still answer relation-aware impact queries.
- Added explicit `refresh --semantic-worktree` handling so seed `--worktree`
  mode no longer opts semantic indexing into dirty worktree snapshots.
- Hardened release-4 paths after review: untracked directories are expanded,
  rename/copy old paths are included for change impact, impact returns
  relations even when the root symbol fills the symbol limit, and bundle export
  rejects symlink output paths before opening them.
- Updated `docs/semantic_agent_guide.md` for the new semantic worktree flag.

Verification:

- Focused release-4 tests passed for impact traversal, snapshot fallback,
  change detection, refresh worktree flag behavior, and symlink bundle output
  rejection.
- `go test ./...` passed after release-4 implementation and hardening.
- `mise run check` passed after release-4 implementation and hardening.
- `entire review` reported no actionable findings for release 4.

### Release 5: Local Boundaries

Implemented in Entire Brain:

- Added `entire brain routes`, `entire brain tools`, and
  `entire brain workflows` commands with stable JSON/text output over local
  semantic boundary symbols and handler relations.
- Added `entire brain tests <symbol-or-text>` to suggest relevant local test
  symbols from semantic matches, relation context, and same-directory signals.
- Implemented boundary views for both SQLite-backed generations and raw
  snapshot-only imports.
- Updated `docs/semantic_agent_guide.md` to include boundary and test-suggestion
  intake steps.

Verification:

- Focused release-5 tests passed for routes/tools/workflows, relevant-test
  suggestions, and snapshot fallback.
- `go test ./...` passed after release-5 implementation.
- `mise run check` passed after release-5 implementation.
- `entire review` reported no actionable findings for release 5.

### Release 6: Local Workspaces

Implemented in Entire Brain:

- Added `entire brain workspace create`, `add`, `refresh`, `query`, and
  `impact` for local-only multi-repo workspaces.
- Added workspace brain layout under `brain/workspaces/<name>/` with
  deterministic `workspace.json` and `README.md`.
- Workspace membership stores repo-key identity plus local-only resolved repo
  path hints, and workspace query/impact fan out across existing member repo
  brains.
- Workspace refresh now reports per-repo semantic freshness, validates path-hint
  repo keys, rejects unsafe workspace names/repo keys, and refuses symlinked
  workspace path components.
- Hardened related semantic freshness behavior after review: ignored files are
  filtered consistently for dirty checks, worktree fingerprints, and
  `brain changes`; worktree-backed semantic bundle exports are rejected before
  producing non-importable bundles.

Verification:

- Focused release-6 tests passed for workspace lifecycle/query/impact,
  workspace freshness, unsafe repo keys/names, symlinked workspace paths,
  path-hint repo-key mismatch, and ignore-aware worktree/change behavior.
- `go test ./...` passed after release-6 implementation and hardening.
- `mise run check` passed after release-6 implementation and hardening.
- `entire review` reported no actionable findings for release 6.

### Release 7: Local Agent Transport

Implemented in Entire Brain:

- Added `entire brain mcp`, a local stdio-only MCP/JSON-RPC adapter.
- Exposed MCP tools that wrap existing CLI JSON contracts:
  `brain_stale`, `brain_query`, `brain_context`, `brain_impact`, and
  `brain_changes`.
- Kept CLI JSON output as the source of truth by returning the existing command
  `--json` payloads as MCP text content.
- Hardened the MCP adapter to reject oversized/negative frames before
  allocation and to return tool errors for invalid integer arguments instead of
  silently falling back.
- Took per-repo semantic index locks around workspace query/impact reads so
  concurrent refreshes cannot race manifest/store access.
- Normalized `.brainignore` recursive `**` matching across in-process snapshot,
  warning, untracked, and change filtering so it matches the git pathspec
  exclusions used for dirty/fingerprint checks.
- Added `docs/semantic_mcp_guide.md` and updated the semantic agent guide with
  local MCP usage guidance.

Verification:

- Focused release-7 tests passed for MCP initialize/tools-list and
  `brain_query`, `brain_context`, `brain_impact`, and `brain_changes` tool
  calls over local semantic data.
- Focused hardening tests passed for invalid MCP integer arguments, oversized
  and negative MCP frames, and locked workspace query/impact reads.
- Focused recursive `.brainignore` tests passed for nested semantic redaction
  and direct matcher behavior.
- `go test ./...` passed after release-7 implementation and hardening.
- `mise run check` passed after release-7 implementation and hardening.
- `entire review` reported no actionable findings for release 7 after
  hardening.

### Release 8: Comprehensive CI Testing

Implemented in Entire Brain:

- Added `mise run test:phase1`, a race-enabled deterministic Phase 1 semantic
  contract suite for `index`, `stale`, `query`, `context`, `impact`, `changes`,
  bundle import/export, local workspaces, MCP, and semantic refresh tests.
- Added a `phase1-semantic` GitHub Actions job that primes module downloads,
  then runs the Phase 1 semantic suite with `GOPROXY=off` and `GOSUMDB=off` so
  the test phase itself cannot fetch modules.
- Added a command-level JSON contract test that drives the local semantic
  workflow through the root CLI and asserts the provider snapshot invocation
  includes `--no-network`.
- Added Phase 1 command-runner egress checks in that contract test to fail on
  network-style external commands such as `git fetch`, `curl`, `wget`, `ssh`,
  `scp`, or `gh`.
- Hardened semantic writes to reject symlinked brain roots and existing
  symlinked repo brain parent components before creating audit logs, locks,
  snapshots, generations, or bundle-import state.
- Hardened workspace path resolution to reject a symlinked `brain/` root before
  writing workspace manifests.
- Redacted provider `repo_root` from persisted semantic snapshots, including
  `--skip-sem` snapshots, so bundle exports cannot leak local checkout paths.
- Sanitized the active snapshot during bundle export as a defense for legacy or
  imported snapshots that still contain `repo_root`.
- Bounded parse-cache content hashing for indexed files so large files are not
  read fully into memory during generation builds.
- Updated `mise run check` so local and CI checks run the Phase 1 semantic
  suite in addition to lint, full race tests, and cross-builds.

Verification:

- Focused symlink-root and symlink-parent hardening tests passed for semantic
  index writes and workspace creation.
- Focused provider, `--skip-sem`, and legacy/imported-style bundle snapshot
  redaction tests passed.
- Focused oversized content-hash tests passed.
- Focused generated bundle round-trip test passed for export, import into a
  fresh brain dir, `stale`, and `query`.
- `go test ./internal/cli -run TestPhase1SemanticCommandJSONContracts` passed.
- `mise run test:phase1` passed.
- `go test ./...` passed after release-8 implementation.
- `mise run check` passed after release-8 implementation.
- `entire review` reported no actionable findings for release 8 after
  hardening and round-trip coverage.

### Current Phase 1 Gap Check

Compared against `docs/semantic_brain_plan.md`, the eight Phase 1 releases are
implemented and validated for the local semantic brain path. Fresh indexing,
incremental SQLite generations, query/context, impact/changes, boundary views,
workspaces, stdio MCP, local bundles, no-egress provider invocation, and
deterministic CI coverage are present.

Live self-validation with the fixed `../entire-sem` provider passed for this
repo:

- `entire sem doctor --json`
- `entire sem snapshot --repo . --format ndjson --no-network`
- `entire brain index . --sem-binary <fixed-provider> --worktree --force`
- `stale`, `query`, `context`, `impact`, `changes`, `routes`, `tools`,
  `workflows`, `tests`
- workspace create/add/refresh/query/impact
- bundle export/import, `gc`, and MCP `tools/list`
- A follow-up hardening pass now marks failed `git rev-parse HEAD` and
  `git branch --show-current` lookups as unsafe freshness axes instead of
  comparing against empty current values.

Gap-closure work for the original Phase 1 plan:

- Added `entire brain repair`, which rebuilds derived SQLite generations from
  the active local semantic snapshot without invoking the provider.
- Added `entire brain reset --semantic-only --force`, which removes semantic
  artifacts and semantic manifest metadata while preserving other brain sources.
- Added `entire brain reset --force` for removing the generated repo brain
  directory without touching source files.
- Added structured JSON error envelopes for commands that opt into `--json`.
- Added explicit Phase 1 performance-budget smoke coverage for index and query.
- Added write-failure and read-only filesystem coverage for semantic indexing.
- Added workspace contract freshness fields alongside per-repo semantic
  freshness so workspace refresh/query/impact output can distinguish semantic
  data freshness from cross-repo contract freshness.
- Expanded GitHub Actions so the deterministic Phase 1 semantic suite runs on
  Linux, macOS, and Windows, not only Linux.

Completed validation for this gap-closure pass:

- Focused semantic lifecycle, workspace contract, JSON-error, performance,
  filesystem, and cross-platform path tests passed.
- `go test ./...` passed.
- `mise run check` passed.
- `entire review` initially found Windows path assertions and partial JSON error
  coverage; both were fixed. The follow-up `entire review` reported no
  actionable findings.
- Commit/push and GitHub Actions follow-up on all three operating systems are
  next.
