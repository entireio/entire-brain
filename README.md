# Entire Brain

Entire Brain is an external-command plugin for the Entire CLI. It builds a local,
inspectable "brain" for a repository — and this is the fastest way to understand
what it does, how to install it, and how humans and agents actually use it.

## What This Is

Entire is a Git-native platform for AI-assisted software work. Its base layer is
session capture: `entire-cli` installs Git and agent hooks for supported coding
agents, records prompts, transcripts, tool activity, files touched, token usage,
and checkpoint metadata, then stores that context on a dedicated Entire-managed
ref (`entire/checkpoints/v1`) instead of mixing it into normal code history. A
checkpoint is the retained link between an agent session and the commit or
intermediate work state it produced.

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

## Development source of truth

Entire Brain has no released product version. Development starts from the
locally fetched current mainline; do not resume an old WIP/integration checkout
or use an older Brain binary as a benchmark control. The Agent Brain harness
builds from the active checkout and refuses to run unless `HEAD` contains local
`origin/main`. All causal arms use that same binary and vary only memory
delivery.

GraphMark owns cross-product benchmark evidence and split integrity. See
[`benchmarks/agent-brain/CONDITIONS.md`](benchmarks/agent-brain/CONDITIONS.md)
for the normative condition and comparison contract, and
[`benchmarks/agent-brain/README.md`](benchmarks/agent-brain/README.md) for
harness operation and the retired-corpus notice.

## Install

Prerequisites:

- Entire CLI installed and available as `entire`
- Entire already enabled in the repository you want to use
- The target agent hooks already installed for that repository
- Git
- Go 1.26 toolchain for `entire-brain`
- A cgo-capable compiler/toolchain for `entire-graph`

### 1. Clone the repositories

Clone `entire-graph` and `entire-brain` side by side. The directory names matter:
`scripts/install.sh` expects `entire-graph` to be the sibling checkout next to
`entire-brain`.

```sh
cd /path/to/your/source-directory

git clone https://github.com/entireio/entire-graph.git
git clone https://github.com/ashtom/entire-brain.git
```

### 2. Install the plugins

Run the installer from the `entire-brain` checkout:

```sh
cd /path/to/your/source-directory/entire-brain
scripts/install.sh
```

This builds and installs both plugins, writes the default `entire-brain`
configuration file, then runs `entire brain doctor`. `entire-brain` uses a
pure-Go default build; `entire-graph` uses tree-sitter native parser bindings, so
its local source build needs cgo.

Other install paths:

- `scripts/install-local.sh` — one-command local source install of just this plugin.
- `mise install && mise run check && mise run build && entire plugin install ./entire-brain` — build without the sibling `entire-graph` step.
- `scripts/release.sh` — local release archives with `SHA256SUMS`.

See [docs/operations.md](docs/operations.md) for target, cgo, and shared
baseline details.

Verify:

```sh
entire graph version
entire graph doctor --json
entire brain version
entire brain doctor
entire plugin doctor
```

If `doctor` reports the semantic provider is missing, check that `entire-graph`
and `entire-brain` were cloned side by side with the names above.

### 3. Build the deterministic brain

Run the first refresh in the Entire-enabled repository you want agents to work in:

```sh
cd /path/to/your/repo

entire brain refresh --agent none
entire brain status
```

Refresh refuses a dirty worktree by default. Run it on a clean checkout, or use
`entire brain refresh --worktree` only when you intentionally want seed/docs and
the semantic index to include the same current uncommitted state. Use the
advanced `refresh index --worktree` path when only the semantic layer needs
updating. Worktree-backed semantic indexes are rejected by bundle export.

`--agent none` keeps the first build deterministic and token-free, with no
hosted-model calls. Refresh exports captured sessions, builds the local
history/doc indexes, asks `entire-graph` for a semantic snapshot, and stores the
derived brain under Entire's local plugin data directory.

At this point the brain can answer from captured history, docs, semantic code
structure, runtime traces, patterns, and any existing durable facts. It has not
yet extracted new durable facts from retained sessions.

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
entire brain watch --distill --distill-every 24h --model gpt-5.4-mini --effort low --budget 1
```

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
  unsafe. It includes the current worktree by default, never exports checkpoint
  sessions, and is capped at 60 seconds; set `semantic: true` only for small repositories and use
  `brain_index_repository` as the separate long-running semantic step for large
  repositories. Set `worktree: false` only when the snapshot must be committed
  HEAD; use `entire brain refresh sessions` from the CLI for checkpoint history
- retrieval tools such as `brain_query` and `brain_get` for facts, docs, history
- semantic tools such as `brain_code`, `brain_context`, `brain_impact`, and
  `brain_tests` for code navigation and validation planning
- review, workspace, and pattern tools such as `brain_regressions`,
  `brain_workspace_graph`, `brain_workspace_review`, and `brain_patterns` when
  the task calls for them

Some MCP tools write local state (for example `brain_ingest_traces` persists
runtime edges as `RUNTIME_TRACE` facts); pattern MCP tools are read-only. The
MCP and CLI surfaces are intentionally close but not perfect mirrors — prefer the
native surface your agent client has, then translate only when needed. See
[docs/semantic_mcp_guide.md](docs/semantic_mcp_guide.md) for the full tool surface.

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
layers — `brain_query`/`brain_search`, then `brain_get` for specific ids. This is
the main reason Entire capture matters: the original prompt, attempts,
validation, correction, and rationale survive the code diff and become available
to the next agent.

### Ask across facts, history, and docs

When the question is not tied to one symbol, use the unified retrieval layer.
`query` is the hybrid path (lexical + vector, RRF) over durable facts, indexed
history, and docs; `search` for exact keywords, `vsearch` for semantic matches,
`get`/`multi-get` when a result returns an id. Every result carries an `id`.

```sh
entire brain query "how does checkpointing work" --json
entire brain search "checkpoint" --json
entire brain vsearch "preventing data races" --json
entire brain get fact:<id> --json
```

`query`, `search`, and `vsearch` also take `--source` (`all` | `fact` |
`history` | `conversation` | `doc`) to restrict retrieval to one layer. The
default is unchanged (`all` = facts + classified history + docs).

### Recall prior conversations (experimental, opt-in)

`--source conversation` searches captured request/response exchanges from
exported session transcripts — what was asked, what the agent concluded — and
`get conversation:<id>` expands one exchange to a bounded request/response pair
with its exact transcript range:

```sh
entire brain query "why did we reject the cache rewrite" --source conversation --json
entire brain search "SQLITE_BUSY" --source conversation --json
entire brain get conversation:<id> --json
```

Conversation queries take structured filters — `--after`/`--before` (RFC3339 or
YYYY-MM-DD session time), `--session <id>`, `--agent <harness>`, and `--branch`
— which error on any other source rather than being silently ignored. Results
carry `matched_terms` (which query tokens actually hit) and are diversity-capped
so one long session cannot crowd out every other trajectory; filtering to a
session lifts the cap. Re-exported duplicate sessions are collapsed at index
time (newest export wins).

Exchanges are extracted deterministically and locally (no model calls), indexed
lexically only (`vsearch --source conversation` is unsupported), and never enter
default retrieval or published bundles. Every result is labeled
`verification_required` with a `historical_conversation` caveat: recalled
conversation text is quoted historical evidence that may be stale, mistaken, or
adversarial — verify it against current code before acting on it, and never
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
entire brain inspect graph-schema --json                 # relation/schema inventory
entire brain inspect graph-ui semantic-graph.html        # local static graph explorer
entire brain inspect trace-path "<caller>" "<callee>" --json
entire brain inspect dead-code --json
entire brain inspect boundaries --kind tool --json
entire brain inspect changes --json                      # read-only, diff-hunk-scoped symbol mapping
# add --write-report only when semantic/changes/latest.json should be persisted
```

### Review risk without a clean diff

For long sessions, manual edits, or suspected regressions from memory, use
diff-less regression review — `brain_regressions`/`brain_review`. It compares the
current working tree against what session history asserts the code used to be and
flags suspected regressions (`file:line`, expected vs current, confidence,
provenance). Prefer `--location-only` first for a fair investigation: it points
to suspected files and lines without handing over the expected answer text.
`entire brain review --json` emits a versioned `reviewReport` contract; see
[docs/diffless_review_seam.md](docs/diffless_review_seam.md).

### Connect runtime evidence to source

When static structure is not enough, import runtime trace edges with
`brain_ingest_traces` / `inspect ingest-traces`. The brain compares runtime edges
with known static relations, persists them as `RUNTIME_TRACE` graph facts, and
makes them available to graph-schema, trace-path, and brief/context flows —
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
- judged evaluation commands

Use `--agent none`, `--dry-run`, local loopback Ollama, or
`ENTIRE_BRAIN_NO_EGRESS=1` / `ENTIRE_BRAIN_LOCAL_ONLY=1` when the repo must stay
strictly local. No-egress mode enforces locality for no-agent, dry-run, and
loopback-Ollama paths by validating URLs, redirects, and resolved dial targets. A
custom `--agent command` runner is a trusted local command and is not enforceably
loopback-only; no-egress mode cannot stop that runner from making its own network
calls.

To keep specific sessions out of the brain's projections, use
`entire brain privacy list|exclude|include|purge`. `exclude` tombstones a
session so every derived layer (history records, conversation exchanges, FTS,
vector stores, caches) skips it on rebuild while keeping the exported
transcript; `purge` additionally deletes the exported transcript copy and the
derived stores (`--dry-run` reports exactly what would be removed first).
Tombstones are brain-local and survive re-export: a purged session that the
capture layer re-exports stays un-indexed until an explicit `include`. Note the
canonical capture on `entire/checkpoints/v1` is the capture layer's data —
purging the brain does not rewrite checkpoint history.

Remember that base Entire session capture stores transcripts and metadata on the
repository's `entire/checkpoints/v1` branch — anyone with access to that branch
can read captured prompts, tool activity, and retained transcript data. Entire
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
| `ENTIRE_BRAIN_NO_EGRESS` / `ENTIRE_BRAIN_LOCAL_ONLY` | (unset) | Strict local-only mode; enforces locality for no-agent, dry-run, and loopback-Ollama paths. |
| `ENTIRE_BRAIN_MCP_DEBUG_LOG` | (unset) | Path the stdio MCP adapter appends frame-level debug lines to. Diagnostics only. |

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

- [docs/operations.md](docs/operations.md) — build targets, cgo, shared baseline
- [docs/semantic_mcp_guide.md](docs/semantic_mcp_guide.md) — the full MCP tool surface
- [docs/diffless_review_seam.md](docs/diffless_review_seam.md) — the diff-less review contract
- [docs/durable_facts_plan.md](docs/durable_facts_plan.md) — durable-facts design and eval
