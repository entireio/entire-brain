package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
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

const patternCorpusStagingPrefix = ".corpus.sqlite.build-"

const (
	patternCorpusPublicationMarkerName = ".corpus.sqlite.publish.json"
	patternCorpusPublicationMarkerPath = "patterns/" + patternCorpusPublicationMarkerName
	patternCorpusPublicationMarkerMax  = 4 << 10
	patternCorpusPublicationVersion    = 1
)

// beforePatternCorpusPublish is a deterministic failure/race seam. Production
// leaves it as a no-op; tests use it after the complete staging database has
// passed integrity checks but before the live corpus transaction begins.
var beforePatternCorpusPublish = func() error { return nil }

// patternCorpusPublishBoundary is a deterministic crash-boundary seam. Once
// the durable publication marker exists, an injected error deliberately leaves
// the marker (and, when still present, the candidate) in place. Readers then
// fail closed until the next locked writer completes recovery.
var patternCorpusPublishBoundary = func(string) error { return nil }

type patternCorpusPublication struct {
	SchemaVersion int    `json:"schema_version"`
	Candidate     string `json:"candidate"`
	SHA256        string `json:"sha256"`
	Size          int64  `json:"size"`
}

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
	// episode_shapes is the deterministic shape of each episode
	// (conversation | read_only | shell | write), derived from its tools,
	// commands, and file actions. Populated as a post-pass each build.
	`CREATE TABLE IF NOT EXISTS episode_shapes (
		episode_id TEXT PRIMARY KEY,
		shape TEXT NOT NULL,
		FOREIGN KEY (episode_id) REFERENCES episodes(id) ON DELETE CASCADE
	)`,
	// themes are conversational/read-only/judgment-heavy clusters that the
	// command/task channel misses.
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
	// deep_dossiers cache the bounded evidence-deep consolidation (capped anchors,
	// top ones with redacted transcript excerpts) + deep verifier result for a
	// pattern. Built on demand by the explicit `patterns verify --deep`, never by
	// refresh. Keyed by pattern id; cached by the backing-set fingerprint.
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

// patternCorpusReadDB owns both an immutable SQLite connection and the private
// snapshot backing it. Close always removes the snapshot, including when the
// SQLite close itself reports an error.
type patternCorpusReadDB struct {
	*sql.DB
	cleanup  func()
	closeErr error
	closeOne sync.Once
}

func (db *patternCorpusReadDB) Close() error {
	db.closeOne.Do(func() {
		db.closeErr = db.DB.Close()
		db.cleanup()
	})
	return db.closeErr
}

// rejectInterruptedPatternCorpusPublication makes the marker the publication
// fail-closed bit. It is checked on both sides of every snapshot so a reader can
// never accept a main/WAL combination observed inside the multi-file commit
// window.
func rejectInterruptedPatternCorpusPublication(brainDir string) error {
	_, present, err := privacyArtifactInfo(brainDir, patternCorpusPublicationMarkerPath, "pattern corpus publication marker")
	if err != nil {
		return err
	}
	if present {
		return fmt.Errorf("%s: pattern corpus publication was interrupted; run a locked pattern refresh to recover", memoryErrStateUnsafe)
	}
	return nil
}

// openPatternCorpusReadDB opens a bounded, descriptor-pinned private snapshot
// of the corpus. SQLite never sees the Brain-controlled pathname, and immutable
// mode prevents even the private reader from creating journals or shared-memory
// files. Reads never create or migrate schema.
func openPatternCorpusReadDB(brainDir string) (*patternCorpusReadDB, error) {
	if err := rejectInterruptedPatternCorpusPublication(brainDir); err != nil {
		return nil, err
	}
	_, present, err := privacyArtifactInfo(brainDir, patternCorpusPath, "pattern corpus")
	if err != nil {
		return nil, err
	}
	if !present {
		if markerErr := rejectInterruptedPatternCorpusPublication(brainDir); markerErr != nil {
			return nil, markerErr
		}
		return nil, fmt.Errorf("pattern corpus not found (run `entire brain patterns refresh` first): %w", os.ErrNotExist)
	}
	snapshotPath, cleanup, err := snapshotPrivacySQLiteStore(brainDir, patternCorpusPath)
	if err != nil {
		return nil, err
	}
	if err := rejectInterruptedPatternCorpusPublication(brainDir); err != nil {
		cleanup()
		return nil, err
	}
	db, err := sql.Open(sqliteDriverName, sqliteReadOnlyDSN(snapshotPath))
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("%s: open private pattern corpus snapshot: %w", memoryErrStateCorrupt, err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		cleanup()
		return nil, fmt.Errorf("%s: open private pattern corpus snapshot: %w", memoryErrStateCorrupt, err)
	}
	return &patternCorpusReadDB{DB: db, cleanup: cleanup}, nil
}

// patternCorpusHasTable reports whether a table exists in this snapshot.
//
// Reads open an immutable snapshot and deliberately never apply schema, so an
// on-disk corpus written before an ADDITIVE table (themes, deep_dossiers) still
// has to answer queries against it. The read path previously opened read-write
// and ran the CREATE TABLE IF NOT EXISTS schema for exactly this reason; with
// that gone, a legacy corpus made `patterns list` and `patterns skills` fail
// with "no such table" where they used to return an empty set. There is no
// read-time version gate to lean on (schema_version is written but never
// compared), so the readers check for the table and treat absent as empty. The
// next `patterns refresh` creates it.
func patternCorpusHasTable(db *sql.DB, table string) (bool, error) {
	var name string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%s: inspect pattern corpus schema: %w", memoryErrStateCorrupt, err)
	}
	return true, nil
}

// openPatternCorpusReadDBIfPresent preserves the only graceful fallback: a
// genuinely absent rebuildable corpus. Unsafe aliases, inconsistent snapshots,
// corruption, and permission failures remain errors and must reach the command
// boundary rather than masquerading as "not built".
func openPatternCorpusReadDBIfPresent(brainDir string) (*patternCorpusReadDB, bool, error) {
	db, err := openPatternCorpusReadDB(brainDir)
	if err == nil {
		return db, true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	return nil, false, err
}

// validatePatternCorpusReadSafety is the cheap command-boundary check used by
// best-effort surfaces whose legacy loader signatures cannot return errors. The
// snapshot opener repeats these checks and pins descriptors, so a path race can
// only make the optional read disappear; it can never redirect SQLite outside
// the Brain. Missing corpora remain a supported legacy/fresh-install state.
func validatePatternCorpusReadSafety(brainDir string) error {
	if err := rejectInterruptedPatternCorpusPublication(brainDir); err != nil {
		return err
	}
	_, present, err := privacyArtifactInfo(brainDir, patternCorpusPath, "pattern corpus")
	if err != nil {
		return err
	}
	if !present {
		return rejectInterruptedPatternCorpusPublication(brainDir)
	}
	_, _, err = privacyArtifactInfo(brainDir, patternCorpusPath+"-wal", "pattern corpus WAL")
	if err != nil {
		return err
	}
	return rejectInterruptedPatternCorpusPublication(brainDir)
}

func requirePatternCorpusAvailable(brainDir string) error {
	if err := validatePatternCorpusReadSafety(brainDir); err != nil {
		return err
	}
	_, present, err := privacyArtifactInfo(brainDir, patternCorpusPath, "pattern corpus")
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("%s: pattern corpus is unavailable; run `entire brain patterns refresh`", memoryErrSourceStale)
	}
	return nil
}

// buildPatternCorpus (re)indexes the brain's sessions into patterns/corpus.sqlite.
// Idempotent: unchanged sessions are skipped, changed sessions are replaced in a
// transaction, removed transcripts are pruned, and an indexer-version bump forces
// a full re-index. Deterministic and token-free.
func buildPatternCorpus(brainDir string, now time.Time) error {
	// Session manifests, exported transcripts, facts, tombstones, corpus
	// verifiers, and purge all use this lock for their mutation boundary. The
	// deterministic build can therefore take a coherent input snapshot and a
	// purge cannot verify clean before a stale corpus is published.
	return withBrainWriteLock(brainDir, func() error {
		return buildPatternCorpusLocked(brainDir, now)
	})
}

func buildPatternCorpusLocked(brainDir string, now time.Time) error {
	if err := recoverPatternCorpusPublicationLocked(brainDir); err != nil {
		return err
	}
	manifest, err := loadPatternCorpusManifest(brainDir)
	if err != nil {
		return err
	}
	guard, err := loadSessionReadGuard(brainDir, manifest)
	if err != nil {
		return err
	}
	path, cleanup, err := preparePatternCorpusStaging(brainDir)
	if err != nil {
		return err
	}
	defer cleanup()
	db, err := sql.Open(sqliteDriverName, path)
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			_ = db.Close()
		}
	}()
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
	// Session tombstones (Phase 4): excluded sessions and duplicate manifest
	// entries that alias an excluded transcript never enter the corpus; dropping
	// them from the present set prunes previously indexed rows below.
	sessions = guardExportSessions(guard, sessions)
	// Optional enrichment source: durable facts, branch-scoped. Missing is
	// empty, but corrupt/unsafe state fails closed and tombstoned-only facts are
	// filtered before they can be linked back into a rebuilt corpus.
	factsByBranch, err := loadAllFactBranchesForPrivacy(brainDir)
	if err != nil {
		return err
	}
	for branch, facts := range factsByBranch {
		factsByBranch[branch] = guardFactRecords(guard, facts)
	}

	present := map[string]bool{}
	for _, s := range sessions {
		rel := strings.TrimSpace(s.TranscriptPath)
		if rel == "" {
			continue
		}
		id := repoKey + "/" + filepath.ToSlash(rel)
		content, size, mtime, sha, readErr := readPatternCorpusTranscript(context.Background(), brainDir, filepath.ToSlash(rel))
		if readErr != nil {
			if errors.Is(readErr, os.ErrNotExist) {
				return &historySessionInventoryError{
					Path:   filepath.ToSlash(rel),
					Reason: "manifest-listed canonical transcript is missing; refusing a partial corpus publication",
					Err:    readErr,
				}
			}
			return readErr
		}
		present[id] = true
		if corpusSessionUnchanged(db, id, size, mtime, sha) {
			continue
		}
		if err := indexSessionIntoCorpus(db, repoKey, s, id, rel, content, size, mtime, sha, factsByBranch); err != nil {
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
	// Episode shapes (deterministic, token-free). Themes are NOT built here:
	// they are agent-proposed semantic clusters, created only by the explicit
	// `patterns verify --themes` (egress-gated) — refresh stays token-free.
	if err := classifyEpisodeShapes(db, now); err != nil {
		return err
	}
	if err := buildSynapses(db, now); err != nil {
		return err
	}
	_, finalPolicy, err := loadSessionTombstonesChecked(brainDir)
	if err != nil {
		return err
	}
	if finalPolicy.Identity != guard.policyIdentity {
		return fmt.Errorf("%s: session tombstones changed while building the pattern corpus", memoryErrPrivacyDirty)
	}
	if err := setCorpusMeta(db, map[string]string{
		"schema_version":          strconv.Itoa(patternCorpusSchemaVersion),
		"pattern_indexer_version": strconv.Itoa(patternIndexerVersion),
		"privacy_policy_identity": guard.policyIdentity,
		"repo_key":                repoKey,
		"generated_at":            now.UTC().Format(time.RFC3339),
		"sessions_fingerprint": func() string {
			if manifest == nil || manifest.Sources == nil {
				return ""
			}
			return sessionSourceFingerprint(manifest.Sources.Sessions)
		}(),
	}); err != nil {
		return err
	}
	run, err := patternRunFromStaging(db, now)
	if err != nil {
		return err
	}
	if err := finalizePatternCorpusStagingLocked(brainDir, path, db); err != nil {
		return err
	}
	closed = true
	// Operational run history describes only successfully published corpora.
	// A failed staging build or commit must leave both the old corpus and its
	// last-run report intact.
	_ = appendPatternRun(brainDir, run)
	return nil
}

// withPatternCorpusMutationLocked clones the live corpus into a private
// staging database, applies one complete mutation there, and publishes only a
// normalized, integrity-checked result. The caller must hold the Brain lock.
func withPatternCorpusMutationLocked(brainDir string, mutate func(*sql.DB) error) error {
	if err := recoverPatternCorpusPublicationLocked(brainDir); err != nil {
		return err
	}
	_, present, err := privacyArtifactInfo(brainDir, patternCorpusPath, "pattern corpus")
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("pattern corpus not found (run `entire brain patterns refresh` first): %w", os.ErrNotExist)
	}
	path, cleanup, err := preparePatternCorpusStaging(brainDir)
	if err != nil {
		return err
	}
	defer cleanup()
	db, err := sql.Open(sqliteDriverName, path)
	if err != nil {
		return err
	}
	defer db.Close()
	for _, pragma := range []string{"PRAGMA busy_timeout=5000", "PRAGMA foreign_keys=ON"} {
		if _, err := db.Exec(pragma); err != nil {
			return err
		}
	}
	for _, stmt := range patternCorpusSchema {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	if err := mutate(db); err != nil {
		return err
	}
	return finalizePatternCorpusStagingLocked(brainDir, path, db)
}

func finalizePatternCorpusStagingLocked(brainDir, path string, db *sql.DB) error {
	// No committed rows may remain only in a staging WAL.
	var busy, logFrames, checkpointedFrames int
	if err := db.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointedFrames); err != nil || busy != 0 || (logFrames > 0 && checkpointedFrames != logFrames) {
		return fmt.Errorf("checkpoint staged pattern corpus before publication")
	}
	// Candidate construction legitimately uses more than one database/sql
	// connection. Close the pool before changing journal mode, which requires
	// the normalizer to be the sole live connection.
	if err := db.Close(); err != nil {
		return err
	}
	normalizer, err := sql.Open(sqliteDriverName, path)
	if err != nil {
		return err
	}
	normalizer.SetMaxOpenConns(1)
	if _, err := normalizer.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		_ = normalizer.Close()
		return err
	}
	if _, err := normalizer.Exec(`PRAGMA journal_mode=DELETE`); err != nil {
		_ = normalizer.Close()
		return fmt.Errorf("normalize staged pattern corpus journal: %w", err)
	}
	var integrity string
	if err := normalizer.QueryRow(`PRAGMA quick_check`).Scan(&integrity); err != nil || integrity != "ok" {
		_ = normalizer.Close()
		return fmt.Errorf("%s: staged pattern corpus failed integrity check", memoryErrStateCorrupt)
	}
	if err := normalizer.Close(); err != nil {
		return err
	}
	if err := beforePatternCorpusPublish(); err != nil {
		return err
	}
	return publishPatternCorpusLocked(brainDir, path)
}

func loadPatternCorpusManifest(brainDir string) (*exportManifest, error) {
	// Derivation must use the same header-first, strict version classifier as
	// every other Brain-manifest consumer. In particular, never partially decode
	// or normalize a vNext document into the current schema before rebuilding.
	return loadBrainManifest(brainDir)
}

// preparePatternCorpusStaging creates a process-private build leaf outside the
// Brain tree. When a corpus already exists it clones a checked, descriptor-bound
// SQLite snapshot so expensive verifier caches survive the deterministic
// rebuild. SQLite never receives a path beneath a replaceable Brain directory
// while analysis is in progress.
func preparePatternCorpusStaging(brainDir string) (string, func(), error) {
	tempDir, err := os.MkdirTemp("", "entire-brain-pattern-corpus-")
	if err != nil {
		return "", nil, fmt.Errorf("create private pattern corpus staging directory: %w", err)
	}
	stagePath := filepath.Join(tempDir, "corpus.sqlite")
	stage, err := os.OpenFile(stagePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	cleanup := func() {
		if stage != nil {
			_ = stage.Close()
		}
		_ = os.RemoveAll(tempDir)
	}
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("create staged pattern corpus: %w", err)
	}
	_, present, err := privacyArtifactInfo(brainDir, patternCorpusPath, "pattern corpus")
	if err != nil {
		cleanup()
		return "", nil, err
	}
	if present {
		snapshot, snapshotCleanup, err := snapshotPrivacySQLiteStore(brainDir, patternCorpusPath)
		if err != nil {
			cleanup()
			return "", nil, err
		}
		defer snapshotCleanup()
		source, err := os.Open(snapshot)
		if err != nil {
			cleanup()
			return "", nil, err
		}
		copied, copyErr := io.Copy(stage, io.LimitReader(source, privacySQLiteSnapshotMaxBytes+1))
		closeErr := source.Close()
		if copyErr != nil || closeErr != nil || copied > privacySQLiteSnapshotMaxBytes {
			cleanup()
			return "", nil, fmt.Errorf("copy checked pattern corpus into staging")
		}
	}
	if err := stage.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return stagePath, cleanup, nil
}

var patternCorpusSidecarNames = []string{"corpus.sqlite-wal", "corpus.sqlite-shm", "corpus.sqlite-journal"}

type pinnedPatternCorpusDirectory struct {
	brainDir     string
	brainInfo    os.FileInfo
	patternsInfo os.FileInfo
	root         *os.Root
	patternsRoot *os.Root
}

func (p *pinnedPatternCorpusDirectory) Close() {
	if p == nil {
		return
	}
	if p.patternsRoot != nil {
		_ = p.patternsRoot.Close()
	}
	if p.root != nil {
		_ = p.root.Close()
	}
}

func openPinnedPatternCorpusDirectory(brainDir string, create bool) (*pinnedPatternCorpusDirectory, bool, error) {
	brainInfo, err := os.Lstat(brainDir)
	if err != nil {
		return nil, false, fmt.Errorf("%s: inspect Brain root for corpus publication: %w", memoryErrStateUnsafe, err)
	}
	if brainInfo.Mode()&os.ModeSymlink != 0 || !brainInfo.IsDir() {
		return nil, false, fmt.Errorf("%s: Brain root is not a stable directory", memoryErrStateUnsafe)
	}
	root, err := os.OpenRoot(brainDir)
	if err != nil {
		return nil, false, fmt.Errorf("%s: open Brain root for corpus publication: %w", memoryErrStateUnsafe, err)
	}
	openedBrain, err := root.Stat(".")
	if err != nil || !openedBrain.IsDir() || !os.SameFile(brainInfo, openedBrain) {
		_ = root.Close()
		return nil, false, fmt.Errorf("%s: Brain root changed while it was being pinned", memoryErrStateUnsafe)
	}
	if create {
		if err := root.MkdirAll("patterns", 0o700); err != nil {
			_ = root.Close()
			return nil, false, fmt.Errorf("%s: create pinned pattern corpus directory: %w", memoryErrStateUnsafe, err)
		}
	}
	patternsInfo, err := root.Lstat("patterns")
	if os.IsNotExist(err) && !create {
		_ = root.Close()
		return nil, false, nil
	}
	if err != nil {
		_ = root.Close()
		return nil, false, fmt.Errorf("%s: inspect pattern corpus directory: %w", memoryErrStateUnsafe, err)
	}
	if patternsInfo.Mode()&os.ModeSymlink != 0 || !patternsInfo.IsDir() {
		_ = root.Close()
		return nil, false, fmt.Errorf("%s: pattern corpus directory is not a regular directory", memoryErrStateUnsafe)
	}
	patternsRoot, err := root.OpenRoot("patterns")
	if err != nil {
		_ = root.Close()
		return nil, false, fmt.Errorf("%s: pin pattern corpus directory: %w", memoryErrStateUnsafe, err)
	}
	pinned := &pinnedPatternCorpusDirectory{
		brainDir: brainDir, brainInfo: brainInfo, patternsInfo: patternsInfo,
		root: root, patternsRoot: patternsRoot,
	}
	if err := pinned.validate(); err != nil {
		pinned.Close()
		return nil, false, err
	}
	return pinned, true, nil
}

func (p *pinnedPatternCorpusDirectory) validate() error {
	currentBrain, err := os.Lstat(p.brainDir)
	if err != nil || currentBrain.Mode()&os.ModeSymlink != 0 || !os.SameFile(p.brainInfo, currentBrain) {
		return fmt.Errorf("%s: Brain root changed during corpus publication", memoryErrStateUnsafe)
	}
	pinnedBrain, err := p.root.Stat(".")
	if err != nil || !pinnedBrain.IsDir() || !os.SameFile(p.brainInfo, pinnedBrain) {
		return fmt.Errorf("%s: pinned Brain root changed during corpus publication", memoryErrStateUnsafe)
	}
	currentPatterns, err := p.root.Lstat("patterns")
	if err != nil || currentPatterns.Mode()&os.ModeSymlink != 0 || !os.SameFile(p.patternsInfo, currentPatterns) {
		return fmt.Errorf("%s: pattern corpus directory changed during publication", memoryErrStateUnsafe)
	}
	pinned, err := p.patternsRoot.Stat(".")
	if err != nil || !pinned.IsDir() || !os.SameFile(p.patternsInfo, pinned) {
		return fmt.Errorf("%s: pinned pattern corpus directory changed during publication", memoryErrStateUnsafe)
	}
	return nil
}

func inspectPinnedPatternCorpusLeaf(root *os.Root, name, label string) (os.FileInfo, bool, error) {
	info, err := root.Lstat(name)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("%s: inspect %s: %w", memoryErrStateUnsafe, label, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("%s: %s is not a regular file", memoryErrStateUnsafe, label)
	}
	return info, true, nil
}

func removePinnedPatternCorpusSidecars(pinned *pinnedPatternCorpusDirectory, useHooks bool) error {
	for _, sidecar := range patternCorpusSidecarNames {
		if useHooks {
			if err := patternCorpusPublishBoundary("before_remove_" + sidecar); err != nil {
				return err
			}
		}
		_, present, err := inspectPinnedPatternCorpusLeaf(pinned.patternsRoot, sidecar, "pattern corpus sidecar "+sidecar)
		if err != nil {
			return err
		}
		if present {
			if err := pinned.patternsRoot.Remove(sidecar); err != nil {
				return fmt.Errorf("remove stale pattern corpus sidecar %s: %w", sidecar, err)
			}
		}
		if useHooks {
			if err := patternCorpusPublishBoundary("after_remove_" + sidecar); err != nil {
				return err
			}
		}
	}
	return nil
}

func readPinnedPatternCorpusPublication(pinned *pinnedPatternCorpusDirectory) (patternCorpusPublication, os.FileInfo, error) {
	var publication patternCorpusPublication
	info, present, err := inspectPinnedPatternCorpusLeaf(pinned.patternsRoot, patternCorpusPublicationMarkerName, "pattern corpus publication marker")
	if err != nil {
		return publication, nil, err
	}
	if !present || info.Size() < 1 || info.Size() > patternCorpusPublicationMarkerMax {
		return publication, nil, fmt.Errorf("%s: pattern corpus publication marker is missing or malformed", memoryErrStateUnsafe)
	}
	marker, err := pinned.patternsRoot.OpenFile(patternCorpusPublicationMarkerName, os.O_RDONLY|fileLockOpenFlags()|memoryStateReadOpenFlags(), 0)
	if err != nil {
		return publication, nil, fmt.Errorf("%s: open pattern corpus publication marker: %w", memoryErrStateUnsafe, err)
	}
	defer marker.Close()
	opened, err := marker.Stat()
	if err != nil || !os.SameFile(info, opened) || opened.Size() != info.Size() {
		return publication, nil, fmt.Errorf("%s: pattern corpus publication marker changed while opening", memoryErrStateUnsafe)
	}
	if err := rejectOpenFileHardlink(patternCorpusPublicationMarkerName, marker, opened, "pattern corpus publication marker"); err != nil {
		return publication, nil, fmt.Errorf("%s: %w", memoryErrStateUnsafe, err)
	}
	data, err := io.ReadAll(io.LimitReader(marker, patternCorpusPublicationMarkerMax+1))
	if err != nil || int64(len(data)) > patternCorpusPublicationMarkerMax {
		return publication, nil, fmt.Errorf("%s: read bounded pattern corpus publication marker", memoryErrStateUnsafe)
	}
	current, present, err := inspectPinnedPatternCorpusLeaf(pinned.patternsRoot, patternCorpusPublicationMarkerName, "pattern corpus publication marker")
	if err != nil || !present || !os.SameFile(info, current) || current.Size() != info.Size() || !current.ModTime().Equal(info.ModTime()) {
		return publication, nil, fmt.Errorf("%s: pattern corpus publication marker changed while reading", memoryErrStateUnsafe)
	}
	publication, err = decodePatternCorpusPublication(data)
	if err != nil {
		return patternCorpusPublication{}, nil, err
	}
	return publication, info, nil
}

// validatePinnedPatternCorpusDatabase copies a descriptor-pinned leaf to a
// private directory before asking SQLite to inspect it. Recovery never lets
// SQLite open a Brain-controlled pathname or create a sidecar there.
func validatePinnedPatternCorpusDatabase(root *os.Root, name string, publication patternCorpusPublication) (os.FileInfo, error) {
	info, present, err := inspectPinnedPatternCorpusLeaf(root, name, "pattern corpus recovery candidate")
	if err != nil {
		return nil, err
	}
	if !present || info.Size() != publication.Size {
		return nil, fmt.Errorf("%s: pattern corpus recovery candidate is missing or changed", memoryErrStateUnsafe)
	}
	source, err := root.OpenFile(name, os.O_RDONLY|fileLockOpenFlags()|memoryStateReadOpenFlags(), 0)
	if err != nil {
		return nil, fmt.Errorf("%s: open pattern corpus recovery candidate: %w", memoryErrStateUnsafe, err)
	}
	defer source.Close()
	opened, err := source.Stat()
	if err != nil || !os.SameFile(info, opened) || opened.Size() != info.Size() {
		return nil, fmt.Errorf("%s: pattern corpus recovery candidate changed while opening", memoryErrStateUnsafe)
	}
	if err := rejectOpenFileHardlink(name, source, opened, "pattern corpus recovery candidate"); err != nil {
		return nil, fmt.Errorf("%s: %w", memoryErrStateUnsafe, err)
	}
	tempDir, err := os.MkdirTemp("", "entire-brain-pattern-recovery-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tempDir)
	snapshotPath := filepath.Join(tempDir, "corpus.sqlite")
	snapshot, err := os.OpenFile(snapshotPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	hash := sha256.New()
	copied, copyErr := io.Copy(io.MultiWriter(snapshot, hash), io.LimitReader(source, privacySQLiteSnapshotMaxBytes+1))
	syncErr := snapshot.Sync()
	closeErr := snapshot.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil || copied != publication.Size || copied > privacySQLiteSnapshotMaxBytes || hex.EncodeToString(hash.Sum(nil)) != publication.SHA256 {
		return nil, fmt.Errorf("%s: pattern corpus recovery candidate failed its durable digest", memoryErrStateUnsafe)
	}
	after, statErr := source.Stat()
	current, currentPresent, currentErr := inspectPinnedPatternCorpusLeaf(root, name, "pattern corpus recovery candidate")
	if statErr != nil || currentErr != nil || !currentPresent || !os.SameFile(info, after) || !os.SameFile(info, current) || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return nil, fmt.Errorf("%s: pattern corpus recovery candidate changed while validating", memoryErrStateUnsafe)
	}
	if err := validatePrivacySQLiteSnapshot(snapshotPath, name); err != nil {
		return nil, err
	}
	return info, nil
}

// recoverPatternCorpusPublicationLocked completes an interrupted marked
// publication. The marker names and hashes one normalized candidate. If that
// candidate has already been renamed, the same digest must be present at the
// live name. Any ambiguous state remains marked and therefore unreadable.
func recoverPatternCorpusPublicationLocked(brainDir string) error {
	_, present, err := privacyArtifactInfo(brainDir, patternCorpusPublicationMarkerPath, "pattern corpus publication marker")
	if err != nil || !present {
		return err
	}
	pinned, present, err := openPinnedPatternCorpusDirectory(brainDir, false)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("%s: pattern corpus publication directory disappeared during recovery", memoryErrStateUnsafe)
	}
	defer pinned.Close()
	publication, markerInfo, err := readPinnedPatternCorpusPublication(pinned)
	if err != nil {
		return err
	}
	if err := pinned.validate(); err != nil {
		return err
	}
	candidateInfo, candidatePresent, err := inspectPinnedPatternCorpusLeaf(pinned.patternsRoot, publication.Candidate, "pattern corpus recovery candidate")
	if err != nil {
		return err
	}
	if candidatePresent {
		candidateInfo, err = validatePinnedPatternCorpusDatabase(pinned.patternsRoot, publication.Candidate, publication)
	} else {
		candidateInfo, err = validatePinnedPatternCorpusDatabase(pinned.patternsRoot, "corpus.sqlite", publication)
	}
	if err != nil {
		return err
	}
	if err := removePinnedPatternCorpusSidecars(pinned, false); err != nil {
		return err
	}
	if candidatePresent {
		current, stillPresent, err := inspectPinnedPatternCorpusLeaf(pinned.patternsRoot, publication.Candidate, "pattern corpus recovery candidate")
		if err != nil || !stillPresent || !os.SameFile(candidateInfo, current) {
			return fmt.Errorf("%s: pattern corpus recovery candidate changed before rename", memoryErrStateUnsafe)
		}
		if err := pinned.patternsRoot.Rename(publication.Candidate, "corpus.sqlite"); err != nil {
			return fmt.Errorf("recover pinned pattern corpus: %w", err)
		}
	}
	if err := syncPinnedDirectory(pinned.patternsRoot); err != nil {
		return fmt.Errorf("sync recovered pattern corpus directory: %w", err)
	}
	if _, err := validatePinnedPatternCorpusDatabase(pinned.patternsRoot, "corpus.sqlite", publication); err != nil {
		return err
	}
	currentMarker, markerPresent, err := inspectPinnedPatternCorpusLeaf(pinned.patternsRoot, patternCorpusPublicationMarkerName, "pattern corpus publication marker")
	if err != nil || !markerPresent || !os.SameFile(markerInfo, currentMarker) {
		return fmt.Errorf("%s: pattern corpus publication marker changed before recovery commit", memoryErrStateUnsafe)
	}
	if err := pinned.patternsRoot.Remove(patternCorpusPublicationMarkerName); err != nil {
		return fmt.Errorf("remove recovered pattern corpus marker: %w", err)
	}
	if err := syncPinnedDirectory(pinned.patternsRoot); err != nil {
		return fmt.Errorf("sync recovered pattern corpus marker removal: %w", err)
	}
	return pinned.validate()
}

// publishPatternCorpusLocked installs a complete, normalized staging database
// through descriptor-rooted creation and rename. Replacing a SQLite main file
// and removing its possible old sidecars is not one portable atomic operation,
// so a durable marker brackets that window. A crash before the marker leaves
// the old logical database untouched; a crash after it makes every reader fail
// closed until the locked recovery above completes either the named candidate
// or the already-renamed database.
func publishPatternCorpusLocked(brainDir, stagingPath string) error {
	if err := recoverPatternCorpusPublicationLocked(brainDir); err != nil {
		return err
	}
	staging, err := os.Open(stagingPath)
	if err != nil {
		return err
	}
	defer staging.Close()
	stagingInfo, err := staging.Stat()
	if err != nil || !stagingInfo.Mode().IsRegular() || stagingInfo.Size() < 1 || stagingInfo.Size() > privacySQLiteSnapshotMaxBytes {
		return fmt.Errorf("%s: invalid staged pattern corpus", memoryErrStateUnsafe)
	}
	pinned, _, err := openPinnedPatternCorpusDirectory(brainDir, true)
	if err != nil {
		return err
	}
	defer pinned.Close()
	if _, markerPresent, err := inspectPinnedPatternCorpusLeaf(pinned.patternsRoot, patternCorpusPublicationMarkerName, "pattern corpus publication marker"); err != nil || markerPresent {
		if err != nil {
			return err
		}
		return fmt.Errorf("%s: pattern corpus publication marker unexpectedly exists", memoryErrStateUnsafe)
	}
	if _, currentPresent, err := inspectPinnedPatternCorpusLeaf(pinned.patternsRoot, "corpus.sqlite", "live pattern corpus"); err != nil {
		return err
	} else if !currentPresent {
		// A first publication has no old logical database; the marker still
		// brackets its transition from absent to complete.
	}
	for _, sidecar := range patternCorpusSidecarNames {
		if _, _, err := inspectPinnedPatternCorpusLeaf(pinned.patternsRoot, sidecar, "pattern corpus sidecar "+sidecar); err != nil {
			return err
		}
	}

	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	tempName := patternCorpusStagingPrefix + hex.EncodeToString(random[:])
	temp, err := pinned.patternsRoot.OpenFile(tempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL|fileLockOpenFlags(), 0o600)
	if err != nil {
		return fmt.Errorf("%s: create pinned corpus publication leaf: %w", memoryErrStateUnsafe, err)
	}
	removeTemp := true
	defer func() {
		_ = temp.Close()
		if removeTemp {
			_ = pinned.patternsRoot.Remove(tempName)
		}
	}()
	hash := sha256.New()
	copied, copyErr := io.Copy(io.MultiWriter(temp, hash), io.LimitReader(staging, privacySQLiteSnapshotMaxBytes+1))
	if copyErr != nil || copied != stagingInfo.Size() || copied > privacySQLiteSnapshotMaxBytes {
		return fmt.Errorf("%s: staged pattern corpus changed or exceeded bounds during publication", memoryErrStateUnsafe)
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := syncPinnedDirectory(pinned.patternsRoot); err != nil {
		return fmt.Errorf("sync pattern corpus candidate directory entry: %w", err)
	}
	publication := patternCorpusPublication{
		SchemaVersion: patternCorpusPublicationVersion,
		Candidate:     tempName,
		SHA256:        hex.EncodeToString(hash.Sum(nil)),
		Size:          copied,
	}
	markerData, err := json.Marshal(publication)
	if err != nil {
		return err
	}
	markerData = append(markerData, '\n')
	if err := patternCorpusPublishBoundary("before_marker_create"); err != nil {
		return err
	}
	marker, err := pinned.patternsRoot.OpenFile(patternCorpusPublicationMarkerName, os.O_WRONLY|os.O_CREATE|os.O_EXCL|fileLockOpenFlags(), 0o600)
	if err != nil {
		return fmt.Errorf("%s: create pattern corpus publication marker: %w", memoryErrStateUnsafe, err)
	}
	written, writeErr := marker.Write(markerData)
	syncErr := marker.Sync()
	closeErr := marker.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil || written != len(markerData) {
		_ = pinned.patternsRoot.Remove(patternCorpusPublicationMarkerName)
		return fmt.Errorf("write durable pattern corpus publication marker")
	}
	// Retain the candidate as soon as a complete marker file can become
	// visible. Even if the following directory sync reports an error, recovery
	// must have both artifacts available whenever that marker survives.
	removeTemp = false
	if err := syncPinnedDirectory(pinned.patternsRoot); err != nil {
		return fmt.Errorf("sync pattern corpus publication marker: %w", err)
	}
	// From this point on the marker is the durable fail-closed bit.
	if err := patternCorpusPublishBoundary("after_marker_create"); err != nil {
		return err
	}
	if err := pinned.validate(); err != nil {
		return err
	}
	if err := removePinnedPatternCorpusSidecars(pinned, true); err != nil {
		return err
	}
	if err := patternCorpusPublishBoundary("before_main_rename"); err != nil {
		return err
	}
	if err := pinned.patternsRoot.Rename(tempName, "corpus.sqlite"); err != nil {
		return fmt.Errorf("publish pinned pattern corpus: %w", err)
	}
	if err := patternCorpusPublishBoundary("after_main_rename"); err != nil {
		return err
	}
	if err := syncPinnedDirectory(pinned.patternsRoot); err != nil {
		return fmt.Errorf("sync published pattern corpus: %w", err)
	}
	if err := patternCorpusPublishBoundary("after_main_sync"); err != nil {
		return err
	}
	if err := pinned.validate(); err != nil {
		return err
	}
	publicationMarker, markerInfo, err := readPinnedPatternCorpusPublication(pinned)
	if err != nil || publicationMarker != publication {
		return fmt.Errorf("%s: pattern corpus publication marker changed before commit", memoryErrStateUnsafe)
	}
	if err := patternCorpusPublishBoundary("before_marker_remove"); err != nil {
		return err
	}
	currentMarker, markerPresent, err := inspectPinnedPatternCorpusLeaf(pinned.patternsRoot, patternCorpusPublicationMarkerName, "pattern corpus publication marker")
	if err != nil || !markerPresent || !os.SameFile(markerInfo, currentMarker) {
		return fmt.Errorf("%s: pattern corpus publication marker changed before removal", memoryErrStateUnsafe)
	}
	if err := pinned.patternsRoot.Remove(patternCorpusPublicationMarkerName); err != nil {
		return fmt.Errorf("commit pattern corpus publication: %w", err)
	}
	if err := patternCorpusPublishBoundary("after_marker_remove"); err != nil {
		return err
	}
	if err := syncPinnedDirectory(pinned.patternsRoot); err != nil {
		return fmt.Errorf("sync pattern corpus publication commit: %w", err)
	}
	if err := patternCorpusPublishBoundary("after_marker_remove_sync"); err != nil {
		return err
	}
	return pinned.validate()
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

func indexSessionIntoCorpus(db *sql.DB, repoKey string, s exportSession, id, rel string, content []byte, size, mtime int64, sha string, factsByBranch map[string][]factRecord) error {
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

// readPatternCorpusTranscript uses the same descriptor-bound canonical session
// opener as history projection. It performs one bounded read, then calls the
// mandatory finish check to prove the leaf and every path component retained
// their exact identity for the complete read.
func readPatternCorpusTranscript(ctx context.Context, brainDir, rel string) (data []byte, size, mtime int64, sha string, err error) {
	f, finish, err := openCanonicalHistoryTranscript(ctx, brainDir, rel)
	if err != nil {
		return nil, 0, 0, "", err
	}
	info, statErr := f.Stat()
	if statErr != nil {
		_ = finish()
		return nil, 0, 0, "", statErr
	}
	data, readErr := safeReadAll(contextCheckingReader{ctx: ctx, r: f}, maxDocumentTranscriptBytes, "pattern transcript "+rel)
	finishErr := finish()
	if readErr != nil || finishErr != nil {
		return nil, 0, 0, "", errors.Join(readErr, finishErr)
	}
	sum := sha256.Sum256(data)
	return data, info.Size(), info.ModTime().UnixNano(), hex.EncodeToString(sum[:]), nil
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
