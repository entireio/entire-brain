# Entire Brain V2 Pattern Corpus Plan

## Objective

Implement a clean-room, Entire Brain native pattern-corpus layer that improves
the whole brain: `brief`, `query`, `overview`, `review`, MCP, handoff,
workspaces, and optional skill formation.

Do not design this as a standalone skill extractor. Skill formation is one
downstream consumer. The core product goal is better repository memory, task
guidance, cross-repo awareness, provenance, and drift detection.

Do not copy naming, schema, commands, or docs from any external implementation.
Use only Entire Brain's existing concepts: sessions, history, facts, docs,
semantic graph, workspaces, refresh, brief, query, review, MCP, and local
storage.

This plan assumes the Phase 0 pattern-consolidation work may already exist on
the implementation branch. If it has not landed yet, treat compatible readable
pattern artifacts and skill-memory behavior as prerequisites to preserve or
fold into this work, not as requirements to duplicate.

## Vocabulary

Use brain-themed internal terms.

| Term | Meaning |
| --- | --- |
| Episode | One user request, the agent work that followed, and the next feedback signal. |
| Pattern | Recurring work, judgment, risk, validation habit, or cross-repo behavior. |
| Consolidation | Evidence-backed summary of a recurring pattern. |
| Dossier | Stored consolidation artifact: trigger, steps, variants, verification, failure modes, refs, verifier result. |
| Synapse | Internal-only weighted evidence edge connecting brain records. |
| Decision | User or system state about a pattern or consolidation, such as accepted, declined, stale, edited, missing, or suppressed. |

`Synapse` must not be exposed as a CLI command or user-facing concept. It is an
internal data model for explainable scoring, provenance, and drift.

Example synapse:

```json
{
  "from_id": "episode:abc",
  "to_id": "fact:def",
  "kind": "supports",
  "weight": 0.82,
  "source_anchor": "sessions/main/x.jsonl:123"
}
```

Initial synapse kinds:

- `mentions`
- `supports`
- `reinforces`
- `contradicts`
- `varies`
- `derived_from`
- `affects`
- `validated_by`
- `failed_by`

## Constraints

- Keep public commands aligned to user jobs.
- Do not introduce hidden commands for this work.
- Do not require hosted model calls in `refresh`.
- Keep generated artifacts local and inspectable.
- Preserve workspace support as a first-class requirement.
- Preserve existing readable NDJSON exports where useful.
- Add richer internal indexing only behind existing command surfaces.
- Redact secrets before any CLI, JSON, MCP, agent, cache, or draft egress.
- Preserve the local-only boundary: no implicit network operations, no remote
  service, and MCP remains stdio-only.
- Honor strict no-egress mode and existing agent-gating behavior.

## Design Considerations

The expected upside is broad: better task packets, better retrieval context,
better diff-less review, better workspace awareness, better provenance, and more
reliable optional skill formation. Treat those upsides as hypotheses to prove,
not assumptions.

For each implementation phase, evaluate outcomes against existing Entire Brain
features. The target is not "more pattern data"; the target is better answers
without regressing refresh, watch, retrieval, workspace, local-only, or
inspectability guarantees.

Evaluate and document these tradeoffs:

- **Refresh and watch cost:** Pattern corpus refresh should be incremental,
  bounded, deterministic, and token-free. A pattern-corpus failure should not
  make the normal brain unusable by default. Decide which failures should be
  warnings in `refresh` and which should fail an explicit `patterns refresh`.
- **Retrieval noise:** Pattern, episode, and consolidation records may improve
  `query`/`brief`, but they may also crowd out facts/history/docs. Prove that
  relevance improves or stays neutral before adding them to default retrieval.
- **Agent boundaries:** Optional agent-assisted consolidation must have an
  explicit command or flag boundary. It must not run from plain `refresh`,
  `watch`, `brief`, `query`, MCP read tools, or workspace refresh.
- **Workspace storage boundaries:** Workspace aggregation should store
  repo-qualified references, summaries, ids, and breakdowns. It must not copy
  raw member transcripts or generated member brain records into another repo's
  brain.
- **User-state safety:** Rebuildable corpus data and user decisions must have a
  hard separation. Deleting or rebuilding `patterns/corpus.sqlite` must not
  delete accepted, declined, suppressed, edited, or missing-state decisions.
- **Optional source layers:** Semantic graph and durable facts should enrich the
  corpus when present. Missing semantic/fact layers must degrade gracefully and
  must never trigger distillation.
- **Branch and freshness scope:** Durable facts are branch-scoped, semantic data
  has freshness state, and history/docs are indexed snapshots. Pattern records
  must preserve those scopes instead of flattening them into one global truth.
- **Raw text and secrets:** Prefer redacted display text plus source anchors in
  the corpus. Raw retained transcript content should remain in the existing
  transcript source, not be duplicated into new generated stores unless there is
  a tested reason.

For each tradeoff, add validation that proves the upside and verifies the
downside risk is controlled.

## Phase 1: Internal Pattern Corpus

Add `patterns/corpus.sqlite` as a rebuildable internal cache.

Keep existing readable files such as `patterns/episodes.ndjson`,
`patterns/tasks.ndjson`, `patterns/procedures.ndjson`,
`patterns/practices.ndjson`, and `patterns/skill-memory.ndjson`, but stop
treating flat files as the main analytical layer.

Suggested internal tables:

- `episodes`
- `episode_commands`
- `episode_tools`
- `episode_files`
- `episode_facts`
- `episode_symbols`
- `episode_commits`
- `grams`
- `meta_hits`
- `synapses`
- `patterns`
- `pattern_evidence`
- `dossiers`
- `themes`

User decisions must not live only in the rebuildable corpus. Store them in
`patterns/decisions.ndjson` or `patterns/decisions.sqlite`, and keep
`patterns/skill-memory.ndjson` as the compatibility bridge for existing skill
state until it can be cleanly migrated.

Pattern run history should also be outside the rebuildable corpus. Store an
append-only `patterns/runs.ndjson` activity ledger so status surfaces can explain
what happened during refresh, eval, dossier generation, and decision updates
without relying on transient logs.

Implementation notes:

- Build the corpus during `entire brain refresh` after
  sessions/history/docs/semantic/facts-when-present.
- Rebuild deterministically and token-free.
- Use existing write locks and atomic helpers.
- Add a schema version and parser/indexer version.
- Use stable ids based on identity fields only.
- Preserve branch on every branch-sensitive record, and preserve source
  freshness state when linking to semantic, history, docs, or facts.
- Keep user-state records separate from rebuildable records.
- Make corpus refresh incremental and bounded enough for normal `refresh` and
  `watch` use.
- Treat explicit pattern-maintenance commands as the stricter failure surface;
  normal refresh should report recoverable pattern-corpus issues without
  breaking unrelated brain sources.

Indexing and idempotency requirements:

- Store a parser/indexer version on every indexed session.
- Store per-session fingerprints based on transcript identity and content.
- Re-indexing an unchanged session must skip it.
- Re-indexing a changed session must delete and replace all derived rows for
  that session in one transaction.
- Deleted or missing transcripts must be pruned from the corpus without leaving
  orphaned episodes, commands, synapses, or pattern evidence.
- Corpus writes should use SQLite transactions and the existing brain write
  lock.

Validation:

- Running refresh twice on unchanged input must not duplicate corpus rows.
- A schema/indexer version bump must force a rebuild.
- Changed sessions replace their derived rows instead of appending duplicates.
- Deleted transcripts prune derived rows cleanly.
- Corrupt corpus state must be surfaced clearly, not silently ignored.
- Existing `refresh` and `watch --once` workflows stay deterministic and do not
  become materially slower on unchanged input.
- `ENTIRE_BRAIN_NO_EGRESS=1` and `ENTIRE_BRAIN_LOCAL_ONLY=1` do not change the
  deterministic corpus path except to forbid any optional agent/network step.
- `go test ./...`, `go vet ./...`, `gofmt -l -s .`.

### Corpus SQLite Schema

Use `prepareBrainRelativeSQLiteFile` and the existing `sqliteDriverName`. Mirror
the semantic store's shape: `PRAGMA journal_mode=WAL`, `meta(key,value)`, explicit
indexes, and integrity validation. Use additive migrations where possible; bump
`pattern_indexer_version` when parser or scoring logic changes in a way that
requires re-indexing sessions.

Column names must follow existing Entire Brain JSON and SQLite conventions:
`schema_version`, `repo_key`, `checkpoint_id`, `turn_id`, `branch`,
`source_path`, `transcript_path`, `start_line`, `end_line`, `content_sha`,
`fingerprint`, `created_at`, `updated_at`, `stable_id_version`, and
`freshness_state`. Use `qualified_name`, `file_path`, `commit`, and `tree` when
linking to semantic records, matching the semantic provider contract.

Required `meta` keys:

- `schema_version`: integer string, initial `1`
- `pattern_indexer_version`: integer string
- `repo_key`
- `generated_at`
- `sessions_fingerprint`
- `semantic_fingerprint`, when semantic source is present
- `facts_fingerprint`, when fact source is present
- `history_fingerprint`, when history source is present

Required tables:

```sql
PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;

CREATE TABLE meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE indexed_sessions (
  id TEXT PRIMARY KEY,
  repo_key TEXT NOT NULL,
  session_id TEXT NOT NULL,
  checkpoint_id TEXT,
  transcript_path TEXT NOT NULL,
  branch TEXT,
  author_name TEXT,
  agent TEXT,
  created_at TEXT,
  size INTEGER NOT NULL,
  mtime_unix_nano INTEGER NOT NULL,
  content_sha TEXT NOT NULL,
  parser_version INTEGER NOT NULL,
  episodes INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE episodes (
  id TEXT PRIMARY KEY,
  episode_key TEXT NOT NULL UNIQUE,
  repo_key TEXT NOT NULL,
  workspace TEXT,
  session_id TEXT NOT NULL,
  checkpoint_id TEXT,
  turn_id TEXT,
  turn_ord INTEGER NOT NULL,
  branch TEXT,
  author_name TEXT,
  agent TEXT,
  created_at TEXT,
  source_path TEXT NOT NULL,
  start_line INTEGER NOT NULL,
  end_line INTEGER NOT NULL,
  intent_raw TEXT,
  intent_sig TEXT,
  n_tools INTEGER NOT NULL DEFAULT 0,
  tool_mix TEXT,
  files TEXT,
  n_cmds INTEGER NOT NULL DEFAULT 0,
  exit_fails INTEGER NOT NULL DEFAULT 0,
  outcome TEXT NOT NULL DEFAULT 'neutral',
  outcome_source TEXT,
  freshness_state TEXT
);

CREATE TABLE episode_tools (
  episode_id TEXT NOT NULL,
  ord INTEGER NOT NULL,
  tool_type TEXT NOT NULL,
  tool_name TEXT,
  line INTEGER,
  PRIMARY KEY (episode_id, ord),
  FOREIGN KEY (episode_id) REFERENCES episodes(id) ON DELETE CASCADE
);

CREATE TABLE episode_commands (
  episode_id TEXT NOT NULL,
  ord INTEGER NOT NULL,
  head TEXT NOT NULL,
  raw_redacted TEXT,
  line INTEGER,
  exit_code INTEGER,
  failed INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (episode_id, ord),
  FOREIGN KEY (episode_id) REFERENCES episodes(id) ON DELETE CASCADE
);

CREATE TABLE grams (
  episode_id TEXT NOT NULL,
  n INTEGER NOT NULL,
  gram TEXT NOT NULL,
  PRIMARY KEY (episode_id, n, gram),
  FOREIGN KEY (episode_id) REFERENCES episodes(id) ON DELETE CASCADE
);

CREATE TABLE meta_hits (
  episode_id TEXT NOT NULL,
  meta_id TEXT NOT NULL,
  quote_redacted TEXT,
  line INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (episode_id, meta_id, line),
  FOREIGN KEY (episode_id) REFERENCES episodes(id) ON DELETE CASCADE
);

CREATE TABLE episode_files (
  episode_id TEXT NOT NULL,
  path TEXT NOT NULL,
  action TEXT NOT NULL,
  line INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (episode_id, path, action, line),
  FOREIGN KEY (episode_id) REFERENCES episodes(id) ON DELETE CASCADE
);

CREATE TABLE episode_facts (
  episode_id TEXT NOT NULL,
  fact_id TEXT NOT NULL,
  branch TEXT,
  kind TEXT,
  paths TEXT,
  weight REAL NOT NULL DEFAULT 1.0,
  PRIMARY KEY (episode_id, fact_id),
  FOREIGN KEY (episode_id) REFERENCES episodes(id) ON DELETE CASCADE
);

CREATE TABLE episode_symbols (
  episode_id TEXT NOT NULL,
  symbol_id TEXT NOT NULL,
  file_path TEXT,
  relation_kind TEXT NOT NULL DEFAULT 'mentions',
  freshness_state TEXT,
  PRIMARY KEY (episode_id, symbol_id, relation_kind),
  FOREIGN KEY (episode_id) REFERENCES episodes(id) ON DELETE CASCADE
);

CREATE TABLE episode_commits (
  episode_id TEXT NOT NULL,
  commit TEXT NOT NULL,
  branch TEXT,
  subject_redacted TEXT,
  authored_at TEXT,
  reached_default_branch INTEGER,
  reverted INTEGER,
  PRIMARY KEY (episode_id, commit),
  FOREIGN KEY (episode_id) REFERENCES episodes(id) ON DELETE CASCADE
);

CREATE TABLE synapses (
  id TEXT PRIMARY KEY,
  from_id TEXT NOT NULL,
  to_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  weight REAL NOT NULL DEFAULT 1.0,
  source_anchor TEXT,
  evidence_sha TEXT,
  created_at TEXT NOT NULL
);

CREATE TABLE patterns (
  id TEXT PRIMARY KEY,
  type TEXT NOT NULL,
  scope TEXT NOT NULL,
  repo_key TEXT,
  workspace TEXT,
  cluster_key TEXT NOT NULL UNIQUE,
  title TEXT NOT NULL,
  intent_sig TEXT,
  gram TEXT,
  meta_id TEXT,
  theme_id TEXT,
  strength REAL NOT NULL,
  strength_label TEXT NOT NULL,
  support INTEGER NOT NULL,
  n_repos INTEGER NOT NULL DEFAULT 1,
  n_authors INTEGER NOT NULL DEFAULT 0,
  n_branches INTEGER NOT NULL DEFAULT 0,
  outcome_success INTEGER NOT NULL DEFAULT 0,
  outcome_corrected INTEGER NOT NULL DEFAULT 0,
  outcome_neutral INTEGER NOT NULL DEFAULT 0,
  outcome_failed INTEGER NOT NULL DEFAULT 0,
  fingerprint TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'active',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE pattern_evidence (
  pattern_id TEXT NOT NULL,
  episode_id TEXT NOT NULL,
  rank INTEGER NOT NULL,
  outcome TEXT,
  source_path TEXT NOT NULL,
  start_line INTEGER NOT NULL,
  end_line INTEGER NOT NULL,
  PRIMARY KEY (pattern_id, episode_id),
  FOREIGN KEY (pattern_id) REFERENCES patterns(id) ON DELETE CASCADE,
  FOREIGN KEY (episode_id) REFERENCES episodes(id) ON DELETE CASCADE
);

CREATE TABLE dossiers (
  cluster_key TEXT PRIMARY KEY,
  pattern_id TEXT NOT NULL,
  fingerprint TEXT NOT NULL,
  schema_version INTEGER NOT NULL,
  json_redacted TEXT NOT NULL,
  verifier_json_redacted TEXT,
  verdict TEXT,
  confidence REAL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY (pattern_id) REFERENCES patterns(id) ON DELETE CASCADE
);

CREATE TABLE themes (
  theme_id TEXT PRIMARY KEY,
  title TEXT NOT NULL,
  description TEXT,
  episode_keys TEXT NOT NULL,
  fingerprint TEXT NOT NULL,
  evidence_json_redacted TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
```

Required indexes:

```sql
CREATE INDEX idx_episodes_repo_branch ON episodes(repo_key, branch);
CREATE INDEX idx_episodes_intent_sig ON episodes(intent_sig);
CREATE INDEX idx_episodes_outcome ON episodes(outcome);
CREATE INDEX idx_episodes_source ON episodes(source_path, start_line);
CREATE INDEX idx_commands_head ON episode_commands(head);
CREATE INDEX idx_grams_gram ON grams(gram);
CREATE INDEX idx_meta_hits_meta ON meta_hits(meta_id);
CREATE INDEX idx_episode_files_path ON episode_files(path);
CREATE INDEX idx_episode_facts_fact ON episode_facts(fact_id);
CREATE INDEX idx_episode_symbols_symbol ON episode_symbols(symbol_id);
CREATE INDEX idx_episode_commits_commit ON episode_commits(commit);
CREATE INDEX idx_synapses_from ON synapses(from_id);
CREATE INDEX idx_synapses_to ON synapses(to_id);
CREATE INDEX idx_patterns_type_scope ON patterns(type, scope);
CREATE INDEX idx_patterns_strength ON patterns(strength DESC);
CREATE INDEX idx_pattern_evidence_episode ON pattern_evidence(episode_id);
```

Decision storage schema, if SQLite is chosen:

```sql
CREATE TABLE decisions (
  pattern_id TEXT PRIMARY KEY,
  status TEXT NOT NULL,
  substatus TEXT,
  skill_name TEXT,
  installs_json TEXT,
  evidence_fingerprint TEXT,
  content_sha TEXT,
  note TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
```

Allowed `decisions.status` values: `active`, `declined`, `suppressed`,
`missing`, `edited`. Allowed `substatus` values: `current`, `update`,
`reconsider`, `orphaned`, `partial`.

`patterns/runs.ndjson` record shape:

```json
{
  "schema_version": 1,
  "run_id": "run:...",
  "kind": "refresh",
  "started_at": "2026-06-16T00:00:00Z",
  "finished_at": "2026-06-16T00:00:02Z",
  "repo_key": "gh/example/repo",
  "workspace": "",
  "sessions_scanned": 12,
  "episodes_indexed": 34,
  "patterns_changed": 3,
  "dossiers_changed": 0,
  "warnings": [],
  "outcome": "success"
}
```

### ID And Fingerprint Rules

Use stable, content-derived ids. Exclude volatile fields such as score, recency,
outcome counts, and generated summaries.

- `indexed_sessions.id`: `repo_key + "/" + transcript_path`
- `episode_key`: `repo_key + "/" + transcript_path + "#" + turn_ord`
- `episode.id`: `episode:` + sha256(`repo_key`, `transcript_path`, `turn_ord`,
  `start_line`) truncated consistently with existing fact ids
- `pattern.id`: `pattern:` + sha256(`type`, `scope`, `repo_key`, `workspace`,
  `cluster_key`)
- `synapse.id`: `synapse:` + sha256(`from_id`, `to_id`, `kind`,
  `source_anchor`)
- `dossiers.fingerprint`: sha256 over sorted member `episode_key`,
  `start_line`, `end_line`, `outcome`, `n_cmds`, and linked fact ids
- `patterns.fingerprint`: sha256 over sorted evidence ids and the normalized
  cluster key

### Parser And Normalization Spec

Create a parser conformance fixture set under `internal/cli/testdata/patterns/`.
The fixtures must cover every transcript shape Entire Brain currently supports.

Required parser rules:

- User turns open a new episode; the next substantive user turn supplies the
  feedback signal for the previous episode.
- Wrapper/injected requests must not become episodes.
- Tool calls accumulate on the active episode.
- Shell commands are extracted only from executor tool calls, never from prose
  or tool output.
- Tool output may contribute `exit_code`, `failed`, and validation failure hints,
  but never commands.
- Multi-line shell commands must preserve a redacted display form and a
  normalized `head`.
- `bash -lc` wrappers should be unwrapped when this can be done safely.
- Environment-variable prefixes are stripped from `head`, while
  `raw_redacted` keeps a redacted display.
- Noise commands used only for navigation or inspection should be down-weighted
  for scoring, not discarded from the evidence record.
- Command grams are built within one episode only, with n = 2..4, deduped within
  the episode.
- `tool_mix` is a comma-separated `type:count` string sorted by type.
- `files` is a JSON array of unique file paths sorted lexicographically.
- `outcome` is one of `success`, `corrected`, `neutral`, `failed`, or `end`.
- `outcome_source` is one of `user_feedback`, `commit_success`,
  `command_failure`, `checkpoint_error`, or `none`.
- `episode_commits` is populated only from local git evidence. It must not fetch.
  Commit links are advisory outcome evidence, not proof, unless the commit is
  reachable from local refs and the source anchor verifies.

Initial deterministic `meta_id` values:

- `read_only_diagnosis`
- `correction_followup`
- `validation_loop`
- `review_request`
- `handoff_resume`
- `workspace_workflow`
- `user_preference`
- `no_egress_constraint`

Required fixture assertions:

- modern Codex transcript with shell command in summary
- modern Claude transcript with inline command
- multi-line shell command
- legacy tool-use transcript
- shell output containing command-looking text
- failed command output
- user correction after prior episode
- commit-success episode
- wrapper request ignored
- branch duplicate session id with different transcript path

### Candidate Queries And Scoring

Candidate generation must be deterministic and queryable from the corpus. Store
the resulting rows in `patterns` and link evidence in `pattern_evidence`.

Minimum family rules:

- `procedure`: group `grams.gram`; require at least 3 episodes or 2 sessions.
- `task`: group by `(episodes.intent_sig, grams.gram)` where both exist; require
  at least 3 episodes and at least 2 command-bearing episodes.
- `practice`: group `meta_hits.meta_id` and fact kind/path/locus links; require
  at least 2 episodes or one high-confidence durable fact.
- `risk`: group corrected/failed episodes by `(intent_sig, gram)` or affected
  `symbol_id`; require at least 2 corrected/failed episodes, or one
  closed-negative/gotcha fact plus one matching episode.
- `workspace`: same families, but scope is `workspace` and `n_repos >= 2`.
- `theme`: semantic/conversational cluster with verified member episodes; defer
  agent-assisted theme proposal until the deterministic families are stable.

Candidate SQL sketches:

```sql
-- Procedures.
SELECT gram, COUNT(DISTINCT episode_id) AS support
FROM grams
GROUP BY gram
HAVING support >= 3;

-- Tasks: intent and method co-occur in the same episodes.
SELECT e.intent_sig, g.gram, COUNT(DISTINCT e.id) AS support
FROM episodes e
JOIN grams g ON g.episode_id = e.id
WHERE e.intent_sig IS NOT NULL AND e.n_cmds > 0
GROUP BY e.intent_sig, g.gram
HAVING support >= 3;

-- Practices.
SELECT meta_id, COUNT(DISTINCT episode_id) AS support
FROM meta_hits
GROUP BY meta_id
HAVING support >= 2;

-- Risks.
SELECT e.intent_sig, g.gram, COUNT(DISTINCT e.id) AS support
FROM episodes e
LEFT JOIN grams g ON g.episode_id = e.id
WHERE e.outcome IN ('corrected', 'failed')
GROUP BY e.intent_sig, g.gram
HAVING support >= 2;

-- Workspace breadth.
SELECT p.cluster_key, COUNT(DISTINCT e.repo_key) AS n_repos
FROM patterns p
JOIN pattern_evidence pe ON pe.pattern_id = p.id
JOIN episodes e ON e.id = pe.episode_id
GROUP BY p.cluster_key
HAVING n_repos >= 2;
```

Base scoring:

```text
support_score     = min(log2(1 + support) / log2(31), 1)
span_score        = min(span_days / 120, 1)
recency_score     = 1.0 if newest <= 30d, 0.6 if <= 90d, else 0.3
specificity_score = idf-weighted command/fact/symbol specificity in [0,1]
outcome_score     = 0.5 when no labeled outcome, otherwise
                    clamp01(0.5 + 0.5 * (success - corrected - failed) / support)
diversity_score   = average of repo, author, and branch diversity in [0,1]
```

Family scoring:

```text
procedure_score = 0.30*support + 0.15*span + 0.10*recency
                + 0.25*specificity + 0.20*outcome

task_score      = 0.35*support + 0.10*span + 0.10*recency
                + 0.25*specificity + 0.20*outcome

practice_score  = 0.30*support + 0.25*specificity + 0.20*outcome
                + 0.15*diversity + 0.10*recency

risk_score      = 0.30*support + 0.30*corrected_or_failed_rate
                + 0.20*specificity + 0.20*recency

workspace_score = 0.35*repo_breadth + 0.25*support + 0.20*specificity
                + 0.20*outcome
```

Strength labels:

- `high`: score >= 0.70
- `medium`: score >= 0.50
- `low`: score < 0.50

Promotion threshold:

- A pattern is eligible for a dossier at score >= 0.65.
- A generic workflow requires at least one fact, symbol, validation, failure, or
  workspace variant synapse before it can cross the promotion threshold.
- A pattern with `risk_score >= 0.65` should enrich `brief`/`review` before it
  is considered for skill formation.

### Theme Discovery Spec

Themes are optional semantic clusters for judgment-heavy or read-only episodes.
They must not be required for the deterministic refresh path.

Deterministic seed selection:

- Select episodes with `n_cmds = 0` or `tool_mix` dominated by read/search tools.
- Exclude wrapper requests and low-information continuations.
- Group seed candidates by overlapping intent terms, fact links, symbols, and
  `meta_hits`.
- Require at least 4 candidate episodes before a theme can be proposed.

Optional agent-assisted theme proposal:

- Must be behind an explicit command or flag.
- Must receive redacted spans only.
- Must return theme id, title, description, member `episode_key` values, and
  reason.
- Must be verifier-checked before insertion into `themes`.

Theme fingerprint:

- sha256 over sorted member `episode_key` values, member outcomes, linked facts,
  and theme title.

### Dossier Schema

`dossiers.json_redacted` must encode this JSON object:

```json
{
  "schema_version": 1,
  "pattern_id": "pattern:...",
  "cluster_key": "task:review-release\u0000gram:go-test",
  "fingerprint": "sha256:...",
  "title": "...",
  "trigger": "...",
  "preconditions": [],
  "workflow": [],
  "variants": [],
  "verification": [],
  "failure_modes": [],
  "facts": [],
  "source_anchors": [
    {
      "session_id": "session",
      "checkpoint_id": "checkpoint",
      "transcript": "sessions/main/session/transcript.jsonl",
      "start_line": 10,
      "end_line": 40,
      "outcome": "corrected"
    }
  ],
  "confidence": 0.0
}
```

`verifier_json_redacted` must encode:

```json
{
  "schema_version": 1,
  "verdict": "accepted",
  "reason": "...",
  "unsupported_claims": [],
  "conflated_subpatterns": [],
  "required_edits": [],
  "evidence_fingerprint": "sha256:..."
}
```

Allowed verifier verdicts: `accepted`, `rejected`, `needs_split`,
`low_confidence`.

## Phase 2: Upgrade Episodes Into Brain Events

Current episodes should become the join point across brain sources.

Add or improve episode fields:

- repo key
- workspace membership at index time
- session id
- checkpoint id
- branch
- author
- agent
- created time
- intent text
- intent signature
- tool sequence
- executed commands
- files read/written/edited
- semantic symbols affected
- linked durable facts
- linked history records
- reinforcement/outcome signal
- source anchors

Episode identity and spans:

- Every episode must have a stable key derived from session id plus turn ordinal.
- Store `start_line` and `end_line`, not only one source line.
- Evidence exports for consolidation must slice by those spans.
- Store command line numbers when available.
- Store failure hints such as non-zero exits or recognizable failed validation
  output when available.
- Keep branch and checkpoint identity on the episode so branch-scoped retrieval
  and blame remain meaningful.

Command extraction must count only commands actually executed by the agent, not
commands mentioned in prose or examples.

Semantic symbols, durable facts, and history links are enrichment layers. If any
of those sources are absent, stale, or corrupt, episode extraction should still
produce useful core records and report the missing enrichment as diagnostics.
Episode extraction must not invoke fact distillation.

Validation:

- Fixture transcripts prove command extraction across supported transcript
  dialects.
- Tool output is never parsed as a command.
- Episode file links work for read, write, edit, and shell-driven file changes
  when recoverable.
- Episode spans can be used to export exact evidence slices.
- Redaction tests cover command strings, file paths, transcript excerpts, and
  JSON.
- Missing semantic, facts, or docs sources degrade gracefully.
- Branch-scoped facts do not leak into another branch's pattern evidence unless
  explicitly promoted or queried cross-branch.

## Phase 3: Multi-Channel Pattern Discovery

Replace a single task-candidate model with multiple pattern families.

Pattern families:

- `task`: recurring user intent plus method plus outcome.
- `procedure`: repeatable operational workflow.
- `practice`: durable way-of-working from facts, corrections, validations, and
  user steering.
- `risk`: recurring regressions, failed validations, reverts, blind spots, or
  "do not repeat" evidence.
- `workspace`: cross-repo behavior discovered across workspace members.
- `theme`: semantic or conversational cluster from read-only or judgment-heavy
  episodes.

Generic command loops may remain visible as procedures, but must not become
high-value task candidates unless supported by repo-specific facts,
validations, failures, or conventions.

Corroboration requirements:

- The highest-value candidates should come from joins across intent, method,
  evidence, and outcome.
- Do not rank by intent alone.
- Do not rank by command shape alone.
- Prefer candidates where the same user intent repeatedly co-occurs with
  similar methods, facts, files/symbols, validations, or outcomes.
- Use synapses to explain why a candidate exists.

Use synapses to connect:

- pattern -> supporting episodes
- pattern -> facts
- pattern -> files/symbols
- pattern -> validations/failures
- workspace pattern -> repo variants
- consolidation -> source records

Validation:

- Candidate eval reports quality per family.
- Generic `git add/commit/push` style patterns are not promoted unless
  repo-specific evidence exists.
- Workspace candidates include per-repo evidence and tolerate repos with no
  facts.
- Existing `brief` and `overview` do not become noisier on unrelated tasks.
- At least one evaluation compares the new candidate model against the current
  task-candidate implementation and records wins, losses, and uncertain cases.

## Phase 4: Consolidation Records

Add consolidation as a reusable internal artifact, not only a step before skill
writing.

A consolidation should summarize:

- trigger
- preconditions
- canonical workflow
- repo/workspace variants
- verification behavior
- failure modes
- relevant facts
- source anchors
- confidence
- stale/current state

Selection rules:

- Prefer corrected/failed episodes first.
- Then successful episodes.
- Then recent neutral episodes.
- Include enough examples to represent variants.
- Do not include raw unredacted transcript content.

Agent use:

- Refresh remains deterministic and token-free.
- Optional agent-assisted consolidation may be explicit and cached.
- Optional agent-assisted consolidation must never run from plain `refresh`,
  `watch`, `brief`, `query`, MCP read tools, or workspace refresh.
- Cache by evidence fingerprint.
- If evidence fingerprint is unchanged, reuse the cached consolidation.
- If corrected episodes or significant new evidence appear, mark stale.

Verifier schema:

A verified consolidation should store:

- `verdict`: `accepted`, `rejected`, `needs_split`, or `low_confidence`
- `reason`
- `unsupported_claims`
- `conflated_subpatterns`
- `required_edits`
- `evidence_fingerprint`

Validation:

- Consolidation must cite exact anchors.
- A verifier path must be able to reject conflated patterns.
- Cached consolidation must invalidate when evidence materially changes.
- Redaction must apply before agent input and persisted cache output.
- Agent-assisted consolidation has tests proving it is reachable only through
  explicit write/maintenance surfaces.

## Phase 5: Integrate With Existing Brain Surfaces

Patterns must improve Entire Brain broadly.

Update these surfaces:

- `brief`: include only task-relevant patterns, practices, risks, and
  consolidations.
- `overview`: show strongest current repo/workspace patterns without flooding.
- `query/search/get`: allow addressable pattern, episode, and consolidation
  records where appropriate.
- `review`: use risk/practice/consolidation evidence for diff-less regression
  context.
- `handoff`: include relevant recent episodes and consolidations.
- MCP: expose read-only status/list/get surfaces matching public CLI jobs.
- workspace commands: aggregate from raw evidence/corpus, not only merged
  summaries.

Do not add hidden commands. If a new command is needed, it must map to a real
user job and be documented.

Default retrieval integration must be source-aware and capped. `get` may address
pattern, episode, and consolidation ids directly, but adding these records to
default `search`/`query` ranking requires evidence that facts/history/docs
quality is not degraded.

Existing machine-contract surfaces should remain stable. Do not rename or widen
the hidden review contract as part of this work; only enrich its internal
context when validation shows better findings without more noise.

Validation:

- `brief` with unrelated tasks returns no ambient pattern noise.
- `brief` with matching tasks returns anchored, useful pattern context.
- `query/search` quality over facts/history/docs stays neutral or improves when
  pattern records are enabled.
- MCP output is redacted and stable.
- MCP remains stdio-only and read-only for pattern inspection unless a future
  documented write-capable action is explicitly added.
- Workspace output includes repo breakdown and does not copy generated member
  repo data into other repo brains.

## Phase 6: Product-Level Evaluation And Drift

Add quality checks that measure whether the pattern corpus improves Entire
Brain.

Evaluation targets:

- candidate precision by pattern family
- generic-noise rate
- missing-evidence rate
- redaction leak rate
- refresh/watch latency on unchanged and changed brains
- retrieval quality before/after pattern records join retrieval
- branch-scope correctness
- no-egress/local-only compliance
- workspace coverage
- usefulness of `brief` with pattern context
- usefulness of `review` with risk/practice context
- consolidation staleness accuracy

Prefer tests and fixtures first. Add a public eval command only if it matches
the existing eval philosophy and is documented. Do not add hidden eval commands.

Drift states:

- current
- stale
- update recommended
- edited
- missing
- declined but reconsiderable
- suppressed

Quality gates:

- Pattern corpus work must not make existing refresh/watch workflows materially
  worse on unchanged input.
- `brief` must not include pattern context for unrelated tasks.
- Top candidates must include at least two independent source anchors unless
  explicitly fact-backed.
- Generic workflows must remain below the promotion threshold without
  repo-specific corroboration.
- Workspace patterns must require evidence from at least two member repos.
- Redaction leak tests must run over CLI text, JSON, MCP, corpus rows used for
  egress, consolidation input, and generated drafts.
- Branch-scoped evidence must stay branch-correct.
- Local-only/no-egress modes must block every optional agent or network path.

Validation:

- Tests cover drift after new evidence, corrected evidence, deleted files,
  edited generated artifacts, and missing installed artifacts.
- Existing skill-memory behavior continues to work.
- Decisions are preserved across refresh.
- Rebuildable corpus data can be deleted and regenerated without losing user
  decisions.
- Tests cover deleting `patterns/corpus.sqlite` while preserving decisions.
- Tests cover retrieval with pattern records enabled and disabled.
- Tests cover branch-scoped facts and no-egress mode.

## Deliverables

1. Update the plan doc to describe this v2 direction without external-project
   references.
2. Add `patterns/corpus.sqlite` generation behind refresh.
3. Preserve readable NDJSON exports.
4. Add synapse storage and internal helpers.
5. Rework pattern discovery around the corpus.
6. Add consolidation records and cache invalidation.
7. Wire useful pattern context into `brief`, `overview`, `query/get`, `review`,
   MCP, and workspaces.
8. Add focused validation fixtures and product-level eval tests.
9. Record outcome evidence for each design consideration: upside observed,
   downside risk, validation performed, and remaining uncertainty.
10. Keep command surface small, documented, and user-job-oriented.

## Definition Of Done

- Existing tests pass.
- New corpus/idempotency tests pass.
- New redaction tests pass.
- New workspace tests pass.
- New pattern quality tests pass.
- `entire brain refresh` remains deterministic and token-free by default.
- Public docs describe the feature as part of Entire Brain's memory system, not
  as a standalone skill pipeline.

## Implementation status (2026-06-16) — stacked on `claude/patterns-phase0-reinforcement`

**Phase 1 foundation landed: the rebuildable pattern corpus.**

- `patterns/corpus.sqlite` (`pattern_corpus.go`) created with the full target schema (WAL, `meta`, `indexed_sessions`, `episodes` + `episode_tools`/`episode_commands`/`grams`, plus the still-empty `meta_hits`/`episode_files`/`synapses`/`patterns`/`pattern_evidence` tables for additive growth) and the required indexes.
- **Idempotent per-session indexing**, reusing the v1 deterministic extraction (`transcriptEpisodeSegments`, `toolCallsFromObj`/`normalizeCommand`, `classifyReinforcement` cues, `redactText`): per-session fingerprint (size+mtime+content_sha) skip; changed sessions delete+replace their derived rows in one transaction (FK `ON DELETE CASCADE`); deleted transcripts are pruned; an `pattern_indexer_version` bump forces a clean re-index. Stable content-derived ids per the plan.
- Stored redacted (`intent_raw`, `raw_redacted`) — secrets never enter the corpus. User decisions are **not** stored here (they remain the separate skill-memory user-state), so the corpus is freely deletable/rebuildable.
- Built during `entire brain refresh` (warning-on-failure, so a corpus issue never breaks the rest of the brain) and during the explicit `entire brain patterns refresh` (fatal — the stricter surface).
- Validated: idempotency (no duplicate rows on re-build), version-bump rebuild, changed-session replace, deleted-session prune; `go vet` + `gofmt` clean. Real run on this repo's brain: 131 sessions → 674 episodes, 9.8k commands, 12.4k grams.

**Next slices (this branch):** richer evidence (`episode_files`, `meta_hits` classifier, fact/symbol/commit links), synapses, the corpus-driven multi-family candidate/scoring layer (replacing the flat-file discovery as the analytical source), consolidation dossiers, then surface integration and product eval — each proven against the existing brain before it joins default retrieval.

**Phase 2 landed: episodes as brain events.** Corpus episodes now carry the
operational evidence Phase 3 corroboration needs (`pattern_corpus_enrich.go`):
- `episode_commands` get exit codes + `failed` (correlated to outputs by call id) and `episodes.exit_fails`.
- `episode_files` (apply_patch `*** Update/Add/Delete File`, Claude `edit/write/read` tools) with actions; `episodes.files` JSON.
- `meta_hits` deterministic way-of-working classifier (review_request, validation_loop, handoff_resume, read_only_diagnosis, correction_followup, user_preference, no_egress_constraint).
- `episode_facts` branch-scoped durable-fact links (text + locus overlap), graceful when no facts exist; facts never leak across branches.
- Tool OUTPUT is never parsed as a command; everything is redacted at rest.
- **Evidence** (parser fixtures + live run on this brain): 588 file refs, 3,315 fact links, 713 meta-hits, 145 failed commands, 8,196 exit codes; redaction leak check = 0. `pattern_indexer_version` bumped to 2 (forces re-index; idempotency test proves it).

**Phase 3 landed: multi-family candidate discovery with a working promotion gate** (`pattern_candidates.go`). The corpus now drives candidates, replacing the v1 flat-file task model:
- Four families rebuilt from the corpus on every refresh: `task` (intent+method), `procedure` (diagnostic command shape), `risk` (corrected/failed work), `practice` (way-of-working meta-hit). Stored in `patterns`/`pattern_evidence`; deleted+rebuilt deterministically.
- **Scoring** per the plan: support / recency / outcome / diversity / command-specificity, family-weighted (`taskScoreV2` etc.).
- **Promotion gate** is the core fix over v1. `applyPromotionGate` caps any candidate below the dossier threshold (0.65) unless it is repo-corroborated, and procedures are never self-promotable (generic command loops stay visible but diagnostic). A task additionally needs command specificity ≥ `promotionSpecFloor` (0.35) — so a generic shape that incidentally touched a fact-bearing file does not promote.
- **Specificity is per-constituent-command, not per-sequence** (the decisive bug fix). v1 measured the idf of the whole n-gram *sequence* — "git push ▷ git status" is rare as an exact 3-command string, so it scored as "specific" despite being built from generic commands. `gramSpecificity` now splits the gram on `gramSep` (" ▷ ", recoverable because heads contain spaces) and takes the MAX per-command idf from `loadCommandDF` (distinct-episode document frequency per command head). Generic-command shapes now score low; a shape containing a genuinely rare command ("mise deploy", "goose up") scores high. `pattern_indexer_version` bumped to 3 (gram format changed).
- **Evidence (Phase 3)** (live run on this brain, 325 command-episodes): promotable generic git **tasks dropped from 6 → 1**. The lone survivor (`commit:push` / `git push ▷ git status`, support 13) is fully corroborated (fact+validation+failure) and its distinctive command `git push` sits at 38/325 ≈ 12% of episodes (spec 0.371) — a genuinely-less-generic command than the ubiquitous status/diff/log/rev-parse cluster it now outranks. Procedures: 695 candidates, **0 promotable** (gate cap verified). The deterministic gate is by design a coarse filter; the Phase 4 agent verifier remains the final arbiter that rejects "commit and push" as obvious. Idempotent (1,047 patterns stable across re-refresh); redaction leak check = 0; `go vet`/`gofmt`/full `internal/cli` suite green.

**Phase 4 landed: consolidation dossiers + an explicit, egress-gated, cached agent verifier** (`pattern_dossier.go`, `pattern_verify.go`).
- **Deterministic dossier** assembled token-free for every promotable pattern (`dossiers` table): trigger, workflow (the actual command heads), variants (other shapes for the same intent), verification (validation commands actually run in the evidence), failure modes (corrected/failed anchors), corroborating facts (id+kind+paths), exact source anchors (session/checkpoint/transcript/lines/outcome), and confidence. Built inside `buildPatternCandidates`, so it rides the normal refresh — no agent, no network.
- **Cache survives rebuilds.** Dossiers are not FK-cascaded by the `DELETE FROM patterns` rebuild; the consolidation is refreshed each time, but a cached agent verdict is kept and only marked `stale` when the **evidence fingerprint** (anchors + facts + verification + confidence bucket) moves under it. Dossiers for patterns that drop below threshold are pruned.
- **Verifier is the final arbiter and is reachable only through `entire brain patterns verify`** — never `refresh`/`watch`/`brief`/`query`/MCP/workspace (proven by `TestRefreshDoesNotInvokeVerifier`). It is egress-gated (`rejectAgentForNoEgress`, proven by `TestVerifyDossiersNoEgressRejected` — the runner is never called under `ENTIRE_BRAIN_NO_EGRESS`), redacts the dossier again before it leaves the brain, and is cached by evidence fingerprint (second run serves from cache, agent called exactly once). Verdicts are constrained to `accepted`/`rejected`/`needs_split`/`low_confidence`.
- **Evidence (Phase 4)** (live run on this brain): 4 dossiers for the 4 promotable patterns (3 practices + `commit:push`), all `current`, **0 verified after refresh** (token-free guarantee held), redaction leak check = 0, fingerprints byte-identical and count stable across re-refresh (idempotent). Full `internal/cli` suite + `vet` + `gofmt` green.

**Phase 5 (slice 1) landed: corpus consolidations on the brief, overview, and `get`** (`pattern_surface.go`). The corpus dossiers now reach the surfaces an agent actually reads, behind the plan's no-noise / no-degradation discipline:
- **`brief`** carries task-relevant consolidations (`report.consolidations`): trigger + workflow + verification + failure modes + one anchor, ranked by task-term overlap then confidence and capped — an unrelated task carries **none** (same gating as the existing pattern view). A verifier `rejected` verdict suppresses a consolidation; everything is graceful when no corpus exists.
- **`overview`** shows `strongest_consolidations` (top current dossiers by confidence, capped at 3, excluding stale/rejected) — strength without flooding.
- **`get pattern:<id>`** addresses a consolidation dossier directly (the plan explicitly allows pattern/episode/consolidation ids in `get`). Unknown ids report "not found"; a missing corpus is not an error.
- Redaction holds at every new egress (brief JSON/CLI, overview, `get`); dossiers are redacted at rest and re-redacted defensively.
- **Evidence (Phase 5 slice 1)** (live run on this brain): `brief "review the current branch changes"` → 1 anchored consolidation (`review_request`); `brief "rename a css color variable in the theme"` → **0** (no ambient noise); `overview` → top-3 consolidations by confidence; `get pattern:<id>` → full rendered record; redaction leak check = 0; full `internal/cli` suite + `vet` + `gofmt` green.
- **Deliberately deferred** (each gated on its own evidence, per "only proceed with evidence"): wiring consolidations into **default `search`/`query` ranking** (the plan requires Phase 6 proof that facts/history/docs quality is not degraded first — `get`-by-id is in, ranking is not); and the **`review`/`handoff`/MCP-read/workspace** projections. The read surface and ranking are intentionally separated so default retrieval quality is never silently changed.
  - **⚠ OBSOLETE NOTE (superseded 2026-06-17):** the `review`/`handoff`/MCP-read/workspace projections listed above as deferred are **now implemented** (see priorities 2 and 6 in the as-built table below). Default `search`/`query` ranking integration remains intentionally not done (opt-in `--patterns` pointers instead).

**Phase 6 (slice 1) landed: quality gates as tests** (`pattern_quality_test.go`), following the plan's "prefer tests and fixtures first." Each test encodes one invariant so the feature cannot silently regress:
- **≥2 anchors or fact-backed**: every promotable dossier carries at least two independent source anchors unless it is fact-backed (`TestQualityPromotableHaveAnchorsOrFactBacked`).
- **Redaction boundary over the consolidation**: a fact path carrying a home-dir username and a `ghp_` token is stripped from the dossier JSON at rest and from the verifier's defensive re-redaction — the egress the agent would see (`TestQualityConsolidationRedactionBoundary`). This complements the existing leak checks over `episode_commands.raw_redacted` and `meta_hits.quote`.
- **Rebuildable corpus is deletable without losing user decisions**: deleting `patterns/corpus.sqlite` (+ WAL/SHM) and rebuilding preserves skill-memory — the separate user-state store (`TestQualityCorpusDeletablePreservesDecisions`).
- **Branch-scoped evidence stays branch-correct**: a fact that exists only on `main` links to the `main` episode and never to an identical `feature` episode (`TestQualityBranchScopedFactLinks`).
- Together with earlier gates already in the suite — generic workflows stay below threshold without corroboration (`TestCandidatePromotionGate`), `brief` carries no pattern noise for unrelated tasks (`TestBriefConsolidationsTaskGated`), refresh is idempotent / unchanged sessions are skipped (`TestCorpus*` idempotency), no-egress blocks the verifier and refresh never calls an agent (`TestVerifyDossiersNoEgressRejected`, `TestRefreshDoesNotInvokeVerifier`) — the plan's quality-gate list is covered by tests.
- **Default retrieval is provably neutral**: ranking was not changed (only additive `get`-by-id and task-gated brief/overview sections), so facts/history/docs `search`/`query` quality is unchanged by construction — which is the prerequisite the plan sets before any future ranking integration.
- **No public eval command** was added: a precision/noise-rate eval would need labeled fixtures and is not yet warranted; the plan permits a documented eval command but prefers tests first, and adding a hidden one is forbidden. The drift sub-states (current/update/edited/missing/reconsider/declined) already live in skill-memory; corpus dossiers carry `current`/`stale`.

**Redaction hardening (found by running the extractor on the entire-cli brain, 22.9k episodes).** The shared `redactText` home-path pattern was capital-`/Users/`-only and missed contributor home paths typed lowercase on case-insensitive macOS volumes (`/users/<name>/…`) and home paths embedded in non-boundary contexts (`file:///Users/<name>`, gitbash `/c/Users/<name>`, Windows `C:\Users\<name>`, `sed 's|…|/Users/<name>/…|'`). The fix splits into two rules: **canonical roots** (`/Users/`, `\Users\`, `/home/`) are stripped wherever they appear (unambiguous home dirs); **lowercase `/users/`** is stripped only at a path-token boundary, so a lowercase-typed home is caught while API/repo paths (`api.github.com/users/<login>`, `…/platform/users/components`, `users/me`) are preserved. `pattern_indexer_version` bumped (→5) so existing corpora re-index under the stricter redaction. **Evidence**: on the CLI brain all audited home-username leaks and all `/Users|\Users|/home/<name>` paths went to **0** while 8 API `/users/` paths were correctly preserved; `TestRedactText` covers file://, gitbash, Windows, sed, and API-preservation cases. This is shared infra, so it hardens redaction for facts/history/docs/brief/get across the whole product, not just patterns.

---

## Implementation status (2026-06-17) — PR #44 completion (as-built)

This table is the authoritative as-built status. It uses the eight-priority numbering from the PR-completion brief (which differs from the phase numbering above). It supersedes the "deliberately deferred" notes in the Phase 5/6 (slice 1) sections: the workspace, review, handoff, MCP, and query-discoverability items listed there as deferred are now implemented.

| # | Item | Status | Where | Validation |
|---|------|--------|-------|------------|
| 1 | `patterns` lists the V2 corpus | **implemented + validated** | `pattern_surface.go` (`loadCorpusPatternViews`), `patterns_cmd.go` | `TestPatternsList*`; live: 162 task / 333 procedure / 5 practice rows, dossier state shown |
| 2 | Workspace V2 candidates | **implemented + validated** | `pattern_workspace.go`, `workspace_pattern_repos` table | `TestWorkspaceCorpus*`, `TestSplitWorkspaceIDPattern`; live 2-repo ws: 539 cross-repo patterns from 2/2 corpora, repo breakdown, member drill-in, 0 leaks |
| 3 | `episode_symbols`, `episode_commits`, `synapses`, themes table, run history | **implemented + validated** | `pattern_corpus_links.go`, `pattern_corpus_enrich.go` (commits), `pattern_runs.go` | `TestCorpusNewTablesAndSynapses`, `TestPatternRunRecorded`; live: 848 symbol links, 79 commits, 5572 synapses, run shown in `patterns status` |
| 4 | Deep dossiers + deep verifier (bounded evidence export incl. redacted transcript spans) | **implemented + validated** | `pattern_deep.go`, `pattern_verify.go`, `deep_dossiers` table, `patterns verify --deep` | `TestDeep*`, `TestDeepDossierIncludesRedactedSpanExcerpts`; live (real codex): verified 4 → re-run cached 4; deep verifier rejected 3/4 incl. generic commit:push, 1 needs_split; 0 leaks. Bounded: ≤40 anchors, top 12 carry ≤60-line/≤1800-byte redacted excerpts |
| 5 | Themes / latent practices (agent-proposed semantic clusters — SR4) | **implemented + validated** | `pattern_themes.go`, `episode_shapes`/`themes` tables, `patterns verify --themes` | `TestThemeProposalGroupsByMeaningNotIntentSig`, `TestThemeProposalCachedBySample`, `TestThemeProposalNoEgressRejected`, `TestClassifyEpisodeShape` |
| 6 | Wire corpus into review/handoff/MCP/query | **implemented + validated** | `handoff.go`, `regression.go` (`review --patterns`), `mcp.go`, `retrieve_cmd.go` (`query/search --patterns`) | `TestReviewPatternContext`, `TestRelatedPatternPointers`, `TestHandoffConsolidations*`; live: handoff 4 consolidations, query `--patterns` 5 pointers, default query byte-stable, MCP brain_patterns corpus-backed |
| 7 | Skill lifecycle on V2 ids | **implemented + validated** | `pattern_skill_bridge.go`, `skill_cmd.go` | `TestSkillProposalsRequireAcceptedDeepDossier`, `TestSkillFormUnderCorpusIDAndLifecycle`, `TestSkillFilterDeclinedAndReconsider` |
| 8 | Docs + PR as-built | **implemented** | this table, PR #44 description | final validation below |

### SR5: workspaces merge by meaning, not exact keys
Exact `(type,intent_sig,gram,meta_id)` aggregation stays as the cheap signal, but `entire brain workspace patterns verify` adds an agent judgment layer: it groups each member repo's promotable task candidates into cross-repo **families** by semantic trigger + procedure purpose — equivalent workflows that differ in command spelling/order merge (e.g. `mise deploy` ≈ `make release`), while generic git workflows are rejected. Each accepted family records the **common** workflow and the **per-repo variations**, stored as a workspace family dossier (`deep_dossiers`, `family:` ids). `workspace patterns skills` lists accepted families; `workspace patterns skills form` requires an accepted family and synthesizes a skill describing what is common and what varies per repo. Explicit, egress-gated, cached by the member-candidate fingerprint. Validated by `TestWorkspaceFamilyMergesEquivalentWorkflows` (different command names → one family + per-repo variation), `TestWorkspaceFamilyRejectsGeneric`, `TestWorkspaceFamilyNoEgressRejected`.

### SR6: quality gates on generated skill content
Tests that fail if skills go generic, asserting on the synthesis INPUT (the lever a real agent acts on) and the gating: a release/check skill input carries the exact command + verification; a corrected episode becomes a failure-mode/gotcha; a generic git dossier returns `NOT_A_SKILL`; a shallow-only candidate is not formable; a theme skill cites the verified latent practice (description) not an `intent_sig`; a workspace skill includes per-repo variation when evidence differs (`pattern_skill_quality_test.go`).

### Skills are formed from verified deep dossiers, not corpus rows (PR #44 follow-up)
The skill pipeline runs strictly through verified evidence, not shallow SQL rows + snippets:
- **`patterns skills` lists proposals, not raw task rows.** A proposal is a promotable task **with an accepted deep dossier** (`loadSkillProposals`), minus skill-memory-suppressed ones. A promotable task with no/`rejected`/`needs_split`/`low_confidence` deep dossier is visible in `patterns` (diagnostic) but is **not** a formable proposal.
- **`patterns skills form <id>` synthesizes from the accepted deep dossier** (`pattern_skill_deep.go`): the agent CONVERTS the verified dossier — trigger/Use-when, canonical workflow (exact commands), variations, verification, failure-modes-with-recoveries, parameters, facts, redacted transcript excerpts, and the verifier's required edits — into SKILL.md. It does not re-infer a procedure from snippets, and it still returns `NOT_A_SKILL` for generic dossiers. If no accepted deep dossier exists, the form refuses with "needs deep verification first: run `entire brain patterns verify --deep`" — it never silently falls back to shallow evidence (legacy non-corpus repos keep the old shallow path). Validated by `TestDeepSkillEvidenceFromDossier`, `TestFormRequiresAcceptedDeepDossier`, `TestDeepSkillSynthesisRejectsGeneric`.

### Skills come from non-obvious knowledge, not command n-grams (PR #44 follow-up, 2026-06-17)
A review of a dozen real Claude Code skills (Anthropic `docx`/`pdf`/`mcp-builder`/`webapp-testing`; community `systematic-debugging`/`differential-review`/`yeet`) established that a skill is a **packaged domain capability** encoding NON-OBVIOUS, repo-specific knowledge a capable agent does not already have — not a recorded sequence of commands the agent already knows. Mining recurring command/tool-call n-grams (`git add ▷ git push`, `go vet ▷ golangci-lint`) targets the wrong thing; the deep verifier correctly accepting **0** of those on real brains was the signal, not a bug. The dozen also showed there is no single skill shape: two archetypes recur.

The skill channel is now sourced from the brain's non-obvious knowledge, in two archetypes, with command sequences **demoted to supporting evidence** (`deepDossierRecord.Archetype`, `buildDeepSkillEvidence`):

- **Procedure skills (archetype `procedure`)** — from corrected/failed episodes. `entire brain patterns verify --lessons` (explicit, egress-gated, cached by sample fingerprint) samples corrected/failed episodes and asks an agent to group recurring **failure→recovery lessons** by meaning, dropping generic mistakes any agent already avoids. Accepted lessons are stored as `deep_dossiers` under `lesson:` ids carrying the failure mode, recovery, the negative trigger, and the distilled non-obvious knowledge (`pattern_skill_lessons.go`).
- **Capability skills (archetype `capability`)** — from durable conventions. `entire brain patterns verify --conventions` samples active `gotcha`/`convention`/`invariant` facts (preferring those corroborated by more episodes) and asks an agent to keep only the genuinely non-obvious ones and group related facts into a capability; preferences/decisions are excluded. Accepted ones are stored as `deep_dossiers` under `convention:` ids carrying the member fact texts + distilled rules (`pattern_skill_conventions.go`).

Both reuse the existing `deep_dossiers → accepted-dossier → convert-to-SKILL.md` pipeline (the same precedent as `theme:`/`family:` ids): `loadSkillProposals` now also returns accepted `lesson:`/`convention:` proposals (`loadKnowledgeSkillProposals`), and `patterns skills form` resolves those ids straight from their accepted dossiers. The synthesis prompt requires the description to carry BOTH a "Use when …" and a "Do NOT use when …" trigger (real skills under-trigger), leads the body with the knowledge/lesson and treats commands as a quick-reference, and still emits `NOT_A_SKILL` when a dossier reduces to generic commands + discipline. Validated by `TestProposeSkillLessonsRoundTrip`, `TestProposeSkillLessonsRejectsGeneric`, `TestProposeSkillLessonsEgressGated`, `TestProposeSkillConventionsRoundTrip`, `TestSampleCapabilityFactsFiltersKind`.

**Surfaces using the V2 corpus:** `refresh`/`patterns refresh` (build), `patterns` (list/status/verify[/--deep/--themes/--lessons/--conventions]/skills[/form]), `brief` (consolidations + themes), `overview` (strongest consolidations + themes), `get`/`multi-get` (`pattern:`/`theme:` ids), `review --patterns`, `brief --handoff`, MCP (`brain_patterns`, `brain_patterns_status`, `brain_get`), `query`/`search --patterns`, `workspace patterns`/`workspace refresh`/`workspace patterns verify`/`workspace patterns skills [form]`/`workspace get`.

**Tables (all created; population status honest):** `episodes`, `episode_tools`, `episode_commands`, `grams`, `meta_hits`, `episode_files`, `episode_facts`, `episode_symbols`, `episode_commits`, `episode_shapes`, `patterns`, `pattern_evidence`, `dossiers`, `deep_dossiers`, `themes`, `synapses`, `workspace_pattern_repos`, `indexed_sessions`, `meta`. Populated by a build EXCEPT `deep_dossiers` (only via the explicit `patterns verify --deep` for `pattern:` ids, `--lessons` for `lesson:` ids, and `--conventions` for `convention:` ids) and `themes` (only via the explicit `patterns verify --themes` — agent-proposed; refresh populates `episode_shapes` but not `themes`).

### SR4: themes are agent-proposed semantic clusters, not intent_sig buckets
Themes are no longer grouped by `intent_sig` at refresh. Refresh stays token-free (it only classifies `episode_shapes`). The explicit, egress-gated `patterns verify --themes` samples recent read-only/conversation episodes (capped, redacted intent+excerpt) and asks an agent to group them into coherent latent practices BY MEANING — so differently-worded episodes about the same practice cohere, while same-`intent_sig` episodes about unrelated topics do not. The agent curates membership and assigns a verdict (it can drop members / mark `needs_split`); each accepted seed is then deterministically expanded by intent overlap, fingerprinted, and stored. Cached by the sample fingerprint (unchanged sample → no re-spend). Theme records carry the agent's description (not an intent label), and theme→skill flows through that verified record. Validated by the SR4 tests above.

**Build paths (no second path to discover):** `entire brain refresh` and `entire brain patterns refresh` both build the full corpus + symbols + candidates + synapses + run record (deterministic, token-free; themes are agent-proposed via `patterns verify --themes`). `entire brain workspace refresh` and `entire brain workspace patterns refresh` both build the workspace corpus. Agent verification (`patterns verify [--deep|--themes]`) is the only agent path and is explicit, egress-gated, and cached — never reached from refresh/watch/brief/query/MCP/workspace.

**Not implemented (with reason):** a public precision/noise-rate **eval command** — the plan prefers tests/fixtures first and forbids hidden eval commands; the quality invariants are covered by `pattern_quality_test.go` and per-priority tests instead. Default `search`/`query` **ranking integration** of pattern records — the plan gates it on a no-regression proof; shipped instead as opt-in `--patterns` discoverability so default retrieval quality cannot change. These are deferred deliberately, not blocked.
