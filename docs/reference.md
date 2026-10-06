# Entire Brain

Entire Brain is an external-command plugin for the Entire CLI. It builds a local,
inspectable "brain" for a repository — and this is the fastest way to understand
what it does, how to install it, and how humans and agents actually use it.

## What This Is

Entire is a Git-native platform for AI-assisted software work. Its base layer is
session capture: `entire-cli` installs Git and agent hooks for supported coding
agents, records prompts, transcripts, tool activity, files touched, token usage,
and checkpoint metadata, then stores that context outside normal code history.
Current repositories use one Entire-managed ref per checkpoint under
`refs/entire/checkpoints/`; repositories created with the legacy backend retain
the aggregate `entire/checkpoints/v1` branch. Brain reads both during a backend
transition. A checkpoint is the retained link between an agent session and the
commit or intermediate work state it produced.

`entire-graph` and `entire-brain` add the local reasoning layer on top of that
captured history. `entire-graph` is the semantic provider: it parses source code
locally and emits versioned code-structure records and semantic diffs.
`entire-brain` consumes those records plus Entire sessions, checkpoint history,
docs, runtime traces, durable facts, and pattern evidence into a local
repository brain.

The result is a local memory system for humans and agents. Humans install,
refresh, and curate the brain. Agents consume it through configured hooks, MCP
tools, file-based intake instructions, or direct JSON commands when MCP is not
available. The practical effect is that an agent can start work with retained
context, freshness signals, semantic navigation, prior decisions, likely tests,
and known failure modes instead of rediscovering them from scratch.

The plugin binary is named `entire-brain` and is invoked through Entire as
`entire brain`.

## Agent setup and capability discovery

| Command | Purpose |
|---|---|
| `entire brain init-agents [path]` | Install the guide and managed pointers in AGENTS.md/CLAUDE.md |
| `entire brain agent-guide` | Print the canonical coding-agent operating guide |
| `entire brain capabilities [--json]` | List compiled features, retrieval sources, requirements, and experimental features |
| `entire brain refresh index` | Build the semantic index from committed HEAD (use `--worktree` for dirty code) |

`init-agents` accepts `--repo <path>` instead of the positional path and
`--json` to return a `changed_files` array (empty when already up to date).
With neither path form it uses the host's repository root, then the nearest Git
repository. Outside a repository, supply an explicit project path to install.
It writes `.entire/agent-guide.md` and reconciles legacy Graph and Brain blocks
into shared `<!-- entire-agent:begin -->` / `<!-- entire-agent:end -->` pointers.
Existing user text and supported instruction aliases are preserved. Unsafe write
paths and malformed markers are refused. The guide is plugin-managed; keep user
instructions outside managed blocks. No Brain setup or services run.

`agent-guide --repo <path>` previews the same repository-specific instructions.
Outside a repository it prints the standalone reference without detection.
See [the coordination contract](agent-coordination.md) for routing and migration.

`guide` remains a compatibility alias for `agent-guide`.
`capabilities --json` emits schema version 1: `build`, `query`, `sources`,
`features`, `experimental`, `graph_provider`, and `readiness_command`.
A source's `semantic_compiled` field reports build support only;
`semantic_requires` lists additional runtime requirements. No repository,
provider invocation, or model initialization is needed. This is distinct from
`status --details --json`, which checks a particular Brain's readiness.
Language/relation inventories come from `entire graph capabilities --json`.

## Development source of truth

Entire Brain releases are tagged (the public line starts at `0.1.0`), but development
still starts from the locally fetched current mainline — a tag is a marker on
main, not a branch to work from. Do not resume an old WIP/integration checkout
or use an older Brain binary as a benchmark control. The Agent Brain harness
builds from the active checkout and refuses to run unless `HEAD` contains local
`origin/main`. All causal arms use that same binary and vary only memory
delivery.

GraphMark owns cross-product benchmark evidence and split integrity. See
[`benchmarks/agent-brain/CONDITIONS.md`](../benchmarks/agent-brain/CONDITIONS.md)
for the normative condition and comparison contract, and
[`benchmarks/agent-brain/README.md`](../benchmarks/agent-brain/README.md) for
harness operation and the retired-corpus notice.

## Install

The whole journey is two commands: one installs the plugins on this machine,
one onboards a repository. Do not stop after the first — installing gives no
repository a brain.

```sh
# once per machine
git clone https://github.com/entireio/entire-brain.git
entire-brain/scripts/install.sh

# once per repository you want a brain for
cd /path/to/your/repo
entire brain setup
```

The rest of this section is what each of those two commands does, in order.

### 1. Install the plugins on this machine

```sh
git clone https://github.com/entireio/entire-brain.git
entire-brain/scripts/install.sh
```

That is the whole *install* — and the install is not the setup. It builds two
plugins and registers them with the Entire CLI, so `entire brain` and `entire
graph` start working. **No repository has a brain yet.** Onboarding one is
[step 3](#3-onboard-a-repository-entire-brain-setup), and it is one more command.

`scripts/install.sh` finds the `entire-graph` semantic provider on your machine
or clones it for you, so nothing has to be arranged in advance and this checkout
can live anywhere under any name.

**Prerequisites.** One command does not mean no dependencies — it means you no
longer have to arrange directories. These are still required, and `install.sh`
checks all of them before it builds anything, reporting every missing one in a
single pass with the fix for each:

- the Entire CLI, on `PATH` as `entire`
- Git
- a Go toolchain, 1.27 or newer, for `entire-brain`
- a cgo-capable C compiler for `entire-graph`, which uses tree-sitter native
  parser bindings — `xcode-select --install` on macOS, `build-essential` on
  Debian/Ubuntu. `entire-brain` itself is a pure-Go build and does not need cgo.

Nothing about a *repository* is a prerequisite of this install. What a repository
needs before `entire brain setup` — and what it does not — is
[step 3a](#3a-what-setup-needs-first-and-what-it-does-not).

**What it does to your machine.** Four things, and nothing else:

1. builds `entire-graph` and registers it with the Entire CLI
   (`entire plugin install`),
2. builds `entire-brain` and registers it the same way,
3. writes the default `entire-brain` plugin configuration
   (`entire brain config init`),
4. runs `entire brain doctor`.

The two binaries are built inside their checkouts and linked into Entire's
managed plugin directory under your home directory. Nothing is installed
system-wide and nothing asks for `sudo`.

**Where `entire-graph` comes from.** Three routes, tried in order; the installer
prints the one it took.

1. `ENTIRE_GRAPH_DIR`, when you set it. An explicit override is used exactly as
   given — if it does not hold an `entire-graph` checkout the run stops there
   rather than quietly installing something else.
2. A checkout already on this machine: next to this one (or next to the main
   checkout, when you are in a linked worktree), the route-3 cache, and the
   usual source directories under `$HOME`. A candidate has to *be*
   `entire-graph` rather than merely be named that, so it is accepted only when
   `cmd/entire-graph` is present.
3. Otherwise a shallow clone of the public
   [`entireio/entire-graph`](https://github.com/entireio/entire-graph) into
   `${XDG_CACHE_HOME:-~/.cache}/entire-brain/entire-graph`. It is a public
   repository, so no credentials are needed; the clone takes a few seconds and
   later runs reuse and refresh it.

If none of the three can work, the installer fails naming all three and how to
satisfy each. `ENTIRE_INSTALL_OFFLINE=1` disables route 3 for an air-gapped
machine.

**This is the source-build path, and it is not the only one.**
`scripts/get-brain.sh` is a `curl | bash` installer that fetches a released
binary for your platform, verifies it against the release `checksums.txt`, and
registers it -- no clone, no Go toolchain, no C compiler, because Brain's
default build is pure Go. It fails closed rather than installing something it
could not verify. `entire plugin install brain` does the same through the plugin
index. Build from source when you intend to change Brain, or when you want the
`brain_cgo` build; otherwise install a release. `scripts/bootstrap.sh` is the equivalent for a
machine with nothing at all — it clones `entire-brain` when it is not already
there (falling back to `gh` for the private-repo credentials) and then runs
`install.sh`.

### 2. Verify

```sh
entire graph version
entire graph doctor --json
entire brain version
entire brain doctor
entire plugin doctor
```

If `doctor` reports the semantic provider is missing, re-run
`scripts/install.sh` and read the route line it prints under
`==> entire-graph semantic provider`; that is the checkout it built the provider
from.

**`entire brain doctor` exits non-zero when it reports an `error` finding about
the brain**, so it can be used as a CI or script gate directly. Warnings do not
fail it -- "not built yet" is a warning, and a fresh brain has several. Tighten
or loosen that with `--fail-on`, which mirrors `status --fail-on`: the whole
report is printed first, then the exit code is set.

```sh
entire brain doctor                   # default: nonzero on any brain `error`
entire brain doctor --fail-on warn    # strict: nonzero on brain warnings too
entire brain doctor --fail-on none    # report only, always exits 0
```

Findings about the **host environment** -- whether the `entire` executable is on
PATH, and the host adapter the Entire CLI owns -- are reported at their real
severity but never gate. They are claims about a different product on the
machine, not about the brain doctor was asked to diagnose: a brain is fully
readable and queryable with no `entire` on PATH, so failing on it would make
`doctor` unusable from CI, a container, or a fresh checkout. Those findings
carry `"scope": "environment"` in `doctor --json`, so a caller that does want
to gate on the machine can do so explicitly.

### Managing the provider checkout yourself

If you would rather own the `entire-graph` checkout, clone both repositories and
run the same installer. Directory names do not matter, and the two checkouts do
not have to be siblings:

```sh
git clone https://github.com/entireio/entire-graph.git
git clone https://github.com/entireio/entire-brain.git
cd entire-brain
scripts/install.sh
```

Side-by-side checkouts are discovered without any configuration. Set
`ENTIRE_GRAPH_DIR=/path/to/entire-graph` only when your provider checkout lives
somewhere the installer would not look, or when you want to pin a specific one.

Other install paths:

- `scripts/bootstrap.sh` — clone `entire-brain` if it is not already here, then install.
- `scripts/install-local.sh` — build and register just this plugin; the provider is left alone.
- `mise install && mise run check && mise run build && entire plugin install ./entire-brain` — the same, with the full check suite in front of it.
- `scripts/release.sh` — local release archives with `SHA256SUMS`.

See [docs/operations.md](operations.md) for target, cgo, and shared
baseline details.

### 3. Onboard a repository: `entire brain setup`

This is the step the install exists for, and the next command you run. Go to a
repository you actually work in:

```sh
cd /path/to/your/repo
entire brain setup
```

One command, three phases, each with an honest cost label:

```sh
entire brain setup                             # build now, backfill in the background, install the watcher
entire brain setup --no-backfill               # build + watcher, no token spend up front
entire brain setup --no-backfill --no-daemon   # phase 1 only: spends nothing at all, changes no service
entire brain status                            # backfill progress + daemon health
entire brain setup --uninstall-daemon          # stop and remove the watcher
```

| phase | blocking? | spends tokens? | installed by default? |
|---|---|---|---|
| 1. **instant core** — sessions, semantic index, seed, docs, history, entities | yes, seconds | **no**, deterministic | — |
| 2. **fact backfill** — distill past sessions into durable facts, newest first | no, detached | **yes** | yes, unless `--no-backfill` |
| 3. **watcher daemon** — a launchd agent / systemd user unit that keeps every set-up repo fresh | no | **yes**, per window | **yes, with no prompt**, unless `--no-daemon` |

Read the three rows as three separate consents, because `setup` asks for none of
them:

- **The instant core is free and deterministic.** No hosted-model call, no
  token. When it returns — seconds on a small repo, about a minute on a large
  one — the brain is queryable.
- **The backfill SPENDS TOKENS.** It distills your past sessions with an agent
  CLI, detached, newest first, capped at **25 sessions per pass**
  (`--backfill-budget`; `--backfill-budget 0` explicitly means the whole corpus
  in one spend). `--no-backfill` skips it entirely. It is also skipped on its
  own when no agent CLI is available.
- **The watcher is installed as a persistent background service, by default,
  without asking.** On macOS that is a launchd agent with `RunAtLoad` and
  `KeepAlive` loaded through `launchctl load -w`; on Linux a systemd user unit
  with `Restart=always`, enabled with `systemctl --user enable --now`. It
  survives logout and reboot, and its gated distill step calls an agent, so it
  is a **recurring** spend, not a one-off. There is no confirmation prompt
  anywhere in `setup`. What there is, is a pre-flight line — the FIRST thing a
  run that will register a watcher prints, before any work, naming the label,
  the unit path, and both ways out:

  ```
  setup: background watcher: this run will install io.entire.brain-watch.<id> at
  ~/Library/LaunchAgents/io.entire.brain-watch.<id>.plist — a persistent launchd
  service that starts again at every login; pass --no-daemon to skip it, or
  remove it later with `entire brain setup --uninstall-daemon`
  ```

  A re-run that finds the watcher already there says it *keeps* it rather than
  claiming a second install, and `--no-daemon` says nothing about a service it
  will not install. Pass `--no-daemon` if you do not want it, and
  `entire brain setup --uninstall-daemon` to stop and remove one you already
  have.

**So a default `setup` spends money, and then keeps spending.** It is not a
one-off build: it installs a background service whose gated step calls an agent.
Four gates bound it, and none of them is a quota you have to remember:

- `--backfill-budget` caps the background pass. It defaults to **25 sessions**,
  newest first — the newest sessions carry the facts you actually want. Setup
  prints what the cap did.
- `--distill-every` (default 24h) plus each repo's persisted watch cursor allow
  **one** gated agent run per repo per window, and survive restarts. That bounds
  how *often* the daemon spends; `--backfill-budget` bounds how *much* each
  spend costs — setup installs the watcher with the same per-pass session cap it
  gave the backfill, so a window can never quietly distill the whole remaining
  corpus.
- The distill cache reuses unchanged sessions unless forced, across every entry
  point.
- `--model` / `--effort` keep each call cheap; the daemon uses whatever the
  setup for *that workspace* was given.

**`setup` exits 0 whenever the brain is queryable** — that is, whenever at least
one instant-phase component built. A component that fails is reported as one
line and the run carries on; `entire brain status` then shows it as failed and
`entire brain doctor` prints the reason. It exits non-zero only when the brain
directory cannot be built or every component failed.

**Then use it.** These are the three commands worth knowing on day one:

```sh
entire brain status                      # what was built, what is stale, is the watcher alive
entire brain overview                    # what this project is: stack, boundaries, commands, recent decisions
entire brain brief "<the task at hand>"  # a bounded context packet for that task
```

### 3a. What `setup` needs first, and what it does not

`setup` hard-fails on exactly one thing: **the target must be a local path that
is a git repository**. Everything else it either handles or degrades around.

**Required before you run it:**

- The two plugins from step 1, registered with the Entire CLI. `entire brain`
  has to resolve.
- A local directory that is a git repository with at least one commit. `setup`
  checks this up front, before it registers a workspace, writes any state or
  installs anything, and refuses with `not a git repository: <path>` — because
  every deterministic source it builds reads git history. A remote URL is
  rejected too: `setup requires a local repository path`.

**Not required — `setup` does not need these and will not do them for you:**

- **`entire enable` in the repository.** `setup` deliberately does *not* wire
  the session-end hook: hook registration belongs to the Entire CLI's repository
  enablement, which owns the whole lifecycle hook set, and duplicating it here
  would leave two hooks racing to distill the same session. `setup` **reports**
  the hook as one advisory line and carries on — it is never a failure.
- **Captured Entire sessions.** With none (a repository Entire has never been
  enabled in), the export returns an empty inventory and the `sessions`
  component is green — an empty answer is a true one. You get a brain from code,
  docs and git history; you do not get one from your past agent work, which is
  the source it is most interesting for. The export fails only when Entire
  cannot prove the inventory is empty rather than merely unreadable — a
  configured checkpoint remote it cannot enumerate, offline or unauthenticated —
  and the hint then names `entire checkpoint list` as what Entire itself can see.
- **A working `entire graph` provider.** A missing or mismatched provider fails
  only the `semantic` component.
- **A supported service manager.** On a platform with neither launchd nor
  systemd the daemon phase is skipped with the reason printed, not failed.
- **An agent CLI.** No agent, no backfill; the rest still runs.

So the order that gets you the *best* brain, as opposed to merely a successful
command, is:

```sh
entire enable                              # in the repo: capture sessions + wire the session-end hook
git add .entire .claude && git commit      # enable wrote these and did not commit them
entire brain setup                         # then onboard: build, backfill, watch
```

`entire enable` first is a quality decision, not a correctness one. Run it after
`setup` and nothing breaks — the next backfill pass and the watcher pick the new
sessions up. Skip it entirely and the brain still builds, permanently without
the one source it is most interesting for.

**The commit in the middle is not optional, and it is the one thing about this
order that surprises people.** `entire enable` writes `.entire/settings.json`
and your agent's hook settings (`.claude/settings.json` for Claude Code) and
leaves them uncommitted, so `setup` run immediately afterwards finds a dirty
worktree and refuses to seed or index it — two more red ✗ on a first run,
caused by the command directly above. Both files are project configuration that
belongs in the repository anyway, so committing them is the real fix rather than
a workaround; `setup` prints the same remedy if you hit it. (`--worktree`, which
the refusal names, is a `refresh` flag — `setup` does not accept it.)

**A repository that is new to Entire now reaches an all-green `setup` with no
user action** — including a `git init` repository with no remote at all, on
which every one of the eight instant components builds:

```
Brain ready in 3.6s
  instant     + sessions  + seed  + history  + docs  + semantic  + patterns  + entities  + memory  (3.6s)
```

`Brain ready (degraded)` still exists, and when you do see it, read it as
information rather than as failure: setup exits 0, the brain is queryable, and
the summary names the component that did not build with a hint under it. What
actually produces it is the uncommitted worktree above, a machine with no
`entire graph` provider (`x semantic index: provider_doctor_failed: …`, fixed by
`scripts/install.sh`), or a checkpoint inventory Entire cannot read
(`x session export`).

`x semantic index: repo key mismatch` used to belong on that list, and no longer
does. `entire-brain` and `entire-graph` name the same repository differently —
the provider recognises `github.com` remotes and otherwise falls back to
`local/<basename>`, while the brain recognises many hosts and hashes the path of
a repository with no usable remote — so every `git init` repository and every
non-GitHub origin failed the semantic component, and the hint blamed the
reader's repository for a disagreement between two of our own tools. The brain
now recognises the provider's own spelling of the repository it just asked the
provider to index and stores the snapshot under its own key, so **a fresh
`git init` repository reaches an all-green `setup` with no user action.** A
mismatch that still appears means the snapshot really does name another
repository — in practice one cached by the provider before this repo's origin
changed — and the hint says to rebuild the index.

Re-running `setup` is safe and non-duplicating: it finds the existing workspace
membership and daemon instead of creating second ones, resumes a backfill that
is still running rather than starting a rival pass, and keeps the
`--interval`/`--distill-every`/`--model`/`--effort` a previous run was given
unless you pass the flag again. One machine gets exactly one watcher, shared by
every repo you set up.

That last sentence is enforced rather than hoped for. The installed unit is
byte-identical for every repo and every workspace — it runs `workspace watch
--distill` and nothing else — and everything that varies per workspace lives in
a machine-level watch plan (`<state>/watch-plan.json`) that the running watcher
re-reads on every pass. So setting up a second repo, even into a different
workspace with a different interval and model, adds a row to that plan; it does
not rewrite the unit, does not restart the running service, and cannot leave the
first repo unwatched. `entire brain status` names the workspaces the watcher
covers, so you can see you are still in there.

### 3b. The manual path: `entire brain refresh`

`setup` is the recommended route and the one the rest of this reference assumes.
`refresh` is the same deterministic build on its own, with no backfill, no
workspace registration and no watcher — useful when you want the brain and
nothing else, or when you are scripting a build:

```sh
cd /path/to/your/repo

entire brain refresh --agent none
entire brain status
```

Refresh refuses a dirty worktree by default. Run it on a clean checkout, or use
`entire brain refresh --worktree` only when you intentionally want seed/docs to
include the current uncommitted state — this does not touch the semantic index.
Use the advanced `refresh index --worktree` path when the semantic layer itself
needs to reflect uncommitted state. Worktree-backed semantic indexes are
rejected by bundle export.

`--agent none` keeps the build deterministic and token-free, with no
hosted-model calls. Refresh exports captured sessions, builds the local
history/doc indexes, asks `entire-graph` for a semantic snapshot, and stores the
derived brain under Entire's local plugin data directory.

A brain built this way answers from captured history, docs, semantic code
structure, runtime traces, patterns, and any existing durable facts. It has not
extracted new durable facts from retained sessions, and nothing keeps it fresh —
that is what `setup`'s phases 2 and 3 add.

### 3c. Trying it: `scripts/trial-setup.sh`

Before you point `setup` at a repository you care about, you can watch it run
against a throwaway one. One command, no arguments:

```sh
scripts/trial-setup.sh
```

It builds `entire-brain` from this checkout, creates a scratch git repository
with real content and history (a synthesised Go + Markdown tree, ~130 files over
13 commits), runs `setup` twice to show a re-run is idempotent, runs `status
--verbose` and `doctor`, exercises the failure modes (a directory that is not a
git repository, an offline run, two concurrent setups), then tears the sandbox
down and proves nothing leaked.

It is safe on a real machine. Every path the binary can write is redirected into
`.trial-setup/`: `HOME`, the four `XDG_*_HOME` directories, the four
`ENTIRE_PLUGIN_*_DIR` directories, and `ENTIRE_BRAIN_DAEMON_DIR`. It also sets
`ENTIRE_BRAIN_DAEMON_NO_REGISTER=1`, and **both of those last two are needed**:
`ENTIRE_BRAIN_DAEMON_DIR` moves the plist / systemd unit *file*, while
`launchctl load -w` and `systemctl --user enable --now` act on your live session
whatever directory the file came from. Set only the directory — as a CI job or a
smoke test naturally would — and a "sandboxed" run still registers a real,
restart-forever agent on the machine. `ENTIRE_BRAIN_DAEMON_NO_REGISTER` is the
knob that withholds the service-manager call while still writing the unit, so
what *would* have been installed stays inspectable.

The teardown compares `launchctl list` / `systemctl --user` and the user unit
directories against a snapshot taken before the run, so an existing watcher of
your own is never mistaken for a leak. The run spends no agent tokens
(`--no-backfill` throughout, and the watcher is never started).

```sh
TRIAL_DIR=/path/to/scratch scripts/trial-setup.sh   # sandbox somewhere else
TRIAL_KEEP=1 scripts/trial-setup.sh                 # keep the sandbox to poke at
```

### 3d. Watching it: `scripts/demo-setup.sh`

`trial-setup.sh` answers *is this safe*. `demo-setup.sh` answers *what does it
look like*. One command, no arguments, and — like `scripts/install.sh` — no
prior arrangement: a fresh clone of this repository is the whole prerequisite.

```sh
scripts/demo-setup.sh
```

The `entire-graph` provider it needs is resolved by the same three routes
`install.sh` uses, and the run prints which one it took: `ENTIRE_GRAPH_DIR`, then
a checkout already on this machine (including the cache `install.sh` clones
into), then a shallow clone — into the sandbox, so it goes away with the rest of
the run.

`setup` renders a single in-place line — a braille spinner, a determinate bar
that colour-ramps red → amber → green as it fills, and a tick row of components
landing one by one — and every one of those is gated on the destination being a
terminal. **Pipe it and you get the deliberate degraded fallback**: whole lines,
ASCII, no colour. So this script never pipes, captures or tees the run, and
refuses to start if its stdout is not a terminal. To keep a transcript anyway,
give it a pty: `script -q /dev/null scripts/demo-setup.sh`.

It arranges the three things a bare `setup` in a scratch directory does not
have:

- **A repository with real substance.** This checkout is cloned (~1,200 tracked
  files, the full commit history), so the semantic index runs for about a minute
  instead of half a second and the repaints are actually visible.
- **A working `entire-graph` provider, installed *into the sandbox*.** Without
  it the semantic component fails and the run ends on a red ✗. It is found on
  this machine or cloned, never assumed to be sitting next to this checkout, and
  a candidate counts only when it really is `entire-graph`. `entire plugin
  install` honours the sandboxed `HOME`, so the provider is registered where the
  sandbox's `entire` looks for it and your real plugin registry is untouched.
  The demo clone is given a GitHub `origin` URL so the run models a real
  checkout rather than a scratch directory; a repository with no remote indexes
  just as green. Nothing is fetched from it.
- **Captured sessions, and a stub agent to distill them with.** The facts bar is
  `distilled/total sessions`; with no sessions it is an empty grey track that
  never moves. Sessions are seeded into the demo clone's checkpoint ref in the
  shape `entire` itself writes, then distilled in paced batches so the bar is
  watched filling and ramping instead of jumping from nothing to done.

**No tokens are spent.** The distill agent is a stub shell script that reads the
transcript on stdin and prints fixed fact lines; it never calls a model.

Sandboxing and the teardown leak-diff are the same as `trial-setup.sh` — `HOME`,
the four `XDG_*_HOME`, the four `ENTIRE_PLUGIN_*_DIR`, `ENTIRE_BRAIN_DAEMON_DIR`
and `ENTIRE_BRAIN_DAEMON_NO_REGISTER=1` — plus a check that `entire-graph` was
registered inside the sandbox rather than in your real plugin root.

```sh
DEMO_SESSIONS=60 scripts/demo-setup.sh              # seed more sessions
DEMO_BATCH=10 scripts/demo-setup.sh                 # coarser ramp, fewer passes
DEMO_PAUSE=0 scripts/demo-setup.sh                  # no pause between passes
DEMO_KEEP=1 scripts/demo-setup.sh                   # keep the sandbox to poke at
DEMO_GRAPH_CLONE=1 scripts/demo-setup.sh            # ignore local checkouts, always clone

ENTIRE_GRAPH_DIR=/path/to/entire-graph scripts/demo-setup.sh   # use this checkout
ENTIRE_GRAPH_REPO=<url> scripts/demo-setup.sh                  # clone from elsewhere
ENTIRE_INSTALL_OFFLINE=1 scripts/demo-setup.sh                 # never reach the network
```

### 3e. Proving it end to end: `scripts/demo-agent-session.sh`

`trial-setup.sh` answers *is this safe*. `demo-setup.sh` answers *what does it
look like*. This one answers **does the loop actually work** — and it is the only
one of the three that runs the real `entire` CLI.

The reason it exists is that `demo-setup.sh` writes checkpoint refs into its demo
repository with git plumbing. That proves `setup` can read a checkpoint ref, and
nothing else. The chain that *produces* one —

```
entire enable → an agent session runs → the session-end hook fires
              → a checkpoint ref is written → entire-brain setup finds it
              → distill → the brain answers
```

— is four components and three seams, none of them exercised by a synthesised
ref. Every bug this feature has shipped lived on one of those seams.

```sh
scripts/demo-agent-session.sh
```

It runs the whole chain in a sandbox and **asserts at each seam**, so it fails
loudly rather than printing a green summary over a broken link: `entire enable`
must leave a session-end hook behind, the session must leave a checkpoint ref
behind, `entire checkpoint list` must read it back, `setup`'s manifest must
report a non-zero session count, and `brief` must return *both* the exported
transcript and the fact distilled from it. Every step prints its command and its
exit code.

**What is real.** `entire enable`; the git hooks and agent hook settings it
installs; the host CLI's own lifecycle hook verbs (`entire hooks claude-code
session-start | user-prompt-submit | stop | session-end`); the session state
machine behind them; the post-commit git hook that writes the persistent
checkpoint; the checkpoint ref itself; `entire checkpoint list`; and then
`setup`, `distill`, `status`, `overview` and `brief`.

**What is substituted, and it is one thing: the model.** A live agent would call
an API and spend tokens, so the script writes the session transcript (a Claude
Code JSONL file) itself and hands it to the real hooks exactly as the agent host
would. The hook contract is a JSON object on stdin naming a transcript path, so
nothing downstream can tell the difference — but the sentences in that transcript
were typed by the script, not generated. **Zero tokens are spent, at any step**,
including distillation, which uses the same kind of stub agent `demo-setup.sh`
does.

Two things the run makes concrete that are easy to get wrong from the docs alone:

- **`entire checkpoint` cannot create a checkpoint.** It is `list`, `explain`,
  `tokens` and `search` only. The persistent checkpoint is written by the
  **post-commit git hook** `entire enable` installed, so committing the session's
  work is part of the loop rather than tidying up after it.
- **A freshly enabled repository uses `refs/entire/checkpoints/<shard>/<ULID>`,
  one ref per checkpoint.** The aggregate `refs/heads/entire/checkpoints/v1`
  branch that `demo-setup.sh` seeds is the legacy backend. Brain reads both; a
  demo that only writes the legacy layout is not demonstrating what a new user
  gets.

Sandboxing goes one variable further than `demo-setup.sh`: `ENTIRE_CONFIG_DIR`
on top of `HOME`, the four `XDG_*_HOME`, the four `ENTIRE_PLUGIN_*_DIR`,
`ENTIRE_BRAIN_DAEMON_DIR` and `ENTIRE_BRAIN_DAEMON_NO_REGISTER=1`. `entire
enable` is the one step here that writes outside the repository — git hooks,
`.entire/settings.json`, your agent's settings, and login contexts — so teardown
diffs your real Entire config store as well as launchd/systemd state.

The `entire-graph` provider is **optional** here, unlike in `demo-setup.sh`:
without it only the semantic component fails, and the session loop this script
exists to prove does not depend on it.

```sh
DEMO_KEEP=1 scripts/demo-agent-session.sh                            # keep the sandbox to poke at
DEMO_DIR=/path/to/scratch scripts/demo-agent-session.sh              # sandbox somewhere else
ENTIRE_GRAPH_DIR=/path/to/entire-graph scripts/demo-agent-session.sh # use this provider checkout
ENTIRE_BRAIN_BIN=/path/to/entire-brain scripts/demo-agent-session.sh # skip the build
```

### 3f. The whole journey, end to end

Every command a new user runs, in order, with what each one costs. Steps 1 and 4
are once per machine and once per repository; steps 5–7 are the loop you stay in.

```sh
# 1. once per machine — installs two plugins, no tokens, no services
git clone https://github.com/entireio/entire-brain.git
entire-brain/scripts/install.sh

# 2. once per repository — capture sessions, wire the agent's lifecycle hooks
cd /path/to/your/repo
entire enable --agent claude-code          # --agent makes it non-interactive;
                                           # no login and no network needed

# 3. commit what enable just wrote — setup will not index a dirty worktree
git add .entire .claude && git commit -m "chore: enable Entire session capture"

# 4. once per repository — build the brain, backfill facts, install the watcher
entire brain setup                         # --no-backfill --no-daemon spends
                                           # no model tokens; builds local memory

# 5. work — the hooks capture the session; your commit writes the checkpoint
#    (there is no command to run here; this is just using your agent)
git commit -m "..."

# 6. ask
entire brain overview
entire brain brief "<the task at hand>"

# 7. keep it current — automatic if you kept the watcher in step 4
entire brain setup                         # safe to re-run; resumes, never duplicates
```

| step | spends tokens? | installs a persistent service? |
|---|---|---|
| 1. `install.sh` | no | no |
| 2. `entire enable` | no | no — git hooks and agent settings in the repo only |
| 3. the commit | no | no |
| 4. `entire brain setup` | **yes**, phase 2 backfill unless `--no-backfill` | **yes**, unless `--no-daemon` |
| 5. the session-end hook | **yes**, once per session, once `entire enable` has wired it | no |
| 6. `overview` / `brief` | no | no |
| 7. the watcher | **yes**, one gated run per `--distill-every` window | already installed by step 4 |

Two of those spends are recurring and neither prompts: the watcher installed in
step 4, and the session-end hook wired in step 2, which distills each session as
it ends. `entire brain stop` pauses both machine-wide, along with new `setup`
backfills, until `entire brain start`; passes already running finish first. To
remove them instead, `entire brain setup --uninstall-daemon` removes the first
and `entire disable` in the repository removes the second.

Cloning a repository you don't have locally yet? `entire brain add <repo-url>`
clones it, fetches its Entire checkpoint history, and runs this same build in
one step; afterward explore the clone with `entire brain dash` or `search`.

### 4. Distill durable facts

Distillation is the egress-gated agent step that turns captured sessions into
durable project knowledge: decisions, constraints, preferences, gotchas,
conventions, and invariants.

```sh
entire brain distill --dry-run --json                                  # estimate cost first
entire brain distill --agent codex --model gpt-5.4-mini --effort low   # extract
entire brain facts status --json
entire brain facts tree --depth 1
```

Distillation sends redacted transcript chunks to the selected agent unless you
use a local loopback agent such as Ollama. It is incremental and cached:
unchanged sessions are skipped, near-duplicate facts are reconciled against the
branch's existing facts, and low-confidence merge/supersede decisions are queued
for `entire brain facts review`.

For active repos, keep the brain current with the watcher — deterministic
refreshes are free; token-spending work is opt-in and separately gated:

```sh
entire brain watch                                                                    # deterministic refresh only (NO tokens)
entire brain watch --distill --distill-every 24h --model gpt-5.4-mini --effort low
```

`--distill-every` plus the persisted watch cursor are the durable spend guard:
one gated agent run per repo per window, across restarts. `--max-sessions` caps
how many not-yet-distilled sessions ONE gated pass processes (0 = no cap), so
the frequency guard and the volume guard are separate knobs. `--budget` is a
different, weaker thing — it counts gated runs for the life of the **process**
and never resets, so on a long-lived daemon `--budget 1` means one run *ever*,
not one per window. Use it only for a bounded foreground run.

The watcher keeps memory fresh in two tiers, like a brain: on every tick it
runs the cheap **short-term** path (`entire brain refresh delta`; incremental
checkpoint export plus an overlay index of only the transcripts that changed;
seconds even on very large brains), so an in-flight session's earlier turns
and parallel terminals' work are searchable near-real-time. The change-gated
full refresh is **consolidation**: it absorbs the short-term overlay into
long-term memory (full index, FTS, vectors) and clears the buffer. Between
changes it is additionally throttled by `--consolidate-every` (default 30m);
the throttle is skipped whenever the delta was incomplete (a failed scan or a
full buffer), so partial short-term coverage always consolidates promptly. You
can run `entire brain refresh delta` by hand any time you want immediate
recall of just-captured work.

If you also want generated seed summaries, run refresh with an agent instead of
the deterministic seed path (`entire brain refresh --agent auto --seed-model
gpt-5.4-mini --seed-effort low`). Seed synthesis orients an agent on the
repository; durable facts are branch-scoped retained knowledge — the two are
separate.

## How Agents Use The Brain

Agents rely on a capture layer plus four consumption surfaces. The capture layer
produces the raw material; the consumption surfaces are how an agent actually
queries or receives brain context. Which surface matters depends on the agent
client and how the repository is configured.

### Capture layer: Entire hooks produce the raw material

`entire-cli` installs lifecycle hooks for agents such as Claude Code, Codex,
Gemini CLI, OpenCode, Cursor, Factory AI Droid, Copilot CLI, and Pi. Those hooks
start and stop sessions, capture prompts and transcripts, track changed files,
record subagent work where the agent exposes it, and attach checkpoint metadata
to commits. This is background infrastructure: the agent does not remember to
save a transcript, and the human does not paste context into the brain.

If an agent is not seeing Entire context at all, check the base Entire
integration first, not the brain (`entire status`, `entire agent`).

### 1. MCP is the preferred agent tool surface

For agents that support MCP, register `entire brain mcp` as a local stdio MCP
server. It opens no network listener and wraps the local brain's JSON contracts
as tools. Once MCP is available, an agent should call MCP tools instead of
shelling out to the CLI. The normal first call is `brain_brief`, then targeted
follow-ups:

- `brain_brief` for task-shaped context, history hits, likely files, and tests
- `brain_status` for a compact freshness/coverage preflight; set
  `details: true` for the full status JSON contract
- `brain_refresh` for a bounded seed/docs refresh when retrieval freshness is
  unsafe. Its seed and docs include the current worktree by default, it never
  exports checkpoint sessions, and it is capped at 60 seconds; set
  `semantic: true` only for small repositories and use
  `brain_index_repository` as the separate long-running semantic step for large
  repositories. `worktree` covers seed and docs only -- the semantic index is
  always built from committed HEAD, so a dirty semantic snapshot needs
  `brain_index_repository` with `worktree: true`. Set `worktree: false` only
  when seed and docs must come from committed
  HEAD; use `entire brain refresh sessions` from the CLI for checkpoint history
- retrieval tools such as `brain_query` and `brain_get` for facts, docs, history
- semantic tools such as `brain_code`, `brain_context`, `brain_impact`, and
  `brain_tests` for code navigation and validation planning
- `brain_entity_history` for "which checkpoints and sessions changed this
  function/class", answered from the persisted entity index rather than by
  re-reading history (build it once with `entire brain entities backfill`)
- review, workspace, and pattern tools such as `brain_regressions`,
  `brain_workspace_graph`, `brain_workspace_review`, and `brain_patterns` when
  the task calls for them

Some MCP tools write local state (for example `brain_ingest_traces` persists
runtime edges as `RUNTIME_TRACE` facts); pattern MCP tools are read-only. The
MCP and CLI surfaces are intentionally close but not perfect mirrors — prefer the
native surface your agent client has, then translate only when needed. See
[docs/semantic_mcp_guide.md](semantic_mcp_guide.md) for the full tool surface.

### 2. Intake instructions are the file-based fallback

When MCP is not available, an agent can use the brain through the intake
instructions in `templates/`. The templates tell the agent to locate the brain
with `entire brain path "$PWD"`, then read staged context instead of blindly
opening everything: `README.md` and `manifest.json` in the brain directory, seed
files under `seed/agent/`, `seed/history-gaps.md` when present, and only
task-relevant transcripts selected from the manifest. Treat gaps as uncertainty
rather than inventing rationale.

### 3. Direct CLI is a compatibility surface

For clients without MCP, direct `entire brain ... --json` calls are the
compatibility surface (humans and scripts use the same commands). Treat it like a
tool API, not an invitation to run a long command tour:

The provider naming migration is an intentional compatibility break. Direct CLI
automation must use `entire graph`, `--graph-binary`, `--skip-graph`,
`--graph-timeout`, and `--graph-inactivity-timeout`. MCP server deployments use
`ENTIRE_BRAIN_GRAPH_BINARY` for the trusted provider-binary override. Aliases for
prior provider-facing names are not supported.

```sh
entire brain status --json
entire brain brief "<task>" --json
# then the smallest next question:
entire brain query "<decision or prior-work question>" --json
entire brain inspect code "<symbol or concept>" --json
entire brain inspect tests "<symbol-or-id>" --json
entire brain inspect regressions "<task or invariant>" --location-only --json
```

`status` without `--json` prints a SHORT human report: the brain's identity,
the onboarding block (fact-backfill progress, watcher health, instant-phase
components) and a one-line health verdict. `status --verbose` prints the full
text report — coverage histograms, freshness axes, blind spots, live state —
with repeated warnings collapsed to one line per reason; `entire brain doctor`
lists the skipped files in full. `status --json` is unchanged by either flag.

`status --json` preserves the full status contract. Check
`semantic.freshness.severity` before graph inspection and
`retrieval.freshness.severity` before query/get. A semantic-only
`refresh index` does not rebuild seed/docs: use `entire brain refresh --agent
none` when retrieval is stale, adding `--worktree` only when current
uncommitted content should be included. `status --json --details` remains an
accepted compatibility spelling for callers that already use it.
`inspect code --json`, `inspect context --json`, `inspect impact --json`, and
`inspect tests --json` likewise return compact semantic records by default; add
`--details` only when provider metadata is needed. Their defaults are 10 code
results, 5 context symbols, 20 impact symbols, and 3 test suggestions;
`--limit` remains available for deliberate expansion.

Prefer `query` for broad facts/history/docs, `search` for exact terms, semantic
`inspect` subcommands for code-graph questions, and `get`/`multi-get` when a
prior result returned an id. Retrieval returns ten bounded excerpts by default;
raise `--limit` deliberately instead of treating ranked search as a full-record
dump. Don't broaden into repo-wide text search until the brain's targeted
context has been used.

### 4. Hidden hooks deliver context at the moment of relevance

`entire-brain` also has a hidden hook surface for harness integrations. These are
not installed by base Entire enablement; a harness must be configured to invoke
them (for example Claude Code `PreToolUse`/`PostToolUse` hooks). They surface a
small, high-confidence fact exactly when it matters — facts anchored to a file
before editing it, gotchas after a failed command, optional one-session refresh
after a session ends. The contract is intentionally quiet: with no relevant
fact, no local brain, or an environment problem, the hook exits successfully with
no stdout output, so it never breaks the agent workflow.

## Usage Scenarios

The examples name the preferred MCP tool when there is one. Agent clients without
MCP should use the equivalent `entire brain ... --json` command.

### Orient an agent on a new task

Start by asking for task-shaped context, not a generic summary. The normal tool
is `brain_brief` — it returns freshness, likely files and symbols, relevant
history, durable facts, likely tests, and blind spots. Use it before broad shell
search. For repo-level orientation instead of task-specific, use `overview`
(`entire brain overview --json`): a compact project map of stack signals,
entrypoints, commands, key documents, and recent decisions.

For interactive human exploration instead of scripted queries, `entire brain
dash` opens a terminal dashboard over facts, sessions, history, and semantic
records, and `entire brain viz` opens a local, no-egress browser view of the
semantic call graph. Both are read-only and make no agent calls.

### Keep the brain fresh

Freshness is part of every answer — a stale index, missing provider, dirty-
worktree mismatch, or parser blind spot changes how much an agent should trust
the brain. Agents check freshness through `brain_status`/`status --json` before
relying on semantic answers; if unsafe, an agent with write permission can
request a refresh, otherwise it reports the limitation and falls back to direct
inspection.

```sh
entire brain status
entire brain refresh --agent none
entire brain watch
```

When semantic artifacts are out of sync or damaged, use the maintenance paths
instead of deleting the whole brain (`refresh index`, `repair`, `reset
--semantic-only --force`). Use `refresh index --worktree` only when you
intentionally want uncommitted state indexed; exported bundles reject
worktree-backed semantic indexes.

### Continue or explain prior work

When the question is "why is this like this?", "what did the previous agent
try?", or "where did this session leave off?", search the history and facts
layers — `brain_query`, then `brain_get` for specific ids. This is
the main reason Entire capture matters: the original prompt, attempts,
validation, correction, and rationale survive the code diff and become available
to the next agent.

### Ask across facts, history, and docs

When the question is not tied to one symbol, use the unified retrieval layer.
`query` is the hybrid path (lexical + vector, RRF) over durable facts, indexed
history, and docs; `query --keyword` for exact keywords, `query --semantic` for semantic matches,
`get`/`multi-get` when a result returns an id. Every result carries an `id`.

```sh
entire brain query "how does checkpointing work" --json
entire brain query --keyword "checkpoint" --json
entire brain query --semantic "preventing data races" --json
entire brain get fact:<id> --json
```

`query` also takes `--source` (`all` | `fact` |
`history` | `conversation` | `doc`) to restrict retrieval to one layer. The
default is unchanged (`all` = facts + classified history + docs).

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

### Recall prior conversations (experimental, opt-in)

`--source conversation` searches captured request/response exchanges from
exported session transcripts (what was asked, what the agent concluded), and
`get conversation:<id>` expands one exchange to a bounded request/response pair
with its exact transcript range:

```sh
entire brain query "why did we reject the cache rewrite" --source conversation --json
entire brain query --keyword "SQLITE_BUSY" --source conversation --json
entire brain get conversation:<id> --json
```

Conversation queries take structured filters: `--after`/`--before` (RFC3339 or
YYYY-MM-DD session time), `--session <id>`, `--agent <harness>`, and `--branch`.
These error on any other source rather than being silently ignored. Results
carry `matched_terms` (which query tokens actually hit) and are diversity-capped
so one long session cannot crowd out every other trajectory; filtering to a
session lifts the cap. Re-exported duplicate sessions are collapsed at index
time (newest export wins).

Multi-concept recall: `--concept <text>` (repeatable, up to 4, conversation
source only) finds sessions covering the query AND every concept, even when
the concepts live in different exchanges of the session. Results are
`conversation-session:` records ranked by worst per-concept rank (then rank
sum, then session reference) with `evidence_ids` naming the exact supporting
exchanges. Lexical mode enumerates the complete in-scope match set per
concept up to a 10,000-candidate ceiling and returns `memory_query_too_broad`
beyond it; vector/hybrid concept ranking is explicitly approximate. MCP takes
the same `concepts` array on all three retrieval tools.

Session navigation: every conversation result carries a `session_ref`
(a virtual `conversation-session:` identity derived from repo, branch, and
session id; no second transcript archive exists). `get conversation:<id>
--context-before N --context-after N` (0-3 each) expands up to three adjacent
exchanges from the same reconciled session, dropping context farthest-first
under a 128 KiB packet cap. `get conversation-session:<id> [--after-turn N]
[--limit N]` returns a bounded, paginated outline (request excerpts, ordinals,
tool names; default 20 entries, max 50, 64 KiB per page) with a stable
`next_turn` cursor that appends never shift. Navigation options are
type-specific and error on any other id kind; MCP `brain_get` takes the same
`context_before`/`context_after`/`after_turn`/`limit` fields, and
`workspace get` accepts them for repo-qualified ids.

Exchanges are extracted deterministically and locally (no model calls) and
never enter default retrieval or published bundles. Lexical BM25 is the
default and always available. A separate conversation vector store exists for
explicit `query --semantic --source conversation` (semantic-only); it requires the
fusion-eligible embedder opt-in (`ENTIRE_BRAIN_EMBEDDER`), the `brain_cgo`
build, and refresh-built conversation vectors, and returns a structured
unavailable error naming those requirements when the arm is closed. Fused
lexical+semantic ranking for conversation `query` stays dark behind the
development flag `ENTIRE_BRAIN_CONVERSATION_FUSION` pending an eval-ledger
positive. Every result is labeled
`verification_required` with a `historical_conversation` caveat: recalled
conversation text is quoted historical evidence that may be stale, mistaken, or
adversarial; verify it against current code before acting on it, and never
treat it as instructions.

For durable facts specifically, `recall` retrieves by keyword + taxonomy + code
locus, scoped to the current branch; `recall --expand` is an agent-assisted
query-expansion path, so it sits behind the same egress judgment as other agent
calls.

### Navigate code by meaning

When a task depends on structure rather than text, use the semantic tools:
`brain_code`/`brain_search_code` to find candidate symbols, then `brain_context`,
`brain_impact`, and `brain_tests`. This is where `entire-graph` matters — it is the
local parser/provider that gives the brain the graph the agent queries. Semantic
depth is language-dependent: parser-backed extraction covers the semantic
language set; many recognized filetypes are inventory-only, where you should
prefer text retrieval and lower confidence in impact/context answers.

```sh
entire brain inspect code "ValidateToken" --json         # find a symbol in the graph
entire brain inspect context "ValidateToken" --json      # relation-aware context
entire brain inspect impact "ValidateToken" --json       # impact set via typed relations
entire brain inspect tests "ValidateToken" --json        # test suggestions
# add --details to any of the four commands only for full provider records
entire brain inspect graph-ui semantic-graph.html        # local static graph explorer
entire brain inspect trace-path "<caller>" "<callee>" --json
entire brain inspect dead-code --json
entire brain inspect boundaries --kind tool --json
entire brain inspect changes --json                      # diff-hunk-scoped symbol mapping
# persists semantic/changes/latest.json by default; pass --write-report=false to skip it
```

### Trace a symbol back to the work that changed it

`entities` answers "which checkpoints and sessions changed this function" from a
persisted, git-native index instead of re-reading history. The index is built
once per repository and then kept current by the deterministic (token-free)
refresh that the watch loop and the `session-end` hook already run.

Each indexed commit stores its semantic delta document (`schema_version` `1.0`,
produced by `entire graph diff`) as a git-meta record on the commit, plus a
reverse `entity -> commits` list and a rename/move alias, so a symbol's history
survives the names it has been through. The local join of commits to checkpoints
and sessions is derived state, pinned to the git-meta ref and rebuilt whenever
the index moves.

Coverage is tracked as a **window**, not a mark: a per-branch `floor..tip` range
in which every first-parent commit is indexed. Freshness ticks push the tip
forward over commits that landed since (so a tick reads only the new ones), and
bounded `--limit` passes pull the floor backward until it reaches the root.

```sh
entire brain entities backfill                 # index history (--limit 0 for all)
entire brain entities backfill --checkpoints-only
entire brain entities history "ValidateToken" --json
entire brain entities history "ValidateToken" --branch main
entire brain entities show <commit|checkpoint-id>   # the stored delta document
```

`--checkpoints-only` is a filtered convenience pass: it indexes commits carrying
an `Entire-Checkpoint` trailer and deliberately leaves plain commits alone, so it
does **not** move the window. Coverage of a branch always comes from an ordinary
pass.

Distill uses the same index to sharpen fact provenance: a fact whose locus names
an indexed entity is anchored to the checkpoint of ITS OWN session that actually
changed that entity, rather than to the session's last checkpoint. With no index,
no match, an ambiguous one, or a candidate from another session or branch, the
previous anchor is kept unchanged.

### Review risk without a clean diff

For long sessions, manual edits, or suspected regressions from memory, use
diff-less regression review — `brain_regressions`/`brain_review`. It compares the
current working tree against what session history asserts the code used to be and
flags suspected regressions (`file:line`, expected vs current, confidence,
provenance). Prefer `--location-only` first for a fair investigation: it points
to suspected files and lines without handing over the expected answer text.
`entire brain review --json` emits a versioned `reviewReport` contract; see
[docs/diffless_review_seam.md](diffless_review_seam.md).

### Connect runtime evidence to source

When static structure is not enough, import runtime trace edges with
`brain_ingest_traces` / `inspect ingest-traces`. The brain compares runtime edges
with known static relations, persists them as `RUNTIME_TRACE` graph facts, and
makes them available to trace-path and brief/context flows —
useful for incidents and performance work where "what actually happened?" matters
more than "what could call this?"

### Work across multiple repositories

Use workspaces when behavior crosses repository boundaries. Humans create and
refresh the workspace; agents consume it through MCP workspace tools or direct
workspace CLI commands. The workspace brain keeps member repos local and builds
cross-repo contracts and graph edges from their existing local brains.

```sh
entire brain workspace create platform
entire brain workspace add platform ../api --name api
entire brain workspace add platform ../web --name web
entire brain workspace refresh platform --full
entire brain workspace inspect context platform "checkout" --json
entire brain workspace inspect impact platform "checkout" --json
entire brain workspace review platform "checkout regression" --json
```

### Preserve durable project knowledge

Durable facts are short, provenance-anchored statements about decisions,
constraints, preferences, pitfalls, and standing rules. Humans write them
directly when they know the rule; agents distill candidates from captured
sessions when explicitly asked. Facts have a lifecycle: review queued
merge/supersede proposals, promote branch facts that should carry forward,
retract facts that are no longer true, and garbage-collect stale retractions.

```sh
entire brain remember "Prefer table-driven tests for parser edge cases" --path preferences.tests
entire brain distill --agent codex --model gpt-5.4-mini --effort low
entire brain facts review
entire brain facts promote --from <branch> --strategy keep-both
entire brain facts retract <fact-id>
entire brain facts gc --force
entire brain inspect blame <fact-id> --json
```

### Extract reusable agent skills

Skill extraction forms skills only from accepted deep dossiers: verified evidence
that a pattern is non-obvious and useful to future agents (recurring task
patterns, verified themes, corrected/failed episodes, durable gotchas and
conventions, cross-repo families). A human or explicitly authorized automation
runs the egress-gated verification and formation; refresh, watch, brief, query,
MCP reads, and workspace reads do not silently call it.

```sh
entire brain patterns
entire brain patterns verify --deep
entire brain patterns skills            # list formable proposals
entire brain patterns skills form <id>  # preview evidence + generated SKILL.md
entire brain patterns skills form <id> --yes
```

`form <id>` previews the evidence, generated `SKILL.md`, and destination without
writing; if no accepted deep dossier exists it refuses and tells you to verify
first (it never falls back to shallow evidence). `--yes` writes the skill;
existing files require `--force`. Generated skills target the cross-agent
`standard` destination (`.agents/skills/`) by default; use
`--target claude-code|codex|factoryai-droid|all` for an agent-specific
destination.

### Measure brain quality

The brain includes evaluation harnesses for maintainers who need evidence that a
retriever or fact layer is actually helping — not required for everyday use, but
the right surface before claiming a quality lift.

```sh
entire brain facts eval-gen > facts-tasks.json
entire brain facts eval --tasks facts-tasks.json --retriever facts --json > facts-eval.json
entire brain facts eval-compare --a <before.json> --b <after.json>
entire brain bench semantic .
entire brain status --json --details
```

`facts eval-compare` runs a paired t-test with Holm correction and rejects
non-proof or mismatched relevance sources unless `--allow-proxy-comparison` is
explicit. Treat the `semantic` section of `status --json --details` as audit
evidence for the reported provider output (`status --fail-on release` is the
compact CI gate form), not a global coverage claim; public semantic claims
should name the covered languages, relation types, freshness state, and
benchmark records behind them.

### Tune retrieval

The default vector arm uses a bundled, pure-Go Model2Vec static model — zero
config, offline. For higher semantic recall, opt into a transformer embedder
(EmbeddingGemma-300M) served by Ollama or a compatible loopback endpoint:

```sh
ollama pull embeddinggemma
ENTIRE_BRAIN_EMBEDDER=ollama entire brain query "preventing data races" --json
```

It measured about +14% useful-facts-per-1k-tokens over Model2Vec on the facts
eval, driven mostly by reachability (it surfaces conceptually related facts that
share no query term). The embedder is chosen once, on first use: if the opt-in is
set but the server does not return an embedding, the brain falls back to
Model2Vec with a one-line stderr notice. Switching embedders re-namespaces the
vector cache, so the two never mix. Changing the embedder changes retrieval
behavior, not the underlying source of truth.

## Global Facts

A fact normally belongs to a repository and a branch. That is right for a claim
about one codebase and wrong for the rest: "I prefer table-driven tests", "we
deploy on Thursdays", "the staging cluster is in eu-west-1" are true everywhere,
and recording them in one repo's brain makes them invisible from the next.

```sh
entire brain remember "We deploy on Thursdays." --global
entire brain facts global                     # list them
entire brain facts global --all --json
entire brain facts retract <fact-id> --global
```

An MCP-connected agent records one the same way, through `brain_remember`:

```json
{"name": "brain_remember", "arguments": {
  "fact": "We deploy on Thursdays.",
  "path": "project.deployment.convention",
  "global": true
}}
```

`global` cannot be combined with `branch`: a fact that applies everywhere has no
branch to belong to.

Global facts live in their own store beside the per-repo ones — `global/` next
to `repos/` under the plugin data directory, which `entire brain facts global`
prints. They are ordinary facts: the same format, the same taxonomy, the same
retraction and garbage collection.

### How they surface

Every read surface merges them into the repository's results: `recall`,
`query`, `search`, `vsearch` and `brief` on the CLI, and `brain_query`,
`brain_search`, `brain_vsearch` and `brain_brief` over MCP. `brief` surfaces
them even in a repository that has no facts of its own — a repo that has never
been told the statement is the case global facts exist for. They go through the
same guard, filters and ranking as repository facts, and are labelled:

```
fact:76aa6fd6 decision [architecture.deployment.convention] (global)
  The staging cluster is in eu-west-1.
```

The label matters: acting on a general convention as though this repository had
declared it is a different thing, and an agent reading unlabelled output cannot
tell the difference. `recall` and `brief` report the same distinction as
`global_fact_ids` in JSON. Unified `query`, `search`, `vsearch`, and `get` results
include `(global)` in each global fact's heading.

Where a repository records the same statement, the repository's copy wins —
fact ids are content-derived, so it is the same fact, and the local one carries
that repository's provenance, which is the evidence somebody would check.

Only a live repository fact wins that way. Once the local copy is retracted or
superseded it makes no claim, so the global statement becomes visible again
rather than staying hidden behind a fact nobody can read: retracting a local
duplicate falls back to the global one instead of silently losing it in that
repository.

### Turning them off

Use `--no-global` on a single `recall`, `search`, `query`, or `vsearch` request,
or `no_global: true` with `brain_search`, `brain_query`, or `brain_vsearch` over MCP.
Set `ENTIRE_BRAIN_NO_GLOBAL_FACTS=1` for a
machine where repository answers must not be influenced by anything outside the
repository. The variable is fail-closed like the other guards: an unrecognised
value disables global facts rather than leaving them on.

### Outside a repository

`remember` used to fail outside a checkout. It now records the fact globally and
says so:

```
not in a repository, so this was remembered globally; it will be recalled from every repo
```

This is decided by asking git whether the directory is a working tree, not by
whether a path resolves — any existing directory resolves, and the branch then
defaults to `main`, which would file the fact against a repository that does not
exist. Passing `--branch` still requires a repository: that names a specific
branch, and filing it globally would be a different write than the one asked for.

### What a global fact does not have

A commit anchor. There is no repository to cite, and an anchor pointing at
whatever happened to be checked out would be worse than none — `verify` would
resolve it and report the fact as evidenced by a commit it has nothing to do
with. `--global` and `--branch` cannot be combined for the same reason.

Note that multi-repository *code* analysis is a separate feature: see
`entire brain workspace` for brains spanning several repositories at once.
## Documents

Brain indexes markdown from `docs/` and, alongside it, the documents that are
not code: PDF, Word, Excel and PowerPoint. A design doc somebody wrote in Word
and a spec that arrived as a PDF are project knowledge exactly like a `.md`
file, and were previously invisible to every query.

```sh
entire brain docs extract docs/spec.pdf   # the text it gets out of one file
entire brain docs extract docs/spec.pdf --json
```

Extraction runs during `refresh`, with the rest of the seed. It is fully
deterministic and local — no model, no network, no external binaries — so it
costs no tokens and works offline.

### What is read

| Format | Read | Not read |
| --- | --- | --- |
| `.pdf` | the text layer, page by page | scans (no text layer), encrypted files |
| `.docx` | body, footnotes, endnotes; tables as tab-separated rows | headers and footers, which repeat on every page |
| `.xlsx` | every sheet, labelled with its own name; shared strings resolved | charts, formulas (the computed value is read) |
| `.pptx` | slides in deck order, then speaker notes | slide masters and layouts |

Legacy `.doc`, `.xls` and `.ppt` are a different format (OLE compound files),
not a variant of the above, and are not read. Images are not read: that needs
OCR, which would mean shipping a native dependency.

### Where they are read from

`docs/`, the same place markdown is taken from — not the whole tree. A `.xlsx`
under test fixtures or a vendored PDF manual is data rather than documentation,
and indexing those would bury the documents somebody meant to be read.

Extracted text is written into the brain as `seed/docs/<path>.md` with a header
naming the source, so it is inspectable as plain text and it is obvious the file
is derived. If that path belongs to an existing source file or directory, more
`.md` suffixes are added until the derived path is free. The original document
is never modified.

### When a document cannot be read

Brain reports it and continues; the refresh still succeeds. The reason lands in
the seed's file index and in the refresh warnings:

```
warning: document not indexed: docs/scanned-form.pdf: the PDF has no text layer
  (it is probably a scan; OCR is out of scope for this build)
```

Nothing is written for that document. An empty markdown file in the seed would
be indexed as a document that exists and says nothing, which is worse than an
absence somebody can see.

Three failures are reported rather than guessed at, because each would otherwise
produce a plausible-looking result that is wrong:

- **A scan** has no text layer at all. Indexing it as an empty document would
  count as a successful ingest.
- **An encrypted PDF** cannot be read without its key.
- **A font this build cannot map.** Modern PDFs embed subset fonts whose bytes
  are glyph indices rather than characters; Brain decodes them through the
  font's `/ToUnicode` map, and where that is missing or unusable it refuses
  rather than emit text that reads like language and matches no query.

### Untrusted input

A document is untrusted input and is treated as such: file size, decompressed
size, and archive entry count are all capped before parsing starts, so a
compression bomb costs a bounded read. Extracted text is stripped of control
characters and is never executed or interpreted — it is indexed as text, and the
[recall threat model](recall_threat_model.md) covers how retrieved content is
handled from there.

## Webhooks

A brain's knowledge is only visible to whoever runs a command against it.
Webhooks let something else react instead: a CI job that re-runs when a new
invariant is recorded, a channel that shows decisions as they are made, an index
that rebuilds when the brain refreshes.

Point it at an endpoint and it is on:

```sh
export ENTIRE_BRAIN_WEBHOOK_URL=https://hooks.example.com/entire
export ENTIRE_BRAIN_WEBHOOK_SECRET=a-long-random-string   # recommended
entire brain webhook test        # prove the endpoint works before trusting it
entire brain webhook status      # what is on, and why it is off when it is
entire brain webhook events      # what this brain sends
```

Unset `ENTIRE_BRAIN_WEBHOOK_URL` and nothing is sent and no connection is made.
There is no config file and no default endpoint.

### Events

| Event | Sent when |
| --- | --- |
| `fact.recorded` | `remember` records a durable fact |
| `fact.retracted` | `facts retract` marks a fact no longer true |
| `brain.refreshed` | `refresh` completes |
| `webhook.test` | only by `webhook test` |

A `fact.recorded` POST looks like this:

```json
{
  "event": "fact.recorded",
  "timestamp": "2026-09-21T10:00:00Z",
  "repo": "entire-brain",
  "branch": "main",
  "fact": {
    "id": "b3f1c2d4",
    "kind": "invariant",
    "paths": ["architecture.storage.invariant"]
  }
}
```

### What is and is not sent

**Fact text is withheld by default.** The payload carries the id, kind and
taxonomy paths — enough to trigger work, not enough to turn one environment
variable into a feed of everything the brain has learned. Set
`ENTIRE_BRAIN_WEBHOOK_INCLUDE_TEXT=1` to add a `text` field when you want the
fact itself in your channel.

`repo` is the repository's directory name, never its path, so an event does not
disclose the layout of your filesystem.

Global fact events carry `"global": true` and omit `repo` and `branch`: the
current checkout does not own a global fact. The same text-redaction and
no-egress settings apply to both scopes.

### Verifying the sender

With `ENTIRE_BRAIN_WEBHOOK_SECRET` set, each POST carries
`X-Entire-Signature-256: sha256=<hex>`, the HMAC-SHA256 of the exact request
body. Compute the same over the raw body and compare in constant time:

```python
import hashlib, hmac
expected = "sha256=" + hmac.new(secret.encode(), raw_body, hashlib.sha256).hexdigest()
if not hmac.compare_digest(expected, request.headers["X-Entire-Signature-256"]):
    abort(401)
```

Without a secret no signature header is sent at all, rather than one computed
over an empty key — which would verify for anyone who guessed the key was empty.

### Limits worth knowing

- **Redirects are refused, not followed.** A 307 preserves the method and body,
  so following one would deliver the payload and its signature to a host you did
  not configure. Point `ENTIRE_BRAIN_WEBHOOK_URL` at the final URL.
- **`http` and `https` only.** Other schemes are rejected.
- **Delivery never fails the command.** If the endpoint is down, the fact is
  still recorded and a warning goes to stderr. There is no retry queue: this is
  a notification channel, not a delivery guarantee. Treat a missed event as
  possible and re-read state through `/v1/facts` or the CLI when it matters.
- **`ENTIRE_BRAIN_NO_EGRESS` and `ENTIRE_BRAIN_LOCAL_ONLY` win.** Either one
  silences webhooks whatever else is configured.

## Privacy And Egress

Default brain artifacts are local and inspectable. Deterministic refresh,
semantic indexing, local history indexing, and MCP tool calls do not require a
hosted model, and the MCP adapter is stdio-only.

Some operations perform network egress, either by sending selected context to a
model or by fetching over the network:

- a configured `checkpoint_remote` lets `refresh` (and its `sessions` export
  stage) `git fetch` checkpoint history into a throwaway temp repo — network
  egress, not a hosted-model call, skipped under no-egress mode
- seed synthesis during `refresh --agent auto`
- fact distillation with `distill --agent ...`
- query expansion with `recall --expand`
- pattern verification and skill synthesis
- explicitly configured session-abstract generation
- judged evaluation commands
- `publish` uploads the serialized local brain (manifest, semantic snapshots,
  branch overlays, durable facts) to hosted Entire; opt-in and off by default,
  requiring both the command and `ENTIRE_BRAIN_ALLOW_HOSTED=1`
- webhooks POST a small JSON event to `ENTIRE_BRAIN_WEBHOOK_URL` when a fact is
  recorded or retracted or the brain is refreshed; off until that variable is
  set. The payload carries the fact id, kind and taxonomy paths and the
  repository's directory name — not its path, and not the fact text unless
  `ENTIRE_BRAIN_WEBHOOK_INCLUDE_TEXT` is set. Redirects are refused rather than
  followed, so the payload cannot be delivered to a host you did not configure.
  See [Webhooks](#webhooks).

Use `--agent none`, `--dry-run`, local loopback Ollama, or
`ENTIRE_BRAIN_NO_EGRESS=1` / `ENTIRE_BRAIN_LOCAL_ONLY=1` when the repo must stay
strictly local. No-egress mode enforces locality for no-agent, dry-run, and
loopback-Ollama paths by validating URLs, redirects, and resolved dial targets. A
custom `--agent command` runner is a trusted local command and is not enforceably
loopback-only; no-egress mode cannot stop that runner from making its own network
calls.

To keep specific sessions out of the brain's projections, use
`entire brain privacy list|exclude|include|purge`. `exclude` tombstones a
session so every derived layer (history records, conversation exchanges,
pattern episodes and corpus, FTS, vector stores, caches) skips it on rebuild
while keeping the exported transcript; `purge` additionally deletes the
exported transcript copy and the derived stores, removes durable facts whose
only provenance is the purged session (facts corroborated by other sessions
keep their remaining anchors), filters its pattern episodes, and clears its
distill-cache entries (`--dry-run` reports exactly what would be removed
first). Skill-memory (your accept/decline curation) is never touched.
`privacy verify` proves excluded/purged sessions are absent from every
inspectable projection and from their lifecycle jobs, cancellation markers,
optional abstracts, and metadata-only egress receipts (exit non-zero with named
violations otherwise; a re-purge repairs them), and `privacy retention
--max-age <dur> [--branch b]
[--purge] [--dry-run]` applies an age-based policy in one command. The full
prompt-injection and secret-retention threat model lives in
[docs/recall_threat_model.md](recall_threat_model.md).
Tombstones are brain-local and survive re-export: a purged session that the
capture layer re-exports stays un-indexed until an explicit `include`. Note the
canonical capture in Entire's configured checkpoint backend is the capture
layer's data; purging the brain does not rewrite checkpoint history.

Remember that base Entire session capture stores transcripts and metadata on the
repository's Entire-managed per-checkpoint refs (or the legacy
`entire/checkpoints/v1` branch) — anyone with access to those refs can read
captured prompts, tool activity, and retained transcript data. Entire
redacts detected secrets before writing checkpoint metadata, but redaction is
best-effort and does not cover every local working artifact. Entire also writes
temporary shadow branches such as `entire/<short-hash>` whose code-file snapshots
are raw, unredacted blobs of the working tree; Entire does not push shadow
branches — do not push them manually, or unredacted source could reach the
remote. Review the Entire CLI security and privacy guide before enabling Entire
on sensitive or public repositories.

## Storage And Configuration

The parent Entire CLI supplies the plugin directories that make the brain durable:

| Variable | Purpose |
|---|---|
| `ENTIRE_PLUGIN_CONFIG_DIR` | Plugin config, including `brain.json`. |
| `ENTIRE_PLUGIN_DATA_DIR` | Durable brains under `repos/<repo-key>/`; workspaces under `workspaces/<name>/`. |
| `ENTIRE_PLUGIN_STATE_DIR` | Regenerable cursors under `repos/<repo-key>/` (for example watcher state). |
| `ENTIRE_PLUGIN_CACHE_DIR` | Cache data. |
| `ENTIRE_REPO_ROOT` | Current git checkout when invoked inside a repo. |

Repo keys are derived from the repository origin. For example,
`github.com/entireio/cli` becomes `gh/entireio/cli`.

### Environment toggles

Optional `ENTIRE_BRAIN_*` variables tune retrieval and diagnostics. None are
required for normal use.

| Variable | Default | Effect |
|---|---|---|
| `ENTIRE_BRAIN_EMBEDDER` | (unset → Model2Vec) | Set to `ollama` for the transformer embedder (EmbeddingGemma) on the vector arm. Falls back to Model2Vec with a stderr notice if a one-time startup probe finds the server returns no embedding. |
| `ENTIRE_BRAIN_OLLAMA_MODEL` | `embeddinggemma` | Model requested from the embed server when `ENTIRE_BRAIN_EMBEDDER=ollama`. |
| `ENTIRE_BRAIN_EMBED_URL` | `http://localhost:11434/api/embed` | Embed endpoint (Ollama, or qmd's node-llama-cpp server). Must accept `{"model","input"}` and return `{"embeddings":[[…]]}`. |
| `ENTIRE_BRAIN_FACTS_BM25` | (unset → token-overlap) | `1`/`true`/`yes`/`on` switches the facts lexical arm to FTS5 BM25. Experimental; measured at parity, kept for A/B'ing the lexical engine. |
| `ENTIRE_BRAIN_ACTION_CHECKLIST` | disabled | Set to `1`/`true`/`yes`/`on` to render high-confidence production-symbol evidence as an inspection action. Trusted symbol and directly associated test evidence narrow the normal brief in either mode; the flag changes only the action rendering. Intended for controlled agent ablations until stable lift is demonstrated. |
| `ENTIRE_BRAIN_CONVERSATION_FUSION` | disabled | Development flag: fuse conversation BM25 with calibrated exchange vectors in `query --source conversation`. Dark pending an eval-ledger positive (see `docs/eval_ledger.md`); lexical ranking is the shipped default. |
| `ENTIRE_BRAIN_BRIEF_CONVERSATION` | disabled | Development flag: include conversation hits in `brain_brief`. Off until qualified for the compact budget. |
| `ENTIRE_BRAIN_NO_EGRESS` / `ENTIRE_BRAIN_LOCAL_ONLY` | (unset) | Strict local-only mode; enforces locality for no-agent, dry-run, and loopback-Ollama paths. |
| `ENTIRE_BRAIN_ALLOW_INSECURE_API_URL` | (unset) | Allows a plaintext `http://` `--api-url`/`ENTIRE_API_URL` pointing at a **non-loopback** host. Off by default and fail-closed (only `1`/`true`/`yes`/`on`/`enable`/`enabled` enables it): hosted publish, facts sync, and the hosted proposal queue otherwise require `https://`, since they send brain content and the API bearer token. `http://` to a loopback host (`127.0.0.0/8`, `::1`, `localhost`) never needs this. |
| `ENTIRE_BRAIN_MCP_DEBUG_LOG` | (unset) | Path the stdio MCP adapter appends frame-level debug lines to. Diagnostics only. |
| `ENTIRE_BRAIN_MCP_ALLOW_ANY_PATH` | (unset) | Lets the MCP `brain_index_repository` tool index a path outside the bound repository root (`ENTIRE_REPO_ROOT`). |
| `ENTIRE_BRAIN_MCP_ALLOW_CROSS_REPO` | (unset) | Lets MCP tools act outside the bound repository: `brain_delete_project` with a foreign `repo_key`, `brain_list_projects` returning every local project, and the `brain_workspace_*` tools on a workspace the bound repository is not a member of, or one whose repos live outside the bound repository's parent directory (so sibling checkouts under a common parent are in scope by default). Without the override, unbound servers refuse these operations, and workspace keys must match their local checkouts. Off by default — the MCP surface is driven by an agent whose context can be poisoned, and deleting a brain is irreversible. The plain CLI is unaffected. |

## Development

Run without installing:

```sh
go run ./cmd/entire-brain --help
```

When running outside the Entire CLI, set the plugin directories explicitly or
allow the XDG fallbacks:

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

GitHub Actions runs generic tests and the deterministic Phase 1 semantic suite on
Linux, macOS, and Windows.

## Further reading

- [docs/operations.md](operations.md) — build targets, cgo, shared baseline
- [docs/semantic_mcp_guide.md](semantic_mcp_guide.md) — the full MCP tool surface
- [docs/diffless_review_seam.md](diffless_review_seam.md) — the diff-less review contract
- [docs/durable_facts_plan.md](durable_facts_plan.md) — durable-facts design and eval
