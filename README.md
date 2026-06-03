# Entire Brain

Entire Brain is an external-command plugin for the Entire CLI. It builds a
local, inspectable "brain" for a repository from Entire session history, seeded
repository context, a local history index, and optional semantic facts from
`entire-sem`.

The plugin binary is named `entire-brain` and is invoked through Entire as:

```sh
entire brain
```

All Phase 1 semantic features are local-only. They read local repositories and
write local plugin data; they do not publish, hydrate, or serve brain data over
the network.

## Install

```sh
mise install
mise run check
mise run build
entire plugin install ./entire-brain
entire brain doctor
```

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

## Common Workflows

### Start a Coding Task

```sh
entire brain refresh --history-index
entire brain status . --json
entire brain brief "update the README" --json
entire brain search "README" --json
entire brain show <semantic-id> --json
```

Add `--semantic` to `refresh` when an `entire sem` provider is available.
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

### Export Session History

```sh
entire brain export --history-index
entire brain history-index .
entire brain export --output /tmp/repo-brain
entire brain path .
```

`export` writes the newest known checkpoint version of each Entire session into
the persistent brain directory. `--history-index` also builds a local
decision/rationale index from exported sessions. `history-index` can rebuild
that derived index later. `path` prints the persistent brain directory and
creates it when needed for a local checkout. Export warning details are printed
after the warning count and recorded in the generated `README.md` and
`manifest.json`.

### Seed Repository Context

```sh
entire brain seed .
entire brain seed . --agent none
entire brain refresh --force-seed --history-index
```

`seed` writes deterministic repository context under `seed/`, including file
indexes, docs, commands, entrypoints, conventions, risks, and history gaps.
`refresh` updates session history and creates or refreshes the seed when useful.

### Build a Semantic Brain

Install or build a provider that supports `entire sem`, then index:

```sh
entire brain refresh --semantic --history-index --sem-binary entire
entire brain index . --sem-binary entire --force
entire brain stale --json
```

For a directly built provider wrapper, point `--sem-binary` at that executable.
The provider must support:

```sh
entire sem doctor --json
entire sem snapshot --repo . --format ndjson --no-network
```

`index` stores semantic snapshots, a SQLite query store, metrics, parse cache,
and branch overlays in the local brain directory. It refuses to replace an
existing semantic source unless `--force` is set, and it refuses dirty worktrees
unless `--worktree` is set. `refresh --semantic` forces a semantic rebuild after
session and seed refresh; add `--semantic-worktree` to represent the current
dirty worktree, or `--all-branches` to write recent local branch overlays
without fetching remotes.

Useful semantic commands:

```sh
entire brain status . --json
entire brain brief "main" --json
entire brain search "main" --json --limit 10
entire brain show <semantic-id-or-name> --json
entire brain query "main" --json --limit 10
entire brain context "main" --json --limit 10
entire brain impact "main" --json --depth 2 --limit 20
entire brain changes --json
entire brain routes --json
entire brain tools --json
entire brain workflows --json
entire brain tests "main" --json
entire brain repair .
entire brain reset . --semantic-only --force
```

Use `--worktree` on `index` only when you intentionally want the current dirty
worktree represented. Bundle export rejects worktree-backed semantic indexes.
`repair` rebuilds derived semantic stores from the active local snapshot.
`reset --semantic-only --force` removes semantic artifacts and manifest metadata
without touching seed or session sources. `reset --force` removes the generated
brain directory for the repo.

Commands that support `--json` emit structured JSON error envelopes on failure:

```json
{
  "code": "command_failed",
  "message": "..."
}
```

### Bundle a Local Semantic Brain

```sh
entire brain bundle export --output /tmp/repo-brain.tar
shasum -a 256 /tmp/repo-brain.tar
entire brain bundle import /tmp/repo-brain.tar --sha256 <sha256>
entire brain gc --older-than 30d
```

Bundles are local files only. Import verifies checksums, schema compatibility,
repo identity, archive paths, size limits, and semantic store integrity.

### Work Across Local Repos

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

The MCP adapter is stdio-only and exposes local wrappers for `stale`, `query`,
`context`, `impact`, and `changes`. See `docs/semantic_mcp_guide.md`.

## Storage

The parent Entire CLI supplies these directories:

| Variable | Purpose |
|---|---|
| `ENTIRE_PLUGIN_CONFIG_DIR` | Plugin config, including `brain.json`. |
| `ENTIRE_PLUGIN_DATA_DIR` | Durable brains under `brain/<repo-key>/`. |
| `ENTIRE_PLUGIN_STATE_DIR` | Regenerable cursors under `brain/<repo-key>/`. |
| `ENTIRE_PLUGIN_CACHE_DIR` | Cache data. |
| `ENTIRE_REPO_ROOT` | Current git checkout when invoked inside a repo. |

Repo keys come from the repository origin. For example,
`github.com/entireio/cli` becomes `gh/entireio/cli`.

## Development

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

Key docs:

- `docs/semantic_brain_plan.md`
- `docs/progress-so-far.md`
- `docs/semantic_agent_guide.md`
- `docs/semantic_mcp_guide.md`
