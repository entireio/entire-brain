# Seed Command Plan

## Goal

Add `entire brain seed` as a way to create useful brain context for repositories
that have little or no Entire session history. A seed is repository-derived
baseline knowledge: what the project is, how it is structured, how to work on
it, and what conventions or risks matter.

The seed must coexist with real session exports. When Entire is enabled after a
project already exists, the seed explains the pre-Entire baseline and the
session logs explain later development history.

This plan is intentionally phased. Every phase must be fully tested before the
next phase builds on it.

## Core Design Decisions

1. A brain has multiple sources. `seed` owns baseline repository knowledge;
   `export` owns real session history.
2. `manifest.json` and `README.md` are shared brain-level artifacts. Individual
   commands must not render or overwrite them independently.
3. Deterministic analysis is required. Agent synthesis is optional and additive.
4. Persistent writes should be staged and promoted atomically so failed agent
   runs or partial exports do not corrupt an existing brain.
5. The oldest-commit/oldest-session rule is a heuristic, not proof. The brain
   should record the decision and confidence.

## Command Shape

```sh
entire brain seed [path]
entire brain seed ../agentviz
entire brain seed --output ./agentviz-brain
entire brain seed --update
entire brain seed --force
```

Seed flags:

| Flag | Purpose |
| --- | --- |
| `--output, -o` | Write to an explicit output directory. The directory must be empty unless `--force` is set. |
| `--update` | Refresh an existing seed in place. Persistent mode behaves like update once a seed exists. |
| `--force` | Overwrite an existing explicit output directory or replace an existing persistent seed. |
| `--include-tests` | Include tests in inventory and summaries. Default: `true`. |
| `--max-file-bytes` | Skip very large source/doc files. Default proposal: `256k`. |
| `--max-files` | Cap scanned files. Default proposal: `2000`. |
| `--format` | Output format. Initial value: `markdown+json`. |
| `--worktree` | Include tracked files plus selected untracked instruction/docs files. Default: `false`. |

Agent flags:

| Flag | Purpose |
| --- | --- |
| `--agent none\|codex\|command` | Select agent-backed synthesis. Default: `none` until the deterministic seed and agent contract are stable. |
| `--agent-command <argv...>` | Explicit command mode. Parsed as argv, not through a shell. |
| `--agent-quick-timeout <duration>` | Timeout for the quick synthesis phase. Default: `2m`. |
| `--agent-deep-timeout <duration>` | Timeout for the deep synthesis phase. Default: `10m`. |
| `--agent-timeout-action keep-quick\|continue\|fail` | Non-interactive action when the deep phase times out. Default: `keep-quick`. |
| `--agent-max-input-bytes <bytes>` | Bound the packet sent to the agent. Default proposal: `500000`. |
| `--interactive` | Allow timeout prompts when stdin is a terminal. Default: auto for TTY only. |
| `--no-interactive` | Disable prompts even when stdin is a terminal. |
| `--require-agent` | Fail if required agent synthesis does not complete. By default this requires at least quick synthesis success. |

## Output Layout

Persistent output uses the same repo-key storage model as export:

```text
${data}/brain/<repo-key>/
  manifest.json
  README.md
  seed/
    repo-overview.md
    architecture.md
    commands.md
    conventions.md
    risks.md
    file-index.json
    docs/
      README.md
      CLAUDE.md
      SECURITY.md
      ...
    agent/
      quick-overview.md
      overview.md
      architecture.md
      risks.md
      maintenance-guide.md
      open-questions.md
      prompt.json
      response.json
  sessions/
    ...
```

The seed must not fake session transcripts. Session history remains owned by
`export`. Seed output is a separate source in the combined brain.

## Shared Brain Package

Add a shared internal package or module before implementing `seed`:

```text
internal/cli/brain.go
internal/cli/brain_test.go
```

Responsibilities:

- load an existing flat export manifest or combined manifest
- migrate flat export manifests into the combined structure
- merge seed source updates without dropping session source data
- merge session source updates without dropping seed source data
- preserve unknown manifest fields where practical
- render one combined `README.md`
- write `manifest.json` and `README.md` atomically through a staging directory
- validate that source-owned cleanup only touches source-owned files

This avoids the current risk where `export` writes `manifest.json` and
`README.md` directly and would erase seed metadata on the next run.

## Manifest Model

The combined manifest should use a new schema version. Existing flat export
manifests are read as legacy schema and migrated in memory.

```json
{
  "schema_version": 2,
  "generated_at": "2026-05-30T00:00:00Z",
  "repo_root": "/path/to/repo",
  "repo_key": "gh/example/project",
  "sources": {
    "seed": {
      "generated_at": "2026-05-30T00:00:00Z",
      "commit": "abc123",
      "worktree_mode": "tracked",
      "file_fingerprint": "sha256:...",
      "summary_path": "seed/repo-overview.md",
      "documents": [],
      "entrypoints": [],
      "commands": [],
      "history_baseline": {
        "oldest_commit_at": "2024-01-01T00:00:00Z",
        "oldest_session_at": "2026-01-01T00:00:00Z",
        "seed_required": true,
        "reason": "oldest commit predates oldest session",
        "confidence": "heuristic"
      },
      "warnings": []
    },
    "sessions": {
      "generated_at": "2026-05-30T00:00:00Z",
      "transcript_mode": "compact",
      "scope": "all",
      "checkpoint_limit": 10000,
      "checkpoints_scanned": 0,
      "session_count": 0,
      "oldest_session_at": null,
      "latest_checkpoint_id": "",
      "branches": [],
      "sessions": [],
      "warnings": []
    }
  },
  "warnings": []
}
```

Agent metadata is nested under `sources.seed.agent`:

```json
{
  "mode": "codex",
  "quick": {
    "status": "success",
    "timeout": "2m",
    "input_fingerprint": "sha256:...",
    "output_paths": ["seed/agent/quick-overview.md"]
  },
  "deep": {
    "status": "timeout",
    "timeout": "10m",
    "timeout_action": "keep-quick",
    "input_fingerprint": "sha256:...",
    "output_paths": []
  }
}
```

Legacy compatibility:

- Existing consumers expecting flat `sessions` should be considered. If needed,
  keep top-level `sessions` and `branches` as compatibility aliases during one
  transition period.
- `export` should read either schema and write schema 2 after migration.
- `seed` should read either schema and write schema 2 after migration.

## Brain README Ownership

One renderer owns the root `README.md`.

README order:

1. Brain summary.
2. Seeded baseline: what the repo is and how it is structured.
3. Session history: what changed while Entire was recording.
4. Historical gaps: whether the oldest commit predates the oldest session.
5. Warnings and incomplete analysis notes.

Command-specific files may exist under `seed/` or `sessions/`, but the root
README must always be produced from the combined manifest.

## Seed Cursor

Seed maintains state outside the durable brain output:

```text
${state}/brain/<repo-key>/seed.json
```

Cursor fields:

- schema version
- repo root
- repo key
- last git commit
- worktree mode
- tracked files with hash, size, mtime fallback, category, and inclusion reason
- generated deterministic artifact paths
- deterministic packet fingerprint
- quick agent input fingerprint, status, and output paths
- deep agent input fingerprint, status, and output paths

Do not delete valid agent output just because deterministic markdown was
regenerated. Agent output is invalidated only when its input fingerprint changes
or the user passes `--force`.

## Deterministic Analysis

The deterministic pass always runs, even when agent synthesis is enabled.

Source policy:

- Default mode is committed repo state: use `git ls-files`.
- `--worktree` includes tracked files plus selected untracked instruction/docs
  files. It must record `worktree_mode: "worktree"` in the manifest.
- Files ignored by git remain excluded unless explicitly allowlisted later.
- Fallback filesystem walk is used only when not inside a git worktree.

Steps:

1. Resolve target repo with existing local path logic.
2. Resolve persistent brain paths with existing repo-key logic.
3. Inventory files with `git ls-files`; fall back to filesystem walk outside
   git repos.
4. Apply denylist and allowlist rules.
5. Collect high-signal docs:
   - `README*`
   - `CLAUDE.md`
   - `AGENTS.md`
   - `CONTRIBUTING.md`
   - `SECURITY.md`
   - `.github/copilot-instructions.md`
   - `.codex/*`
   - `.cursor/rules*`
   - `docs/**/*.md`
6. Parse project metadata:
   - `package.json`
   - `go.mod`
   - `pyproject.toml`
   - `Cargo.toml`
   - equivalent common manifests
7. Produce path-based architecture categories:
   - entrypoints
   - application code
   - server/API code
   - libraries
   - hooks/contexts
   - tests
   - docs/config
8. Extract lightweight source structure when docs are sparse:
   - exported package/module names
   - top-level functions/classes/components
   - route definitions
   - CLI commands
   - test names and fixture locations
9. Extract commands from metadata files.
10. Extract conventions from docs and agent instruction files.
11. Write deterministic markdown and JSON artifacts.

## File Safety And Redaction

Before copying content into `seed/docs` or the agent packet:

- Skip binary/media files.
- Skip generated directories such as `node_modules`, `dist`, `build`,
  `coverage`, `.git`, vendored dependency trees, and lockfiles unless used only
  for metadata.
- Skip likely secret files, including `.env`, `.env.*`, private keys,
  credentials, tokens, certificates, npm/yarn auth files, and cloud provider
  config.
- Include `SECURITY.md` as documentation, but do not infer or expose private
  vulnerability details from issue trackers or external systems.
- Store only excerpts for very large docs.
- Record skipped files and reasons in `seed/file-index.json`.

The plan intentionally avoids network access for seed v1. Any later network
mode should be explicit and documented separately.

## Agent Synthesis

Agent synthesis is additive. It should never be the only source of seed
knowledge.

The agent receives a bounded seed packet:

- repository inventory
- selected docs and excerpts
- package metadata
- entrypoint candidates
- source/test layout
- lightweight source structure
- deterministic summaries

The agent should not blindly ingest every repository file.

### Agent Invocation Contract

Agent command mode must be argv-safe. Do not run `--agent-command` through a
shell.

Input to the agent is JSON on stdin:

```json
{
  "schema_version": 1,
  "phase": "quick",
  "repo": {
    "root": "/path/to/repo",
    "key": "gh/example/project",
    "commit": "abc123"
  },
  "limits": {
    "max_output_bytes": 200000
  },
  "inventory": [],
  "documents": [],
  "metadata": {},
  "deterministic_summaries": {}
}
```

Expected stdout is JSON:

```json
{
  "schema_version": 1,
  "status": "success",
  "model": "gpt-5.5",
  "artifacts": {
    "quick-overview.md": "# Overview\n..."
  },
  "warnings": []
}
```

Validation rules:

- stdout must be valid JSON.
- artifact paths must be relative, clean, and stay under `seed/agent/`.
- output bytes must stay under the configured limit.
- missing required quick artifact makes quick phase fail.
- missing required deep artifacts makes deep phase incomplete.
- stderr is captured as warnings unless the command exits non-zero.

### Quick Phase

The quick phase is designed to finish fast and leave the seed useful even if
the deep phase times out.

Default timeout: `2m`.

Expected output:

```text
seed/agent/quick-overview.md
```

Content:

- what the project does
- main architecture
- key commands
- most important conventions
- open questions or likely blind spots

### Deep Phase

The deep phase receives the deterministic packet plus the quick result.

Default timeout: `10m`.

Expected output:

```text
seed/agent/overview.md
seed/agent/architecture.md
seed/agent/risks.md
seed/agent/maintenance-guide.md
seed/agent/open-questions.md
```

If the deep phase times out:

- Prompt only when stdin is a terminal and interactive mode is enabled:

  ```text
  Deep synthesis has been running for 10m. Keep quick result, continue, or fail?
  [k]eep quick / [c]ontinue / [f]ail:
  ```

- In non-interactive mode, use `--agent-timeout-action`.
- Default action is `keep-quick`.
- The command exits `0` when deterministic seed and quick synthesis are
  available.
- With `--require-agent`, timeout defaults to `fail` unless configured
  otherwise.

Timeout action semantics:

| Action | Behavior |
| --- | --- |
| `keep-quick` | Cancel deep synthesis, keep deterministic and quick outputs, mark deep status as timeout. |
| `continue` | Wait another deep timeout interval, then prompt or apply action again. |
| `fail` | Cancel deep synthesis and exit non-zero. Do not promote partial output into persistent brain unless only a temp/staging directory was used. |

## Update Lifecycle

`seed` updates similarly to `export`:

- Persistent mode updates the existing seed in place.
- Explicit `--output` remains one-shot unless `--force`.
- Seed owns only `seed/*` and its manifest subsection.
- Export owns `sessions/*` and its manifest subsection.
- Cleanup must never delete the other source's files.
- Root `manifest.json` and root `README.md` are shared and written only through
  the shared brain package.

Update triggers:

1. User runs `entire brain seed --update`.
2. No seed exists.
3. Seed cursor is stale relative to selected source files.
4. Oldest git commit is older than oldest session log.
5. User runs a future `entire brain refresh`.

## Session Coexistence

When real Entire sessions later exist, `export` should add or update
`sessions/*` without removing `seed/*`.

The seed should always be created when the oldest commit happened before the
oldest session log. This will be common because most projects predate Entire
being enabled.

Implementation:

```sh
git log --reverse --format=%aI --max-count=1
```

Compare the oldest commit timestamp to the oldest session timestamp from the
session source in the combined manifest. If:

```text
oldest_commit_at < oldest_session_created_at
```

then ensure `seed/*` exists and is current.

Caveat: this is a heuristic. Rebases, squashes, imported history, rewritten
dates, and late-added checkpoint refs can make the comparison misleading. The
manifest should record the decision as `confidence: "heuristic"` and include a
warning when timestamps are missing or suspicious.

If no git history is available, seed when sessions are absent or the seed is
missing.

## Future Refresh Command

Later, add:

```sh
entire brain refresh
```

Planned behavior:

1. Run `export`.
2. Inspect exported session count and oldest session timestamp.
3. Run `seed --update` when:
   - session count is zero
   - seed is missing
   - oldest commit predates oldest session
   - seed cursor is stale
4. Write one combined `manifest.json` and `README.md`.

After `refresh` exists, `path` can call `refresh` instead of only `export` when
materializing a missing persistent brain.

## Implementation Phases

### Phase 1: Shared Brain Manifest And Renderer

Implement the shared brain load/merge/render/write layer and migrate `export`
to use it.

Tests:

- Load current flat export manifest.
- Migrate flat export manifest to schema 2 in memory.
- Merge session source without dropping seed source.
- Preserve seed source when export runs.
- Render README with sessions only.
- Render README with seed only.
- Render README with both sources.
- Atomic write does not corrupt existing brain on failure.
- Existing `export` tests still pass.

### Phase 2: Deterministic Seed

Implement `entire brain seed --agent none`.

Tests:

- `seed` writes manifest and README for a repo with no checkpoints.
- `seed` extracts commands from `package.json`.
- `seed` includes allowed docs such as `README.md`, `CLAUDE.md`, and
  `.github/copilot-instructions.md`.
- `seed` skips secrets, binary files, generated directories, lockfiles, and
  oversized files with recorded reasons.
- `--worktree` includes selected untracked instruction/docs files and records
  worktree mode.
- `--output` rejects non-empty dirs unless `--force`.
- persistent seed uses repo-key storage.
- seed cursor is written and reused.
- stale cursor triggers update.
- deterministic regeneration does not delete unrelated existing `sessions/*`.

### Phase 3: Session Coexistence And Refresh Heuristic

Implement oldest-commit/oldest-session seed creation logic and source-safe
cleanup.

Tests:

- oldest commit before oldest session triggers seed creation.
- oldest commit equal to or after oldest session does not require seed when
  seed is absent.
- missing git history seeds only when sessions are absent or seed is missing.
- suspicious or missing timestamps produce warnings.
- seed then export preserves seed files and seed manifest source.
- export then seed preserves sessions and session manifest source.
- legacy flat manifest then seed produces combined manifest.
- combined manifest then export preserves both sources.
- cleanup of stale session transcripts never removes `seed/*`.

### Phase 4: Agent Quick Phase

Add agent command plumbing and quick synthesis.

Tests:

- agent receives valid bounded JSON packet on stdin.
- argv command execution does not go through a shell.
- valid quick JSON writes `seed/agent/quick-overview.md`.
- invalid JSON fails quick phase and records warnings.
- oversized output fails validation.
- path traversal artifact names are rejected.
- `--require-agent` fails when quick synthesis fails.
- without `--require-agent`, deterministic seed still succeeds when quick
  fails and records agent status.

### Phase 5: Agent Deep Phase And Timeout UX

Add deep synthesis, timeout handling, and interactive/non-interactive behavior.

Tests:

- deep receives quick result in its input packet.
- deep success writes all required artifacts.
- deep timeout with quick success keeps quick output by default.
- non-interactive timeout uses `--agent-timeout-action`.
- TTY interactive timeout can continue, keep quick, or fail.
- `--require-agent` plus deep timeout fails when configured to require deep.
- failed deep phase does not delete previous valid deep output unless input
  fingerprint changed or `--force` was passed.

### Phase 6: Refresh And Path Integration

Add `entire brain refresh`, then consider changing `path` to materialize via
refresh instead of export-only.

Tests:

- refresh exports sessions and seeds when no sessions exist.
- refresh seeds when oldest commit predates oldest session.
- refresh skips seed when not needed and cursor is fresh.
- path materialization preserves existing behavior for repo URLs.
- path materialization of local repos creates combined brain when enabled.

## Agentviz Example

Using `../agentviz`, deterministic seed should capture:

- Product: AGENTVIZ, a session replay visualizer for AI agent workflows.
- Stack: React 18, Vite 6, mixed JS/TS, Node server, MCP server.
- Entrypoints:
  - `bin/agentviz.js`
  - `server.js`
  - `mcp/server.js`
  - `src/main.jsx`
  - `src/AppV2.jsx`
- API routes:
  - `routes/sessions.js`
  - `routes/ai.js`
  - `routes/config.js`
- Core app areas:
  - contexts in `src/contexts`
  - hooks in `src/hooks`
  - parsers and analyzers in `src/lib`
  - UI components in `src/components`
- Commands:
  - `npm run dev`
  - `npm run build`
  - `npm test`
  - `npm run typecheck`
  - `npm run test:e2e:v2`
- Conventions from `CLAUDE.md` and Copilot instructions:
  - product name is always `AGENTVIZ`
  - inline styles only
  - UI changes must follow `docs/ui-ux-style-guide.md`
  - five-artifact sync rule for UI changes

Manual verification after each relevant phase:

```sh
go test ./...
go run ./cmd/entire-brain seed ../agentviz --output /tmp/agentviz-brain
```

After agent phases:

```sh
go run ./cmd/entire-brain seed ../agentviz --agent command --agent-command ./test-seed-agent
```
