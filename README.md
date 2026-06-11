# Entire Brain

Entire Brain is an external-command plugin for the Entire CLI. It builds a
local, inspectable "brain" for a repository from Entire session history, seeded
repository context, a local history index, an index of the brain's own docs (seed
summaries and copied repo markdown), optional semantic records from `entire-sem`,
and a curated layer of durable facts distilled from past sessions.

The plugin binary is named `entire-brain` and is invoked through Entire as:

```sh
entire brain
```

Generated brain artifacts are local and inspectable by default: deterministic
refresh/index/retrieval reads local repositories and writes local plugin data,
and the MCP adapter is stdio-only. Optional agent-gated steps can send selected
context to the configured agent (`refresh --agent auto`, `distill --agent
codex|claude-code`, `recall --expand`, and judged evals). Use `--agent none`,
`--dry-run`, `--agent command` with a local command, or loopback-only Ollama to
avoid hosted model egress. Strict no-egress mode (`ENTIRE_BRAIN_NO_EGRESS=1` or
`ENTIRE_BRAIN_LOCAL_ONLY=1`) can only enforce no-agent, dry-run, and Ollama
paths whose URLs, redirects, and resolved dial targets stay loopback-only;
arbitrary `--agent command` runners are trusted local commands but not
enforceably loopback-only. Separately, repos that configure a
`checkpoint_remote` allow `export`/`refresh` to `git fetch` checkpoint history
from that remote into a throwaway temp repo unless no-egress mode is set.

## Install

```sh
mise install
mise run check
mise run build
entire plugin install ./entire-brain
entire brain
```

## Common Workflows

### Create Or Refresh A Brain

```sh
entire brain refresh
entire brain refresh --agent none
entire brain refresh --output /tmp/repo-brain
entire brain refresh --force
```

`refresh` is the normal entry point. It writes the newest known checkpoint
version of every discoverable Entire session into the persistent brain
directory, builds deterministic repository seed context, runs `entire sem`, and
builds the local semantic context graph used by the `inspect`
`code`/`context`/`impact`/`changes`/`tests` commands and `brief`. It also derives
the local decision/rationale history index from the exported sessions and a doc
index over the brain's seed markdown. `--agent` controls optional seed synthesis:
the default is `auto`, which uses Codex when available, then Claude Code when
available, otherwise deterministic seed-only mode. `--output` writes a complete
brain to an explicit directory. `--force` rebuilds generated sources and
overwrites an explicit output directory when one is provided.

The semantic refresh stores semantic snapshots, a SQLite query store, metrics,
parse cache, and branch overlays in the local brain directory. It refuses dirty
worktrees unless semantic worktree indexing is explicitly enabled through the
advanced `refresh index --worktree` maintenance path.

Use `--worktree` on `refresh index` only when you intentionally want the current
dirty worktree represented. Bundle export rejects worktree-backed semantic indexes.
`repair` rebuilds derived semantic stores from the active local snapshot.
`reset --semantic-only --force` removes semantic artifacts and manifest metadata
without touching seed or session sources. `reset --force` removes the generated
brain directory for the repo.

### Keep The Brain Fresh Automatically

```sh
entire brain watch                       # poll every 5m; deterministic refresh only (NO tokens)
entire brain watch --once                # single pass (handy for a cron/CI hook)
entire brain watch --distill --distill-every 24h --model gpt-5.4-mini --effort low --budget 1
```

`watch` keeps the brain current without manual refreshes, and its token-frugality is **structural,
not a quota**:

- It detects new work cheaply (local + origin checkpoint refs + worktree HEAD), and on a change runs a
  **deterministic refresh** — sessions, semantic index, and history index, with the seed agent **always**
  `none` — which spends **zero agent tokens**.
- The token-spending work — `--distill` and/or agent seed synthesis (`--seed-agent codex|claude-code`) —
  is **off by default** and only ever runs on a brain that was actually refreshed this tick (a failed
  refresh skips it). When enabled, **both steps share one gate**: they run at most once per
  `--distill-every`, on the cheap `--model`/`--effort`, and `--budget N` caps the *number of gated agent
  runs* per process. So `--seed-agent` is bounded exactly like `--distill` — it is **not** per-change
  spend. A persisted cursor (`<state>/repos/<repo-key>/watch.json`) means a restart never re-refreshes
  unchanged state or re-runs the agent work within the interval. (Budget counts reset on restart; the
  durable guard is `--distill-every` + the cursor. A transient failure of one gated step is best-effort —
  it retries on the next interval, not every tick.)

So the default daemon is free; you opt into token spend explicitly and bound it. Run **one watcher per
repo** — the cursor write is atomic, so concurrent watchers won't corrupt it, but they may do redundant
refreshes. On a very active repo with a short `--interval`, note that the free refresh re-indexes on
every commit, so the semantic reindex can be CPU/IO-heavy; widen `--interval` if that matters.

### Work Across Multiple Repos

```sh
entire brain workspace create platform
entire brain workspace add platform ../api --name api
entire brain workspace add platform ../web --name web
entire brain workspace refresh platform
entire brain workspace query platform "checkout" --json
entire brain workspace impact platform "checkout" --json
entire brain workspace watch platform --once          # fan the token-frugal daemon over every member
```

Workspaces coordinate already-local repo brains by repo key and local path hint.
They do not sync or publish generated brain data. `workspace watch` runs the same
deterministic-refresh-is-free / agent-steps-are-gated loop as `watch` across every member repo, with a
single `--budget` shared across all members so a workspace tick can't multiply token spend by repo count.

### Use MCP Locally

```sh
entire brain mcp
```

The MCP adapter is stdio-only and exposes local tools `brain_stale`,
`brain_brief`, the unified retrieval verbs `brain_query` (hybrid lexical+vector
over facts/history/docs), `brain_search`, `brain_vsearch`, `brain_get`, and
`brain_multi_get`, the symbol-graph tools `brain_code`/`brain_context`/`brain_impact`/`brain_changes`/`brain_tests`/`brain_boundaries`,
the diff-less reviewer `brain_regressions`/`brain_review`, and the cross-repo
`brain_workspace_regressions`/`brain_workspace_review`.
See `docs/semantic_mcp_guide.md`.

### Diff-less review (suspected regressions: current tree vs session memory)

The brain can act as a reviewer without a diff: it compares the current working tree against what
session history asserts the code used to be, and flags suspected regressions (`file:line`, expected
vs current, confidence, provenance). Surfaces:

```sh
entire brain inspect regressions "<task + failing symbols>"   # raw anomalies
entire brain review "<task + failing symbols>" --json         # review-shaped findings (hidden; the machine contract)
entire brain workspace regressions <ws> "<query>"             # fan out across a multi-repo workspace
entire brain workspace review <ws> "<query>"                  # same, review-shaped, per repo
```

- `--location-only` (all of the above; `location_only` for the MCP tools) returns only the suspected
  `file:line`, never the expected/current values — so a fair A/B can't paste the answer.
- `--include-deletions` adds the noisier deleted-assignment signal (opt-in).
- `entire brain review --json` emits a **versioned `reviewReport` contract** (`schema_version`); see
  `docs/diffless_review_seam.md`.

`entire brain review` is hidden because it is the **machine contract**, not a human verb. The intended
human surfaces are the cli's `entire review` (a diff-less mode that *would* turn on when the brain is installed)
and `entire labs investigate`. Those consumers live in the `entireio/cli` repo and are **not yet wired**
(review has a CLI prototype; investigate is designed only) — see `docs/diffless_review_seam.md`.

### Ask The Brain

The qmd-inspired retrieval verbs rank across the brain's text layers — durable
facts, indexed history, and docs — and return ids you can fetch in full:

```sh
entire brain query "how does checkpointing work" --json   # hybrid (lexical + vector, RRF) — the default
entire brain search "checkpoint" --json                   # lexical keyword (facts + BM25 history/docs)
entire brain vsearch "preventing data races" --json       # vector / semantic (facts + docs)
entire brain get fact:<id> --json                         # fetch one item by id (fact:… | history:… | doc:…)
entire brain multi-get fact:<id> doc:<id> --json          # fetch several by id
```

All five accept `--json`, `--format json|cli`, and `--branch`; `search`,
`vsearch`, and `query` also take `--limit`/`-n`. Every result carries an `id`
you can pass to `get`/`multi-get`. `--branch` selects the durable-facts branch;
history and docs come from the local indexed brain sources.

#### Semantic embedder (vector arm)

The vector arm behind `vsearch`/`query` embeds with a **bundled, pure-Go
Model2Vec** static model by default — zero config, no daemon, fully offline. For
higher recall you can opt into a **transformer embedder (EmbeddingGemma-300M)**
served over a local HTTP endpoint (Ollama, or qmd's node-llama-cpp server):

```sh
ollama pull embeddinggemma                       # one-time
ENTIRE_BRAIN_EMBEDDER=ollama entire brain query "preventing data races" --json
```

- `ENTIRE_BRAIN_EMBEDDER=ollama` selects the transformer arm. It measured **+14%
  useful-facts-per-1k-tokens** over Model2Vec on the facts eval (pooled across
  repos), driven mostly by *reachability* — it surfaces conceptually-related
  facts that share no query term.
- `ENTIRE_BRAIN_OLLAMA_MODEL` (default `embeddinggemma`) and
  `ENTIRE_BRAIN_EMBED_URL` (default `http://localhost:11434/api/embed`) override
  the model and endpoint. The endpoint just needs to accept `{"model","input"}`
  and return `{"embeddings":[[…]]}`.
- **Graceful fallback (one-time startup probe):** the embedder is chosen once, on
  first use. If the opt-in is set but the embed server does not return an
  embedding then (unreachable, wrong model, or an error response), the brain
  selects the bundled Model2Vec model (one consistent vector space) and prints a
  one-line notice on stderr rather than selecting an embedder that fails every
  call. The probe is not repeated per embed: if a server that was healthy at
  startup later fails mid-run, those individual embeds degrade to lexical for
  that run. Switching embedders re-namespaces the vector cache, so the two never
  mix.

This is the Stage 1b transformer embedder available **without cgo** today; the
in-process single-binary form is deferred (tracked in the alignment plan outside this repository).

`overview` is the fastest way to orient on an unfamiliar repo: it returns a
single project map — stack stats, route/tool/workflow counts, build/test
commands, entrypoints, key documents, and recent decisions newest-first.

```sh
entire brain overview --json
```

`brief` is the per-task entry point: it combines brain availability, freshness,
live git state, semantic context and test suggestions, matching history records,
and the top matching durable facts for the task (sized to the requested
`--limit`). `status`, `show`, and the retrieval verbs above provide smaller
top-level queries for agents and scripts.

For symbol-graph navigation and regression analysis — what the retrieval verbs
don't cover — use the `inspect` specialists:

```sh
entire brain guide
entire brain inspect code "ValidateToken" --json         # find a symbol in the graph
entire brain inspect context "main" --json               # relation-aware context for a symbol
entire brain inspect impact "main" --json                # impact set via typed relations
entire brain inspect changes --json                      # map the working-tree diff to symbols
entire brain inspect tests "main" --json                 # test suggestions for a symbol
entire brain inspect boundaries --kind tool --json       # route / tool / workflow boundaries
entire brain inspect regressions "<task>" --json         # suspected regressions vs session memory
entire brain inspect blame <fact-id> --json              # the source anchors a fact was derived from
```

History content (decisions, validations, tool paths, architecture notes) is no
longer a set of per-kind commands — it is indexed into the history layer and
surfaced through the unified `search`/`query` verbs above. Symbol lookup that used
to be the top-level `search` now lives at `inspect code`; `inspect context`/`impact`
accept a symbol name or a record id.

Use `stale --blind-spots` to list the files the semantic provider could not
fully index, so an agent knows where its semantic answers are untrustworthy:

```sh
entire brain stale --blind-spots
```

### Durable Facts

The brain also keeps a curated **durable-facts** layer: short, self-contained,
provenance-anchored statements about how work on the repo should be done —
resolved decisions and their *why*, standing rules, stated preferences, and
non-obvious constraints. Unlike the history index (read-only excerpts), facts
are distilled, deduplicated, branch-scoped, and agent-writable. Facts carry
retained session/checkpoint anchors; `verify` reports whether those anchors are
verified, stale, orphaned, or unverifiable-here. Full turn-level cryptographic
verification depends on CLI-side turn signing.

```sh
entire brain distill --agent codex                         # extract facts from captured sessions
entire brain distill --dry-run --json                      # estimate sessions/chunks/calls without agent work
entire brain distill --agent codex --jobs 4                # parallelize extraction, reconcile deterministically
entire brain distill --agent ollama --model llama3.2       # local loopback Ollama
entire brain distill --agent codex --model gpt-5.4-mini --effort low   # run it on a fast/cheap model
entire brain remember "Prefer table-driven tests" --path preferences.coding.style
entire brain recall "account deletion" --k 5
entire brain recall "MirrorCommittedMetadataRef" --expand   # agent expands the query first
entire brain facts status --json           # read-only fact/eval readiness summary
entire brain facts tree --depth 1           # navigable map of what the brain knows
entire brain facts tree --path constraints  # drill into a category
```

`distill` runs one extraction call per uncached transcript chunk, plus up to one
reconcile call for chunks that produce candidates, so it is worth running on a
**fast/cheap model**: `--model`/`--effort` pin the model + reasoning effort for
both the distill and reconcile agent calls (codex: `--model <m> --config
model_reasoning_effort=<e>`; claude-code: `--model <m> --effort <e>`).
The same `--model`/`--effort` flags exist on `seed`, and `refresh` forwards them as `--seed-model`/
`--seed-effort`, so a full `entire brain refresh` can synthesize its seed cheaply too.

`distill` is agent-required: it sends line-numbered transcript chunks to the
selected agent under a strict quality gate (`--agent auto` chooses Codex, then
Claude Code; `ollama` and `command` are explicit), then reconciles each candidate
against the branch's existing facts so near-duplicates merge and contradictions
supersede (low-confidence calls are queued for review). It is incremental by
default; `--force` rebuilds. `remember` authors a fact directly (the agent
classifies it when `--path` is omitted). `recall` retrieves by keyword + taxonomy
+ code-locus match, scoped to the current branch; `--scope local|cross-cutting`
separates code facts from how-we-work facts, and `--expand` has the agent
rewrite the query into the facts' vocabulary first.

Manage the fact store:

```sh
entire brain facts status --json
entire brain facts review                   # resolve queued merge/supersede proposals
entire brain facts promote --from <branch> --strategy keep-both
entire brain facts retract <fact-id>        # mark a fact no longer true (gc prunes later)
entire brain facts gc --force               # prune retracted/old-superseded; report orphans
```

The fact store also ships an evaluation harness for measuring retrieval quality
(`facts eval-gen` builds a provenance/silver-labeled benchmark from the brain's
own sessions, `facts eval` reports retrieved items, token estimates, precision,
useful-per-1k, and recall only when labels exist for that retriever, and `facts
eval-compare` does a paired t-test with Holm correction and rejects non-proof or
different relevance sources unless `--allow-proxy-comparison` is explicit). Use `facts eval
--retriever facts|history|query|raw-sessions` to compare distilled facts against
history, unified query, and preprocessed session-chunk baselines before claiming
a quality lift.
Generated source-session fields give source-match credit for history/session
arms; they are provenance/proxy evidence, not recall labels. Task files with
`relevant` ids must set `label_source` (`human`, `judge_refined`, or
`provenance_silver`), and paired proof runs must share the same task-file and
brain-manifest hashes unless an override is called out.
The eval `query` arm is a read-only lexical unified baseline over local brain
layers; it does not write embedding caches or call an embedder. See
`docs/durable_facts_plan.md` for the full design.

For semantic release checks, `entire brain semantic-audit --json` reports the
semantic provider/schema state, counts, file-language/symbol/relation coverage,
warning/failure details, freshness axes, and blind spots in one local audit
payload. Treat that as audit evidence for the reported provider output, not a
global tree-sitter coverage claim; public semantic claims should name the covered
languages, relation types, freshness state, and benchmark or audit records behind
the claim.

## Storage

The parent Entire CLI supplies these directories:

| Variable | Purpose |
|---|---|
| `ENTIRE_PLUGIN_CONFIG_DIR` | Plugin config, including `brain.json`. |
| `ENTIRE_PLUGIN_DATA_DIR` | Durable brains under `repos/<repo-key>/`. |
| `ENTIRE_PLUGIN_STATE_DIR` | Regenerable cursors under `repos/<repo-key>/`. |
| `ENTIRE_PLUGIN_CACHE_DIR` | Cache data. |
| `ENTIRE_REPO_ROOT` | Current git checkout when invoked inside a repo. |

Repo keys come from the repository origin. For example,
`github.com/entireio/cli` becomes `gh/entireio/cli`.

### Environment toggles

Optional `ENTIRE_BRAIN_*` variables tune retrieval and diagnostics. All are
off/default unless set; none are required for normal use.

| Variable | Default | Effect |
|---|---|---|
| `ENTIRE_BRAIN_EMBEDDER` | (unset → Model2Vec) | Set to `ollama` to use the transformer embedder (EmbeddingGemma) for the vector arm instead of the bundled Model2Vec model. Falls back to Model2Vec with a stderr notice if a one-time startup probe finds the server does not return an embedding (unreachable, wrong model, or error response). See [Semantic embedder](#semantic-embedder-vector-arm). |
| `ENTIRE_BRAIN_OLLAMA_MODEL` | `embeddinggemma` | Model name requested from the embed server when `ENTIRE_BRAIN_EMBEDDER=ollama`. |
| `ENTIRE_BRAIN_EMBED_URL` | `http://localhost:11434/api/embed` | Embed endpoint (Ollama, or qmd's node-llama-cpp server). Must accept `{"model","input"}` and return `{"embeddings":[[…]]}`. |
| `ENTIRE_BRAIN_FACTS_BM25` | (unset → token-overlap) | `1`/`true`/`yes`/`on` switches the facts **lexical** arm to FTS5 BM25. Experimental; measured at parity with the default scorer, kept for A/B'ing the lexical engine. |
| `ENTIRE_BRAIN_MCP_DEBUG_LOG` | (unset) | Path to a file the stdio MCP adapter appends frame-level debug lines to. Diagnostics only. |

## Development

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

GitHub Actions runs generic tests and the deterministic Phase 1 semantic suite
on Linux, macOS, and Windows.
