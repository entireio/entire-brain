# Entire Brain

**A local, inspectable memory layer for a Git repository — it turns retained agent sessions, checkpoint history, docs, code structure, and durable facts into something you and your coding agents can actually query.**

[![Release](https://img.shields.io/badge/release-v0.3.0-blue)](https://github.com/entireio/entire-brain/releases/tag/v0.3.0)
[![License](https://img.shields.io/badge/license-MIT-green)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.27-00ADD8)](go.mod)

`entire-brain` is an external-command plugin for the [Entire CLI](https://github.com/entireio/cli). Entire captures what happened while you and your agents worked — prompts, transcripts, tool activity, files touched, checkpoints. Entire Brain reads that capture, combines it with your code's structure and your own curated facts, and serves the result back through a CLI and an MCP server.

The practical effect: an agent starts a task holding your repository's prior decisions, freshness signals, likely tests, and known failure modes, instead of rediscovering them from scratch.

Everything runs locally. Nothing leaves your machine unless you explicitly opt in.

---

## Key Features

- **Durable facts with provenance.** Author facts by hand (`remember`) or distill them from captured sessions (`distill`). Every fact is anchored to real sources, and `verify` re-checks those anchors against what is actually in the repository today.
- **Hybrid retrieval.** Lexical BM25 (`query --keyword`), vector/semantic (`query --semantic`), and Reciprocal Rank Fusion across both (`query`) — over facts, history, docs, and conversations.
- **Semantic code navigation.** Symbol-level structure, impact analysis, dead-code detection, and regression radar, backed by the `entire-graph` provider.
- **An MCP server for agents.** ~36 tools over stdio, so Claude Code, Codex, and any MCP-capable client can read the brain directly. Every response is bounded, so a wide query returns a truncated *answer* rather than blowing the transport.
- **Honest health reporting.** `status` and `doctor` distinguish *not built yet* from *broken*, cross-check the manifest's claims against what the store can actually produce, and exit non-zero when something is genuinely wrong — so CI and agent loops can branch on them.
- **A repository is treated as data, never as trust.** Indexing an untrusted repository does not execute code it carries: `diff.external`, `core.fsmonitor`, `.gitattributes` textconv, `filter.*`, and git hooks are all suppressed at every git invocation.
- **Offline by default.** `ENTIRE_BRAIN_NO_EGRESS` fails closed. Publishing to hosted Entire is strictly opt-in.
- **Inspectable, not a black box.** `dash` browses the brain in a TUI, `viz` renders it as an offline graph in your browser, and every store is a file you can read.

---

## Tech Stack & Dependencies

| Layer | Choice |
|---|---|
| Language | **Go 1.27** |
| CLI framework | `spf13/cobra` |
| Git access | `go-git/go-git/v6` (plus hardened `git` subprocess calls) |
| Storage | SQLite — `modernc.org/sqlite` (pure Go, default) or `mattn/go-sqlite3` (cgo) |
| Vector search | `asg017/sqlite-vec-go-bindings` |
| TUI | `charmbracelet/bubbletea`, `bubbles`, `lipgloss` |
| Agent interface | Model Context Protocol over stdio (JSON-RPC 2.0, `Content-Length` framing) |

**Companion projects**

- **[Entire CLI](https://github.com/entireio/cli)** — the host. Installs hooks and captures sessions. Required.
- **[entire-graph](https://github.com/entireio/entire-graph)** — the semantic provider. Parses source locally and emits code-structure records. Required for semantic features.

### Build tags

The default build is pure Go and fully functional.

| Build | Command | What it adds |
|---|---|---|
| Default | `go build ./cmd/entire-brain` | Everything except the items below |
| `brain_cgo` | `go build -tags brain_cgo ./cmd/entire-brain` | Gemma-class embedder; conversation and history join vector search |
| `sqlite_fts5` | `go build -tags "brain_cgo sqlite_fts5" ./cmd/entire-brain` | SQLite FTS5 full-text index |

> Building without these tags degrades *explicitly* — affected commands say what is unavailable and why. They never silently return empty results.

---

## Quick Start

### Prerequisites

1. **Go 1.27 or newer** — `go version`
2. **Git 2.30 or newer** — `git --version`
3. **The Entire CLI**, installed and on your `PATH` — `entire --version`
4. **Repository access.** `entire-brain` is currently a private repository. Confirm access before you start:
   ```bash
   git ls-remote https://github.com/entireio/entire-brain.git >/dev/null && echo "access ok"
   ```
   If that fails, request access — the steps below cannot work without it.

### Option A — the scripted install (recommended)

This builds `entire-graph` and `entire-brain`, registers both with the Entire CLI, writes the default configuration, and runs a health check. It resolves `entire-graph` automatically and prints which route it used.

```bash
git clone https://github.com/entireio/entire-brain.git
cd entire-brain
./scripts/install.sh
```

Verify:

```bash
entire brain version    # -> 0.3.0
entire brain doctor     # -> findings, exit 0 on a healthy environment
```

### Option B — manual build

```bash
git clone https://github.com/entireio/entire-brain.git
cd entire-brain

# Build with the version stamped in; without -ldflags the binary reports "dev".
go build -ldflags "-X main.version=0.3.0" -o entire-brain ./cmd/entire-brain

# Register it with the Entire CLI.
entire plugin install ./entire-brain

entire brain version
```

### Build your first brain

From inside any repository you want a brain for:

```bash
cd /path/to/your/repo
entire brain setup
```

`setup` is the one command that takes you from nothing to useful. It:

1. builds the brain immediately from what is already on disk,
2. starts a **background** fact backfill, capped at 25 sessions by default,
3. installs a watcher that keeps the brain fresh.

Prefer to spend no tokens and install nothing in the background?

```bash
entire brain setup --no-backfill --no-daemon
```

Confirm it worked:

```bash
entire brain status
```

---

## Usage Examples

### Check what the brain knows

```bash
# One-line health and coverage summary.
entire brain status

# The full picture: sources, freshness, blind spots.
entire brain status --verbose

# What is this project? Stack, boundaries, commands, recent decisions.
entire brain overview
```

### Record and retrieve facts

```bash
# Author a durable fact about this repository.
entire brain remember "Retries are capped at 3; the 4th failure must page."

# Retrieve facts for the current branch.
entire brain recall "retry policy"

# Re-check every fact's anchors against the current tree.
entire brain verify
```

### Search

```bash
# Lexical (BM25 over history and docs, token overlap over facts).
entire brain query --keyword "rate limiter"

# Vector / semantic.
entire brain query --semantic "how do we handle backpressure"

# Hybrid: lexical + vector, fused with RRF. Usually the one you want.
entire brain query "why did we drop the queue abstraction"
```

Supply the query text positionally or with `--query`; flags can appear before or
after it:

```bash
entire brain query --keyword --query "RetryPolicy"
entire brain query --query "handling temporary failures" --semantic
```

The default mode is hybrid. `--keyword` and `--semantic` are mutually
exclusive; supplying both a positional query and `--query` is an error.
The same forms work with `entire brain workspace query <workspace>`.
The old `search` and `vsearch` commands remain hidden compatibility aliases.
Mode flags appear only in query-command help, not as global flags.

`query` also takes `--source` (`all` | `fact` | `history` | `doc` | `conversation`) to narrow the corpus, and `--json` for machine-readable output:

```bash
entire brain query "auth middleware" --source fact --json | jq '.results[0]'
```

### Navigate the code semantically

```bash
# What does changing this symbol affect?
entire brain inspect impact --symbol ValidateToken

# What is unreachable?
entire brain inspect dead-code --json

# What changed since the brain was last indexed?
entire brain inspect changes
```

### Build a task packet for an agent

```bash
# A bounded context packet scoped to one task.
entire brain brief "add rate limiting to the upload endpoint"
```

### Serve the brain to an agent over MCP

```bash
entire brain mcp
```

Register it with an MCP-capable client — for Claude Code:

```bash
claude mcp add entire-brain -- entire brain mcp
```

Print a ready-made client configuration bound to the current repository:

```bash
entire brain mcp --print-config
```

### Explore interactively

```bash
entire brain dash   # TUI: status, facts, sessions, history, semantic
entire brain viz    # offline graph in your browser
```

### Keep it fresh

```bash
entire brain refresh          # rebuild from what is on disk (deterministic, free)
entire brain refresh index    # rebuild the semantic index only
entire brain watch            # keep it fresh automatically
```

---

## Configuration

Configuration lives in the plugin config directory; `entire brain config init` writes the defaults and `entire brain path` prints where a repository's brain is stored.

### Environment variables

| Variable | Default | Purpose |
|---|---|---|
| `ENTIRE_BRAIN_NO_EGRESS` | enabled | Block all outbound network access. **Fails closed** — an unrecognized value is treated as enabled. |
| `ENTIRE_BRAIN_LOCAL_ONLY` | enabled | Restrict operation to local sources only. |
| `ENTIRE_BRAIN_ALLOW_HOSTED` | unset | Opt in to hosted Entire features. |
| `ENTIRE_BRAIN_EMBEDDER` | auto | Embedding backend for vector search. |
| `ENTIRE_BRAIN_EMBED_URL` | unset | Endpoint for a remote embedder. |
| `ENTIRE_BRAIN_OLLAMA_URL` | `http://localhost:11434` | Local Ollama endpoint. |
| `ENTIRE_BRAIN_GRAPH_BINARY` | resolved | Explicit path to the `entire-graph` provider. |
| `ENTIRE_BRAIN_THEME` | auto | TUI colour theme. |
| `ENTIRE_BRAIN_MCP_DEBUG_LOG` | unset | Path for MCP protocol debug logging. Records tool names and result metadata, never query text. |
| `ENTIRE_BRAIN_MCP_ALLOW_CROSS_REPO` | disabled | Allow MCP tools to read outside the bound repository. |
| `ENTIRE_BRAIN_MCP_ALLOW_ANY_PATH` | disabled | Disable MCP path containment. **Leave this off.** |
| `ENTIRE_BRAIN_CONVERSATION_FUSION` | unset | Development flag for the conversation fusion arm of hybrid retrieval. |
| `ENTIRE_BRAIN_BRIEF_CONVERSATION` | unset | Development flag for including conversation context in `brief`. |
| `ENTIRE_BRAIN_MAX_SNAPSHOT_BYTES` | bounded | Cap on a single retained snapshot. |
| `ENTIRE_API_URL` | Entire default | Hosted API endpoint. HTTPS is enforced. |
| `ENTIRE_API_TOKEN` | unset | Hosted API token. Required only for `publish`. |
| `ENTIRE_REPO_ROOT` | inferred | Repository root, normally set by the Entire CLI when it dispatches the plugin. |

### Storage locations

These follow the XDG base directory specification and are normally set by the host CLI. Override them to isolate a brain completely — useful for testing:

```bash
export ENTIRE_PLUGIN_DATA_DIR=/tmp/sandbox/data
export ENTIRE_PLUGIN_CACHE_DIR=/tmp/sandbox/cache
export ENTIRE_PLUGIN_STATE_DIR=/tmp/sandbox/state
export ENTIRE_PLUGIN_CONFIG_DIR=/tmp/sandbox/config
```

---

## Contributing

1. **Branch from current `main`.** Tags mark releases on `main`; they are not branches to work from.
2. **Run the full suite before you push** — it takes about three and a half minutes, so there is no reason to skip it:
   ```bash
   go test ./... -count=1
   gofmt -l -s .
   go vet ./...
   ```
3. **Every fix needs a test that fails without it.** Prove it: revert the fix's behaviour while keeping identifiers compiling, run the test, and put the failure output in the pull request.
4. **Reproduce before you fix.** Paste the real output of the defect on unpatched `main`, then the output after.
5. **Re-test combinations after merging.** Two individually-green pull requests can break `main` together.
6. **Never commit `.env` or `.session` files.** Guard before every commit:
   ```bash
   git add -A -n | grep -iE "\.env|\.session" && echo "ABORT"
   ```
7. **Re-record release evidence before tagging.** The evidence manifests pin source files by content hash; edits to pinned files mark a lane stale.

Be explicit about what you did *not* verify. A pull request that names its gaps is worth more than one that implies there are none.

---

## License

[MIT](LICENSE).

---

## Further Reading

- **[docs/reference.md](docs/reference.md)** — the complete reference: every command, the capability matrix, architecture, and operational detail.
- [docs/recall_threat_model.md](docs/recall_threat_model.md) — threat model and privacy posture.
- [docs/semantic_mcp_guide.md](docs/semantic_mcp_guide.md) — the semantic and MCP surfaces in depth.
- [docs/release_readiness_audit.md](docs/release_readiness_audit.md) — what must hold before a release is cut.
