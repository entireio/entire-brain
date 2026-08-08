# Memory Lifecycle Plan: Receipts, Trigger Gate, Scope Boundaries, Autonomous Decay

Status: proposed (2026-07-03). Owner: TBD.

Four features that close the loop between what the brain serves and what it
should keep serving, designed for entire-brain's constraint: **no human in the
loop in steady state**.

Decisions locked by the sponsor:

- Decay runs fully autonomously. No approval queue, no reminders.
- Locus-less facts (preferences / how-we-work) decay **slower** and are demoted
  only by the **drag signal**, never by time alone.
- Deterministic-first: every token-spending step is gated and budgeted exactly
  like `watch --distill` (interval + `--model`/`--effort`/`--budget`).

## What already exists (build on it, do not duplicate)

| Existing seam | File | What it gives us |
| --- | --- | --- |
| Locus-drift v1 | `internal/cli/facts_staleness.go` | Recall-time flag for facts whose file-path locus tokens no longer exist. Explicitly scoped to unambiguous file tokens; symbol/dir resolution deferred to the worktree overlay seam. |
| Silent delivery contract | `internal/cli/hook_cmd.go` | `hook pre-edit` / `hook post-failure` with SILENT-HONEST-EMPTY + NEVER-FAILS rules. This *is* the trigger-layer contract; it just doesn't govern `brief` yet. |
| Outcome signal | `internal/cli/episodes.go` | Episodes with reinforcement labels (Phase 0 classifier + commit-success signal), derived token-free from exported sessions during refresh. |
| Anchor verdicts | `facts verify` | Per-anchor verified / stale / orphaned / unverifiable-here. |
| Contradiction path | `distill` reconcile | Near-duplicates merge, contradictions supersede. Already autonomous. |
| Scope tiering | `internal/cli/facts_locus.go` | `local` vs `cross-cutting` classification from taxonomy paths. |
| Fact schema | `internal/cli/facts.go` | `Locus []string`, `Provenance []factAnchor{SessionID, Commit, CheckpointID, TurnID, ...}`, `Kind`, `Status`, `Confidence`. |
| Sem query store | brain semantic layer | Symbol→file resolution, `inspect changes/impact/dead-code/trace-path/query-graph`, snapshots + branch overlays. |
| Budgeted agent loop | `internal/cli/watch.go` | The gated, cursor-persisted, budget-capped host for any recurring agent step. |

## Shared substrate: the vitality ledger

One new store feeds all four features:
`repos/<repo-key>/facts/vitality.ndjson` (append-only events) plus a compacted
`vitality.json` index (per-fact rollup), both brain-local, never exported by
default. Events:

```json
{"t":"served",   "fact":"<id>", "at":"...", "surface":"brief|recall|query|hook-pre-edit|hook-post-failure|mcp:<tool>", "task_hint":"<query text hash>", "head":"<worktree HEAD>", "branch":"..."}
{"t":"silenced", "at":"...", "surface":"brief", "reason":"below-threshold|low-risk", "top_score":0.31, "head":"..."}
{"t":"outcome",  "fact":"<id>", "at":"...", "episode":"<episode id>", "label":"reinforced|neutral|contradicted|reverted"}
{"t":"validated","fact":"<id>", "at":"...", "rung":0, "verdict":"renewed|reanchored|quarantined|retracted", "evidence":"locus-untouched|assertion-pass|agent-reassert|..." , "commit":"<HEAD at validation>"}
```

Rollup per fact: `served_count`, `last_served`, `useful` (outcome=reinforced),
`drag` (outcome=contradicted|reverted, or repeated served with zero locus
overlap in the following episode), `last_validated`, `valid_until`, decayed
exponentially by age so old evidence fades. `factRecord` itself gains nothing
except one new `Status` value (`"quarantined"`); everything else lives in the
sidecar so the fact store format stays stable.

Concurrency: same filelock discipline as the fact store; events written
best-effort (a failed vitality write must never fail a read path — mirror the
NEVER-FAILS hook rule).

---

## Feature A — Serve receipts and outcome evidence

**A1. Serve logging.** Every surface that emits a fact writes a `served` event:
`brief`, `recall`, `query`/`search`/`vsearch` (only for fact-layer hits), `hook
pre-edit` / `post-failure`, and the MCP verbs (`brain_brief`, `brain_query`,
`brain_search`, `brain_vsearch`, `brain_get` on `fact:` ids). Include the
surface name — hook-served facts carry a stronger "was in the moment" prior
than broad query hits.

**A2. Outcome join (deterministic, token-free, runs in refresh).** The episode
extractor already segments sessions and labels reinforcement. Add a join pass:
for each new episode, collect `served` events in its time/branch window and
attach the episode's label to those facts as `outcome` events:

- `reinforced` — episode reinforcement positive (next-turn classifier +
  commit-success) **and** the episode's work touched at least one locus token
  of the fact (paths from apply-patch targets when available; else commit
  file lists via the episode's checkpoint anchors).
- `reverted` — the episode's checkpoint commits were reverted or the
  checkpoint discarded (derivable from checkpoint refs + `git log` locally).
- `contradicted` — a later reconcile superseded/retracted the fact from a
  candidate distilled out of that same session. Strongest drag signal; wire it
  where reconcile already decides supersession.
- `neutral` — everything else. Neutral is not drag: a served fact that merely
  wasn't load-bearing must not be punished (that would train the brain toward
  silence).

**Drag definition (the sponsor-chosen demotion signal):**
`drag(fact) = w_c*contradicted + w_r*reverted + w_i*ignored`, where `ignored`
counts episodes where the fact was served with high rank yet the episode's
touched files had zero locus overlap *repeatedly* (≥N distinct episodes). All
weights recency-decayed. Publish per-fact usefulness/drag in `facts status
--json` and `facts tree` so the numbers are inspectable (transparency replaces
the human reviewer).

**Non-goals:** no cross-machine aggregation, no egress, no per-user analytics.

## Feature B — Trigger gate on `brief`

`hook pre-edit` already implements the contract; extend it to the task-level
entry point:

**B1. Gate.** Before assembling output, `brief` computes token-free signals:
top fact relevance score, locus overlap between matched facts and the live git
state (files in the working diff / recently touched paths), fact vitality
(drag-heavy facts rank down, quarantined facts are excluded), and a task-risk
proxy from sem — are the diff-touched files on route/tool/workflow boundaries
(`inspect boundaries`) or do they have a large `impact` set? Low relevance + low
risk ⇒ emit the machine-readable silence result (`{"silence": true, "reason":
...}`, exit 0) instead of content. `--no-gate` preserves current behavior;
hosts opt in.

**B2. Silence telemetry.** Every silence decision writes a `silenced` event
with the top score. The tuning loop is then measurable, not vibes: a later
episode that touched files matching a *silenced* fact's locus is a
"missed-useful" — counted in `facts status`, and the gate thresholds live in
`brain.json` where they can be adjusted per repo (by a human or an agent,
offline — the steady-state loop itself needs no one).

**B3. Scenario tests.** Fixture-based
expectation tests: given a seeded brain and a task, assert `expect:
silent-skip` or `expect: fact <id> surfaced`. Deterministic, no agent.

## Feature C — Scope boundary records

**C1. Records.** `repos/<repo-key>/scope.json` and
`workspaces/<name>/scope.json`: `{name, path_globs, repos, tags
(workflow/environment), consequence: normal|high}`. Authored via `entire brain
scope add|list|rm` or by an agent; no steady-state human.

**C2. Enforcement (deterministic, read-path only).**
- Facts resolve to owning scopes by matching `Locus` against `path_globs`
  (file tokens directly; symbol tokens via the sem symbol→file store).
- Within one repo: a fact whose scope doesn't intersect the task's scope
  (derived from the files in play) ranks down in `brief`/`recall`; it is not
  hidden — cross-scope suppression is a ranking prior, not censorship.
- Across repos (`workspace` surfaces): facts do **not** cross repo boundaries
  unless (a) classified `cross-cutting` by the existing `facts_locus.go`
  tiering, or (b) their scope record is shared by both members. This is where
  silent lesson-generalization actually bites, and it's enforceable with data
  already present.
- `consequence: high` scopes flip the trigger gate conservative for matching
  tasks: never silent-skip, and drag-demotion for facts in that scope requires
  the stronger signals (contradicted/reverted, not ignored).

**C3. Sem-assisted suggestions.** `workspace inspect graph` already computes
shared contracts between member repos; emit suggested scope records from those
boundaries (`scope suggest --json`). Suggestion only — writing them stays an
explicit command.

## Feature D — Autonomous decay and refresh

A fact is never "approved"; it is **recently evidenced or it fades**. Evidence
comes from four sources: anchors (brain), locus vs code (sem + git),
corroboration (history/docs/reconcile), and usage outcomes (Feature A).

**D1. TTL by kind** (defaults in `brain.json`):

| Kind | valid_until basis | Rationale |
| --- | --- | --- |
| invariant, gotcha, convention (with locus) | 45d or any locus-touch event, whichever first | Code-referent facts rot with the code. |
| decision, closed-negative | no time TTL; event-driven only (contradiction, orphaned referent) | Decisions don't expire by calendar; they get superseded. |
| preference / cross-cutting (locus-less) | 180d soft; **never demoted by time alone — drag only** | Sponsor decision: slow decay, drag-driven demotion. Time expiry only queues them for rung-2 re-validation, never for quarantine. |

**D2. The validation ladder** (each fact due for validation climbs until a rung
decides; runs inside `watch`/`refresh`):

- **Rung 0 — deterministic renew (free).** All of: every anchor `verify`s
  non-orphaned; every locus file token exists (existing `factLocusDrift`);
  locus untouched since last validation — resolve locus symbols to files via
  the sem store, then `git log <last_validated_commit>..HEAD -- <files>` is
  empty; no reconcile contradiction since. ⇒ `validated/renewed`, extend
  `valid_until`, stamp current commit. Expected to clear the large majority.
- **Rung 1 — deterministic re-anchor (free).** Locus was touched, but
  mechanical checks still pass: symbols still exist in the current snapshot
  (moved files re-resolve and update `Locus`); the fact is not about dead code
  (`inspect dead-code`); and, when the fact carries a **structural assertion**
  (D4), the assertion re-executes true. ⇒ `validated/reanchored` with a fresh
  provenance anchor at the current commit.
- **Rung 2 — agent-gated re-validation (budgeted).** Only facts that failed
  rungs 0–1, batched, run under the `watch` distill gate (same
  interval/model/effort/budget accounting; decay validation and distill share
  one budget so a tick can't double-spend). The agent gets the fact text, the
  *current* code at its (re-resolved) locus, and the most recent related
  history records; strict JSON verdict: `reassert` (renew + fresh anchor),
  `rewrite` (supersede with corrected text, normal reconcile path), `retract`,
  or `unsure`.
- **Rung 3 — quarantine, not deletion.** `unsure`, rung-2 budget exhaustion
  (fact stays past `valid_until` with failed rungs 0–1), or drag over
  threshold ⇒ `Status: quarantined`: excluded from `brief`/hook injection and
  ranked last in `recall` with an explicit `stale: referent changed` marker
  (extends the existing drift-flag UX). Quarantined facts re-enter through any
  rung passing later (locus restored, agent reasserts) — quarantine is
  reversible. `retract` + existing `gc` remain the only destructive ends, and
  only on confirmed contradiction or permanently gone referent.

**D3. Locus-less facts (the honest weak spot, per decision).** They skip rungs
0–1 (nothing to check mechanically). Time expiry only marks them
`revalidation-due`; they are demoted solely when drag crosses threshold
(contradicted/reverted-weighted — `ignored` alone is *not* sufficient for
locus-less facts, since locus overlap is undefined for them; use contradicted +
reverted only). Rung 2 may re-validate them against recent history records
(behavioral conformance) when budget allows, lowest priority.

**D4. Structural assertions (stretch, high leverage).** Optional
`check` field on a fact: a sem query + expected outcome (e.g. `query-graph`
match count == 0 for "A never imports B"; `trace-path` exists for "X is called
via Y"). Distill may propose one when the fact text is graph-shaped; `verify`
and rung 1 execute it. Turns the strongest invariants into facts that can
*prove themselves* forever, token-free.

---

## Phasing

**Phase 1 (deterministic core, no agent, no behavior change to reads):**
vitality ledger + serve logging (A1); decay rungs 0–1 wired into
`refresh`/`watch` with `facts vitality`/`facts status` reporting; scope records
+ `scope add/list/suggest` (C1, C3). Everything observable, nothing enforced.

**Phase 2 (evidence + gating):** episode-outcome join and drag rollup (A2);
`brief` trigger gate behind `--gate` with silence telemetry and scenario tests
(B); scope ranking priors within-repo (C2 partial). Quarantine status exists
but only drag/rung outcomes from real data populate it.

**Phase 3 (autonomy + enforcement):** rung-2 agent validation under the shared
watch budget; quarantine enforced in `brief`/hooks; workspace cross-repo scope
enforcement; missed-useful tuning report; D4 assertions if Phase 1–2 metrics
justify.

Gate between phases: run Phase 1–2 in observe-mode on ≥2 real repos and check
(a) rung-0 clears ≥70% of due facts, (b) drag flags fewer than ~5% of served
facts, (c) missed-useful rate under the gate's thresholds is acceptable. If (b)
explodes, the outcome join is mislabeled — fix before enforcement.

## Testing & guarantees

- All Phase 1–2 paths deterministic and offline; rung 2 obeys the existing
  no-egress modes (`--agent none` disables it; Ollama loopback allowed).
- Scenario fixtures for: serve→outcome joins (seeded sessions), gate
  silent-skip/surfaced expectations, rung transitions (touch a locus file in a
  fixture repo → rung 0 fails, rung 1 re-anchors), quarantine reversibility.
- `facts eval` gains a `--vitality` slice: does usefulness-weighted ranking
  beat the current ranking on the labeled benchmark? Adoption of ranking
  changes requires the same paired-proof bar as the embedder work.
- Never-fails rule everywhere: a broken vitality ledger degrades to current
  behavior with a one-line stderr notice.

## Risks / open questions

- **Outcome attribution is the load-bearing inference** (served ∧ episode
  reinforced ∧ locus overlap ⇒ useful). It will be wrong sometimes; that's
  tolerable because consequences are graded (ranking prior → quarantine →
  never auto-delete) and reversible.
- **Feedback loops:** drag-demotion + silence gate can starve a fact of the
  exposure needed to prove usefulness. Mitigation: epsilon exposure — a small
  fraction of `recall` results may include one quarantine-adjacent fact,
  marked as such (bounded exploration).
- **Locus quality decides rung-0/1 power.** The staleness v1 comment is right
  that symbol tokens need the worktree overlay seam; sequence this plan after
  (or with) that seam so symbol loci resolve honestly.
- **Multi-branch:** vitality and validation are branch-scoped like facts;
  `facts promote --from` must carry or reset vitality (proposal: carry
  outcomes, reset valid_until).
