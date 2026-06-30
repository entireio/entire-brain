# Getting Started

## What This Is

Entire is a Git-native platform for AI-assisted software work. Its base layer is
session capture: `entire-cli` installs Git and agent hooks for supported coding
agents, records prompts, transcripts, tool activity, files touched, token usage,
and checkpoint metadata, then stores that context on a dedicated Entire-managed
ref (`entire/checkpoints/v1`) instead of mixing it into normal code history. A
checkpoint is the retained link between an agent session and the commit or
intermediate work state it produced.

`entire-sem` and `entire-brain` add the local reasoning layer on top of that
captured history. `entire-sem` is the semantic provider: it parses source code
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

## Install

Prerequisites:

- Entire CLI installed and available as `entire`
- Entire already enabled in the repository you want to use
- The target agent hooks already installed for that repository
- Git
- Go 1.26 toolchain for `entire-brain`
- A cgo-capable compiler/toolchain for `entire-sem`

### 1. Clone The Repositories

Choose a local directory where you keep source checkouts, then clone
`entire-sem` and `entire-brain` side by side. The directory names matter:
`scripts/install.sh` expects `entire-sem` to be the sibling checkout next to
`entire-brain`.

```sh
cd /path/to/your/source-directory

git clone https://github.com/suhaanthayyil/entire-sem.git
git clone https://github.com/ashtom/entire-brain.git
```

These are the current source repositories used by this project.

### 2. Install The Plugins

Run the installer from the `entire-brain` checkout:

```sh
cd /path/to/your/source-directory/entire-brain
scripts/install.sh
```

This builds and installs both plugins, writes the default `entire-brain`
configuration file, then runs `entire brain doctor`.
`entire-brain` uses a pure-Go default build. `entire-sem` uses tree-sitter native
parser bindings, so its local source build needs cgo.

Verify manually:

```sh
entire sem version
entire sem doctor --json
entire brain version
entire brain doctor
entire plugin doctor
```

If `doctor` reports that the semantic provider is missing, check that
`entire-sem` and `entire-brain` were cloned side by side with the names shown
above. See [Operations](operations.md) for `entire-brain` build details and
`../entire-sem/docs/operations.md` for `entire-sem` release/cgo details.

### 3. Build The Deterministic Brain

Run the first refresh in the Entire-enabled repository you want agents to work
in:

```sh
cd /path/to/your/repo

entire brain refresh --agent none
entire brain status
```

The semantic refresh refuses a dirty worktree by default. Run it on a clean
checkout, or use the advanced `entire brain refresh index --worktree` path only
when you intentionally want the current uncommitted state indexed. Worktree-
backed semantic indexes are rejected by bundle export.

`--agent none` keeps the first build deterministic and token-free, with no
hosted-model calls. Refresh exports captured sessions, builds the local
history/doc indexes, asks `entire-sem` for a semantic snapshot, and stores the
derived brain under Entire's local plugin data directory.

At this point the brain can answer from captured history, docs, semantic code
structure, runtime traces, patterns, and any existing durable facts. It has not
yet extracted new durable facts from retained sessions.

### 4. Distill Durable Facts

Distillation is the egress-gated agent step that turns captured sessions into
durable project knowledge: decisions, constraints, preferences, gotchas,
conventions, and invariants. If your goal is a full brain with newly extracted
durable facts, this is the next step after deterministic refresh:

```sh
entire brain distill --agent codex --model gpt-5.4-mini --effort low
```

To estimate cost before spending agent calls, run a dry run first:

```sh
entire brain distill --dry-run --json
```

To inspect the resulting facts:

```sh
entire brain facts status --json
entire brain facts tree --depth 1
```

Distillation sends redacted transcript chunks to the selected agent unless you
use a local loopback agent such as Ollama. It is incremental and cached:
unchanged sessions are skipped, near-duplicate facts are reconciled against the
branch's existing facts, and low-confidence merge/supersede decisions are
queued for `entire brain facts review`.

For active repos, keep the brain current with the watcher:

```sh
entire brain watch
```

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
into the brain. `entire-brain refresh` later ingests the retained sessions and
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
- review, workspace, and pattern tools such as `brain_regressions`,
  `brain_workspace_graph`, `brain_workspace_review`, and `brain_patterns` when
  the task calls for them

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

The agent should use the brief before broad shell search or file reads. If the
brief points to likely files, inspect those first. If freshness is degraded, the
agent should say so and either refresh the brain or lower confidence in semantic
answers.

For repo-level orientation rather than task-specific orientation, use
`overview`. It returns a compact project map: stack signals, entrypoints,
commands, key documents, and recent decisions.

```sh
entire brain overview --json
```

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
history and facts layers. The normal tools are `brain_query` or `brain_search`,
followed by `brain_get` for specific ids.

This is the main reason Entire capture matters: the original prompt, attempts,
validation, correction, and rationale can survive the code diff and become
available to the next agent.

### Ask Across Facts, History, And Docs

When the question is not tied to one symbol, use the unified retrieval layer.
`query` is the normal hybrid path over durable facts, indexed history, and docs.
Use `search` for exact keywords, `vsearch` for semantic matches, and
`get`/`multi-get` when a result returns an id worth reading in full.

```sh
entire brain query "how does checkpointing work" --json
entire brain search "checkpoint" --json
entire brain vsearch "preventing data races" --json
entire brain get fact:<id> --json
```

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

This is where `entire-sem` matters. It is not an agent memory layer; it is the
local parser/provider that gives `entire-brain` the graph the agent queries.
Semantic depth is language-dependent: parser-backed extraction covers the
semantic language set, while many recognized filetypes are inventory-only. For
inventory-only files, prefer text retrieval and lower confidence in
impact/context answers.

For deeper graph work, use the graph specialists: schema inventory, local graph
UI, source snippets, directed trace paths, dead-code candidates, boundary
enumeration, and working-tree change mapping.

```sh
entire brain inspect graph-schema --json
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
