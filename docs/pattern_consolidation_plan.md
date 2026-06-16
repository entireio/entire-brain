# Pattern Consolidation Plan

This plan adds a pattern layer to Entire Brain: the brain notices repeated repository work, summarizes it with retained evidence, and forms selected patterns into reusable agent skills.

The design keeps the public surface small and task-oriented. Internal terms should support the brain metaphor without leaking implementation details into the user workflow.

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

## Episode Layer

Build episodes from exported session transcripts. An episode is the normalized unit used by pattern detection:

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

- durable facts with kinds such as `preference`, `convention`, `gotcha`, `decision`, and `closed-negative`
- history records around validations, corrections, and reviews
- conversational or read-only episodes
- repeated user steering or verification expectations

Each pattern receives a stable id, type, scope, strength, source anchors, supporting episodes, and variant metadata.

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

## Skill Memory

Track user decisions in `patterns/skill-memory.ndjson`:

```json
{
  "pattern_id": "pattern:...",
  "scope": "repo",
  "status": "active",
  "skill_name": "verify-release-readiness",
  "skill_path": "~/.agents/skills/verify-release-readiness/SKILL.md",
  "fingerprint": "sha256:...",
  "content_sha": "sha256:...",
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

## Skill Formation

`patterns form` prepares a skill draft from the selected pattern card. It writes selected skills only after the user chooses an install destination.

Interactive flow:

1. Render the full pattern card.
2. Render the proposed `SKILL.md` draft.
3. Ask where to install it:
   - Global: `~/.agents/skills/<skill-name>/SKILL.md`
   - Repo-local: if a repo-local convention is implemented
   - Draft only: print the draft without writing
   - Cancel
4. Write only after the user chooses a write destination.

The initial implementation may support only global install, draft-only output, and cancel. Add repo-local or workspace-local destinations after the project settles those conventions.

Default destination:

```text
~/.agents/skills/<skill-name>/SKILL.md
```

Repo-local destination:

```text
<repo>/.agents/skills/<skill-name>/SKILL.md
```

If the project settles on a different repo-local convention, use that consistently and document it in `README.md`.

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

Status and overview integration:

- `status` reports pattern source presence, freshness, counts, and update recommendations.
- `overview` includes the strongest current patterns.
- `brief` includes relevant patterns for the task.
- MCP exposes the same job-oriented surface: list patterns, status, and form only if the client explicitly asks for a write-capable action.

## Implementation Phases

### Phase 1: Episodes

- Add `patterns` source manifest.
- Add episode extraction from exported sessions.
- Persist `patterns/episodes.ndjson`.
- Classify basic reinforcement.
- Add `entire brain patterns refresh`.
- Add `entire brain patterns status`.
- Test Codex and Claude transcript fixtures.

### Phase 2: Procedures

- Extract command and tool-sequence procedures.
- Build procedure ids and supporting synapses.
- Score repo-local procedure patterns.
- Add `entire brain patterns` read-only listing.
- Render basic pattern cards.

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

Keep the first PR narrow:

1. Add the `patterns` manifest source and storage paths.
2. Extract and persist `episodes.ndjson`.
3. Add `patterns refresh` and `patterns status`.
4. Add tests for episode extraction and reinforcement labels.

Then add procedure grouping, cards, skill memory, and workspace consolidation in separate PRs.
