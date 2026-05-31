# Entire Brain

Entire Brain is an external-command plugin for the Entire CLI. It builds a
local, inspectable "brain" for a repository from Entire session history, seeded
repository context, and optional semantic facts from `entire-sem`.

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

### Export Session History

```sh
entire brain export
entire brain export --output /tmp/repo-brain
entire brain path .
```

`export` writes the newest known checkpoint version of each Entire session into
the persistent brain directory. `path` prints that directory and creates it when
needed for a local checkout.

### Seed Repository Context

```sh
entire brain seed .
entire brain seed . --agent none
entire brain refresh --force-seed
```

`seed` writes deterministic repository context under `seed/`, including file
indexes, docs, commands, entrypoints, conventions, risks, and history gaps.
`refresh` updates session history and creates or refreshes the seed when useful.

### Build a Semantic Brain

Install or build a provider that supports `entire sem`, then index:

```sh
entire brain index . --sem-binary entire
entire brain stale --json
```

For a directly built provider wrapper, point `--sem-binary` at that executable.
The provider must support:

```sh
entire sem doctor --json
entire sem snapshot --repo . --format ndjson --no-network
```

Useful semantic commands:

```sh
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
