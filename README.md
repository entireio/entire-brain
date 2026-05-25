# Entire Brain

Entire Brain is an external-command plugin for the Entire CLI.

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

Some commands, such as `doctor` and `config`, expect to run through the Entire
CLI so `ENTIRE_PLUGIN_DATA_DIR` is present. For standalone testing, set it:

```sh
ENTIRE_PLUGIN_DATA_DIR="$(mktemp -d)" go run ./cmd/entire-brain doctor
```

## Entire Plugin Contract

The parent CLI supplies these variables when it dispatches a plugin:

| Variable | Meaning |
|---|---|
| `ENTIRE_CLI_VERSION` | Parent CLI version, such as `0.42.0` or `dev`. |
| `ENTIRE_REPO_ROOT` | Absolute git worktree root when invoked inside one. |
| `ENTIRE_PLUGIN_DATA_DIR` | Per-plugin durable storage directory. The plugin should create it before writing. |

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
