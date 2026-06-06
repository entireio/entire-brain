# Durable Facts Plan

This plan describes how Entire Brain can grow from a read-only derivation layer
over captured sessions into a curated, branch-aware, blameable **durable fact
memory** — without abandoning the property that makes Entire's data trustworthy:
every fact is a view over retained, signed source, not a standalone artifact you
have to trust.

Entire already captures more raw context than most agent-memory systems: full
session transcripts, checkpoints tied to commits, multiple agents, rewind and
resume. Entire Brain already *derives* read-only history records and decisions
from that corpus. What is missing is a distilled, deduplicated, agent-writable
fact layer that survives across sessions, is scoped to a branch, and can be
traced back to the exact signed turn it came from.

## Core Principle: Derive And Cite, Never Extract And Discard

Other agent-memory systems extract a fact with an LLM and keep only the
extracted text. The source turn is gone, so the fact cannot be re-derived,
audited, or verified — it must simply be trusted.

Entire retains signed ground-truth sessions. Therefore every durable fact in
this design is a **view over retained, signed source**:

- **Re-derivable** — a fact can be regenerated from its originating turns.
- **Blameable** — a fact points to the session, commit, and turn it came from.
- **Verifiable** — a fact's provenance is checked against Entire's existing
  checkpoint signatures.

This gives the full value of curated, versioned, branch-aware memory *plus*
auditability that extract-and-discard systems structurally cannot offer. Entire
Brain does not need its own Merkle/cryptographic store: checkpoint signing
already anchors the source; the brain only has to cite it.

## Goals

- Add an agent-writable, curated fact store (`remember` / `recall`) layered on
  the existing brain, with the same on-disk inspectability as other brain
  sources.
- Add a batch `distill` pass that turns captured Entire sessions into durable
  facts under a strict quality gate, instead of relying on per-turn capture.
- Classify facts into a stable, shallow taxonomy so `brief`, `recall`, and
  `inspect decisions` stay precise as the corpus grows.
- Scope facts to a branch and promote/merge them when the code branch merges.
- Give every fact provenance: blame to the originating signed turn, and verify
  against the existing checkpoint signature.
- Add embedding-based recall over the fact layer only (a small, high-value
  corpus), keeping the brain's "not a code vector DB" stance intact.

## Non-Goals

- Do not introduce a separate cryptographic versioned store. Provenance reuses
  Entire's checkpoint signing.
- Do not make embeddings mandatory. Keyword and taxonomy recall must remain
  fully useful with no model and no network.
- Do not build a browser or graph UI for facts. CLI, JSON, and MCP only.
- Do not turn the fact store into a general user-preference store decoupled from
  the repository. Facts are repo-scoped knowledge, derivable from or cited to
  repository work.
- Do not publish, hydrate, or serve fact data over the network. The local-only
  boundary from the Semantic Brain Plan applies unchanged.

## Concepts

A **fact** is one durable, self-contained statement about the repository or how
work on it should be done: a resolved decision (with its *why*), a standing
rule, a stated preference that affects the work, a non-obvious invariant or
constraint. Ephemeral task state, tool mechanics, and anything already
discoverable from code, git log, or README are explicitly not facts.

Each fact carries:

- `id` — stable content-derived identifier.
- `paths` — one or more taxonomy paths (see Taxonomy).
- `text` — the statement (third person when about the user).
- `branch` — the branch the fact belongs to.
- `provenance` — list of source anchors: `{session_id, commit, turn_id,
  checkpoint_id}`, each pointing at a signed checkpoint turn.
- `origin` — `distilled` (from sessions) or `authored` (from `remember`).
- `confidence` and `status` — `active`, `superseded`, `retracted`.
- `related_ids` — sibling facts when one statement spans two taxonomy paths.

Facts are stored as inspectable files in the durable brain directory, branch
overlays included, consistent with existing brain sources:

```
facts/
  taxonomy.json                 # active taxonomy snapshot
  <branch>/
    facts.ndjson                # one fact per line
    embeddings/                 # optional, fact-layer only
```

## Taxonomy

A shallow, stable taxonomy keeps recall precise. Paths are exactly three
segments — `category.subcategory.type` (for example
`preferences.coding.style`, `architecture.boundaries.rationale`) — lowercase
letters, digits, and underscores only. A fact may be stored under at most two
paths when it genuinely belongs to two top-level categories.

The taxonomy ships with a small default set embedded under `internal/cli/data/`.
In Phase A the **top-level categories are fixed** by that default set; the
classifier may invent a new *three-level* path only under an existing top-level
category. User-extendable top-level categories are a later phase. The brain owns
`taxonomy.json` and regenerates it from the embedded default on `refresh`.

Classification is performed by the seed agent (Codex, then Claude Code) as part
of distillation — it is not a separate deterministic step. The no-agent path
does not classify or distill at all (see Distillation Model); it leaves the
deterministic decision extractor as the fallback knowledge source.

Taxonomy drift is handled conservatively. Facts store their assigned paths
literally. If the taxonomy changes between runs and a fact's top-level category
still exists, the fact remains valid. A fact whose top-level category no longer
exists is reported as an orphan in `status` and is never auto-deleted; it is
re-pathed only when distillation re-derives it or the user re-`remember`s it.

## The Quality Gate

Distillation and authored capture both pass through one quality gate whose
**default outcome is to record nothing**. A turn produces a fact only when all
of the following hold:

1. The fact is durable — still relevant weeks later.
2. A future session genuinely benefits from it.
3. It is not already discoverable from code, git log, or README.
4. It is a resolved decision, standing rule, stated preference, project fact, or
   non-obvious constraint — not ephemeral state, tool mechanics, or chit-chat.

In-flight or deferred decisions ("TBD", "pending review", "come back to this")
are skipped until they resolve. A standing rule phrased as feedback ("from now
on…", "don't do X anymore") is durable and is captured. The gate is the
distillation prompt itself (`templates/entire-brain-distill.md`), capped at a
small number of facts per source chunk to avoid pollution. It is therefore an
agent-only gate; without an agent there is no distillation (see below).

## Distillation Model

Distillation is the engine behind `distill` and `refresh`. Its shape is chosen
to bound cost and to make turn-level provenance free.

- **Unit: session, chunked by token budget.** The agent is given a
  line-numbered transcript chunk for one session and emits facts, each citing
  the source line offset. That line offset *is* the turn-level provenance — no
  per-turn LLM call is needed. One agent invocation per chunk means a full
  rebuild is on the order of the session count (~hundreds of calls), not the
  turn count (tens of thousands).
- **Agent required.** Distillation needs the seed agent (Codex, then Claude
  Code). With no agent, `distill` is a no-op and the deterministic decision
  extractor remains the knowledge source (see Decisions Are Distilled Facts).
- **Incremental by default; full rebuild only on `--force`.** Incremental runs
  skip sessions whose fingerprint is unchanged (reusing the history index's
  session-fingerprint approach). `refresh --force` / `distill --force` recompute
  all `origin=distilled` facts from scratch. These are not in tension: one is
  the steady-state path, the other the rebuild path.
- **Chronological processing.** Both paths process sessions in `created_at`
  order so supersession chains reconstruct deterministically regardless of which
  path ran (see Supersession in Appendix A).
- **Off by default in `refresh`.** Because it costs tokens and needs an agent,
  `refresh` does not distill unless given `--distill`. `entire brain distill`
  runs it directly.

## Commands (Entire Brain)

- `entire brain remember "<fact>" [--path <a.b.c>] [--branch <b>]` — author a
  fact. Without `--path`, the seed agent classifies it; with no agent, `--path`
  is required. Records `origin=authored`. Provenance points at the current HEAD
  checkpoint when one exists; if HEAD has no checkpoint (dirty worktree, capture
  off), the anchor records the commit only and the fact is still stored.
- `entire brain recall "<query>" [--branch <b>] [--k N] [--scope
  local|cross-cutting] [--expand] [--all] [--json]` — retrieve facts. Keyword +
  taxonomy + code-locus ranking by default (see Recall Ranking and Appendix D);
  `--scope` restricts to code vs how-we-work facts; `--expand` has the agent
  rewrite the query into the facts' vocabulary first. Default `k=10`,
  `active`-only unless `--all`. Embedding rerank is still Phase D (not shipped).
- `entire brain distill [--since <ref>] [--branch <b>] [--force] [--json]` —
  batch distillation over captured sessions for the current branch (see
  Distillation Model). Idempotent: facts collapse by content-derived id.
- `entire brain facts review [--branch <b>]` — interactive resolution of
  low-confidence supersession proposals queued by `distill` (see Appendix A,
  Supersession). Non-interactive `--json` lists pending proposals.
- `entire brain facts promote --from <branch> [--into <branch>]
  [--strategy keep-both|prefer-source|prefer-target]` — explicitly carry a
  branch's `active` facts into another branch. There is no automatic
  promote-on-merge in this plan (see Branch Scoping And Promotion).
- `entire brain facts gc [--branch <b>] [--force]` — prune `retracted` facts and
  `superseded` facts older than a retention window, and report orphaned-branch
  and orphaned-taxonomy facts. Parallels the semantic `gc`.
- `entire brain inspect facts "<query>" [--json]` — read-only listing, parallel
  to existing `inspect decisions`.
- `entire brain inspect blame <fact-id> [--json]` — show the originating
  session, commit, and checkpoint anchors for a fact (Phase A: checkpoint
  granularity; Phase B adds the turn anchor).
- `entire brain facts retract <fact-id> [--branch <b>]` — mark a fact no longer
  true (status `retracted`, auditable, not deleted); `facts gc` prunes it later.

Shipped beyond the original Phase A list, from the Appendix D fact-quality work:

- `entire brain facts tree [--path <prefix>] [--depth N] [--scope ...]` — a
  navigable hierarchy with progressive disclosure (the distinct-fact header over
  per-node occurrence counts; see Appendix D).
- `entire brain facts eval-gen [--refine] [--max-facts N]`, `facts eval
  [--judge] [--expand]`, and `facts eval-compare --a --b` — the retrieval
  evaluation harness: a provenance-labeled (optionally judge-refined) benchmark,
  per-stratum precision/recall/useful-per-1k metrics, and a paired t-test with
  Holm correction for honest A/Bs.

`entire brain verify` is Phase B (see Phasing): full anchor verification needs
the turn-level signed anchors from Entire CLI change #1.

`brief` includes the top matching `active` facts for the task in a section
separated from derived history, sized to the requested `--limit` so a compact
request stays compact (not a fixed block). `status` reports fact counts per
branch, pending supersession proposals, and orphan counts.

## Decisions Are Distilled Facts

The brain already derives read-only "decisions" from transcripts with a
deterministic keyword extractor that emits line-referenced **excerpts**.
Distilled facts come from the same transcripts but are self-contained,
classified, deduplicated statements that passed the quality gate. These are not
two parallel knowledge sets — they are the same concept at two quality levels.

The decision: **distillation is the canonical decision source when an agent is
available; the deterministic excerpt extractor is the no-agent fallback.** A
distilled fact under a decision taxonomy path (for example
`architecture.boundaries.rationale`) *is* a decision. `brief` and
`inspect decisions` read from the fact layer when facts exist for the branch,
and fall back to the deterministic extractor otherwise — never both, so the same
rationale is never double-counted. This is distinct from `entire recap`,
`dispatch`, and `activity`, which are user-readable, time-bound summaries rather
than the brain's agent-readable context graph; those are unaffected.

## Recall Ranking

Phase A ranking reuses the existing `history.go` term-scoring machinery so facts
and history rank consistently. A fact's score combines:

- taxonomy-path match against the query terms,
- term overlap between the query and the fact text,
- recency (`updated_at`) as a tiebreak.

`recall` returns `active` facts for the current branch plus any facts promoted
into it, default `k=10`. `brief` caps facts at ~6 and presents them separately
from history. Embedding rerank (Phase D) replaces only the scoring step; the
filtering and branch rules are unchanged.

## Redaction And Export

Facts are distilled from the transcripts the brain already stores, which Entire
CLI redacts **at capture time** (the `redact` package: PII detection and secret
packs). Secrets therefore never reach the transcript and cannot reach a fact;
the fact layer needs no separate prose secret-scanner. Distillation always runs
over the brain's stored (capture-redacted) transcripts, never over un-redacted
raw agent logs.

`.brainignore` is path-level and governs the semantic index, not free prose, so
it cannot reliably scrub an ignored path mentioned inside a fact sentence.
Because of that, and because facts are distilled user decisions and preferences,
**the fact layer is treated as local data and is not bundle-exportable in Phase
A** — consistent with the bundle work's existing refusal to export local audit
and derived data. Cross-machine fact sharing, if ever wanted, is a later phase
with explicit sanitization.

## Changes Needed In Entire CLI

The platform already captures more than enough raw data. The fact layer is fully
functional with **no** CLI changes (Phase A). Two additive, backward-compatible
surfaces unlock later phases; neither changes Entire's capture model.

1. **Turn-level signed anchors (enables Phase B).** Entire already signs
   checkpoints (`./entire-cli/docs/architecture/checkpoint-signing.md`). Expose a stable,
   addressable anchor for a turn within a checkpoint — `{session_id, commit,
   turn_id, checkpoint_id}` — and a way to verify that anchor against the
   checkpoint signature. This is the substrate for turn-level `blame` and for
   `verify`. It is an extension of existing signing, not a new mechanism.

   - New/extended read API (CLI + JSON): `entire checkpoint anchor <turn>` and
     `entire checkpoint verify <anchor>`.
   - Anchors must be resolvable from the exported session data the brain already
     reads, so verification works offline against local refs.

2. **A first-class derived-knowledge store contract (cleanup, optional).** A
   documented plugin-storage convention under `ENTIRE_PLUGIN_DATA_DIR` with
   `branch` and `provenance` as first-class fields, so the brain and future
   plugins share branch-overlay and provenance semantics rather than each
   reinventing them. This is documentation and convention, not new runtime
   behavior.

A platform **merge event/hook was considered and rejected** for now: local
`post-merge` hooks miss squash-merged PRs landed on the remote and then pulled,
and the squashed commit makes the source branch's commits unreachable. Relying
on it would fire inconsistently and produce false provenance gaps. Branch
promotion is therefore manual (see below) until a reliable merge signal exists.

## Branch Scoping And Promotion

Facts are derived onto the current branch's overlay. There is no automatic
promote-on-merge. The supported workflow is that the user (or their agent) runs
`entire brain refresh` on `main` and on the branch they are working on; facts
accrue on whichever branch was indexed. Carrying a branch's facts into another
branch is the explicit `facts promote` command, with one of three strategies:

- `keep-both` (default) — copy source `active` facts in; on a same-path conflict,
  keep both and mark them `conflicting` for later review.
- `prefer-source` — source facts win same-path conflicts.
- `prefer-target` — target facts win same-path conflicts.

A "conflict" here means two `active` facts at the same taxonomy path whose
content-derived ids differ (identical facts collapse by id, so they never
conflict). This is the context-contamination defense: experimental-branch
decisions do not appear in `brief` on `main` until promoted there.

## Embedding Recall (Optional, Fact-Layer Only)

The fact corpus is small and high-value, so an embedding index over facts is
cheap and sharpens `recall` and `brief` without making the brain a code vector
database. The index lives under `facts/<branch>/embeddings/` and is rebuilt by
`distill`/`refresh`. It is strictly optional: when absent or when no embedder is
available, `recall` uses keyword and taxonomy matching. Consistent with the
Semantic Brain Plan, any embedding model must run with a local-only backend in
Phase 1.

## Freshness And Provenance Reporting

Fact freshness reuses the existing freshness model. A fact whose originating
commit is no longer reachable in local refs is surfaced in `status` and
downgraded in `brief`. Phase A reports anchors at checkpoint granularity only;
cryptographic per-anchor results (`verified` / `unsigned` / `tampered`) arrive
with `verify` in Phase B once turn-level signed anchors exist. Until then the
brain never claims `tampered` — an anchor it cannot cryptographically check is
reported `unsigned`, so the absence of turn-level signing never produces a false
tamper signal.

## Phasing

> **Status (shipped):** Phase A is complete — fact store, distill with
> agent-judged merge/supersede reconcile, `remember`/`recall`/`inspect facts`/
> `inspect blame`/`facts review`/`promote`/`gc`/`retract`, and brief/status
> integration. Several Appendix D structural pieces also shipped: `facts tree`,
> scope tiering and code-locus ranking (`recall --scope`), agent query expansion
> (`recall --expand`), and the evaluation harness (`eval-gen`/`eval`/
> `eval-compare`). Still open: embeddings (Phase D, pending a local backend), the
> synthesized hierarchy summaries, and turn-level signing (Phase B, needs Entire
> CLI changes). Measured so far on a small corpus: query expansion is a medium,
> consistent, zero-token-cost effect (Cohen's d ~0.47) but not yet significant
> at n≈8 — the case for growing the benchmark.

- **Phase A (Entire Brain only, no CLI changes):** fact store, `remember` /
  `recall` / `inspect facts` / `inspect blame`, the quality gate, the taxonomy,
  `distill` (agent-required) over captured sessions with checkpoint-level
  provenance, `facts review`, `facts promote`, `facts retract`, and `facts gc`.
  Branch scoping is per indexed branch; promotion is manual.
- **Phase B (with CLI change #1):** turn-level `blame` and the `verify` command,
  anchored to signed checkpoints. **Plus the fact-quality and structure work**
  driven by the Phase A output analysis: locus-indexed facts, scope tiering, and
  a synthesized hierarchy, validated by the audience-driven evaluation loop. See
  Appendix D — this is the highest-leverage Phase B work, not the signing.
- **Phase C (with CLI change #2):** the shared derived-knowledge store contract,
  extended so the locus/kind index and synthesized hierarchy from Appendix D are
  part of the contract other plugins consume.
- **Phase D (optional):** fact-layer embedding recall — now scoped to retrieve
  within a code locus / hierarchy node rather than over a flat per-branch list.

Each phase is independently useful and ships behind the existing local-only
boundary.

## Appendix A: Phase A Data Model

Phase A adds one new brain source, `facts`, alongside the existing `seed`,
`sessions`, `semantic`, and `history` sources. It introduces no Entire CLI
changes; provenance is recorded at checkpoint granularity using data the brain
already exports.

### On-Disk Layout

```
facts/
  manifest.json                 # factSourceManifest
  taxonomy.json                 # factTaxonomy (active snapshot)
  <branch>/
    facts.ndjson                # one factRecord per line, append-then-compact
    embeddings/                 # Phase D only, fact-layer vectors
```

Like the history index, `facts.ndjson` is a regenerable derived artifact: it is
never required to be committed to the repository and is rebuilt by `distill`.
Authored facts (`origin=authored`) are preserved across rebuilds by id; only
`origin=distilled` records are recomputed.

### Types

Field names and tag conventions match the existing brain sources
(`historyRecord`, `semanticSourceManifest`). New code lives in
`internal/cli/facts.go` with tests in `internal/cli/facts_test.go`.

```go
const (
	factsDirName          = "facts"
	factsManifestFileName = "manifest.json"
	factsTaxonomyFileName = "taxonomy.json"
	factsFileName         = "facts.ndjson"
	factsMaxLineBytes     = 64 * 1024
	factsMaxPerChunk      = 6 // quality-gate output cap per source chunk
)

// factSourceManifest is recorded under sources.facts in the brain manifest,
// parallel to historySourceManifest and the semantic source metadata.
type factSourceManifest struct {
	GeneratedAt    time.Time `json:"generated_at"`
	TaxonomyPath   string    `json:"taxonomy_path"`
	Branches       []string  `json:"branches,omitempty"`
	Facts          int       `json:"facts"`
	Distilled      int       `json:"distilled"`
	Authored       int       `json:"authored"`
	Superseded     int       `json:"superseded"`
	Verified       int       `json:"verified"`
	Unsigned       int       `json:"unsigned"`
	TurnsScanned   int       `json:"turns_scanned"`
	TurnsDistilled int       `json:"turns_distilled"`
	Warnings       []string  `json:"warnings,omitempty"`
}

// factRecord is one durable, self-contained statement. The id is content
// derived (sha256 of normalized text + sorted paths) so re-distilling a turn
// is idempotent and dedupe is a map lookup.
type factRecord struct {
	ID         string          `json:"id"`
	Paths      []string        `json:"paths"`           // 1-2 taxonomy paths
	Text       string          `json:"text"`            // third person about the user
	Branch     string          `json:"branch"`
	Origin     string          `json:"origin"`          // "distilled" | "authored"
	Status     string          `json:"status"`          // "active" | "superseded" | "retracted"
	Confidence string          `json:"confidence,omitempty"` // agent-set; gates auto-supersede
	Provenance []factAnchor    `json:"provenance"`      // >=1; signed source turns
	RelatedIDs []string        `json:"related_ids,omitempty"`
	SupersededBy string        `json:"superseded_by,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

// factAnchor cites the source a fact was derived from or authored against.
// In Phase A only the checkpoint-level fields are populated. TurnID is filled
// in Phase B once Entire CLI exposes turn-level signed anchors; Verified is
// set by `verify` against the checkpoint signature.
type factAnchor struct {
	SessionID    string `json:"session_id"`
	Commit       string `json:"commit,omitempty"`
	CheckpointID string `json:"checkpoint_id,omitempty"`
	TurnID       string `json:"turn_id,omitempty"`   // Phase B
	Transcript   string `json:"transcript,omitempty"` // brain-relative path
	Line         int    `json:"line,omitempty"`       // turn offset in transcript
	Verified     bool   `json:"verified,omitempty"`   // set by `verify`
}

// factTaxonomy is the active taxonomy snapshot. Paths are validated against
// factPathPattern; classification may only invent a new three-level path under
// an existing top-level category.
type factTaxonomy struct {
	GeneratedAt time.Time         `json:"generated_at"`
	Categories  map[string]string `json:"categories"` // top-level -> description
	Paths       []factPathDef     `json:"paths"`
}

type factPathDef struct {
	Path        string   `json:"path"` // matches ^[a-z][a-z0-9_]*(\.[a-z0-9_]+){2}$
	Description string   `json:"description"`
	Examples    []string `json:"examples,omitempty"`
}
```

### Identity, Dedupe, And Supersession

- `ID = sha256(normalize(text) + "\x00" + strings.Join(sortedPaths, ","))`,
  hex-truncated like existing brain ids. Exact duplicates collapse by id for
  free — re-distilling a turn that yields the same statement is a no-op, and the
  duplicate's provenance anchors union into the existing fact.
- **Near-duplicates and contradictions are the agent's judgment, not a string
  heuristic.** A token-overlap threshold cannot tell a restatement ("use tabs")
  from a reversal ("use spaces") — both overlap heavily. So during distillation
  the agent is given the existing `active` facts at the candidate's path(s) and
  emits an explicit action per fact: `new`, `merge <id>` (same meaning,
  consolidate), or `supersede <id>` (contradicts/replaces an older fact), each
  with a `confidence`.
- Supersession never deletes: the older fact is marked `superseded` with
  `superseded_by` set — supersession is itself auditable, and `facts gc` prunes
  it only after a retention window.
- **Confidence gates application.** A `supersede`/`merge` at or above the
  configured confidence threshold applies automatically. Below it, the action is
  queued as a pending proposal (resolved via `facts review`) and both facts stay
  `active` and `conflicting` until the user decides — so a low-confidence
  machine judgment never silently rewrites memory.
- Because distillation processes sessions in chronological order, supersession
  evaluates each new fact against the active set as it stood at that point in
  time; a full `--force` rebuild replays the same order and reconstructs the
  identical chain.

### Provenance In Phase A

Each `distill` fact records one or more `factAnchor`s at checkpoint granularity,
drawn from the session manifest the brain already exports (`session_id`,
`commit`, `checkpoint_id`, `transcript_path`, and the line offset of the source
turn within the transcript). The line offset is captured in Phase A even though
the cryptographic turn anchor is not — so when Entire CLI change #1 lands,
existing facts already point at the right turn. In Phase A `inspect blame`
displays these anchors and `status`/`facts gc` flag facts whose commit is no
longer reachable in local refs. There is no `verify` command and no `tampered`
claim until Phase B adds signature-checked anchors, so the absence of turn-level
signing never produces a false tamper signal.

### Command-To-Type Mapping

| Command | Reads | Writes |
|---|---|---|
| `remember` | `taxonomy.json` | one `factRecord` (`origin=authored`) |
| `distill` | session transcripts, existing `facts.ndjson`, `taxonomy.json` | new/merged `factRecord`s (`origin=distilled`), `factSourceManifest` |
| `recall` / `inspect facts` | `facts.ndjson` (+ embeddings, Phase D) | — |
| `inspect blame` | `facts.ndjson` | — |
| `facts review` | `facts.ndjson` (pending proposals) | resolved `Status` / `SupersededBy` |
| `facts promote` | source + target `facts.ndjson` | merged target `facts.ndjson` |
| `facts gc` | `facts.ndjson`, `taxonomy.json`, local git refs | pruned `facts.ndjson` |
| `verify` *(Phase B)* | `facts.ndjson`, signatures | `Verified` flag on anchors |

## Appendix B: The Distill Prompt

The distillation quality gate is shipped as a template under `templates/`,
parallel to the existing intake templates, and is rendered with the active
taxonomy block before being passed to the seed agent (Codex, then Claude Code).
The no-agent path applies the same gate as a deterministic keyword filter. The
full template is `templates/entire-brain-distill.md`.

## Appendix C: Known Limitations And Sharp Edges (Phase A)

These surfaced while implementing and running Phase A. They are deliberate
trade-offs or accepted constraints, not defects; each is recorded so a future
change can revisit it intentionally.

- **Reconcile roughly doubles agent calls.** The per-chunk merge/supersede pass
  is on by default and adds one agent call per productive chunk (the chunk that
  yielded facts is compared against the branch's existing same-path facts). On
  large branches this is the dominant cost. It is the price of keeping the store
  from filling with near-duplicates; runs predating reconcile show the
  alternative (one focused 56-session branch distilled to ~1,270 facts, heavily
  restated). Disable it case-by-case only if cost outweighs dedup quality.

- **Reconcile compares candidates only against *existing* facts, not against
  each other.** Two near-identical facts emitted from the *same* chunk both land
  as `new` (they are only deduped on a later chunk/run, once one is "existing").
  Within-chunk dedup was left out to keep the reconcile prompt bounded and the
  unit one chunk.

- **`keep-both` promotion into a dense branch can queue a very large proposal
  set.** A same-path conflict queues one proposal per conflicting target fact,
  so promoting into a branch already holding hundreds of facts at a path
  produces O(source × same-path targets) proposals (observed ~1,100 from
  promoting two facts into a ~2,300-fact branch). Prefer `prefer-source` /
  `prefer-target` when promoting into a populated branch, or cap/summarize
  keep-both proposals in a later revision.

- **`--confidence 0` does not mean "auto-apply everything".** A threshold `<= 0`
  falls back to the default (0.75), so there is no value that disables gating to
  apply every merge/supersede unattended. Intentional for now (an explicit
  always-apply mode is a future flag if wanted).

- **Distilled facts have no commit anchor until Phase B.** Provenance for
  distilled facts records the session, checkpoint, transcript path, and chunk
  line, but not a commit SHA (only `remember` sets a commit). `inspect blame`
  therefore shows `commit=<unset>` for distilled facts, and the commit-reach
  freshness check has nothing to test for them until turn-level signed anchors
  land in Phase B.

- **Manifest chunk counts describe the last distill run only.** `chunks_scanned`
  / `chunks_distilled` are not updated by `remember` / `facts review` /
  `promote` / `gc` (those preserve the prior values), since those commands do not
  process transcripts.

## Appendix D: Fact Quality And Structure (Phase B/C)

Phase A proves the pipeline produces durable, provenance-anchored facts. It does
not prove they are in the *best shape to build on*. This appendix records what
the first real corpus looks like, the redesign it argues for, and a concrete
evaluation loop to drive that redesign. This is the highest-leverage Phase B
work.

### What the Phase A corpus actually looks like (measured)

From distilling six recent branches across two repos (entire.io and the CLI
monorepo), 524 facts:

- **The taxonomy under-discriminates.** 46–68% of every branch's facts fall into
  just two catch-all paths (`constraints.invariants.general` and
  `architecture.data.flow`). Path-based recall is coarse for the majority.
- **Cross-cutting facts re-distill per branch and never dedupe.** Zero facts
  share an id across the six branches: branch isolation plus content-derived ids
  mean repo/user-level facts (TDD process, review etiquette, formatting) are
  re-extracted, reworded, and stored again on every branch. This class grows
  linearly with branch count and dilutes the branch-specific signal.
- **Output is a flat list with no altitude.** 38–189 atomic facts per branch,
  unordered, unranked. The best ~15–20 carry most of the value; the long tail is
  low-marginal. There is no structure mapping facts to the change or to the code.
- **A few status facts leak the durability gate** ("is being implemented
  test-first", "remaining follow-up work includes…") — present-tense, stale in a
  month.

The high-signal subset (design rationale, invariants, gotchas) is genuinely
useful — better than git log or raw transcripts for the *why*. The structure
around it is the problem.

### Two audiences, two hard constraints

1. **Agents are the primary consumer.** The metric is *useful facts surfaced per
   task within a token budget*, not total facts. Volume is a cost, not a virtue.
   Retrieval must return a small, deduped set scoped to the code being touched.
2. **Humans are a secondary consumer who will not read thousands.** They need
   conciseness and hierarchy: facts must roll up into a navigable outline —
   "use spaces, not tabs" sits under coding-standards → formatting — readable
   top-down with drill-down, never a flat dump.

### Challenging Phase A's assumptions

- *"More facts = better memory."* Wrong for both audiences. Precision and
  token-efficiency dominate; prefer fewer, higher-altitude facts with the
  specifics nested beneath them.
- *"The branch is the scope."* The branch is provenance and recency, not the
  home. Merged knowledge should graduate to a **code locus** and be visible
  regardless of branch; branch-scoping is only for unmerged/experimental
  isolation.
- *"A fixed flat taxonomy is the index."* It conflates three axes. The right
  model is two keys plus a label: **WHERE** (code locus — a path/glob/package/
  symbol, derived from the repo structure, dynamic) and **KIND** (a small fixed
  set: decision / invariant / gotcha / preference / convention), with **topic**
  as a roll-up label. Today's single taxonomy mashes where + kind + topic into
  one string, which is why two buckets swallow most facts.
- *"Atomic facts are the deliverable."* They are the substrate (leaves). The
  deliverable is a synthesized, hierarchical outline whose section summaries roll
  the leaves up.

### Direction: locus-indexed facts under a synthesized hierarchy

- **Index every fact by (locus, kind);** topic/taxonomy becomes a secondary
  label. Tie locus into the existing semantic index so "changed files → relevant
  facts" works directly, and recall returns "the 8 facts about the package you
  are editing," not "200 facts on this branch."
- **Tier by the structure axis:** monorepo root → workspace/app/package →
  module → symbol. Cross-cutting facts live once at the root ("conventions");
  subsystem facts live at their package. No per-branch (or per-package)
  re-distillation of cross-cutting knowledge.
- **Synthesize a living hierarchical outline** (a generated, always-current
  conventions/architecture map): each node carries a concise summary that rolls
  up its children; leaves are atomic facts with provenance. An agent loads the
  root plus the relevant subtree for its task (progressive disclosure, bounded
  tokens); a human reads the outline and drills down.
- **This is the only thing that scales to large monorepos.** A flat global list
  is both unloadable (token budget) and unreadable (human). A tree mirroring the
  code structure lets both audiences scope to the slice they care about, keeps
  retrieval bounded, and keeps cross-cutting facts from duplicating per package
  or per branch.

### Phase B task: audience-driven evaluation and iteration loop

Build an evaluation harness and iterate fact production against the two
audiences, on a large monorepo (the CLI / entire.io) where the scaling pressure
is real. Treat fact count as a cost to minimize, not a goal.

1. **Metrics.**
   - *Agent (token-efficiency + lift):* over a set of held-out tasks ("review
     branch X", "where/how do I change Y"), measure precision@k and tokens spent
     by what `recall`/`brief` surfaces, and the **outcome lift** versus a
     no-facts control — does the fact set change the agent's plan, catch a known
     gotcha, or cut exploration. Target metric: **useful-facts-per-1k-tokens**
     and task-success delta, never fact count.
   - *Human (conciseness + hierarchy):* can a developer grasp a subsystem from
     ≤1 page / ≤N leaves; is the outline navigable; time-to-orient. Spot-check
     that leaves nest under the correct headings (the "spaces not tabs under
     coding-standards → formatting" test).
2. **A/B the structures on one corpus:** (a) flat Phase A facts, (b) locus-
   indexed + scope-tiered, (c) + synthesized hierarchical outline. Run the
   held-out tasks through each and compare on the metrics above.
3. **Iterate:** tighten the durability gate, raise altitude / merge where recall
   surfaces redundant low-value facts, evolve the locus/kind model, and
   re-measure. Loop until useful-facts-per-1k-tokens and human time-to-orient
   both improve and hold.
4. **Stress the monorepo case explicitly:** thousands of facts across many
   packages — verify retrieval stays bounded and scoped, the outline stays
   navigable, and cross-cutting facts do not duplicate per package or branch.

The deliverable of this task is not "more facts" but a structure and a retrieval
path that demonstrably help an agent finish a task in fewer tokens and let a
human orient in one screen.
