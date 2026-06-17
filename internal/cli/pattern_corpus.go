package cli

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Pattern corpus (V2, Phase 1).
//
// A rebuildable internal SQLite cache (`patterns/corpus.sqlite`) that becomes the
// analytical layer for the pattern feature, mirroring the semantic store's shape
// (WAL, a meta table, explicit indexes). The readable NDJSON exports stay as the
// inspectable interchange; this is the queryable index built on top of the SAME
// deterministic, token-free extraction (transcriptEpisodeSegments + the command/
// tool extractor + the reinforcement classifier + redaction).
//
// User decisions never live here — only rebuildable data — so the corpus can be
// deleted and regenerated without losing accept/decline state.
//
// This first slice establishes the schema and IDEMPOTENT per-session indexing
// (episodes + tool/command operations + command grams). Richer evidence layers
// (files, meta-hits, fact/symbol/commit links, synapses) and the candidate/
// dossier layers are populated by later slices against this same schema.

const patternCorpusPath = "patterns/corpus.sqlite"

const (
	patternCorpusSchemaVersion = 1
	// Bump when the parser/extractor output changes in a way that requires
	// re-indexing already-indexed sessions. v2: Phase 2 enrichment (exit codes,
	// files, meta-hits, fact links).
	patternIndexerVersion = 6
)

// patternCorpusSchema is the full target schema (additive). Tables not yet
// populated by this slice are created so later slices are migration-free.
var patternCorpusSchema = []string{
	`PRAGMA journal_mode=WAL`,
	`PRAGMA foreign_keys=ON`,
	`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS indexed_sessions (
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
	)`,
	`CREATE TABLE IF NOT EXISTS episodes (
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
	)`,
	`CREATE TABLE IF NOT EXISTS episode_tools (
		episode_id TEXT NOT NULL,
		ord INTEGER NOT NULL,
		tool_type TEXT NOT NULL,
		tool_name TEXT,
		line INTEGER,
		PRIMARY KEY (episode_id, ord),
		FOREIGN KEY (episode_id) REFERENCES episodes(id) ON DELETE CASCADE
	)`,
	`CREATE TABLE IF NOT EXISTS episode_commands (
		episode_id TEXT NOT NULL,
		ord INTEGER NOT NULL,
		head TEXT NOT NULL,
		raw_redacted TEXT,
		line INTEGER,
		exit_code INTEGER,
		failed INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (episode_id, ord),
		FOREIGN KEY (episode_id) REFERENCES episodes(id) ON DELETE CASCADE
	)`,
	`CREATE TABLE IF NOT EXISTS grams (
		episode_id TEXT NOT NULL,
		n INTEGER NOT NULL,
		gram TEXT NOT NULL,
		PRIMARY KEY (episode_id, n, gram),
		FOREIGN KEY (episode_id) REFERENCES episodes(id) ON DELETE CASCADE
	)`,
	`CREATE TABLE IF NOT EXISTS meta_hits (
		episode_id TEXT NOT NULL, meta_id TEXT NOT NULL, quote_redacted TEXT, line INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (episode_id, meta_id, line),
		FOREIGN KEY (episode_id) REFERENCES episodes(id) ON DELETE CASCADE
	)`,
	`CREATE TABLE IF NOT EXISTS episode_files (
		episode_id TEXT NOT NULL, path TEXT NOT NULL, action TEXT NOT NULL, line INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (episode_id, path, action, line),
		FOREIGN KEY (episode_id) REFERENCES episodes(id) ON DELETE CASCADE
	)`,
	// episode_symbols links an episode to the semantic symbol ids defined in the
	// files it touched (populated only when a semantic index is present).
	`CREATE TABLE IF NOT EXISTS episode_symbols (
		episode_id TEXT NOT NULL,
		symbol_id TEXT NOT NULL,
		name TEXT,
		file_path TEXT,
		PRIMARY KEY (episode_id, symbol_id),
		FOREIGN KEY (episode_id) REFERENCES episodes(id) ON DELETE CASCADE
	)`,
	// episode_commits records git commits an episode landed, parsed from the
	// transcript's local `[branch hash] subject` confirmation — local evidence
	// only, never the network.
	`CREATE TABLE IF NOT EXISTS episode_commits (
		episode_id TEXT NOT NULL,
		commit_hash TEXT NOT NULL,
		branch TEXT,
		subject TEXT,
		line INTEGER,
		PRIMARY KEY (episode_id, commit_hash),
		FOREIGN KEY (episode_id) REFERENCES episodes(id) ON DELETE CASCADE
	)`,
	`CREATE TABLE IF NOT EXISTS episode_facts (
		episode_id TEXT NOT NULL, fact_id TEXT NOT NULL, branch TEXT, kind TEXT, paths TEXT, weight REAL NOT NULL DEFAULT 1.0,
		PRIMARY KEY (episode_id, fact_id),
		FOREIGN KEY (episode_id) REFERENCES episodes(id) ON DELETE CASCADE
	)`,
	`CREATE TABLE IF NOT EXISTS synapses (
		id TEXT PRIMARY KEY, from_id TEXT NOT NULL, to_id TEXT NOT NULL, kind TEXT NOT NULL,
		weight REAL NOT NULL DEFAULT 1.0, source_anchor TEXT, evidence_sha TEXT, created_at TEXT NOT NULL
	)`,
	// themes are conversational/read-only/judgment-heavy clusters that the
	// command/task channel misses. The table is first-class here; population +
	// verification land with theme discovery (Priority 5).
	`CREATE TABLE IF NOT EXISTS themes (
		id TEXT PRIMARY KEY,
		repo_key TEXT,
		scope TEXT NOT NULL DEFAULT 'repo',
		title TEXT NOT NULL,
		description TEXT,
		shape TEXT,
		member_keys TEXT,
		support INTEGER NOT NULL DEFAULT 0,
		fingerprint TEXT NOT NULL,
		strength REAL NOT NULL DEFAULT 0,
		status TEXT NOT NULL DEFAULT 'active',
		verdict TEXT,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS patterns (
		id TEXT PRIMARY KEY, type TEXT NOT NULL, scope TEXT NOT NULL, repo_key TEXT, workspace TEXT,
		cluster_key TEXT NOT NULL UNIQUE, title TEXT NOT NULL, intent_sig TEXT, gram TEXT, meta_id TEXT, theme_id TEXT,
		strength REAL NOT NULL, strength_label TEXT NOT NULL, support INTEGER NOT NULL,
		n_repos INTEGER NOT NULL DEFAULT 1, n_authors INTEGER NOT NULL DEFAULT 0, n_branches INTEGER NOT NULL DEFAULT 0,
		outcome_success INTEGER NOT NULL DEFAULT 0, outcome_corrected INTEGER NOT NULL DEFAULT 0,
		outcome_neutral INTEGER NOT NULL DEFAULT 0, outcome_failed INTEGER NOT NULL DEFAULT 0,
		fingerprint TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'active', created_at TEXT NOT NULL, updated_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS pattern_evidence (
		pattern_id TEXT NOT NULL, episode_id TEXT NOT NULL, rank INTEGER NOT NULL, outcome TEXT,
		source_path TEXT NOT NULL, start_line INTEGER NOT NULL, end_line INTEGER NOT NULL,
		PRIMARY KEY (pattern_id, episode_id),
		FOREIGN KEY (pattern_id) REFERENCES patterns(id) ON DELETE CASCADE,
		FOREIGN KEY (episode_id) REFERENCES episodes(id) ON DELETE CASCADE
	)`,
	// dossiers cache the consolidation record for each promotable pattern. The
	// deterministic consolidation (json_redacted) is rebuilt token-free on every
	// refresh; the optional agent verifier output (verifier_json_redacted/verdict)
	// is cached by evidence fingerprint and survives pattern rebuilds (no FK
	// cascade — reconciled manually so verifier work is not lost). status flips to
	// 'stale' when the evidence fingerprint changes under a cached verdict.
	`CREATE TABLE IF NOT EXISTS dossiers (
		pattern_id TEXT PRIMARY KEY,
		cluster_key TEXT NOT NULL,
		fingerprint TEXT NOT NULL,
		json_redacted TEXT NOT NULL,
		verifier_json_redacted TEXT,
		verdict TEXT,
		verified_fingerprint TEXT,
		status TEXT NOT NULL DEFAULT 'current',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	// deep_dossiers cache the full-span consolidation + deep verifier result for a
	// pattern. Built on demand by the explicit `patterns verify --deep`, never by
	// refresh. Keyed by pattern id; cached by the full-evidence fingerprint.
	`CREATE TABLE IF NOT EXISTS deep_dossiers (
		pattern_id TEXT PRIMARY KEY,
		fingerprint TEXT NOT NULL,
		json_redacted TEXT NOT NULL,
		verifier_json_redacted TEXT,
		verdict TEXT,
		status TEXT NOT NULL DEFAULT 'current',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	// workspace_pattern_repos holds the per-member-repo breakdown for a
	// workspace-scope pattern (which member repos share it and with what support).
	// Only populated in a workspace corpus; empty in a repo corpus.
	`CREATE TABLE IF NOT EXISTS workspace_pattern_repos (
		pattern_id TEXT NOT NULL,
		repo_key TEXT NOT NULL,
		support INTEGER NOT NULL DEFAULT 0,
		outcome_success INTEGER NOT NULL DEFAULT 0,
		outcome_corrected INTEGER NOT NULL DEFAULT 0,
		outcome_neutral INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (pattern_id, repo_key),
		FOREIGN KEY (pattern_id) REFERENCES patterns(id) ON DELETE CASCADE
	)`,
	`CREATE INDEX IF NOT EXISTS idx_episodes_repo_branch ON episodes(repo_key, branch)`,
	`CREATE INDEX IF NOT EXISTS idx_episodes_intent_sig ON episodes(intent_sig)`,
	`CREATE INDEX IF NOT EXISTS idx_episodes_outcome ON episodes(outcome)`,
	`CREATE INDEX IF NOT EXISTS idx_episodes_source ON episodes(source_path, start_line)`,
	`CREATE INDEX IF NOT EXISTS idx_commands_head ON episode_commands(head)`,
	`CREATE INDEX IF NOT EXISTS idx_grams_gram ON grams(gram)`,
	`CREATE INDEX IF NOT EXISTS idx_meta_hits_meta ON meta_hits(meta_id)`,
	`CREATE INDEX IF NOT EXISTS idx_episode_files_path ON episode_files(path)`,
	`CREATE INDEX IF NOT EXISTS idx_episode_facts_fact ON episode_facts(fact_id)`,
	`CREATE INDEX IF NOT EXISTS idx_synapses_from ON synapses(from_id)`,
	`CREATE INDEX IF NOT EXISTS idx_synapses_to ON synapses(to_id)`,
	`CREATE INDEX IF NOT EXISTS idx_patterns_type_scope ON patterns(type, scope)`,
	`CREATE INDEX IF NOT EXISTS idx_patterns_strength ON patterns(strength DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_pattern_evidence_episode ON pattern_evidence(episode_id)`,
}

// openPatternCorpusDB opens an existing corpus database with the standard
// pragmas. It does NOT create or migrate schema — callers that build the corpus
// use buildPatternCorpus. Returns an error if the corpus is absent.
func openPatternCorpusDB(brainDir string) (*sql.DB, error) {
	path := filepath.Join(brainDir, filepath.FromSlash(patternCorpusPath))
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("pattern corpus not found (run `entire brain patterns refresh` first): %w", err)
	}
	db, err := sql.Open(sqliteDriverName, path)
	if err != nil {
		return nil, err
	}
	for _, p := range []string{"PRAGMA busy_timeout=5000", "PRAGMA foreign_keys=ON"} {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, err
		}
	}
	// Apply the schema idempotently so additive tables (e.g. deep_dossiers) exist
	// even when the on-disk corpus predates them — the build path owns population,
	// but a reader/verifier must not fail with "no such table" before the next
	// refresh. CREATE IF NOT EXISTS never touches existing data.
	for _, stmt := range patternCorpusSchema {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, err
		}
	}
	return db, nil
}

// buildPatternCorpus (re)indexes the brain's sessions into patterns/corpus.sqlite.
// Idempotent: unchanged sessions are skipped, changed sessions are replaced in a
// transaction, removed transcripts are pruned, and an indexer-version bump forces
// a full re-index. Deterministic and token-free.
func buildPatternCorpus(brainDir string, now time.Time) error {
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		return err
	}
	path, err := prepareBrainRelativeSQLiteFile(brainDir, patternCorpusPath)
	if err != nil {
		return err
	}
	db, err := sql.Open(sqliteDriverName, path)
	if err != nil {
		return err
	}
	defer db.Close()
	for _, p := range []string{"PRAGMA busy_timeout=5000", "PRAGMA foreign_keys=ON"} {
		if _, err := db.Exec(p); err != nil {
			return err
		}
	}
	for _, stmt := range patternCorpusSchema {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("corpus schema: %w", err)
		}
	}

	// Indexer-version bump forces a full re-index (parser/extractor changed).
	if stored := corpusMeta(db, "pattern_indexer_version"); stored != strconv.Itoa(patternIndexerVersion) {
		if _, err := db.Exec(`DELETE FROM episodes`); err != nil { // cascades children
			return err
		}
		if _, err := db.Exec(`DELETE FROM indexed_sessions`); err != nil {
			return err
		}
	}

	repoKey := ""
	if manifest != nil {
		repoKey = manifest.RepoKey
	}
	var sessions []exportSession
	if manifest != nil && manifest.Sources != nil && manifest.Sources.Sessions != nil {
		sessions = manifest.Sources.Sessions.Sessions
	}
	// Optional enrichment source: durable facts, branch-scoped. Absent/missing
	// facts degrade gracefully (no episode_facts links).
	factsByBranch, _ := loadAllFactBranches(brainDir)

	present := map[string]bool{}
	for _, s := range sessions {
		rel := strings.TrimSpace(s.TranscriptPath)
		if rel == "" {
			continue
		}
		id := repoKey + "/" + filepath.ToSlash(rel)
		present[id] = true
		abs := filepath.Join(brainDir, filepath.FromSlash(rel))
		size, mtime, sha, readErr := transcriptFingerprint(abs)
		if readErr != nil {
			continue // missing/unreadable transcript: leave any prior rows for prune below
		}
		if corpusSessionUnchanged(db, id, size, mtime, sha) {
			continue
		}
		if err := indexSessionIntoCorpus(db, repoKey, s, id, rel, abs, size, mtime, sha, factsByBranch); err != nil {
			return err
		}
	}

	// Prune sessions no longer in the manifest (deleted transcripts).
	if err := pruneCorpusSessions(db, present); err != nil {
		return err
	}

	// Link episodes to semantic symbols (if a semantic index exists), then
	// rebuild candidate patterns, then materialize the synapse explanation graph.
	var sem *semanticSourceManifest
	if manifest.Sources != nil {
		sem = manifest.Sources.Semantic
	}
	if err := linkEpisodeSymbols(db, brainDir, sem); err != nil {
		return err
	}
	if err := buildPatternCandidates(db, repoKey, now); err != nil {
		return err
	}
	if err := buildSynapses(db, now); err != nil {
		return err
	}
	recordPatternRun(db, brainDir, now)

	return setCorpusMeta(db, map[string]string{
		"schema_version":          strconv.Itoa(patternCorpusSchemaVersion),
		"pattern_indexer_version": strconv.Itoa(patternIndexerVersion),
		"repo_key":                repoKey,
		"generated_at":            now.UTC().Format(time.RFC3339),
		"sessions_fingerprint":    brainSessionsFingerprint(brainDir),
	})
}

func corpusSessionUnchanged(db *sql.DB, id string, size, mtime int64, sha string) bool {
	var (
		gotSize, gotMtime int64
		gotSha            string
		parser            int
	)
	err := db.QueryRow(`SELECT size, mtime_unix_nano, content_sha, parser_version FROM indexed_sessions WHERE id = ?`, id).
		Scan(&gotSize, &gotMtime, &gotSha, &parser)
	if err != nil {
		return false
	}
	return gotSize == size && gotMtime == mtime && gotSha == sha && parser == patternIndexerVersion
}

func indexSessionIntoCorpus(db *sql.DB, repoKey string, s exportSession, id, rel, abs string, size, mtime int64, sha string, factsByBranch map[string][]factRecord) error {
	content, err := os.ReadFile(abs)
	if err != nil {
		return nil // tolerate: pruned later if truly gone
	}
	segments := transcriptEpisodeSegments(string(content))

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Replace: delete this session's episodes (cascades children) + its row.
	if _, err := tx.Exec(`DELETE FROM episodes WHERE repo_key = ? AND session_id = ?`, repoKey, s.SessionID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM indexed_sessions WHERE id = ?`, id); err != nil {
		return err
	}

	author := ""
	if len(s.Authors) > 0 {
		author = s.Authors[0].Name
	}
	createdAt := ""
	if !s.CreatedAt.IsZero() {
		createdAt = s.CreatedAt.UTC().Format(time.RFC3339)
	}

	episodeCount := 0
	for ord, seg := range segments {
		feedback := ""
		if ord+1 < len(segments) {
			feedback = segments[ord+1].Request.Text
		}
		committed := workCommitted(seg.WorkText)
		outcome, outcomeSource := corpusOutcome(feedback, committed)
		tools, commands, files := corpusEpisodeOps(seg.WorkText)
		exitFails := 0
		for _, c := range commands {
			if c.failed {
				exitFails++
			}
		}

		startLine := seg.Request.Line
		endLine := startLine
		if ord+1 < len(segments) && segments[ord+1].Request.Line > startLine {
			endLine = segments[ord+1].Request.Line - 1
		}

		epID := "episode:" + hexSHA(repoKey+"\x00"+filepath.ToSlash(rel)+"\x00"+strconv.Itoa(ord)+"\x00"+strconv.Itoa(startLine))
		epKey := repoKey + "/" + filepath.ToSlash(rel) + "#" + strconv.Itoa(ord)

		if _, err := tx.Exec(`INSERT INTO episodes
			(id, episode_key, repo_key, workspace, session_id, checkpoint_id, turn_id, turn_ord, branch, author_name, agent, created_at,
			 source_path, start_line, end_line, intent_raw, intent_sig, n_tools, tool_mix, files, n_cmds, exit_fails, outcome, outcome_source)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			epID, epKey, repoKey, "", s.SessionID, s.LatestCheckpoint, s.TurnID, ord, s.Branch, author, s.Agent, createdAt,
			filepath.ToSlash(rel), startLine, endLine, redactText(truncateString(seg.Request.Text, 280)), intentSignature(seg.Request.Text),
			len(tools), toolMix(tools), filesJSON(files), len(commands), exitFails, outcome, outcomeSource,
		); err != nil {
			return err
		}
		for i, t := range tools {
			if _, err := tx.Exec(`INSERT INTO episode_tools (episode_id, ord, tool_type, line) VALUES (?,?,?,?)`, epID, i, t.name, t.line); err != nil {
				return err
			}
		}
		for i, c := range commands {
			var exit any
			if c.exitCode != nil {
				exit = *c.exitCode
			}
			failed := 0
			if c.failed {
				failed = 1
			}
			if _, err := tx.Exec(`INSERT INTO episode_commands (episode_id, ord, head, raw_redacted, line, exit_code, failed) VALUES (?,?,?,?,?,?,?)`,
				epID, i, c.head, redactText(c.raw), c.line, exit, failed); err != nil {
				return err
			}
		}
		for _, f := range files {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO episode_files (episode_id, path, action, line) VALUES (?,?,?,?)`,
				epID, redactText(f.path), f.action, f.line); err != nil {
				return err
			}
		}
		heads := make([]string, len(commands))
		for i, c := range commands {
			heads[i] = c.head
		}
		for _, g := range commandGrams(heads) {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO grams (episode_id, n, gram) VALUES (?,?,?)`, epID, g.n, g.gram); err != nil {
				return err
			}
		}
		for _, h := range classifyMetaHits(seg.Request.Text, seg.WorkText, len(commands), episodeHasValidationCommand(commands)) {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO meta_hits (episode_id, meta_id, quote_redacted, line) VALUES (?,?,?,?)`,
				epID, h.metaID, h.quote, startLine); err != nil {
				return err
			}
		}
		for _, fl := range linkEpisodeFacts(factsByBranch[s.Branch], seg.Request.Text, files) {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO episode_facts (episode_id, fact_id, branch, kind, paths, weight) VALUES (?,?,?,?,?,?)`,
				epID, fl.factID, fl.branch, fl.kind, fl.paths, fl.weight); err != nil {
				return err
			}
		}
		for _, c := range corpusCommits(seg.WorkText) {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO episode_commits (episode_id, commit_hash, branch, subject, line) VALUES (?,?,?,?,?)`,
				epID, c.hash, c.branch, c.subject, c.line); err != nil {
				return err
			}
		}
		episodeCount++
	}

	if _, err := tx.Exec(`INSERT INTO indexed_sessions
		(id, repo_key, session_id, checkpoint_id, transcript_path, branch, author_name, agent, created_at, size, mtime_unix_nano, content_sha, parser_version, episodes)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, repoKey, s.SessionID, s.LatestCheckpoint, filepath.ToSlash(rel), s.Branch, author, s.Agent, createdAt, size, mtime, sha, patternIndexerVersion, episodeCount,
	); err != nil {
		return err
	}
	return tx.Commit()
}

func pruneCorpusSessions(db *sql.DB, present map[string]bool) error {
	rows, err := db.Query(`SELECT id, repo_key, session_id FROM indexed_sessions`)
	if err != nil {
		return err
	}
	type stale struct{ id, repoKey, sessionID string }
	var drop []stale
	for rows.Next() {
		var s stale
		if err := rows.Scan(&s.id, &s.repoKey, &s.sessionID); err != nil {
			rows.Close()
			return err
		}
		if !present[s.id] {
			drop = append(drop, s)
		}
	}
	rows.Close()
	for _, s := range drop {
		if _, err := db.Exec(`DELETE FROM episodes WHERE repo_key = ? AND session_id = ?`, s.repoKey, s.sessionID); err != nil {
			return err
		}
		if _, err := db.Exec(`DELETE FROM indexed_sessions WHERE id = ?`, s.id); err != nil {
			return err
		}
	}
	return nil
}

// corpusOutcome derives the episode outcome + its source from the next user turn
// and the commit signal, consistent with the v1 reinforcement classifier.
func corpusOutcome(feedback string, committed bool) (string, string) {
	norm := normalizeFeedback(feedback)
	if hasCorrectionCue(norm) {
		return "corrected", "user_feedback"
	}
	if hasApprovalCue(norm) {
		return "success", "user_feedback"
	}
	if committed {
		return "success", "commit_success"
	}
	return "neutral", "none"
}

type corpusToolOp struct {
	name string
	line int
}

// filesJSON renders the episode's unique file paths (redacted) as a sorted JSON
// array for the episodes.files column.
func filesJSON(files []corpusFileRef) string {
	seen := map[string]bool{}
	var paths []string
	for _, f := range files {
		p := redactText(f.path)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		paths = append(paths, p)
	}
	if len(paths) == 0 {
		return ""
	}
	sort.Strings(paths)
	data, err := json.Marshal(paths)
	if err != nil {
		return ""
	}
	return string(data)
}

type corpusGram struct {
	n    int
	gram string
}

// gramSep joins command heads inside a gram. It must be recoverable: command
// heads themselves contain spaces ("git push"), so a plain space join would be
// ambiguous and break per-command specificity (gramSpecificity splits on this).
const gramSep = " ▷ "

// commandGrams builds deduped command n-grams (n=2..4) within one episode, over
// the run-collapsed command heads — the procedure-shape evidence.
func commandGrams(heads []string) []corpusGram {
	collapsed := collapseRuns(heads)
	seen := map[string]bool{}
	var out []corpusGram
	for n := 2; n <= 4; n++ {
		for _, w := range commandWindows(collapsed, n) {
			key := strconv.Itoa(n) + ":" + strings.Join(w, gramSep)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, corpusGram{n: n, gram: strings.Join(w, gramSep)})
		}
	}
	return out
}

func toolMix(tools []corpusToolOp) string {
	counts := map[string]int{}
	for _, t := range tools {
		counts[t.name]++
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+":"+strconv.Itoa(counts[k]))
	}
	return strings.Join(parts, ",")
}

func transcriptFingerprint(abs string) (size, mtime int64, sha string, err error) {
	info, err := os.Stat(abs)
	if err != nil {
		return 0, 0, "", err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return 0, 0, "", err
	}
	sum := sha256.Sum256(data)
	return info.Size(), info.ModTime().UnixNano(), hex.EncodeToString(sum[:]), nil
}

func corpusMeta(db *sql.DB, key string) string {
	var v string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v); err != nil {
		return ""
	}
	return v
}

func setCorpusMeta(db *sql.DB, kv map[string]string) error {
	for k, v := range kv {
		if _, err := db.Exec(`INSERT INTO meta(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
			return err
		}
	}
	return nil
}
