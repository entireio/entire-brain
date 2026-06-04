# Entire Brain

Entire Brain is an external-command plugin for the Entire CLI. It builds a
local, inspectable "brain" for a repository from Entire session history, seeded
repository context, a local history index, and optional semantic facts from
`entire-sem`.

The plugin binary is named `entire-brain` and is invoked through Entire as:

```sh
entire brain
```

All features are local-only (for now). They read local repositories and
write local plugin data; they do not publish, hydrate, or serve brain data over
the network.

## Install

```sh
mise install
mise run check
mise run build
entire plugin install ./entire-brain
entire brain
```

## Common Workflows

### Create Or Refresh A Brain

```sh
entire brain refresh
entire brain refresh --agent none
entire brain refresh --output /tmp/repo-brain
entire brain refresh --force
```

`refresh` is the normal entry point. It writes the newest known checkpoint
version of every discoverable Entire session into the persistent brain
directory, builds deterministic repository seed context, runs `entire sem`, and
builds the local semantic context graph used by `query`, `context`, `impact`,
`changes`, and `brief`. It also derives the local decision/rationale history
index from the exported sessions. `--agent` controls optional seed synthesis:
the default is `auto`, which uses Codex when available, then Claude Code when
available, otherwise deterministic seed-only mode. `--output` writes a complete
brain to an explicit directory. `--force` rebuilds generated sources and
overwrites an explicit output directory when one is provided.

The semantic refresh stores semantic snapshots, a SQLite query store, metrics,
parse cache, and branch overlays in the local brain directory. It refuses dirty
worktrees unless semantic worktree indexing is explicitly enabled through the
advanced `index --worktree` maintenance path.

Use `--worktree` on `index` only when you intentionally want the current dirty
worktree represented. Bundle export rejects worktree-backed semantic indexes.
`repair` rebuilds derived semantic stores from the active local snapshot.
`reset --semantic-only --force` removes semantic artifacts and manifest metadata
without touching seed or session sources. `reset --force` removes the generated
brain directory for the repo.

### Work Across Multiple Repos

```sh
entire brain workspace create platform
entire brain workspace add platform ../api --name api
entire brain workspace add platform ../web --name web
entire brain workspace refresh platform
entire brain workspace query platform "checkout" --json
entire brain workspace impact platform "checkout" --json
```

Workspaces coordinate already-local repo brains by repo key and local path hint.
They do not sync or publish generated brain data.

### Use MCP Locally

```sh
entire brain mcp
```

The MCP adapter is stdio-only and exposes local tools `brain_stale`,
`brain_brief`, the semantic `brain_query`/`brain_context`/`brain_impact`/`brain_changes`,
and indexed session `brain_history`. See `docs/semantic_mcp_guide.md`.

### Ask The Brain

```sh
entire brain brief "update the README" --json
entire brain search "README" --json
entire brain show <semantic-id> --json
```

`brief` is the agent-facing entry point: it combines brain availability,
freshness, live git state, semantic context and test suggestions, and matching
history records. `status`, `search`, and `show` provide smaller top-level
queries for agents and scripts.

For deeper inspection:

```sh
entire brain guide
entire brain inspect code "README" --json
entire brain inspect context "main" --json
entire brain inspect impact "main" --json
entire brain inspect changes --json
entire brain inspect tests "main" --json
entire brain inspect decisions "semantic" --json
entire brain inspect history "semantic" --json
entire brain inspect boundaries --kind tool --json
```

## Storage

The parent Entire CLI supplies these directories:

| Variable | Purpose |
|---|---|
| `ENTIRE_PLUGIN_CONFIG_DIR` | Plugin config, including `brain.json`. |
| `ENTIRE_PLUGIN_DATA_DIR` | Durable brains under `repos/<repo-key>/`. |
| `ENTIRE_PLUGIN_STATE_DIR` | Regenerable cursors under `repos/<repo-key>/`. |
| `ENTIRE_PLUGIN_CACHE_DIR` | Cache data. |
| `ENTIRE_REPO_ROOT` | Current git checkout when invoked inside a repo. |

Repo keys come from the repository origin. For example,
`github.com/entireio/cli` becomes `gh/entireio/cli`.

## Development

For development without installing:

```sh
go run ./cmd/entire-brain --help
```

When running outside the Entire CLI, set plugin directories explicitly or allow
the XDG fallbacks:

```sh
ENTIRE_PLUGIN_CONFIG_DIR="$(mktemp -d)" \
ENTIRE_PLUGIN_DATA_DIR="$(mktemp -d)" \
ENTIRE_PLUGIN_STATE_DIR="$(mktemp -d)" \
ENTIRE_PLUGIN_CACHE_DIR="$(mktemp -d)" \
  go run ./cmd/entire-brain doctor
```

Usual mise tasks:

```sh
mise run fmt         # gofmt -s -w .
mise run lint        # go vet, gofmt check, go mod tidy check, shellcheck
mise run test        # go test ./...
mise run test:ci     # go test -race ./...
mise run test:phase1 # deterministic Phase 1 semantic suite
mise run build       # build ./entire-brain
mise run build-all   # cross-build common targets
mise run check       # lint, race tests, Phase 1 tests, and cross-builds
```

GitHub Actions runs generic tests and the deterministic Phase 1 semantic suite
on Linux, macOS, and Windows.
