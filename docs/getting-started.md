# Getting Started

This guide takes you from nothing to querying a local repository brain. No team
context is assumed.

`entire-brain` is an external-command plugin for the Entire CLI. Once installed
it is invoked as `entire brain ...`. It builds a local, inspectable "brain" for a
repository from retained Entire sessions, seed context, docs, decision history,
a semantic code graph, and durable facts, then exposes retrieval, review, and MCP
surfaces that agents and humans query. Everything it builds stays on your machine.

## Requirements

- The Entire CLI, available on your `PATH` as `entire`. It is the host that
  dispatches `entire brain`, and it is what captures the sessions the brain
  learns from.
- Git.
- A Go toolchain (1.26 or newer) if you install with `go install` or build from
  source. The prebuilt release archive does not need Go.
- The `entire-graph` semantic provider, invoked as `entire graph`. The brain shells
  out to it (`entire graph snapshot`, `entire graph doctor`) to build the semantic
  code graph. Building `entire-graph` from source needs a cgo-capable C compiler,
  because it uses tree-sitter native parser bindings. The brain itself is a
  pure-Go build and does not need cgo.

You can build a brain from history, docs, and facts without the provider (see
"If you do not have the semantic provider yet" below), but the semantic code
graph needs `entire graph`.

The provider is `entire-graph` (the public `entireio/entire-graph` repo),
invoked as `entire graph`. The brain shells out to `entire graph snapshot` and
`entire graph doctor` to build and verify the semantic layer. Install the latest
provider as shown below. If you do not need the semantic code graph, you can skip
the provider and run the brain with `--semantic=false` (see First run).

## Install

There are two components: the brain plugin and the `entire-graph` provider it
calls. Install the provider first, then the brain. Both are registered with the
Entire CLI the same way, using `entire plugin install <path>`, which links a
local `entire-<name>` executable into Entire's managed plugin directory.

### Quick install with `go install`

```sh
# 1. Semantic provider (the entire-graph code-graph plugin the brain shells out to)
CGO_ENABLED=1 go install github.com/entireio/entire-graph/cmd/entire-graph@v0.1.0
entire plugin install "$(go env GOPATH)/bin/entire-graph" --force

# 2. The brain
go install github.com/ashtom/entire-brain/cmd/entire-brain@v0.1.0
entire plugin install "$(go env GOPATH)/bin/entire-brain" --force
```

If `$(go env GOPATH)/bin` is already on your `PATH`, Entire can also discover the
binaries directly after `go install`, without the `entire plugin install` step.

`entire plugin install` currently supports local executable paths only. It does
not fetch from a git URL or a GitHub release, and it does not auto-install the
provider dependency, so both `go install` steps are required.

The `entire-brain` `v0.1.0` module still declares its historical
`github.com/ashtom/entire-brain` path, so the versioned `go install` command must
use that path even though the repository's canonical GitHub URL is now
`github.com/entireio/entire-brain`. Install from source if your environment cannot
resolve the repository redirect through the module proxy.

### Install from source

Cloning gives you the same result and does not depend on the module proxy. Clone
the provider at the `v0.1.0` tag, clone the brain next to it, and run the bundled
installer.

```sh
git clone --branch v0.1.0 https://github.com/entireio/entire-graph.git
git clone https://github.com/entireio/entire-brain.git
cd entire-brain
scripts/install.sh
```

`scripts/install.sh` builds and installs the sibling `entire-graph` provider, then
builds and installs `entire-brain`, writes the default plugin configuration
(`entire brain config init`), and runs `entire brain doctor`. It expects
`entire-graph` to be the sibling checkout next to `entire-brain`; point it
elsewhere with `ENTIRE_GRAPH_DIR=/path/to/entire-graph scripts/install.sh`.

To build and install only the brain from a checkout, run `scripts/install-local.sh`
(equivalently `mise run install`). You still need `entire graph` installed
separately for the semantic layer.

### Release archive (forthcoming)

Prebuilt per-OS/arch archives are the planned packaged distribution channel, but
they have not been published yet. Until they appear on the
[GitHub Releases](https://github.com/entireio/entire-brain/releases) page, use
`go install` or the source installer above.

Each published archive will contain the plugin binary plus `README.md`, `LICENSE`,
and `entire-plugin.yml`, alongside a `SHA256SUMS` file. After downloading and
verifying the checksum, extract and install the binary:

```sh
tar -xzf entire-brain-v0.1.0-darwin-arm64.tar.gz
entire plugin install ./entire-brain-v0.1.0-darwin-arm64/entire-brain --force
```

When provider archives are published, install the matching `entire-graph` archive
the same way so `entire graph` is available.

## Verify

```sh
entire brain version          # plugin version
entire brain doctor           # checks the Entire CLI plugin environment
entire brain status           # summarizes the brain (empty until the first refresh)

entire graph version            # provider version
entire graph doctor --json      # provider diagnostics; expect "no_egress": true
```

`entire brain doctor` reports whether the plugin directories and the semantic
provider are wired up. If it says the provider is missing, confirm `entire graph`
resolves, that `entire graph version` reports `v0.1.0`, and that the installed
binary is named `entire-graph` rather than the retired `entire-sem`.

## First run: build a brain and query it

Run the first build inside a git repository. A repository that has Entire enabled
and some captured agent sessions produces the richest brain, because sessions,
decision history, and distillable facts all come from that captured work. On a
repository with no Entire history the brain still builds from seed context, docs,
and the semantic code graph.

```sh
cd /path/to/your/repo

entire brain refresh --agent none
entire brain status
```

`refresh` exports captured sessions, builds the local history and doc indexes,
asks `entire graph` for a semantic snapshot, and stores the derived brain under
Entire's plugin data directory. `--agent none` keeps this first build
deterministic and token-free, with no hosted-model calls.

The semantic snapshot is built from committed state, so run `refresh` on a clean
checkout. On a dirty worktree the semantic step can be skipped or refused; index
uncommitted state deliberately with `entire brain refresh index --worktree`.
Bundle export rejects worktree-backed semantic indexes.

`entire brain status` then reports the brain's sources, durable-fact readiness,
and semantic coverage, freshness, and blind spots. Add `--json` for a
machine-readable report.

### If you do not have the semantic provider yet

`refresh` runs the semantic step by default, and that step fails if `entire graph`
cannot be verified. To build a brain from sessions, history, docs, and facts
without the provider, disable the semantic step:

```sh
entire brain refresh --agent none --semantic=false
```

Install `entire-graph` `v0.1.0` later and re-run `entire brain refresh --agent none`
to add the semantic code graph.

### Query the brain

Every result carries an `id` you can fetch in full with `get`.

```sh
entire brain overview                          # what the project is: stack, commands, recent decisions
entire brain brief "add rate limiting to the API"   # a bounded, task-shaped context packet
entire brain query "how does checkpointing work"    # hybrid lexical + vector search across the brain
entire brain search "checkpoint"               # exact keyword search
entire brain recall "why did we pick this default"  # durable facts for the current branch
entire brain get fact:<id>                     # fetch one item in full
```

Add `--json` to any of these for machine-readable output. `entire brain guide`
prints the recommended command set for a coding agent.

### Optional: distill durable facts

Distillation is an opt-in agent step that turns captured sessions into durable
project knowledge (decisions, constraints, preferences, gotchas, conventions).
It sends redacted transcript chunks to the selected agent, so it spends tokens
and performs network egress unless you point it at a local loopback agent. Always
estimate first.

```sh
entire brain distill --dry-run --json                                  # estimate work; no agent call
entire brain distill --agent codex --model gpt-5.4-mini --effort low   # extract facts
entire brain facts status                                              # review readiness
```

Distillation is incremental and cached: unchanged sessions are skipped, and
low-confidence merges are queued for `entire brain facts review`.

### Use the brain from an agent

For agents that speak MCP, register the brain as a local stdio MCP server. It
opens no network listener.

```sh
entire brain mcp
```

The agent then calls brain tools (`brain_brief`, `brain_status`, `brain_query`,
`brain_code`, and others) instead of shelling out. See
[semantic_mcp_guide.md](semantic_mcp_guide.md) for the full tool surface.

## What you get

A local brain is assembled from these sources, each refreshed by a named stage:

- Seed: a synthesized orientation to the repository (stack, entrypoints,
  commands, key documents).
- Sessions: transcripts exported from captured Entire agent sessions.
- History: a decision and rationale index derived from those sessions.
- Docs: retrievable chunks of the brain's seed summaries and the repository's
  own markdown.
- Semantic code graph: symbols, relations, callers, callees, impact sets, and
  likely tests, provided by `entire graph`.
- Durable facts: short, provenance-anchored statements about decisions,
  constraints, preferences, gotchas, and conventions.
- Patterns and runtime traces: repeated-work patterns from history, and imported
  runtime edges validated against the static graph.

Local-only by default:

- Deterministic refresh, semantic indexing, local history and doc indexing, and
  MCP tool calls do not require a hosted model, and the MCP adapter is
  stdio-only with no network listener.
- The token-spending and network paths are explicit and opt-in: seed synthesis
  with `refresh --agent auto`, fact distillation with `distill --agent ...`,
  query expansion with `recall --expand`, pattern verification, and judged
  evaluation commands. A configured checkpoint remote also lets `refresh` fetch
  checkpoint history over the network.
- Set `ENTIRE_BRAIN_NO_EGRESS=1` (or `ENTIRE_BRAIN_LOCAL_ONLY=1`), or use
  `--agent none` and `--dry-run`, to keep a repository strictly local. No-egress
  mode enforces locality for the no-agent, dry-run, and loopback-Ollama paths.

Base Entire session capture stores transcripts and metadata on the repository's
`entire/checkpoints/v1` branch. Review the Entire CLI security and privacy guide
before enabling Entire on sensitive or public repositories.

## Further reading

- [../README.md](../README.md): full usage scenarios and how agents consume the brain
- [operations.md](operations.md): build targets, cgo, shared baseline
- [semantic_mcp_guide.md](semantic_mcp_guide.md): the full MCP tool surface
- [diffless_review_seam.md](diffless_review_seam.md): the diff-less review contract
- [durable_facts_plan.md](durable_facts_plan.md): durable-facts design and eval
