package cli

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	"github.com/entireio/entire-brain/internal/cli/internal/semstore"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"github.com/entireio/entire-brain/internal/cli/internal/sqlitedriver"
	patterncore "github.com/entireio/entire-brain/internal/cli/patterns"
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

// onPatternCorpusSessionIndexed names each session a build actually re-indexed.
//
// It is a test seam because there is no other way to tell a SKIP from a
// re-index: nothing in indexed_sessions changes when a byte-identical
// transcript is re-indexed (no indexed_at column, and content_sha, size,
// mtime and parser_version are all unchanged by definition). A test that
// compared those columns passed whether the session was skipped or rebuilt --
// proven by a weakening run that restored the global fingerprint and left the
// guard green. Counting the work is the only honest assertion.
var onPatternCorpusSessionIndexed = func(id string) {}

const (
	patternCorpusSchemaVersion = 1
	// Bump when the parser/extractor output changes in a way that requires
	// re-indexing already-indexed sessions. v2: Phase 2 enrichment (exit codes,
	// files, meta-hits, fact links).
)

func requirePatternCorpusAvailable(brainDir string) error {
	if err := patterncore.ValidateCorpusReadSafety(brainDir); err != nil {
		return err
	}
	_, present, err := brainstore.PrivacyArtifactInfo(brainDir, patterncore.CorpusPath, "pattern corpus")
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("%s: pattern corpus is unavailable; run `"+base.BrainInvocation(os.LookupEnv, "patterns refresh")+"`", brainstore.SourceStaleCode)
	}
	return nil
}

// patternCorpusGraphContext supplies the live checkout and caller's provider
// execution context. Persisted checkout paths are not valid locators after a
// move or bundle import; the corpus can still build when Graph is unavailable.
type patternCorpusGraphContext struct {
	Context context.Context
	Options Options
	RepoDir string
	Binary  string
}

func (g patternCorpusGraphContext) source(brainDir string, manifest *exportManifest) *semanticSourceManifest {
	storage, err := repoStoragePaths(g.Context, g.Options.Runner, g.Options.Env, g.RepoDir)
	if err != nil || !semanticRepoKeyEqual(storage.Key, manifest.RepoKey) {
		return nil
	}
	d, err := fetchGraphStore(g.Context, g.Options, g.RepoDir, g.Binary, graphStoreProfile(manifest))
	if err != nil {
		return nil
	}
	if err := validateSemanticProviderRepoKey(g.Context, g.Options.Runner, g.RepoDir, storage.Key, d.Header.RepoKey, "", g.Binary); err != nil {
		return nil
	}
	return semstore.GraphStoreSource(d, brainDir)
}

// buildPatternCorpus (re)indexes the brain's sessions into patterns/corpus.sqlite.
// Idempotent: unchanged sessions are skipped, changed sessions are replaced in a
// transaction, removed transcripts are pruned, and an indexer-version bump forces
// a full re-index. Deterministic and token-free.
func buildPatternCorpus(brainDir string, now time.Time, graph ...patternCorpusGraphContext) error {
	// Session manifests, exported transcripts, facts, tombstones, corpus
	// verifiers, and purge all use this lock for their mutation boundary. The
	// deterministic build can therefore take a coherent input snapshot and a
	// purge cannot verify clean before a stale corpus is published.
	return brainstore.WithBrainWriteLock(brainDir, func() error {
		return buildPatternCorpusLocked(brainDir, now, graph...)
	})
}

func buildPatternCorpusLocked(brainDir string, now time.Time, graph ...patternCorpusGraphContext) error {
	if err := patterncore.RecoverCorpusPublicationLocked(brainDir, patterncore.DecodeCorpusPublication); err != nil {
		return err
	}
	manifest, err := loadPatternCorpusManifest(brainDir)
	if err != nil {
		return err
	}
	guard, err := bmanifest.LoadSessionReadGuard(brainDir, manifest)
	if err != nil {
		return err
	}
	path, cleanup, err := patterncore.PrepareCorpusStaging(brainDir)
	if err != nil {
		return err
	}
	defer cleanup()
	db, err := sql.Open(sqlitedriver.Name, path)
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
	for _, stmt := range patterncore.CorpusSchema {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("corpus schema: %w", err)
		}
	}

	// Indexer-version bump forces a full re-index (parser/extractor changed).
	if stored := patterncore.CorpusMeta(db, "pattern_indexer_version"); stored != strconv.Itoa(patterncore.IndexerVersion) {
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
	sessions = bmanifest.GuardSessions(guard, sessions)
	// Optional enrichment source: durable facts, branch-scoped. Missing is
	// empty, but corrupt/unsafe state fails closed and tombstoned-only facts are
	// filtered before they can be linked back into a rebuilt corpus.
	factsByBranch, err := loadAllFactBranchesForPrivacy(brainDir)
	if err != nil {
		return err
	}
	for branch, facts := range factsByBranch {
		factsByBranch[branch] = factstore.GuardRecords(guard, facts)
	}

	// Fact enrichment is an input independent of transcript identity. A changed
	// fact inventory rebuilds links even for byte-identical transcripts.
	factData, err := json.Marshal(factsByBranch)
	if err != nil {
		return err
	}
	factFingerprint := base.HexSHA(string(factData))
	// Fact freshness is PER BRANCH, not global.
	//
	// This was one fingerprint over every branch's facts, so a fact recorded
	// on any branch invalidated it and re-indexed every session in the corpus
	// -- "even for byte-identical transcripts", as the comment above says.
	// That rebuild runs inside the brain write lock, which is the same lock a
	// retrieval holds to emit its response, so it is what made a concurrent
	// `brief` fail brain_locked: one `remember` anywhere, and every reader
	// waited on a full link rebuild.
	//
	// Per-branch is sound rather than merely cheaper, and the link builder is
	// what proves it: episodes are linked from factsByBranch[s.Branch]
	// (indexSessionIntoCorpus, see linkEpisodeFacts below), so a fact on
	// branch X can never enrich a session on branch Y. The global fingerprint
	// was strictly more conservative than the actual data dependency.
	//
	// Migration needs no version bump: a corpus written before this has no
	// per-branch keys, every lookup misses, and one full re-index writes them.
	// Computed per branch ON DEMAND, including for a branch with no facts at
	// all. An earlier version only hashed branches present in factsByBranch,
	// which left a factless branch comparing "" against the unset meta value
	// and skipping by coincidence rather than by a recorded fingerprint -- and
	// it recorded nothing, so the branch could never be invalidated later.
	// json.Marshal(nil) is "null", a real hash, so every branch gets one.
	factFingerprintByBranch := map[string]string{}
	branchFactFingerprint := func(branch string) (string, error) {
		if known, ok := factFingerprintByBranch[branch]; ok {
			return known, nil
		}
		branchData, err := json.Marshal(factsByBranch[branch])
		if err != nil {
			return "", err
		}
		fingerprint := base.HexSHA(string(branchData))
		factFingerprintByBranch[branch] = fingerprint
		return fingerprint, nil
	}
	factsCurrentFor := func(branch string) (bool, error) {
		fingerprint, err := branchFactFingerprint(branch)
		if err != nil {
			return false, err
		}
		return patterncore.CorpusMeta(db, patternCorpusFactsMetaKey(branch)) == fingerprint, nil
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
		factsFresh, err := factsCurrentFor(s.Branch)
		if err != nil {
			return err
		}
		if factsFresh && corpusSessionUnchanged(db, id, size, mtime, sha) {
			continue
		}
		if err := indexSessionIntoCorpus(db, repoKey, s, id, rel, content, size, mtime, sha, factsByBranch); err != nil {
			return err
		}
		onPatternCorpusSessionIndexed(id)
	}

	// Prune sessions no longer in the manifest (deleted transcripts).
	if err := pruneCorpusSessions(db, present); err != nil {
		return err
	}

	// Link episodes to semantic symbols (if a semantic index exists), then
	// rebuild candidate patterns, then materialize the synapse explanation graph.
	var sem *semanticSourceManifest
	if len(graph) > 0 {
		sem = graph[0].source(brainDir, manifest)
		if err := graph[0].Context.Err(); err != nil {
			return err
		}
	}
	if err := linkEpisodeSymbols(db, brainDir, sem); err != nil {
		return err
	}
	if err := patterncore.BuildCandidates(db, repoKey, now); err != nil {
		return err
	}
	// Episode shapes (deterministic, token-free). Themes are NOT built here:
	// they are agent-proposed semantic clusters, created only by the explicit
	// `patterns verify --themes` (egress-gated) — refresh stays token-free.
	if err := classifyEpisodeShapes(db, now); err != nil {
		return err
	}
	if err := patterncore.BuildSynapses(db, now); err != nil {
		return err
	}
	_, finalPolicy, err := brainstore.LoadSessionTombstonesChecked(brainDir)
	if err != nil {
		return err
	}
	if finalPolicy.Identity != guard.PolicyIdentity {
		return fmt.Errorf("%s: session tombstones changed while building the pattern corpus", brainstore.PrivacyDirtyCode)
	}
	// Record a fingerprint for every branch that HAS facts. Branches that only
	// have sessions need no loop of their own: the freshness gate above calls
	// branchFactFingerprint(s.Branch) for each indexed session and memoizes
	// it, so they are already in the map. (A loop over sessions was written
	// here first and a weakening run showed removing it changed nothing --
	// dead code, deleted rather than kept with a guard that cannot justify
	// it.)
	for branch := range factsByBranch {
		if _, err := branchFactFingerprint(branch); err != nil {
			return err
		}
	}
	factsMeta := map[string]string{}
	for branch, fingerprint := range factFingerprintByBranch {
		factsMeta[patternCorpusFactsMetaKey(branch)] = fingerprint
	}
	if len(factsMeta) > 0 {
		if err := setCorpusMeta(db, factsMeta); err != nil {
			return err
		}
	}
	if err := setCorpusMeta(db, map[string]string{
		"schema_version":          strconv.Itoa(patternCorpusSchemaVersion),
		"pattern_indexer_version": strconv.Itoa(patterncore.IndexerVersion),
		"facts_fingerprint":       factFingerprint,
		"privacy_policy_identity": guard.PolicyIdentity,
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
	run, err := patterncore.RunFromStaging(db, now)
	if err != nil {
		return err
	}
	if err := patterncore.FinalizeCorpusStagingLocked(brainDir, path, db, patterncore.DecodeCorpusPublication); err != nil {
		return err
	}
	closed = true
	// Operational run history describes only successfully published corpora.
	// A failed staging build or commit must leave both the old corpus and its
	// last-run report intact.
	_ = patterncore.AppendRun(brainDir, run)
	return nil
}

func loadPatternCorpusManifest(brainDir string) (*exportManifest, error) {
	// Derivation must use the same header-first, strict version classifier as
	// every other Brain-manifest consumer. In particular, never partially decode
	// or normalize a vNext document into the current schema before rebuilding.
	return bmanifest.Load(brainDir)
}

// Source anchors in both document and JSONL transcripts use physical lines.
func patternTranscriptEndLine(content string) int {
	content = strings.TrimRight(content, "\r\n")
	if content == "" {
		return 0
	}
	return strings.Count(content, "\n") + 1
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
	return gotSize == size && gotMtime == mtime && gotSha == sha && parser == patterncore.IndexerVersion
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

	endOfTranscript := patternTranscriptEndLine(string(content))
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
		if ord+1 == len(segments) {
			endLine = max(startLine, endOfTranscript)
		} else if segments[ord+1].Request.Line > startLine {
			endLine = segments[ord+1].Request.Line - 1
		}

		epID := "episode:" + base.HexSHA(repoKey+"\x00"+filepath.ToSlash(rel)+"\x00"+strconv.Itoa(ord)+"\x00"+strconv.Itoa(startLine))
		epKey := repoKey + "/" + filepath.ToSlash(rel) + "#" + strconv.Itoa(ord)

		if _, err := tx.Exec(`INSERT INTO episodes
			(id, episode_key, repo_key, workspace, session_id, checkpoint_id, turn_id, turn_ord, branch, author_name, agent, created_at,
			 source_path, start_line, end_line, intent_raw, intent_sig, n_tools, tool_mix, files, n_cmds, exit_fails, outcome, outcome_source)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			epID, epKey, repoKey, "", s.SessionID, s.LatestCheckpoint, s.TurnID, ord, s.Branch, author, s.Agent, createdAt,
			filepath.ToSlash(rel), startLine, endLine, base.RedactText(base.TruncateString(seg.Request.Text, 280)), intentSignature(seg.Request.Text),
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
				epID, i, c.head, base.RedactText(c.raw), c.line, exit, failed); err != nil {
				return err
			}
		}
		for _, f := range files {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO episode_files (episode_id, path, action, line) VALUES (?,?,?,?)`,
				epID, base.RedactText(f.path), f.action, f.line); err != nil {
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
		id, repoKey, s.SessionID, s.LatestCheckpoint, filepath.ToSlash(rel), s.Branch, author, s.Agent, createdAt, size, mtime, sha, patterncore.IndexerVersion, episodeCount,
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
		p := base.RedactText(f.path)
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

// commandGrams builds deduped command n-grams (n=2..4) within one episode, over
// the run-collapsed command heads — the procedure-shape evidence.
func commandGrams(heads []string) []corpusGram {
	collapsed := collapseRuns(heads)
	seen := map[string]bool{}
	var out []corpusGram
	for n := 2; n <= 4; n++ {
		for _, w := range commandWindows(collapsed, n) {
			key := strconv.Itoa(n) + ":" + strings.Join(w, patterncore.GramSep)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, corpusGram{n: n, gram: strings.Join(w, patterncore.GramSep)})
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

// readPatternCorpusTranscript derives metadata from the canonical bytes, so
// checkpoint-backed sessions have the same corpus identity as bundle files.
func readPatternCorpusTranscript(ctx context.Context, brainDir, rel string) (data []byte, size, mtime int64, sha string, err error) {
	data, err = readCanonicalHistoryTranscript(ctx, brainDir, rel)
	if err != nil {
		return nil, 0, 0, "", err
	}
	sum := sha256.Sum256(data)
	return data, int64(len(data)), 0, hex.EncodeToString(sum[:]), nil
}

// patternCorpusFactsMetaKey names a branch's fact fingerprint. The branch is
// hashed rather than interpolated: branch names contain slashes and arbitrary
// bytes, and a raw name would collide with the unprefixed keys beside it (a
// branch literally called "facts_fingerprint" being the obvious one).
func patternCorpusFactsMetaKey(branch string) string {
	return "facts_fingerprint/" + base.HexSHA(branch)
}

func setCorpusMeta(db *sql.DB, kv map[string]string) error {
	for k, v := range kv {
		if _, err := db.Exec(`INSERT INTO meta(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
			return err
		}
	}
	return nil
}
