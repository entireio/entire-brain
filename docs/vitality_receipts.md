# Vitality Serve Receipts (Memory Lifecycle, Phase 1 slice)

Status: implemented (2026-07-12). Design input: the memory-lifecycle plan
(receipts, trigger gate, scope boundaries, autonomous decay). This document
states exactly what of that plan exists in the code today and what is still
Phase 2/3. Nothing here changes retrieval behavior: ranking, gating,
quarantine, decay, and the fact schema are untouched in this phase.

## What is implemented

**Serve receipts (plan feature A1).** Every read surface that emits durable
facts leaves an append-only `served` event recording *that* the fact was
emitted — never *what was asked*:

| Surface | Recorded as |
| --- | --- |
| `recall` | `recall` |
| `search` / `vsearch` / `query` (fact-layer hits only) | `search` / `vsearch` / `query` |
| `get` / `multi-get` (resolved `fact:` ids) | `get` / `multi-get` |
| `brief` (facts section) | `brief` |
| `hook pre-edit` / `hook post-failure` (facts actually emitted after the budget cap) | `hook-pre-edit` / `hook-post-failure` |
| MCP `brain_brief`, `brain_query`, `brain_search`, `brain_vsearch`, `brain_get`, `brain_multi_get` | `mcp:<tool>` |

Each event carries: fact id, UTC timestamp, surface name, branch, worktree
HEAD (best-effort), and `task_sha256` — a SHA-256 of the task/query/trigger
text. The raw text is never persisted (gets are id-addressed and carry no
hash). Review groups (`review:` results), history, and doc hits are not facts
and leave no receipts. Deliberately unreceipted fact-touching paths:
`facts eval`/`eval-gen` (a measurement harness — benchmark serves must not
masquerade as agent usage), `verify` (anchor maintenance, not serving),
`facts tree`/`outline` (store listings), `inspect changes`, and the
workspace/pattern surfaces.

**Storage (the vitality ledger substrate).** Branch-scoped, like facts —
sitting next to each branch's `facts.ndjson` inside the brain:

- `facts/<branch-dir>/vitality.ndjson` — the append-only event log,
- `facts/<branch-dir>/vitality.json` — the compacted per-fact rollup
  (`served` count, first/last served, last surface/head/task-hash, per-surface
  counts).

Both are brain-local and never exported or published; there is no egress and
no cross-machine aggregation. The plan sketched a single repo-level
`facts/vitality.ndjson`; the implementation scopes it per branch because
vitality is branch-scoped exactly like the fact store it describes.

**Concurrency and atomicity.** Writers (appends and compaction) serialize on
`locks/vitality.lock` via the repository's standard file-lock machinery, with
a short (500 ms) timeout so a busy sidecar can only ever delay — never block —
a read. The lock is deliberately separate from the brain `write.lock` and is
never held together with it, so no lock-ordering hazard exists. All rewrites
(rollup, log truncation) go through the brain's atomic, symlink-rejecting
write path.

**Deterministic, idempotent, crash-safe compaction.** The rollup is a pure
fold of the event log (timestamps come from events, JSON map keys marshal
sorted), so the same events always produce byte-identical output, and
recompacting with nothing new writes nothing. Compaction is two-phase: the
rollup is persisted with a marker naming the exact log content absorbed
(length + SHA-256), then the log is truncated; a crash between the phases is
detected by the marker and never double-counts.

**Bounded growth.** The log compacts inline once it passes 1 MiB; the rollup
is hard-capped at 4096 fact entries per branch, evicting the
least-recently-served deterministically. Receipts for facts that later leave
the store are kept (marked `missing` on inspection) until evicted by the cap;
a receipts-aware `facts gc` is Phase 2.

**Never-fails recording.** Recording is best-effort on every surface: an
unwritable sidecar, a lock timeout, or corruption degrades to at most one
bounded stderr diagnostic per invocation and leaves the read result
byte-identical. A corrupt log line is skipped and counted, not fatal; a
corrupt rollup is reported on inspection and rebuilt from the surviving log by
`facts vitality --compact`. The MCP server discards these diagnostics, so the
protocol stream is never polluted.

**Inspection.**

- `entire-brain facts vitality [--branch b] [--fact id] [--limit n]
  [--compact] [--json]` — per-fact receipts joined against the fact store
  (missing facts flagged), most recently served first, plus log health
  (pending events, malformed lines, bytes).
- `entire-brain facts status --json` — a `vitality` block per branch:
  `served_facts`, `served_total`, `pending_events`, and last-served evidence
  (`last_served_at`, `last_served_fact`, `last_surface`).

## What is NOT implemented (remains Phase 2/3)

From the memory-lifecycle plan, everything that consumes receipts:

- **Phase 2 — evidence and gating:** `silenced`/`outcome`/`validated` event
  types; the episode-outcome join and drag rollup (A2); usefulness/drag
  decay-weighting; the `brief` trigger gate with silence telemetry and
  scenario tests (B); scope records and within-repo scope ranking priors (C);
  receipts-aware `facts gc`; carrying vitality across `facts promote`.
- **Phase 3 — autonomy and enforcement:** decay rungs 0–2 and the
  `quarantined` status; quarantine enforcement in `brief`/hooks; workspace
  cross-repo scope enforcement; missed-useful tuning reports; structural
  assertions (D4); any ranking change driven by vitality (subject to the
  `facts eval` paired-proof bar).

Receipts are currently *observable evidence only*: no read path consults them.
