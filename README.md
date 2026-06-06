# Entire Brain

Entire Brain is an external-command plugin for the Entire CLI. It builds a
local, inspectable "brain" for a repository from Entire session history, seeded
repository context, a local history index, optional semantic facts from
`entire-sem`, and a curated layer of durable facts distilled from past sessions.

The plugin binary is named `entire-brain` and is invoked through Entire as:

```sh
entire brain
```

All generated brain data stays local (for now): features read local
repositories and write local plugin data, and they do not publish or serve brain
data over the network. The one network access is opt-in checkpoint discovery —
when a repo configures a `checkpoint_remote`, `export`/`refresh` may `git fetch`
the checkpoint history from that remote into a throwaway temp repo to build the
brain. No other hydration occurs.

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
builds the local semantic context graph used by `query`, `context`, `impact`,
`changes`, and `brief`. It also derives the local decision/rationale history
index from the exported sessions. `--agent` controls optional seed synthesis:
the default is `auto`, which uses Codex when available, then Claude Code when
available, otherwise deterministic seed-only mode. `--output` writes a complete
brain to an explicit directory. `--force` rebuilds generated sources and
overwrites an explicit output directory when one is provided.

The semantic refresh stores semantic snapshots, a SQLite query store, metrics,
parse cache, and branch overlays in the local brain directory. It refuses dirty
worktrees unless semantic worktree indexing is explicitly enabled through the
advanced `index --worktree` maintenance path.

Use `--worktree` on `index` only when you intentionally want the current dirty
worktree represented. Bundle export rejects worktree-backed semantic indexes.
`repair` rebuilds derived semantic stores from the active local snapshot.
`reset --semantic-only --force` removes semantic artifacts and manifest metadata
without touching seed or session sources. `reset --force` removes the generated
brain directory for the repo.

### Work Across Multiple Repos

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

The MCP adapter is stdio-only and exposes local tools `brain_stale`,
`brain_brief`, the semantic `brain_query`/`brain_context`/`brain_impact`/`brain_changes`,
indexed session `brain_history`, the diff-less reviewer `brain_regressions`/`brain_review`,
and the cross-repo `brain_workspace_regressions`/`brain_workspace_review`.
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
(review is prototyped on a held branch; investigate is designed only) — see `docs/diffless_review_seam.md`.

### Ask The Brain

```sh
entire brain brief "update the README" --json
entire brain search "README" --json
entire brain show <semantic-id> --json
```

`brief` is the agent-facing entry point: it combines brain availability,
freshness, live git state, semantic context and test suggestions, matching
history records, and the top matching durable facts for the task (sized to the
requested `--limit`). `status`, `search`, and `show` provide smaller top-level
queries for agents and scripts.

For deeper inspection:

```sh
entire brain guide
entire brain inspect code "README" --json
entire brain inspect context "main" --json
entire brain inspect impact "main" --json
entire brain inspect changes --json
entire brain inspect tests "main" --json
entire brain inspect decisions "semantic" --json
entire brain inspect history "semantic" --json
entire brain inspect facts "checkpoint" --json
entire brain inspect blame <fact-id> --json
entire brain inspect boundaries --kind tool --json
```

### Durable Facts

The brain also keeps a curated **durable-facts** layer: short, self-contained,
provenance-anchored statements about how work on the repo should be done —
resolved decisions and their *why*, standing rules, stated preferences, and
non-obvious constraints. Unlike the history index (read-only excerpts), facts
are distilled, deduplicated, branch-scoped, and agent-writable, and every fact
traces back to the signed session/checkpoint it came from. Like the rest of the
brain, facts stay local and are never published.

```sh
entire brain distill --agent codex          # extract facts from captured sessions
entire brain remember "Prefer table-driven tests" --path preferences.coding.style
entire brain recall "account deletion" --k 5
entire brain recall "MirrorCommittedMetadataRef" --expand   # agent expands the query first
entire brain facts tree --depth 1           # navigable map of what the brain knows
entire brain facts tree --path constraints  # drill into a category
```

`distill` is agent-required: it sends line-numbered transcript chunks to the
seed agent (Codex, then Claude Code) under a strict quality gate, then reconciles
each candidate against the branch's existing facts so near-duplicates merge and
contradictions supersede (low-confidence calls are queued for review). It is
incremental by default; `--force` rebuilds. `remember` authors a fact directly
(the agent classifies it when `--path` is omitted). `recall` retrieves by
keyword + taxonomy + code-locus match, scoped to the current branch; `--scope
local|cross-cutting` separates code facts from how-we-work facts, and `--expand`
has the agent rewrite the query into the facts' vocabulary first.

Manage the fact store:

```sh
entire brain facts review                   # resolve queued merge/supersede proposals
entire brain facts promote --from <branch> --strategy keep-both
entire brain facts retract <fact-id>        # mark a fact no longer true (gc prunes later)
entire brain facts gc --force               # prune retracted/old-superseded; report orphans
```

The fact store also ships an evaluation harness for measuring retrieval quality
(`facts eval-gen` builds a provenance-labeled benchmark from the brain's own
sessions, `facts eval` reports precision / recall / useful-facts-per-1k-tokens,
and `facts eval-compare` does a paired t-test with Holm correction). See
`docs/durable_facts_plan.md` for the full design.

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
