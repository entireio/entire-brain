# Pattern Consolidation Plan

This plan adds a pattern layer to Entire Brain: the brain notices repeated repository work, summarizes it with retained evidence, and forms selected patterns into reusable agent skills.

The design keeps the public surface small and task-oriented. Internal terms should support the brain metaphor without leaking implementation details into the user workflow.

> **Review note (2026-06-16).** This plan was reviewed against the codebase. Inline `> Review:` callouts mark places where the plan's assumptions about existing data do not match the code, plus design risks to resolve before building. A new **Phase 0** captures the highest-risk prerequisite (reinforcement classification). The single most important reuse target is the `distill` pipeline (`internal/cli/distill_cmd.go`, `facts.go`, `facts_locus.go`), which already implements the idempotent upsert, provenance anchoring, deterministic inference fallback, and agent abstraction this feature needs — see [Reuse From Distill](#reuse-from-distill).

## Vocabulary

| Term | Meaning |
| --- | --- |
| Episode | One user request, the agent work that followed, and the next user feedback signal. |
| Procedure | A repeated command, tool, file, or validation workflow. |
| Practice | A repeated judgment, review, diagnosis, or model-steering behavior. |
| Pattern | A recurring procedure or practice. |
| Reinforcement | User approval, correction, commit/checkpoint success, or other feedback. |
| Consolidation | The process of turning a recurring pattern into a durable skill candidate. |
| Pattern card | Evidence summary shown to the user before skill creation or update. |
| Skill memory | Registry of patterns already accepted, declined, or needing update. |
| Synapse | Internal-only weighted edge connecting episodes, facts, procedures, symbols, outcomes, and pattern evidence. |

Avoid anatomical CLI names. User-facing commands should say `patterns`, `form`, `status`, and `refresh`; `synapse` remains an internal data model term only.

## Goals

- Notice repeated procedures and practices from Entire session history.
- Show evidence-backed pattern cards before any write.
- Form selected patterns into reusable skills.
- Track accepted, declined, and changed patterns so the brain does not repeatedly propose the same work.
- Treat workspace-level patterns as first-class, not as a later extension.
- Reuse existing brain sources: sessions, history, facts, docs, semantic graph, workspace membership, write locks, and agent runner infrastructure.

## Non-Goals

- Do not add developer-only CLI commands for raw episodes or synapses.
- Do not make the feature dependent on hosted model calls for the deterministic refresh path.
- Do not overwrite user-edited skill files without explicit confirmation.
- Do not replace durable facts, semantic graph inspection, or retrieval. Patterns are a new layer built from those sources.

## Storage Model

Add a `patterns` source to the brain manifest:

```json
{
  "sources": {
    "patterns": {
      "generated_at": "2026-06-16T00:00:00Z",
      "episodes": 1234,
      "procedures": 42,
      "practices": 18,
      "patterns": 12,
      "skill_memory": 5,
      "fingerprint": "sha256:..."
    }
  }
}
```

Persist derived data under the brain directory:

```text
patterns/
  episodes.ndjson
  procedures.ndjson
  practices.ndjson
  patterns.ndjson
  synapses.ndjson
  cards/
    <pattern-id>.json
    <pattern-id>.md
  skill-memory.ndjson
```

Use the existing brain write lock and atomic write helpers. Treat all files as derived and rebuildable except `skill-memory.ndjson`, which is user-state and must be preserved across refreshes.

> **Review: manifest freshness signal.** There is no central `fingerprint` field on sources in the current manifest — freshness is tracked per-source with input-derived signals (e.g. `SessionsFingerprint` for history, `WorktreeHash` for semantic, the per-session fingerprint distill uses to skip cached work). Define the patterns staleness signal the same way: derive it from the inputs the layer actually consumes (sessions fingerprint + history index + facts presence), so `patterns status: current/stale` is computable without re-running detection. Follow the global write-lock discipline used elsewhere (`withBrainWriteLock` in `internal/cli/filelock.go`): the existing pipeline deliberately runs the transcript-writing phase *unlocked* between locked stages — mirror that shape rather than holding the lock across the whole pattern build.

## Episode Layer

Build episodes from exported session transcripts. An episode is the normalized unit used by pattern detection:

> **Review: episode unit does not exist yet, and its boundary is underspecified.** The brain has sessions and checkpoints (`exportSession` in `export.go`) and turn-level structure inside the v2 `transcript.jsonl`, but no "episode" concept. The plan defines an episode as "request + agent work + *next user feedback signal*" — specify exactly where that boundary falls (within one checkpoint? across checkpoints in a session?) and which record supplies the "next feedback" turn. History extraction (`classifyHistoryFragment` in `history.go`) does produce a `request` kind that can anchor the request side; reuse it rather than re-parsing transcripts.

```json
{
  "id": "episode:...",
  "repo_key": "gh/ashtom/entire-brain",
  "workspace": "platform",
  "session_id": "...",
  "checkpoint_id": "...",
  "branch": "main",
  "author": "Thomas Dohmke",
  "agent": "Codex",
  "intent": "review release readiness",
  "intent_signature": "review:release",
  "tool_sequence": ["search", "read", "edit", "test"],
  "command_sequence": ["go test ./...", "gofmt -w ."],
  "files": ["internal/cli/refresh.go"],
  "reinforcement": "success",
  "source": {
    "path": "sessions/main/...",
    "line": 123
  }
}
```

Episode extraction should:

- Count only actual agent tool calls and executed commands, not examples in prose.
- Preserve source anchors into retained transcripts.
- Classify reinforcement from the next user turn and available Entire checkpoint/session metadata.
- Keep branch, repo, author, agent, and workspace identity on every episode.
- Be deterministic and safe to run during `refresh`.

> **Review: reinforcement classification has no existing foundation — this is the linchpin, see [Phase 0](#phase-0-reinforcement-classification-spike-prerequisite).** No code today labels a turn or checkpoint as `success` / `corrected` / `neutral`. Checkpoint metadata (`checkpointExportSession`) carries `Error`, `TokenUsage`, `Summary` but no outcome. Facts have a `Status` lifecycle (active/superseded/retracted), which is not a feedback signal. Yet reinforcement feeds the strength formula, the card header ("12 success, 2 corrected"), and the skill-memory `reinforcement` block. Do not treat "classify basic reinforcement" as a plumbing bullet — build and validate the rubric in Phase 0 first.

## Synapse Graph

Use synapses internally to preserve why a pattern exists:

```json
{
  "from": "episode:abc",
  "to": "procedure:def",
  "kind": "supports",
  "weight": 0.82,
  "evidence": "sessions/main/...:123"
}
```

Initial synapse kinds:

- `supports`: an episode supports a procedure, practice, fact, symbol, or pattern.
- `reinforces`: a reinforcement signal strengthens or weakens a procedure/practice.
- `mentions`: an episode refers to a file, symbol, command, or fact.
- `varies`: a repo-specific procedure variant belongs to a workspace-level pattern.
- `contradicts`: a correction or later fact weakens a prior pattern interpretation.

Do not expose synapses as commands. They are internal evidence edges used for scoring, cards, and future debugging through files/tests.

> **Review: defer the persisted synapse store for v1.** A standalone `synapses.ndjson` edge store is real build-and-maintain cost, and the plan itself scopes it as internal-only and partly for "future debugging." Patterns already carry supporting-episode lists and source anchors, which cover scoring and cards. Keep evidence as anchors on the pattern record until something actually consumes edges (e.g. cross-pattern contradiction reasoning), then add the store. This drops a whole consistency surface from the early phases.

## Pattern Detection

Detect two pattern families.

### Procedures

Procedures come from repeated operational shapes:

- command n-grams within an episode
- tool-sequence shapes
- repeated file/path clusters
- validation loops
- branch, release, review, or benchmark workflows

### Practices

Practices come from repeated judgment and interaction:

- durable facts with kinds such as `preference`, `convention`, `gotcha`, `decision`, `invariant`, and `closed-negative`
- history records around validations, corrections, and reviews
- conversational or read-only episodes
- repeated user steering or verification expectations

> **Review: two source-shape corrections here.**
> - **Fact kinds are six, not five.** The code (`facts_locus.go`) defines `decision`, `invariant`, `gotcha`, `preference`, `convention`, `closed-negative`. The plan omitted `invariant` (must-hold rules) — a strong practice signal — now added above.
> - **History has no `correction` or `review` kind.** `classifyHistoryFragment` (`history.go`) emits `request`, `decision`, `learning`, `validation`, `architecture`, `tool_call`, `code_fact`. Only `validation` exists; corrections and reviews are not modeled. So "Failure modes from corrections" (used in cards and skill bodies) depends entirely on the Phase 0 reinforcement classifier, not on existing history records — state this dependency explicitly.
> - **Facts are LLM-distilled and frequently absent.** They are produced by the separate, token-heavy `entire brain distill` command, not by `refresh` (see [Refresh Integration](#refresh-integration)). Practices must degrade gracefully to an empty (not broken) result when no facts exist, so the deterministic/token-free guarantee for the refresh path holds.

Each pattern receives a stable id, type, scope, strength, source anchors, supporting episodes, and variant metadata.

> **Review: pattern-id stability is a hard requirement, not a nice-to-have.** `skill-memory.ndjson` is keyed on `pattern_id` and is the one file preserved across refreshes. If an id changes when unrelated episodes are added, every skill-memory linkage breaks silently. Mirror the fact-id scheme exactly: a content hash over the *identity-defining* fields only — `sha256(normalize(canonical signature) + \x00 + scope + \x00 + repo_or_workspace_key)` — and explicitly exclude volatile fields (episode counts, recency, strength) from the hash. Document the invariant: a pattern's id MUST NOT change when supporting episodes are added or reinforcement shifts.

Strength should include:

```text
recurrence
+ span over time
+ recency
+ specificity
+ reinforcement quality
+ repo count
+ author count
+ branch count
```

> **Review: this is a wish-list, not a scoring function.** Eight `+` terms with no weights, no normalization, and no thresholds leave "strongest patterns" and `Strength: high/medium/low` undefined. Two concrete requirements before Phase 2 ships scoring:
> - **Weights + normalized cutoffs.** Specify per-term weights and the numeric boundaries that map to high/medium/low.
> - **Specificity must down-weight ubiquitous shapes.** Without idf-style weighting, universal commands (`go test ./...`, `gofmt -w .`) will dominate every repo. Reuse the existing query-tokenizer/stopword machinery rather than inventing a third regime (see the two intentionally-divergent stopword regimes already in the codebase — extend, don't fork).

## Workspace Semantics

Workspace support is a core design requirement.

There are two scopes:

| Scope | Meaning | Skill implication |
| --- | --- | --- |
| Repo | Repeats inside one repository. | Good for repo-local or repo-specific skills. |
| Workspace | Repeats across multiple repos in a workspace. | Strong signal for portable or team skills. |

Pattern identity must include scope:

```json
{
  "id": "pattern:...",
  "scope": "repo",
  "repo_key": "gh/entireio/cli",
  "workspace": ""
}
```

```json
{
  "id": "pattern:...",
  "scope": "workspace",
  "repo_key": "",
  "workspace": "platform"
}
```

Workspace consolidation should:

- Load member repo brains by workspace membership.
- Combine repo-local episodes without copying generated brain data between repos.
- Notice procedures and practices that recur across repos.
- Preserve repo-specific variants.
- Reward patterns that recur across repos and authors.
- Render pattern cards with a repo breakdown.

Example card section:

```text
Seen in:
- api: 5 episodes, 3 success, 1 corrected
- web: 4 episodes, 2 success
- cli: 7 episodes, 5 success

Common procedure:
...

Repo-specific variations:
- api uses ...
- web uses ...
```

## Public Commands

Keep the public command surface aligned to user jobs.

### Repo Commands

```sh
entire brain patterns
entire brain patterns refresh
entire brain patterns status
entire brain patterns form <pattern-id>
```

`entire brain patterns`

Default read-only view. Shows strongest current patterns with concise evidence summaries and ids.

Options:

```sh
--json
--limit N
--type procedure|practice
--scope repo|workspace
```

`entire brain patterns refresh`

Rebuilds the pattern layer from current brain sources. `entire brain refresh` should run this by default after sessions, history, facts, docs, and semantic sources are current.

Options:

```sh
--force
--json
```

`entire brain patterns status`

Shows freshness and skill memory state:

```text
patterns: current
episodes: 1234
procedures: 42
practices: 18
accepted skills: 5
declined patterns: 3
updates available: 2
```

Options:

```sh
--json
```

`entire brain patterns form <pattern-id>`

Shows the full pattern card, prepares a proposed `SKILL.md` draft, then asks where, if anywhere, to install it. The command must not write files until the user chooses a destination.

Options:

```sh
--name <skill-name>
--destination global|repo
--yes
--json
```

In non-interactive mode, require both `--name` and `--yes`.

### Workspace Commands

Mirror the repo commands without expanding the surface:

```sh
entire brain workspace patterns <workspace>
entire brain workspace patterns refresh <workspace>
entire brain workspace patterns status <workspace>
entire brain workspace patterns form <workspace> <pattern-id>
```

Workspace `patterns` should show cross-repo patterns by default and include repo breakdowns in each card.

## Pattern Cards

A pattern card is the approval surface for skill creation or update.

Card shape:

```text
Pattern: release-readiness-evidence
Type: procedure
Scope: workspace
Strength: high
Reinforcement: 12 success, 2 corrected, 9 neutral
Seen in: 8 sessions, 3 repos, 2 authors

What repeats:
...

Trigger:
Use when ...

Evidence:
- session/checkpoint/path:line
- real command/tool sequence
- supporting fact ids

Would become:
- Steps
- Verification
- Failure modes
```

Rules:

- Always show a card before asking the user to form or update a skill.
- Include retained source anchors.
- Include corrections and failure modes when available.
- Distinguish common workflow from repo-specific variants.
- Do not include raw transcripts or secrets.

> **Review: "no secrets" needs an active mechanism.** Cards show "real command/tool sequence," and commands routinely embed tokens (auth URLs, env assignments). A passive rule is not enough — add an explicit redaction pass over command/tool text, and prefer `path:line` anchors over inlined command strings wherever the anchor alone is sufficient.

## Skill Memory

Track user decisions in `patterns/skill-memory.ndjson`:

```json
{
  "pattern_id": "pattern:...",
  "scope": "repo",
  "status": "active",
  "skill_name": "verify-release-readiness",
  "installs": [
    { "agents": ["copilot-cli", "cursor", "gemini", "pi"], "path": "~/.agents/skills/verify-release-readiness/SKILL.md", "content_sha": "sha256:..." },
    { "agents": ["claude-code", "opencode"], "path": "~/.claude/skills/verify-release-readiness/SKILL.md", "content_sha": "sha256:..." },
    { "agents": ["codex"], "path": "~/.codex/skills/verify-release-readiness/SKILL.md", "content_sha": "sha256:..." }
  ],
  "fingerprint": "sha256:...",
  "sessions": 8,
  "reinforcement": {
    "success": 12,
    "corrected": 2,
    "neutral": 9
  },
  "created_at": "2026-06-16T00:00:00Z",
  "updated_at": "2026-06-16T00:00:00Z",
  "note": ""
}
```

Recommendations:

- `active/current`: accepted skill exists and evidence has not materially changed.
- `active/update`: evidence changed materially; offer update.
- `active/edited`: skill file changed since formation; present a careful update plan.
- `declined/current`: suppress.
- `declined/reconsider`: evidence materially changed; show what changed before asking again.
- `missing`: skill memory points to a missing file; offer to recreate or forget.

Because a skill may be written to several install paths (see `installs[]`), evaluate `edited` and `missing` **per install entry**, not once per record: store `content_sha` per install, and report the worst state across them (e.g. any edited install ⇒ `active/edited`; some-but-not-all paths missing ⇒ partial, offer to re-sync the missing ones).

## Skill Formation

`patterns form` prepares a skill draft from the selected pattern card. It writes selected skills only after the user chooses an install destination.

> **Resolved (2026-06-16): default to the cross-agent standard, then fan out to holdouts.** The Entire CLI integrates with eight agents (`entire agent add`: claude-code, codex, copilot-cli, cursor, factoryai-droid, gemini, opencode, pi). All use the same `<dir>/<skill-name>/SKILL.md` shape with `name` + `description` frontmatter, but the roots differ — see [Skill Install Destinations (per agent)](#skill-install-destinations-per-agent). The key fact: `.agents/skills/` (project) and `~/.agents/skills/` (global) is the **Agent Skills open standard**, read natively by copilot-cli, cursor, gemini, and pi. So the destination model is: write the standard path **once** to cover those four, then add the per-agent path for the holdouts (claude-code → `.claude/skills/`, codex → `.codex/skills/`, factoryai-droid → `.factory/skills/`; opencode rides on the `.claude/skills/` path it also reads). Two-to-four writes cover all eight, not eight separate prompts.

Interactive flow:

1. Render the full pattern card.
2. Render the proposed `SKILL.md` draft.
3. Ask which scope, then which agents to install for (default: detect installed agents via `entire agent list`):
   - Scope: global (home-dir roots) or repo-local (in-repo roots).
   - Agents: default to all installed agents; the writer collapses these to the minimal set of paths (standard path + holdout paths).
   - Draft only: print the draft without writing.
   - Cancel.
4. Write only after the user confirms scope + agent selection.

The initial implementation may support only the standard `.agents/skills/` global path, draft-only output, and cancel — that alone serves copilot-cli/cursor/gemini/pi. Add the holdout per-agent writers (`.claude`, `.codex`, `.factory`, `.opencode`) next. Document the final set in `README.md`.

Default destination (cross-agent standard — covers copilot-cli, cursor, gemini, pi):

```text
~/.agents/skills/<skill-name>/SKILL.md       # global
.agents/skills/<skill-name>/SKILL.md         # repo-local
```

Holdout destinations (agents that do not read the standard path):

```text
~/.claude/skills/<skill-name>/SKILL.md   |  <repo>/.claude/skills/...     # claude-code (also serves opencode)
$CODEX_HOME/skills/<skill-name>/SKILL.md |  <repo>/.codex/skills/...      # codex ($CODEX_HOME ≈ ~/.codex)
~/.factory/skills/<skill-name>/SKILL.md  |  <repo>/.factory/skills/...    # factoryai-droid
~/.config/opencode/skills/<skill-name>/SKILL.md | <repo>/.opencode/skills/...  # opencode (optional; .claude path already covers it)
```

Implementation notes: honor `$CODEX_HOME` rather than hard-coding `~/.codex`; for claude-code the bare `~/.claude/skills/` path is the documented convention but may be plugin-gated on some installs — verify it is loaded, or package as a plugin if guaranteed loading is required.

### Skill Install Destinations (per agent)

Reference for all eight agents the Entire CLI integrates with (`entire agent add`). All use `<dir>/<skill-name>/SKILL.md` with `name` + `description` YAML frontmatter. "Standard" = reads the `.agents/skills/` open standard (agentskills.io).

| Agent | Standard? | Global root(s) | Repo-local root(s) |
| --- | --- | --- | --- |
| `copilot-cli` | yes | `~/.agents/skills/` or `~/.copilot/skills/` | `.agents/skills/`, `.github/skills/`, or `.claude/skills/` |
| `cursor` | yes | `~/.agents/skills/` or `~/.cursor/skills/` | `.agents/skills/` or `.cursor/skills/` |
| `gemini` | yes (alias) | `~/.agents/skills/` or `~/.gemini/skills/` | `.agents/skills/` or `.gemini/skills/` |
| `pi` | yes | `~/.agents/skills/` or `~/.pi/agent/skills/` | `.agents/skills/` or `.pi/skills/` |
| `claude-code` | no | `~/.claude/skills/` (or via plugin) | `<repo>/.claude/skills/` |
| `opencode` | no (Claude-compat) | `~/.config/opencode/skills/`; also reads `~/.claude/skills/` | `.opencode/skills/`; also reads `.claude/skills/` |
| `codex` | no | `$CODEX_HOME/skills/` (≈ `~/.codex/skills/`) | `<repo>/.codex/skills/` |
| `factoryai-droid` | no | `~/.factory/skills/` | `<repo>/.factory/skills/` |

Minimal write set to cover all eight: `.agents/skills/` (the four standard readers) + `.claude/skills/` (claude-code, also serves opencode) + `.codex/skills/` (codex) + `.factory/skills/` (factoryai-droid).

Generated skill shape:

```markdown
---
name: <name>
description: Use when ...
---

# <Name>

## Steps

...

## Verification

...

## Failure Modes

...

## Evidence

Formed by Entire Brain from N sessions with retained source anchors.
```

Skill body rules:

- Keep steps focused on future behavior, not session recap.
- Include verification when evidence shows how work was checked.
- Include failure modes from corrections.
- Keep source anchors concise.
- Never write or overwrite a file before the destination choice is explicit.
- Recompute and store `content_sha` after writing.

## Refresh Integration

`entire brain refresh` should run pattern refresh after:

1. sessions
2. seed
3. history
4. docs
5. semantic
6. facts, when present

The pattern layer should be deterministic and token-free by default. Agent-assisted card synthesis can be optional later, but the initial path should produce usable cards from structured evidence.

> **Review: facts are not produced by `refresh`.** `entire brain distill` is a separate, manual, token-heavy command that calls a model per transcript chunk; `refresh` never invokes it. So step 6 ("facts, when present") is correct only because facts may already exist from a prior distill run — patterns refresh must read facts opportunistically and **must never trigger distillation itself** (that would break the token-free guarantee). The real refresh order in `refresh.go` is sessions → seed → history → history-vectors → docs → semantic → branch overlays; insert pattern refresh after these, gated on its own input-derived freshness signal.
>
> **Review: make refresh incremental.** episodes.ndjson over all history can be large and refresh runs often. Reuse the size+mtime scan cache pattern (`historyScanCache` in `history.go`) and the per-session fingerprint skip that distill uses, so refresh is incremental rather than a full re-scan every time.

Status and overview integration:

- `status` reports pattern source presence, freshness, counts, and update recommendations.
- `overview` includes the strongest current patterns.
- `brief` includes relevant patterns for the task.
- MCP exposes the same job-oriented surface: list patterns, status, and form only if the client explicitly asks for a write-capable action.

## Reuse From Distill

The `entire brain distill` pipeline already solves most of the hard mechanics this feature needs. Mirror it rather than reinventing:

| Distill primitive | Location | Reuse for patterns |
| --- | --- | --- |
| Content-hash idempotent upsert | `upsertFact` (`facts.go`) | Stable pattern/episode ids; safe re-runs during refresh. |
| Provenance-union anchors | `factAnchor` + union logic (`facts.go`) | Episode/pattern source anchors that accumulate across refreshes without duplication. |
| Deterministic inference fallback | `inferFactKind`, `factLocus` (`facts_locus.go`) | When optional agent synthesis misbehaves, fall back to deterministic classification — keeps the token-free path whole. |
| Single mockable agent abstraction | `distillAgentRunner` (`distill.go`) | Reuse the exact injection point for any later optional agent card synthesis; gives tests a seam. |
| Confidence-gated proposals | `applyFactActions` (`facts_merge.go`) | Non-destructive handling of uncertain pattern updates — auto-apply high confidence, queue the rest. |
| Mid-run checkpointing | flush-every-N-calls in `distill_cmd.go` | Only relevant if optional agent synthesis is added; not needed for the deterministic path. |
| Per-session fingerprint skip | distill cache (`distill_cmd.go`) | Incremental refresh — skip episodes whose source session is unchanged. |

Critically, distill is **not** wired into `refresh` and is **token-heavy**. The patterns refresh path is the opposite by design: deterministic, token-free, and part of `refresh`. Borrow distill's data primitives, not its always-call-a-model control flow.

## Implementation Phases

### Phase 0: Reinforcement Classification (Spike, prerequisite) — DONE (2026-06-16)

Reinforcement is the linchpin signal with no existing implementation, and Phases 1–5 plus the card format all encode its output. Resolve it before committing to those formats.

Implemented in `internal/cli/reinforcement.go` (+ `reinforcement_test.go`):

- **Rubric** (documented as the file's package-level comment). Two evidence sources combined by precedence: (1) high-precision phrase cues on the next substantive user turn → `corrected` (negative) or `success` (positive); (2) non-empty `checkpointExportSession.Error` → `corrected`. Precedence: correction cue → approval cue → agent error → `neutral`. Correction cues are deliberately narrow and beat a co-occurring "thanks but…" so the false-`corrected` direction stays rare.
- **Episode boundary (spike resolution):** within one session transcript, each substantive user turn is feedback on the work after the previous one; signals are the user turns after the first (`reinforcementSignalsFromTranscript`). Reuses the dialect helpers (`transcriptUserText`, `parseDocumentConversation`, `isWrapperRequest`) so it works across Codex, Claude, pi, and opencode shapes; injected wrappers are skipped. The episode layer (Phase 1) joins per-checkpoint `Error` into `AgentErrored`.
- **Classifier:** `classifyReinforcement(reinforcementSignal) string`, deterministic and token-free.
- **Fixtures + precision target:** `TestClassifyReinforcement` (25 labeled cases) and `TestReinforcementSignalsFromTranscript` (per-dialect extraction). Targets: accuracy ≥ 0.85, `corrected` precision ≥ 0.90, and **zero** approvals mislabeled as `corrected`. Current eval: accuracy 1.000, corrected precision 1.000, 0 violations.
- **Commit-success positive signal — DONE.** A second evidence source: the episode's work segment (turns between the request and the next request). A landed git commit — detected by its `[branch hash] subject` confirmation line, which git prints only on success and which appears literally in the raw transcript across dialects — labels the episode `success`, below user correction cues in precedence. This was added to recover the positive reinforcement that next-turn cues miss (see Phase 1 finding). `WorkCommitted` on `reinforcementSignal`; segmentation via `transcriptEpisodeSegments`.
- **Deferred refinements (noted in code):** per-checkpoint `Error` → a `WorkFailed`/`corrected` branch (the manifest session record lacks it); test/build-pass as an additional positive signal; re-prompt-same-intent detection; requiring an intervening assistant turn before treating a user turn as feedback.

### Phase 1: Episodes — DONE (2026-06-16)

- Add `patterns` source manifest. → `patternSourceManifest` in `episodes.go`, wired into `brainSources` (`brain.go`). Freshness derives from the shared sessions fingerprint; procedure/practice/pattern/skill-memory counters reserved at zero.
- Add episode extraction from exported sessions. → `buildBrainEpisodes` (`episodes.go`): deterministic, token-free, per substantive user turn, across all four transcript dialects; reuses the dialect helpers. Stable content-hash ids (`episodeID`) over identity-defining fields only.
- Persist `patterns/episodes.ndjson`. → `writeBrainEpisodesAndSource` (atomic, under the write lock); `loadBrainEpisodes` reads it back.
- Apply the Phase 0 reinforcement classifier to label each episode (label is the classification of the *next* substantive user turn).
- Add `entire brain patterns refresh` and `entire brain patterns status` (`patterns_cmd.go`), both with `--json`; bare `patterns` shows status.
- Tests: `episodes_test.go` (extraction, cross-dialect, stable ids, write/load, status freshness) + the Phase 0 reinforcement tests.

**Empirical finding (first real run, this repo's brain — 683 episodes):** the next-turn-only classifier came out **1 success / 10 corrected / 672 neutral**. The `corrected` labels are accurate on inspection ("Try again", "Revert this…", "No, …"), but positive reinforcement was nearly invisible to next-turn cues — on real CLI sessions users rarely type "thanks/lgtm"; they just issue the next request. **Resolution:** wiring the commit-success signal (above) lifted the same 683 episodes to **64 success / 10 corrected / 609 neutral** — spot-checked successes are genuine commit episodes ("commit/push", "Commit the codex folder"). `corrected` stays high-precision; `success` is now usable but skews toward commit-bearing episodes; `neutral` remains "no signal", not "no value". Strength scoring (Phase 2) can use `success` as a weak-positive but should not over-weight it.

Scope deferred to later phases (documented in `episodes.go`): per-episode `tool_sequence` / `command_sequence` / `files` (Phase 2 procedure detection), and per-checkpoint `Error` → a `WorkFailed`/`corrected` branch (the manifest session record does not carry it).
- Test Codex and Claude transcript fixtures.

### Phase 2: Procedures — DONE (2026-06-16)

- Extract command and tool-sequence procedures. → episodes carry `tool_sequence` / `command_sequence` (`episode_tools.go`, cross-dialect, with a dependency-free command normalizer). Procedures are recurring command n-grams (length 2–3, consecutive runs collapsed) over the episode set (`buildBrainProcedures` in `procedures.go`).
- Build procedure ids — stable content-hash (`procedureID`). **Supporting synapses deferred** per the review note: each procedure instead carries `support` (distinct episodes), author/branch counts, reinforcement tally, and up to 3 example source anchors — no separate edge store.
- Score repo-local procedure patterns. → weighted blend in [0,1]: **specificity (idf, weight 0.40)** to suppress ubiquitous-command noise, recurrence (log-saturated support, 0.30), reinforcement quality (success lifts / corrected sinks, 0.20), author+branch diversity (0.10); cutoffs high ≥ 0.6 / medium ≥ 0.4 / low. A specificity floor drops shapes built from ubiquitous commands.
- Add `entire brain patterns` read-only listing (`patterns_cmd.go`): bare `patterns` lists strongest procedures, flags `--json` / `--limit` / `--type` / `--scope`.
- Render basic pattern cards. → `renderProcedureCard` (concise: strength label, command chain, support/authors/branches, reinforcement tally, example anchor + id). The full approval card is Phase 5 (`patterns form`).

**Real run (this repo's brain):** 683 episodes → **266 procedures**. Top by strength are genuine workflows, and the reinforcement pipeline shows through clearly — commit workflows surface as high-success because the commit-success signal feeds strength:

```
[HIGH] git commit → git push        14 episodes, 14↑ 0↓ 0·   strength 0.67
[HIGH] git add → git push           20 episodes, 20↑ 0↓ 0·   strength 0.64
[HIGH] git diff → git add → git push 11 episodes, 11↑ 0↓ 0·  strength 0.60
[HIGH] python3 → benchmarks/agent-brain/run.py  10 episodes  strength 0.62
```

Residual noise (e.g. `cd → echo`) survives via the diversity bonus; the **specificity floor / weight is the documented tuning lever**. Deferred: per-episode `files`, tool-sequence-shape procedures (only command n-grams implemented), workspace scope (Phase 6).

### Phase 3: Practices

- Use durable facts, history records, validation records, corrections, and read-only episodes to detect repeated practices.
- Score practices separately from procedures.
- Render practice cards with facts/history/source anchors.

### Phase 4: Skill Memory

- Add `patterns/skill-memory.ndjson`.
- Detect active, declined, changed, edited, and missing states.
- Add recommendations to `patterns status` and `patterns`.
- Test duplicate suppression and changed-evidence detection.

### Phase 5: Skill Formation

- Add `entire brain patterns form <pattern-id>`.
- Render full card and proposed skill draft before any write.
- Ask for an install destination, then write selected skill files only after confirmation.
- Store skill memory records.
- Refuse accidental overwrite without explicit confirmation.

### Phase 6: Workspace Patterns

- Add workspace pattern refresh/list/status/form commands.
- Aggregate member repo episodes and patterns.
- Preserve repo-specific variants.
- Score cross-repo and multi-author strength.
- Render workspace cards with repo breakdowns.

### Phase 7: Agent/MCP Integration

- Include relevant patterns in `brief`.
- Include strongest patterns in `overview`.
- Add MCP tools matching the public jobs.
- Add JSON contracts for patterns, cards, status, and form results.

## Tests

Add coverage for:

- episode extraction from retained sessions
- actual command detection versus prose examples
- reinforcement classification
- procedure grouping
- practice grouping from facts and history
- synapse generation
- repo-local pattern scoring
- workspace pattern aggregation
- card rendering
- skill memory recommendations
- user-edited skill update detection
- skill formation without accidental overwrite

## Suggested First PR

Land Phase 0 (reinforcement classifier + fixtures + rubric) as its own small PR first — it carries the most risk and unblocks everything else.

Then keep the first feature PR narrow:

1. Add the `patterns` manifest source and storage paths (with an input-derived freshness signal, not a central fingerprint).
2. Extract and persist `episodes.ndjson`, labeled by the Phase 0 classifier.
3. Add `patterns refresh` and `patterns status` (status will report 0 procedures/practices at this stage — scaffolding, not a regression).
4. Add tests for episode extraction and reinforcement labels.

Then add procedure grouping, cards, skill memory, and workspace consolidation in separate PRs.
