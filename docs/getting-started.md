# Getting Started

## Install and discover agent instructions

Run `entire brain init-agents` in the consuming project to install
`.entire/agent-guide.md` plus managed pointers in `AGENTS.md` and
`CLAUDE.md`. Reruns reconcile Graph and Brain blocks into one shared workflow and preserve your own text.
Use `--repo <path>` for another project. Installation does not build indexes,
call an agent, or start services.

`entire brain agent-guide` prints the same guide; `guide` remains an alias.
Use `entire brain capabilities --json` to discover compiled features and their
requirements without a repository. Use `status --details --json` for the
current repository's readiness. Graph's language/relation inventory remains
available through `entire graph capabilities --json`.


This guide takes you from nothing to querying a local repository brain. No team
context is assumed.

It is two commands. The first installs the plugins on this machine; the second
onboards a repository. Do not stop after the first — installing gives no
repository a brain.

```sh
# once per machine
git clone https://github.com/entireio/entire-brain.git
entire-brain/scripts/install.sh

# once per repository you want a brain for
cd /path/to/your/repo
entire brain setup
```

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
- A Go toolchain (1.27 or newer), **only if you build from source**. The
  released binaries need no toolchain at all: Brain's default build is pure Go,
  so the published archives run as-is. See "Installing a release" below.
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
the provider and run the brain with `--semantic=false` (see
"If you do not have the semantic provider yet").

## Install

There are two components: the brain plugin and the `entire-graph` provider it
calls. Both are registered with the Entire CLI the same way, using
`entire plugin install <path>`, which links a local `entire-<name>` executable
into Entire's managed plugin directory. You do not have to fetch or arrange the
provider yourself -- `scripts/install.sh` does both components in one pass.

### One command

```sh
git clone https://github.com/entireio/entire-brain.git
entire-brain/scripts/install.sh
```

This is the whole install: it checks the prerequisites listed above and reports
every missing one at once, resolves `entire-graph` (using `$ENTIRE_GRAPH_DIR`,
then any checkout already on this machine, then a shallow clone of the public
repository into `${XDG_CACHE_HOME:-~/.cache}/entire-brain/entire-graph`), builds
and registers both plugins, writes the default plugin configuration
(`entire brain config init`), and runs `entire brain doctor`. It prints which of
the three provider routes it took. Neither checkout has to have a particular
name, and they do not have to be siblings.

The prerequisites do not go away: Go 1.27 or newer and a cgo-capable C compiler
are still required, because both components are built from source here. What
goes away is having to arrange directories before you start.

`scripts/bootstrap.sh` is the same flow for a machine with nothing on it: it
clones `entire-brain` when it is not already present and then runs
`scripts/install.sh`. Build from source when you intend to change Brain, or when
you want the `brain_cgo` build. For simply using it, install a release instead.

### Installing a release

Releases carry prebuilt binaries for Linux, macOS and Windows on both amd64 and
arm64, with a `checksums.txt` beside them. Two ways to get one:

```sh
# Through the Entire CLI, the way Graph installs.
entire plugin install brain

# Or one self-contained command, no CLI and no toolchain first.
curl -fsSL https://raw.githubusercontent.com/entireio/entire-brain/main/scripts/get-brain.sh | bash
```

The script resolves the release for your platform, verifies the download against
`checksums.txt`, installs to `~/.local/bin`, and registers the plugin. It fails
closed: a checksum that cannot be fetched or does not match aborts the install
rather than continuing. `--nightly` takes the nightly channel, `--version vX.Y.Z`
pins a release, `--dir` changes where it lands.

Versions restart at `0.1.0`, which is the first release of the public line. Tags
older than that predate the Graph rename -- the provider tag builds `entire-sem`
and the brain tag invokes `entire sem` -- so do not combine them with the
`entire graph` commands in this guide.

A versioned `go install` path is still not available.

`entire plugin install` currently supports local executable paths only. It does
not fetch from a git URL or a GitHub release, and it does not auto-install the
provider dependency.

### Managing the provider checkout yourself

The one command above is the source install: until matching Graph-based tags are
available, both components are built from their current `main` branches, which
tracks development and is not a reproducible versioned install. Use matching
release tags once they are published.

If you would rather own the `entire-graph` checkout -- to pin a commit, or to
work on the provider -- clone it too and run the same installer:

```sh
git clone --branch main https://github.com/entireio/entire-graph.git
git clone --branch main https://github.com/entireio/entire-brain.git
cd entire-brain
scripts/install.sh
```

A side-by-side checkout is discovered with no configuration, and so are the usual
source directories under `$HOME`. Set
`ENTIRE_GRAPH_DIR=/path/to/entire-graph scripts/install.sh` only when your
checkout is somewhere the installer would not look, or when you want to pin one
specific checkout; an override that does not hold an `entire-graph` checkout is
an error rather than a silent fallback. `ENTIRE_INSTALL_OFFLINE=1` stops the
installer reaching the network at all, so a machine without egress fails with an
explanation instead of hanging on a clone.

To build and install only the brain from a checkout, run `scripts/install-local.sh`
(equivalently `mise run install`). You still need `entire graph` installed
separately for the semantic layer.

### Release archive (forthcoming)

Prebuilt per-OS/arch archives are the planned packaged distribution channel, but
they have not been published yet. Until they appear on the
[GitHub Releases](https://github.com/entireio/entire-brain/releases) page, use
the pre-release source installer above.

Each published archive will contain the plugin binary plus `README.md`, `LICENSE`,
and `entire-plugin.yml`, alongside a `SHA256SUMS` file. After downloading and
verifying the checksum, extract and install the binary:

```sh
VERSION=vX.Y.Z # use the matching version published by both repositories
tar -xzf "entire-brain-${VERSION}-darwin-arm64.tar.gz"
entire plugin install "./entire-brain-${VERSION}-darwin-arm64/entire-brain" --force
```

When provider archives are published, install the matching `entire-graph` archive
the same way so `entire graph` is available.

## Verify

```sh
entire brain version          # plugin version
entire brain doctor           # checks the Entire CLI plugin environment
entire brain status           # summarizes the brain (empty until you run `entire brain setup` in a repo)

entire graph version            # provider version
entire graph doctor --json      # provider diagnostics; expect "no_egress": true
```

`entire brain doctor` reports whether the plugin directories and the semantic
provider are wired up. If it says the provider is missing, confirm `entire graph`
resolves and that the installed binary is named `entire-graph` rather than the
retired `entire-sem`. For a versioned release, also confirm the brain and provider
report the same published release version.

That is the machine set up. No repository has a brain yet — the next section is
the command that gives one to a repository you actually work in.

## Onboard a repository: `entire brain setup`

Installing put two plugins on this machine. It gave no repository a brain. This
is the step that does, and it is where a first-time reader should go next:

```sh
cd /path/to/your/repo
entire brain setup
```

One command, three phases:

| phase | blocking? | spends tokens? | installed by default? |
| --- | --- | --- | --- |
| 1. **instant core** — sessions, semantic index, seed, docs, history, entities | yes, seconds | **no**, deterministic | — |
| 2. **fact backfill** — distill past sessions into durable facts, newest first, detached | no | **yes** | yes, unless `--no-backfill` |
| 3. **watcher daemon** — one machine-wide launchd agent / systemd user unit | no | **yes**, per window | **yes, with no prompt**, unless `--no-daemon` |

Three things are worth being explicit about before you run it:

- **Phase 2 spends tokens.** It distills your past sessions with an agent CLI,
  capped at 25 sessions per pass (`--backfill-budget`). `--no-backfill` skips it.
- **Phase 3 installs a persistent background service, by default, without
  asking.** launchd with `RunAtLoad`/`KeepAlive` on macOS, a systemd user unit
  with `Restart=always` on Linux. It survives logout and reboot, and its gated
  distill step calls an agent, so it is a recurring spend. `--no-daemon` skips
  it; `entire brain setup --uninstall-daemon` removes one you already have.

  You are told before it happens rather than after. A run that will register a
  watcher opens with the pre-flight line, ahead of any work:

  ```
  setup: background watcher: this run will install io.entire.brain-watch.<id> at
  ~/Library/LaunchAgents/io.entire.brain-watch.<id>.plist — a persistent launchd
  service that starts again at every login; pass --no-daemon to skip it, or
  remove it later with `entire brain setup --uninstall-daemon`
  ```

  A re-run that finds the watcher already installed says it *keeps* it instead,
  and `--no-daemon` prints nothing about a service it will not install.
- **`entire brain setup --no-backfill --no-daemon` spends nothing at all** and
  changes no service — it is phase 1 only.

```sh
entire brain setup --no-backfill --no-daemon   # deterministic build only, zero spend
entire brain status                            # what was built, backfill progress, daemon health
entire brain setup --uninstall-daemon          # stop and remove the watcher
```

### What `setup` needs first, and what it does not

`setup` hard-fails on one thing only: the target must be a **local path that is a
git repository** with at least one commit. It checks that before it registers a
workspace, writes state, or installs anything.

It does **not** require, and will not do for you:

- **`entire enable` in the repository.** Hook registration belongs to the Entire
  CLI's repository enablement, which owns the whole lifecycle hook set;
  duplicating it in `setup` would leave two hooks racing to distill the same
  session. `setup` reports the session-end hook as an advisory line, never a
  failure.
- **Captured Entire sessions.** A repository Entire has never captured a
  session in still builds, and the `sessions` component is green: the export
  returns an empty inventory, which is a true answer. You get a brain from
  code, docs and git history rather than from your past agent work — which is
  the one source it is most interesting for, so this is worth fixing, just not
  by a failure. The export only *fails* when Entire cannot prove the inventory
  is empty rather than merely unreadable — a configured checkpoint remote it
  cannot enumerate, offline or unauthenticated — and the hint then says to work
  a session and re-run, and points at `entire checkpoint list` for what Entire
  itself can see.
- **A working `entire graph` provider**, a supported service manager, or an
  agent CLI — each missing piece degrades exactly its own phase.

So the order that produces the best brain, rather than merely a successful
command, is:

```sh
entire enable                              # capture sessions + wire the session-end hook
git add .entire .claude && git commit      # enable wrote these and did not commit them
entire brain setup                         # then onboard: build, backfill, watch
```

That order is a quality decision, not a correctness one — running `entire enable`
after `setup` works too, and the next backfill pass picks the new sessions up.

**The commit in the middle is not optional.** `entire enable` writes
`.entire/settings.json` and your agent's hook settings (`.claude/settings.json`
for Claude Code) and leaves them uncommitted, so a `setup` run immediately
afterwards finds a dirty worktree and refuses to seed or index it — two red `x`
on a first run, caused by the command directly above it:

```
  instant     + sessions  x seed  + history  + docs  x semantic  + patterns  + entities  + memory
              x seed baseline: dirty_worktree: refusing to seed uncommitted content without --worktree
                hint: commit or stash the working tree, then re-run `entire brain setup`; `entire enable` writes .entire/ and .claude/ without committing them, which is what a first run usually trips over
```

Both files are project configuration that belongs in the repository anyway, so
committing them is the real fix rather than a workaround. (`--worktree`, which
the refusal names, is a `refresh` flag — `setup` does not accept it.)

**A repository new to Entire reaches an all-green `setup` with no user action.**
On a committed `git init` repository with no remote at all, every component
builds:

```
Brain ready in 3.6s
  repo        /path/to/your/repo  (local/your-repo-686eadf04b0f)
  brain       ~/.local/share/entire/plugins/data/brain/repos/local/your-repo-686eadf04b0f
  instant     + sessions  + seed  + history  + docs  + semantic  + patterns  + entities  + memory  (3.6s)
  facts       .............. 0/0 sessions distilled
  backfill    - skipped (--no-backfill)  (0s)
  workspace   + default  (20ms)
  daemon      - skipped: --no-daemon  (0s)
```

`x semantic index: repo key mismatch` used to be the guaranteed second line
here, blamed on the repository having **no git remote**. It was wrong in both
directions — it also fired on every non-GitHub origin, because `entire-brain`
and `entire-graph` spelled the same repository differently — and it is fixed:
the brain now accepts the provider's own spelling of the repository it just
asked the provider to index. A mismatch that still appears means the snapshot
really does describe another repository, in practice one the provider cached
before this repo's origin changed, and the hint says to rebuild it with
`entire brain refresh index --force`.

`setup` still exits 0 whenever at least one component built, and still names the
ones that did not, so `Brain ready (degraded)` is information rather than
failure when you do see it. The things that legitimately produce it:

- **an uncommitted worktree** — `x seed` and `x semantic`, almost always the
  `entire enable` output above, fixed by committing it;
- **no `entire graph` provider** — `x semantic index:
  provider_doctor_failed: ...`, fixed by `scripts/install.sh`;
- **an unreadable checkpoint inventory** — `x session export`, when Entire
  cannot reach the checkpoint remote to prove there is nothing to export.

### Or build the brain by hand: `refresh`

`refresh` is the same deterministic build on its own — no backfill, no workspace,
no watcher. Use it when you want the brain and nothing else, or when scripting:

```sh
cd /path/to/your/repo

entire brain refresh --agent none
entire brain status
```

`refresh` exports captured sessions, builds the local history and doc indexes,
asks `entire graph` for a semantic snapshot, and stores the derived brain under
Entire's plugin data directory. `--agent none` keeps this build deterministic and
token-free, with no hosted-model calls.

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

Install the Graph-based `entire-graph` from source (or from a matching release tag
once available), then re-run `entire brain refresh --agent none` to add the
semantic code graph.

### Exploring a repository you don't have locally

`entire brain add <repo-url>` clones the repository, fetches its Entire
checkpoint history, and runs the build above in one step; afterward explore
the clone with `entire brain dash`, `status`, or `search`.

### Query the brain

Every result carries an `id` you can fetch in full with `get`.

```sh
entire brain overview                          # what the project is: stack, commands, recent decisions
entire brain brief "add rate limiting to the API"   # a bounded, task-shaped context packet
entire brain query "how does checkpointing work"    # hybrid lexical + vector search across the brain
entire brain query --keyword "checkpoint"               # exact keyword search
entire brain recall "why did we pick this default"  # durable facts for the current branch
entire brain get fact:<id>                     # fetch one item in full
```

Add `--json` to any of these for machine-readable output. `entire brain agent-guide`
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

If you ran `entire brain setup` without `--no-daemon`, a watcher is already
installed and keeping this repository fresh; the commands below are the manual
equivalents, for a brain built with `refresh` or a repository set up with
`--no-daemon`.

The default watcher performs deterministic refreshes only. Token-spending work,
such as fact distillation or seed synthesis, is opt-in and separately gated.

To keep the durable-facts layer fresh as new sessions land, enable distillation
explicitly:

```sh
entire brain watch --distill --distill-every 24h --model gpt-5.4-mini --effort low --budget 1
```

If you also want generated seed summaries, run refresh with an agent instead of
the deterministic seed path:

```sh
entire brain refresh --agent auto --seed-model gpt-5.4-mini --seed-effort low
```

Seed synthesis is separate from fact distillation: seed files orient an agent on
the repository, while durable facts are branch-scoped retained knowledge.

## How Agents Use The Brain

Agents rely on a capture layer plus four consumption surfaces. The capture
layer produces the raw material; the consumption surfaces are how an agent
actually queries or receives brain context. Which surface matters depends on the
agent client and how the repository is configured.

### Capture Layer: Entire Hooks Produce The Raw Material

`entire-cli` installs lifecycle hooks for agents such as Claude Code, Codex,
Gemini CLI, OpenCode, Cursor, Factory AI Droid, Copilot CLI, and Pi. Those
hooks start and stop sessions, capture prompts and transcripts, track changed
files, record subagent work where the agent exposes it, and attach checkpoint
metadata to commits.

This capture path is background infrastructure. The coding agent does not need
to remember to save a transcript, and the human does not need to paste context
into the brain. `entire brain refresh` later ingests the retained sessions and
checkpoint history that the hooks produced.

If an agent is not seeing Entire context at all, first check the base Entire
integration, not the brain:

```sh
entire status
entire agent
```

### 1. MCP Is The Preferred Agent Tool Surface

For agents that support MCP, register `entire brain mcp` as a local stdio MCP
server. The server opens no network listener and wraps the local brain's JSON
contracts as tools.

Once MCP is available, an agent should call MCP tools instead of shelling out to
the `entire brain` CLI. The normal first call is `brain_brief` for the task.
Then the agent should make only targeted follow-up calls:

- `brain_brief` for task-shaped context, history hits, likely files, and tests
- `brain_status` to check freshness, coverage, and blind spots
- retrieval tools such as `brain_query` and `brain_get` for facts, docs, and
  history
- semantic tools such as `brain_code`, `brain_context`, `brain_impact`, and
  `brain_tests` for code navigation and validation planning
- `brain_entity_history` for "which checkpoints and sessions changed this
  function/class", answered from the persisted entity index rather than by
  re-reading history (build it once with `entire brain entities backfill`)
- review, workspace, and pattern tools such as `brain_regressions`,
  `brain_workspace_graph`, `brain_workspace_review`, and `brain_patterns` when
  the task calls for them

`brain_brief` keeps its existing JSON-in-text response by default. Clients
experimenting with a smaller agent packet can pass
`packet_format: "compact_v1"`, `packet_format: "compact_v2"`, or
`packet_format: "compact_v3"`; these are opt-in serialization changes over the
same ranked report, not different retrieval treatments. In v2 and v3's hashed
in-band legend, `~` means absent and `^` means the previous value in the same
opcode and column, across interleaved records. V2 limits `^` to metadata
columns; v3 permits it for every exact repeated value. V3 also uses the compact
`entire.brain_brief c3` marker and an unpadded base64url SHA-256 footer. Both
formats use positional rows for a repeated family only when its complete
canonical encoding is strictly smaller than keyed rows; ties remain keyed. Use
`legacy_json` (or omit the argument) when a consumer depends on the existing
JSON contract.

Some MCP tools write local state. For example, `brain_ingest_traces` persists
runtime edges as `RUNTIME_TRACE` facts. Pattern MCP tools are read-only; skill
formation remains a CLI-only write path.

The benchmark harness treats this as the cleanest agent contract: MCP-backed
agents call named `brain_*` tools, and in MCP conditions they are explicitly
kept away from direct `entire brain` shell commands or prewritten history
excerpt files. See [Semantic MCP Guide](semantic_mcp_guide.md) for the full tool
surface.

The MCP and CLI surfaces are intentionally close, but they are not perfect
mirrors. Some CLI-only flows, such as `recall --expand`, have no dedicated MCP
tool. Some MCP tools, such as `brain_review`, wrap machine contracts that are
hidden or discouraged as direct human CLI verbs. Prefer the native surface your
agent client has, then translate only when needed.

### 2. Intake Instructions Are The File-Based Fallback

When MCP is not available, an agent can still use the brain through the intake
instructions in `templates/`. The Codex and Claude templates tell the agent to
locate the brain with:

```sh
entire brain path "$PWD"
```

Then the agent reads staged context instead of blindly opening everything:

- `README.md` and `manifest.json` in the brain directory
- seed files such as `seed/agent/quick-overview.md`,
  `seed/agent/overview.md`, `seed/agent/architecture.md`,
  `seed/agent/risks.md`, `seed/agent/maintenance-guide.md`, and
  `seed/agent/open-questions.md`
- `seed/history-gaps.md`, when present
- only task-relevant transcripts or records selected from the manifest

This is how an agent like Codex can use `entire-brain` without a dedicated MCP
connection: resolve the local brain, read the manifest and seed context, inspect
only the relevant retained history, and treat gaps as uncertainty rather than
inventing rationale.

### 3. Direct CLI Is A Compatibility Surface

For agent clients without MCP, direct `entire brain ... --json` calls are the
compatibility surface. Humans and scripts can use the same commands. Agents
using this surface should treat it like a tool API, not as an invitation to run
a long command tour.

A disciplined direct-CLI intake is:

```sh
entire brain status --json
entire brain brief "<task>" --json
```

Then ask the smallest next question. Examples:

```sh
entire brain query "<decision or prior-work question>" --json
entire brain inspect code "<symbol or concept>" --json
entire brain inspect context "<symbol-or-id>" --json
entire brain inspect tests "<symbol-or-id>" --json
entire brain inspect regressions "<task or invariant>" --location-only --json
```

Agents should prefer `query` for broad facts/history/docs, `search` for exact
terms, semantic `inspect` subcommands for code graph questions, and `get` or
`multi-get` when a prior result returned an id. They should not broaden into
repo-wide text search until the brain's targeted context has been used.

### 4. Hidden Hooks Deliver Context At The Moment Of Relevance

`entire-brain` also has a hidden hook surface for harness integrations. These
hooks are not installed by the base Entire repository enablement flow; a harness
must be configured to invoke them, for example from Claude Code
`PreToolUse`/`PostToolUse` hooks or Entire CLI lifecycle hooks. Until then they
have no effect. They are not intended as human-facing commands, although they
can be hand-tested when
debugging harness wiring. They are designed to surface a small, high-confidence
fact exactly when it matters:

- before editing a file, facts anchored to that file
- after a failed command, gotchas and known dead ends that match the failure
- after a session ends, optional one-session refresh and distillation

The contract is intentionally quiet: if there is no relevant fact, no local
brain, or an environment problem, the hook exits successfully with no output on
stdout. Diagnostic one-liners may still go to stderr. That keeps hooks from
breaking the agent workflow or flooding the transcript. Session-end distillation
is still an explicit token-spending path: it skips when no agent is available
and must respect no-egress/local-only policy.

## Usage Scenarios

The examples below name the preferred MCP tool when there is one. Agent clients
without MCP should use the equivalent `entire brain ... --json` command from
the direct-CLI surface above.

### Orient An Agent On A New Task

The agent should start by asking for task-shaped context, not a generic project
summary. The normal tool is `brain_brief`. The useful output is the freshness
state, likely files and symbols, relevant history records, durable facts, likely
tests, and any blind spots.

Begin substantive orientation with a brief unless equivalent context is already
available. Reuse useful locations without a redundant Graph query. Inspect source
directly when locations are sufficient, including small edits and follow-ups. Do
not automatically refresh or repair tools to answer an ordinary query. See
[coordinated agent instructions](agent-coordination.md).

For repo-level orientation rather than task-specific orientation, use
`overview`. It returns a compact project map: stack signals, entrypoints,
commands, key documents, and recent decisions.

```sh
entire brain overview --json
```

For interactive human exploration instead of scripted queries, `entire brain
dash` opens a terminal dashboard over facts, sessions, history, and semantic
records, and `entire brain viz` opens a local, no-egress browser view of the
semantic call graph. Both are read-only and make no agent calls.

### Keep The Brain Fresh

Freshness is part of every answer. A stale semantic index, missing provider,
dirty-worktree mismatch, or parser blind spot changes how much an agent should
trust the brain.

Agents should check freshness through `brain_status` or `status --json` before
relying on semantic answers. If freshness is unsafe, an agent with write
permission can request a refresh; otherwise it should report the limitation and
fall back to direct repository inspection.

Humans or automation should keep the brain fresh:

```sh
entire brain status
entire brain refresh --agent none
entire brain watch
```

When semantic artifacts are out of sync or damaged, use the semantic
maintenance paths instead of deleting the whole brain:

```sh
entire brain refresh index
entire brain repair
entire brain reset --semantic-only --force
```

Use `refresh index --worktree` only when you intentionally want uncommitted
state in the semantic index. Exported bundles reject worktree-backed semantic
indexes.

### Continue Or Explain Prior Work

When the question is "why is this like this?", "what did the previous agent
try?", or "where did this session leave off?", the agent should search the
history and facts layers. The normal retrieval tool is `brain_query`,
followed by `brain_get` for specific ids.

This is the main reason Entire capture matters: the original prompt, attempts,
validation, correction, and rationale can survive the code diff and become
available to the next agent.

### Ask Across Facts, History, And Docs

When the question is not tied to one symbol, use the unified retrieval layer.
`query` is the normal hybrid path over durable facts, indexed history, and docs.
Use `query --keyword` for exact keywords, `query --semantic` for semantic matches, and
`get`/`multi-get` when a result returns an id worth reading in full.

```sh
entire brain query "how does checkpointing work" --json
entire brain query --keyword "checkpoint" --json
entire brain query --semantic "preventing data races" --json
entire brain get fact:<id> --json
```

Use `entire brain query "text"` for default hybrid retrieval. Select keyword
matching with `--keyword` or semantic matching with `--semantic`; the two
flags are mutually exclusive and belong only to the query command.
Text may instead be supplied as `--query "text"`. Flags work before or after
positional text; combining positional text and `--query` is an error.
For example: `entire brain query --keyword --query "RetryPolicy" --json`.

Workspace retrieval supports the same forms:
`entire brain workspace query <workspace> --semantic --query "retry policy"`.
The old `search` and `vsearch` commands remain hidden compatibility aliases.
MCP agents should use `brain_query` with optional, mutually exclusive
`keyword: true` or `semantic: true` arguments; `brain_search` and
`brain_vsearch` remain compatibility tools.

For durable facts specifically, use `recall`. `recall --expand` is an
agent-assisted query expansion path, so it belongs behind the same egress
judgment as other agent calls.

```sh
entire brain recall "account deletion" --k 5
entire brain recall "MirrorCommittedMetadataRef" --expand
```

### Navigate Code By Meaning

When a task depends on structure rather than text, the agent should use semantic
tools. The normal path is: find candidate symbols, inspect relation-aware
context, inspect likely impact, then ask for tests. The normal tools are
`brain_code` or `brain_search_code`, followed by `brain_context`,
`brain_impact`, and `brain_tests`.

This is where `entire-graph` matters. It is not an agent memory layer; it is the
local parser/provider that gives `entire-brain` the graph the agent queries.
Semantic depth is language-dependent: parser-backed extraction covers the
semantic language set, while many recognized filetypes are inventory-only. For
inventory-only files, prefer text retrieval and lower confidence in
impact/context answers.

For deeper graph work, use the graph specialists: local graph UI,
source snippets, directed trace paths, dead-code candidates, boundary
enumeration, and working-tree change mapping.

```sh
entire brain inspect graph-ui semantic-graph.html
entire brain inspect snippet "<symbol-or-id>" --json
entire brain inspect trace-path "<caller>" "<callee>" --json
entire brain inspect dead-code --json
entire brain inspect boundaries --kind tool --json
entire brain inspect changes --json
```

### Review Risk Without A Clean Diff

For long-running sessions, manual edits, or suspected regressions from memory,
the agent should use diff-less regression review. The normal tools are
`brain_regressions` or `brain_review`.

When the goal is a fair investigation, prefer location-only output first. It
points the agent to suspected files and lines without handing it the expected
answer text.

### Connect Runtime Evidence To Source

When static structure is not enough, import runtime trace edges with
`brain_ingest_traces` or `inspect ingest-traces`. The brain compares the runtime
edges with known static relations, persists them as `RUNTIME_TRACE` graph facts,
and makes them available to graph schema, trace-path, and brief/context flows.

This is useful for incidents and performance/debugging tasks where "what
actually happened?" matters more than "what could call this?"

### Work Across Multiple Repositories

Use workspaces when the behavior crosses repository boundaries: a frontend calls
an API, a service emits an event another service consumes, one repo imports a
package from another, or deployment/config resources connect separate projects.

Humans create and refresh the workspace. Agents consume workspace context
through MCP workspace tools where available, or through direct workspace CLI
commands when MCP does not expose the required traversal. The workspace brain
keeps member repos local and builds cross-repo contracts and graph edges from
their existing local brains.

```sh
entire brain workspace create platform
entire brain workspace add platform ../api --name api
entire brain workspace add platform ../web --name web
entire brain workspace refresh platform --full
```

After the workspace exists, use workspace context, graph, impact, retrieval,
and review flows when the question crosses repos:

```sh
entire brain workspace inspect context platform "checkout" --json
entire brain workspace inspect impact platform "checkout" --json
entire brain workspace inspect graph platform --json
entire brain workspace query platform "checkout" --json
entire brain workspace review platform "checkout regression" --json
entire brain workspace watch platform --once
```

### Preserve Durable Project Knowledge

Durable facts are short, provenance-anchored statements about decisions,
constraints, preferences, pitfalls, and standing rules. Humans can write them
directly when they know the rule. Agents can distill candidate facts from
captured sessions when explicitly asked or when a session-end integration is
configured to do so.

```sh
entire brain remember "Prefer table-driven tests for parser edge cases" --path preferences.tests
entire brain distill --dry-run --json
entire brain distill --agent codex --model gpt-5.4-mini --effort low
entire brain facts tree --depth 1
```

Distillation is the token-spending path. It is incremental and cached, but it
still sends redacted transcript chunks to the selected agent unless you choose a
local loopback agent such as Ollama or use dry-run/no-egress mode.

Durable facts also have a lifecycle. Review queued merge/supersede proposals,
promote branch facts when they should carry forward, retract facts that are no
longer true, and garbage-collect stale retractions.

```sh
entire brain facts review
entire brain facts promote --from <branch> --strategy keep-both
entire brain facts retract <fact-id>
entire brain facts gc --force
entire brain inspect blame <fact-id> --json
```

### Extract Reusable Agent Skills

Skill extraction is not "turn repeated commands into a checklist." The current
pipeline forms skills only from accepted deep dossiers: verified evidence that a
pattern is non-obvious and useful to future agents.

There are several evidence paths:

- recurring task patterns that pass deep verification
- verified themes that capture latent practices by meaning
- corrected or failed episodes grouped into lessons
- durable gotchas, conventions, and invariants grouped into capabilities
- workspace families where equivalent procedures differ across repos

A human or explicitly authorized automation runs the egress-gated verification
and formation commands. The agent performs the verification and `SKILL.md`
synthesis under that explicit request. The verifier is cached. Refresh, watch,
brief, query, MCP reads, and workspace reads do not silently call it.

```sh
entire brain patterns
entire brain patterns verify --deep
entire brain patterns verify --themes
entire brain patterns verify --lessons
entire brain patterns verify --conventions
entire brain patterns skills
entire brain patterns skills form <id>
entire brain patterns skills form <id> --yes
```

`patterns skills` lists formable proposals, not raw pattern rows. `form <id>`
previews the evidence, generated `SKILL.md`, and destination paths without
writing. If no accepted deep dossier exists yet, `form` refuses and tells you to
run verification first; it never falls back to shallow evidence. `--yes` writes
the skill; existing files require `--force`.

By default, generated skills target the cross-agent `standard` destination
(`.agents/skills/`). Use `--target claude-code|codex|factoryai-droid|all` only
when an agent-specific destination is needed.

Workspaces mirror the same flow for cross-repo families:

```sh
entire brain workspace patterns verify <workspace>
entire brain workspace patterns skills <workspace>
entire brain workspace patterns skills form <workspace> <id>
```

Pattern inspection is useful even when you are not forming skills. Use it to see
recurring tasks, practices, risks, themes, verifier state, and whether accepted
skills are stale.

```sh
entire brain patterns
entire brain patterns status
entire brain workspace patterns <workspace>
entire brain workspace patterns status <workspace>
```

### Measure Brain Quality

The brain includes evaluation harnesses for maintainers who need evidence that a
retriever or fact layer is actually helping. These are not required for everyday
use, but they are the right surface before claiming quality improvements.

```sh
entire brain facts eval-gen > facts-tasks.json
entire brain facts eval --tasks facts-tasks.json --retriever facts --json > facts-eval.json
entire brain facts eval-compare --a <before.json> --b <after.json>
entire brain history-eval-gen > history-tasks.json
entire brain history-eval --tasks history-tasks.json --json
```

For semantic performance and provider output, use the semantic benchmark and
status audit instead of relying on anecdotes:

```sh
entire brain bench semantic .
entire brain status --json
```

### Tune Retrieval

The default vector arm uses the bundled local Model2Vec embedder. For higher
semantic recall, opt into a local transformer embedder served by Ollama or a
compatible loopback endpoint:

```sh
ollama pull embeddinggemma
ENTIRE_BRAIN_EMBEDDER=ollama entire brain query "preventing data races" --json
```

Keep this distinction clear: changing the embedder changes retrieval behavior,
not the underlying source of truth. Facts, history, docs, and semantic records
still come from the local brain.

## Privacy And Egress

The default brain artifacts are local and inspectable. Deterministic refresh,
semantic indexing, local history indexing, and MCP tool calls do not require a
hosted model. The MCP adapter is stdio-only.

Some operations perform network egress, either by sending selected context to a
model or by fetching over the network:

- a configured `checkpoint_remote` lets `refresh` and its `sessions` export
  stage fetch checkpoint history from that remote into a throwaway temp repo;
  this is network egress, not a hosted-model call, and is skipped under
  no-egress mode
- seed synthesis during `refresh --agent auto`
- fact distillation with `distill --agent ...`
- query expansion with `recall --expand`
- pattern verification and skill synthesis
- judged evaluation commands
- `publish` uploads the serialized local brain to hosted Entire; opt-in and off
  by default, requiring both the command and `ENTIRE_BRAIN_ALLOW_HOSTED=1`

Use `--agent none`, `--dry-run`, local loopback Ollama, or
`ENTIRE_BRAIN_NO_EGRESS=1` / `ENTIRE_BRAIN_LOCAL_ONLY=1` when the repo must stay
strictly local. No-egress mode enforces locality for no-agent, dry-run, and
loopback-Ollama paths by validating URLs, redirects, and resolved dial targets.
A custom `--agent command` runner is treated as a trusted local command and is
not enforceably loopback-only; no-egress mode cannot stop that runner from
making its own network calls.

Remember that base Entire session capture stores transcripts and metadata on
the repository's `entire/checkpoints/v1` branch. Anyone with access to that
branch can read captured prompts, tool activity, and retained transcript data.
Entire redacts detected secrets before writing checkpoint metadata, but
redaction is best-effort. Redaction covers transcript and checkpoint metadata,
not every local working artifact. Entire also writes temporary shadow branches
such as `entire/<short-hash>` whose code-file snapshots are raw, unredacted
blobs of the working tree. Gitignored files are filtered out only as a partial
defense. Entire does not push shadow branches; do not push them manually, or
unredacted source could reach the remote. Review the Entire CLI security and
privacy guide before enabling Entire on sensitive or public repositories.

## Storage And Configuration

Entire CLI supplies the plugin directories that make the brain durable:

- `ENTIRE_PLUGIN_CONFIG_DIR` for `brain.json`
- `ENTIRE_PLUGIN_DATA_DIR` for generated repo brains and workspaces
- `ENTIRE_PLUGIN_STATE_DIR` for regenerable cursors such as watcher state
- `ENTIRE_PLUGIN_CACHE_DIR` for caches
- `ENTIRE_REPO_ROOT` for the current checkout when invoked inside a repo

Repo keys are derived from the repository origin. For example,
`github.com/entireio/cli` becomes `gh/entireio/cli`.
