# Entire Brain V3 Pattern Product Completion Plan

## Objective

Finish the pattern-corpus work after PR #43 and PR #44 land. Treat those PRs as
the baseline: the corpus, dossiers, explicit verification, workspaces, themes,
skill formation, brief/overview/get/review/handoff/MCP surfaces, and opt-in
query pointers already exist.

V3 is not another extraction rewrite. It closes the remaining product gaps:

- user decisions after a skill proposal or skill write
- a durable decision model beyond the current skill-memory bridge
- proof before adding pattern records to default retrieval ranking
- real-world quality validation across repo and workspace brains

## Merge Baseline

Merge order:

1. Merge PR #43.
2. Rebase or update PR #44 on the merged PR #43 state if needed.
3. Merge PR #44 only after CI is green on the rebased head.
4. Run the local validation suite on `main`.

Baseline validation:

```sh
go test ./...
go vet ./...
gofmt -l -s .
```

Also verify:

- `entire brain refresh --force` builds the pattern layer without invoking an
  agent.
- `ENTIRE_BRAIN_NO_EGRESS=1 entire brain refresh --force` still succeeds for
  deterministic pattern work.
- `entire brain patterns status --json` shows corpus counts and skill-memory
  counts.
- `entire brain patterns skills` lists only accepted, formable proposals.
- `entire brain workspace patterns verify <workspace>` remains the only
  workspace agent judgment path.

## Non-Goals

- Do not add another hidden eval, maintenance, or debug command.
- Do not run an agent from refresh, watch, brief, query, MCP read tools, or
  workspace refresh.
- Do not make default `search` or `query` ranking consume pattern records until
  an eval proves no regression.
- Do not replace the V2 corpus schema or the accepted deep-dossier skill path.
- Do not expose internal anatomical terms as user-facing commands.

## Phase 1: Decision UX After Skill Proposal

### Problem

The current implementation can record an active skill after
`patterns skills form --yes`, and the skill-memory state machine can represent
declined candidates. But users have no normal command path to say "no, do not
show me this again" after a preview or after a generated skill is judged poor.

This leaves the brain able to remember accepted skills but not user rejection.

### Desired Behavior

When `entire brain patterns skills form <id>` produces a preview:

- In non-interactive mode, keep today's safe behavior: print evidence, draft,
  would-write destinations, and exact next commands. Write nothing.
- In interactive TTY mode, ask for one explicit decision after showing the
  preview:
  - write the skill
  - decline this proposal
  - skip without recording a decision
- If the synthesis returns `NOT_A_SKILL`, offer to record that as a declined
  proposal with the returned reason.
- `--yes` remains the automation path for writing and must not become implicit.
- `--json` must never prompt.

Workspace mirrors must behave the same way for
`entire brain workspace patterns skills form <workspace> <id>`.

### Public Commands

Add only commands that users need:

```sh
entire brain patterns skills decline <id> [--note "..."] [--json]
entire brain patterns skills forget <id> [--json]

entire brain workspace patterns skills decline <workspace> <id> [--note "..."] [--json]
entire brain workspace patterns skills forget <workspace> <id> [--json]
```

Command semantics:

- `decline` records a declined decision for the current evidence fingerprint.
  The proposal is suppressed while unchanged and reappears as reconsiderable if
  evidence materially changes.
- `forget` removes the recorded decision for that id. It must not delete an
  installed skill file. If the forgotten decision had installs, print those
  paths so the user can remove files manually if desired.
- Re-running `decline` updates the note and `updated_at`, preserving
  `created_at`.
- `decline` must work for repo proposals and accepted knowledge proposals:
  `pattern:`, `theme:`, `lesson:`, and `convention:` ids.
- Workspace decline/forget must use the workspace store, not a member repo
  brain.

### Validation

Add tests for:

- preview in non-interactive mode writes nothing and records no decision
- interactive write records `active`
- interactive decline records `declined`
- `NOT_A_SKILL` can be recorded as declined with the reason as note
- declined unchanged proposals are suppressed from `patterns skills`
- declined changed proposals reappear as reconsiderable
- `forget` removes the decision and makes the proposal visible again
- workspace decline/forget records in the workspace store
- `--json` never prompts
- existing `--yes` behavior remains unchanged

## Phase 2: Decision Store And Skill-Memory Compatibility

### Problem

The original plan calls for user decisions to be separate from the rebuildable
corpus. The current `patterns/skill-memory.ndjson` satisfies that for skill
formation, but it is named and shaped around skills. V3 should support broader
pattern decisions without breaking existing skill-memory behavior.

### Approach

Introduce `patterns/decisions.ndjson` as the canonical user-decision ledger, and
keep `patterns/skill-memory.ndjson` as the compatibility view for existing
skill lifecycle behavior.

Do not migrate destructively in this phase. Read both stores:

- Existing skill-memory records continue to work.
- New decisions are written to `decisions.ndjson`.
- When a decision includes installed skill paths, mirror enough data to
  `skill-memory.ndjson` so existing lifecycle status remains correct.

Decision record:

```json
{
  "schema_version": 1,
  "subject_id": "pattern:...",
  "subject_type": "pattern|theme|lesson|convention|family",
  "scope": "repo|workspace",
  "workspace": "",
  "status": "active|declined|suppressed",
  "substatus": "current|update|edited|missing|reconsider|orphaned",
  "skill_name": "",
  "installs": [],
  "evidence_fingerprint": "sha256:...",
  "content_sha": "",
  "note": "",
  "created_at": "RFC3339",
  "updated_at": "RFC3339"
}
```

Rules:

- `active` means the user accepted and wrote a skill.
- `declined` means the user rejected the proposal at the current evidence
  fingerprint.
- `suppressed` is reserved for future user-hidden patterns that are not
  necessarily skill proposals; do not surface it until there is a public command
  that needs it.
- Derived substates are computed when reading, not trusted blindly from disk.
- Rebuildable corpus deletion must not delete decisions.

### Validation

Add tests for:

- old `skill-memory.ndjson` still loads and affects listings
- new `decisions.ndjson` declined record suppresses unchanged proposal
- active decision mirrors to skill-memory or produces equivalent lifecycle
  status
- deleting `patterns/corpus.sqlite` preserves both stores
- corrupt decision rows warn and skip without breaking `patterns status`
- workspace decisions are isolated from repo decisions

## Phase 3: Default Retrieval Integration Gate

### Problem

Patterns are currently discoverable through `get`, brief/overview sections,
review/handoff, MCP, and opt-in `query/search --patterns`. Default retrieval
ranking does not include pattern records, by design. That protects existing
facts/history/docs quality.

Do not change default ranking until there is proof.

### Eval Design

Build a public, documented eval path only if it is useful to the user. Prefer an
existing eval surface if one can be extended cleanly; otherwise add a visible
command, not a hidden one:

```sh
entire brain patterns eval retrieval --tasks <file> --json
```

The eval compares:

- default retrieval today
- default retrieval plus pattern records
- opt-in `--patterns` pointer mode

Metrics:

- useful results per 1k tokens
- precision at k
- pattern false-positive rate
- facts/history/docs displacement rate
- per-query win/loss/tie

Task strata:

- code change task
- investigation task
- review/regression task
- handoff/resume task
- workspace/cross-repo task
- unrelated task where pattern noise should be zero

Acceptance before default ranking changes:

- no statistically meaningful regression in useful results per 1k
- no increase in unrelated-task noise
- no material displacement of higher-value facts/history/docs
- at least one stratum where patterns produce measurable wins

Until those conditions are met, keep pattern records opt-in for search/query.

### Validation

Add tests for:

- eval command is documented and visible in help
- eval output is stable JSON
- default query output remains byte-stable when pattern ranking is disabled
- enabling pattern ranking is explicit in the eval arm
- no network or agent is invoked by eval unless a future documented mode says so

## Phase 4: Real-World Product Validation

### Goal

Prove the pattern layer makes Entire Brain better as a whole, not just that the
pipeline passes unit tests.

Run validation on at least:

- `entire-brain`
- `entire-cli`
- `entire-sem`
- one multi-repo workspace that includes at least two of the above

For each repo/workspace, collect:

- pattern corpus counts
- accepted/rejected/low-confidence deep verifier counts
- theme proposal counts
- lesson/convention proposal counts
- workspace family counts
- redaction leak check result
- top brief examples with and without matching pattern context
- review examples with and without pattern context
- handoff examples with consolidation context
- formed skill examples, including declined examples

The result should go into `docs/eval_ledger.md` or a linked focused report.

### Acceptance

V3 is complete only if the report shows:

- pattern context appears when relevant and stays absent when unrelated
- at least three useful repo-local consolidations across the validation repos
- at least one useful workspace family across the validation workspace
- at least one declined proposal stays suppressed and one changed declined
  proposal reappears as reconsiderable
- no redaction leaks in CLI text, JSON, MCP output, persisted corpus rows used
  for egress, or generated skill drafts
- generated skills are materially non-obvious and cite the evidence that makes
  them useful

## Phase 5: Documentation And Cleanup

After V3 implementation:

- Update `docs/pattern_v2.md` with a final status pointer to this V3 plan.
- Add a concise user-facing section to the relevant guide showing the actual
  workflow:
  1. refresh
  2. verify
  3. list proposals
  4. preview a skill
  5. write, decline, or skip
  6. revisit updates/reconsiderations
- Document the workspace mirror workflow.
- Document that default query ranking intentionally excludes pattern records
  until eval evidence supports changing it.
- Document no-egress behavior for all V3 paths.

## Final Definition Of Done

- PR #43 and PR #44 are merged.
- Users can explicitly accept, decline, forget, and revisit skill proposals.
- Decisions survive corpus deletion and rebuild.
- Repo and workspace decisions are isolated correctly.
- Default retrieval remains unchanged unless an eval proves it should change.
- Real-world validation is recorded for repo and workspace brains.
- `go test ./...`, `go vet ./...`, and `gofmt -l -s .` pass.
- GitHub CI is green.
