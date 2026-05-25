# Entire Brain

Entire Brain is an external-command plugin for the Entire CLI. It exports
Entire session history into a compact directory an agent can inspect to learn a
project's development history.

Entire CLI plugins are plain executables named `entire-<name>` on `PATH`.
When a user runs `entire <name>`, the parent CLI dispatches to that binary and
passes the remaining arguments through unchanged.

This project builds a plugin binary named `entire-brain`,
which is invoked as:

```sh
entire brain
```

## Quick Start

### Build the Plugin

```sh
mise install
mise run test
mise run build
```

### Install with the CLI

```sh
entire plugin install ./entire-brain
entire brain doctor
```

### Local Execution

For local development without installing the binary, run it directly:

```sh
go run ./cmd/entire-brain
```

### Subcommands

Some commands, such as `doctor` and `config`, use plugin directories supplied
by the Entire CLI. For standalone testing, either set those variables yourself
or let the plugin fall back to XDG locations:

```sh
ENTIRE_PLUGIN_CONFIG_DIR="$(mktemp -d)" \
ENTIRE_PLUGIN_DATA_DIR="$(mktemp -d)" \
ENTIRE_PLUGIN_STATE_DIR="$(mktemp -d)" \
ENTIRE_PLUGIN_CACHE_DIR="$(mktemp -d)" \
  go run ./cmd/entire-brain doctor
```

### Export Session History

Export the newest known checkpoint version of each unique Entire session per
branch, using default-branch commit reachability so merged work is grouped with
the default branch:

```sh
entire brain export
```

Without `--output`, the export is maintained as the repository's persistent
agent brain under the plugin data directory. The cursor that tracks already
exported sessions is stored separately under the plugin state directory, so
subsequent exports can reuse unchanged transcript files. Pass `--output` for a
one-off export; explicit output directories must be empty.

The export contains:

| Path | Purpose |
|---|---|
| `manifest.json` | Machine-readable index of exported branches, sessions, authors, metadata, timestamps, and transcript paths. |
| `README.md` | Short agent-facing guide, branch folder list, and chronological session table. |
| `sessions/<default-branch>/*.jsonl` | Session transcripts whose checkpoint trailers are reachable from the default branch, usually `sessions/main`. |
| `sessions/branches/<branch>/*.jsonl` | Branch-specific session transcripts for non-default branches that are not reachable from the default branch. |
| `sessions/unknown/*.jsonl` | Session transcripts from older metadata that did not record a branch. |

By default, transcripts use Entire's normalized compact transcript export:

```sh
entire checkpoint explain --transcript <checkpoint-id>
```

Use `--raw` when native agent logs are required instead:

```sh
entire brain export --raw --output ./entire-brain-raw-export
```

The exporter respects the repository's configured checkpoint storage version:
V1 repositories read `entire/checkpoints/v1`, while V2 repositories read
`refs/entire/checkpoints/v2/main`. Checkpoints can live either in the current
repository or in a configured `strategy_options.checkpoint_remote`; the exporter
will use the configured checkpoint remote when local checkpoint refs are absent.
It inspects up to 10,000 checkpoints by default; adjust with
`--checkpoint-limit`. Use `--scope branch` for the current branch's
`entire checkpoint explain --json` list view.

## Entire Plugin Contract

The parent CLI supplies these variables when it dispatches a plugin:

| Variable | Meaning |
|---|---|
| `ENTIRE_CLI_VERSION` | Parent CLI version, such as `0.42.0` or `dev`. |
| `ENTIRE_REPO_ROOT` | Absolute git worktree root when invoked inside one. |
| `ENTIRE_PLUGIN_CONFIG_DIR` | Per-plugin configuration directory. Defaults to `${XDG_CONFIG_HOME:-~/.config}/entire`. |
| `ENTIRE_PLUGIN_DATA_DIR` | Per-plugin durable data directory. Defaults to `${XDG_DATA_HOME:-~/.local/share}/entire`. |
| `ENTIRE_PLUGIN_STATE_DIR` | Per-plugin state directory for cursors and other regenerable state. Defaults to `${XDG_STATE_HOME:-~/.local/state}/entire`. |
| `ENTIRE_PLUGIN_CACHE_DIR` | Per-plugin cache directory. Defaults to `${XDG_CACHE_HOME:-~/.cache}/entire`. |

The default export layout uses the root folder name `entire`:

| Path | Purpose |
|---|---|
| `${config}/brain.json` | Plugin configuration, including generated 3-letter slugs for unknown repo domains. |
| `${data}/brain/<repo-key>/` | Persistent brain export for the current repository. |
| `${state}/brain/<repo-key>/head.json` | Cursor used to avoid re-pulling unchanged session transcripts. |

Repo keys are derived from the repository origin. Known hosts use compact
provider prefixes, for example `github.com/entireio/cli` becomes
`gh/entireio/cli`. Built-in prefixes are `gh` for GitHub, `gl` for GitLab,
`bb` for Bitbucket, `et` for Entire, `tg` for Tangled, and `cs` for
Code Storage. Other domains receive a generated 3-letter prefix stored in
`brain.json` to keep future exports stable and avoid collisions.

The plugin runs in the caller's current working directory. The parent CLI
filters the environment before launching third-party plugins; users can opt
additional variables in with `ENTIRE_PLUGIN_ENV`, for example:

```sh
ENTIRE_PLUGIN_ENV='AWS_*,EDITOR' entire brain
```

External-command plugins do not use a manifest and do not participate in
checkpoint/session protocols. If you need full agent lifecycle integration, use
the separate external agent plugin protocol instead.

## Useful Commands

```sh
mise run fmt        # gofmt -s -w .
mise run lint       # go vet, gofmt check, go mod tidy check, shellcheck
mise run test       # go test ./...
mise run test:ci    # go test -race ./...
mise run build      # build ./entire-brain
mise run build-all  # cross-build common Entire targets
```
