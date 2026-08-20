package cli

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// history_fts.go builds a derived FTS5 BM25 index over the history records and
// ranks the history arm of the unified search/query verbs with it. It is a rebuildable artifact, a
// sibling of history/index.json (the truth): deleting the .sqlite file forces a
// clean rebuild on the next query, and any open/build/query failure falls back
// to the substring scorer, so the index is an optimization, never load-bearing.
//
// BM25 replaces the hand-rolled coverage gate (historyRequiredQueryMatches),
// whose flat term-counting could not tell a high-value term ("embedding") from a
// filler one ("one"/"state"): every relaxation of the gate that admitted a real
// single-strong-term paraphrase also admitted two-weak-term noise. BM25's IDF
// down-weights common terms by corpus frequency, so an OR of the query terms
// surfaces the doc that is strongly about the rare term while honest empties
// (no term present at all) still return nothing.

const (
	historyFTSFileName = "index-fts.sqlite"
	// v3 combines payload-backed direct hydration with conversation exchange
	// indexing. General history ranking excludes exchanges unless callers select
	// the conversation kind explicitly.
	historyFTSSchema  = "3"
	historyFTSPayload = "1"
)

// historyFTSRelevanceCutoff keeps only matches scoring at least this fraction of
// the query's top match. BM25 score magnitude is not comparable across queries,
// but within one query it is: an OR query matches every record sharing any term,
// so a strong query grows a long tail of records that matched only one common or
// stemmed term (e.g. "updates" hitting "Update File") far below the best hit.
// Cutting relative to the top — not an absolute floor — trims that tail without a
// corpus-specific constant. Tunable once a history eval lands.
const historyFTSRelevanceCutoff = 0.30

func historyFTSDBPath(brainDir string) string {
	return filepath.Join(brainDir, historyDirName, historyFTSFileName)
}

func historyFTSDBRelPath() string {
	return filepath.ToSlash(filepath.Join(historyDirName, historyFTSFileName))
}

// historyFTSContent is the searchable text for a record. It indexes two forms so
// both spaced and camelCase queries hit the same identifier: the camel-split,
// lowercased normalization (so "attribution base" matches "AttributionBaseCommit")
// and the raw lowercase (so the single token "attributionbasecommit" matches too)
// — the same dual normalized/raw strategy the substring matcher already uses.
func historyFTSContent(r historyRecord) string {
	raw := r.Summary + " " + strings.Join(r.Terms, " ") + " " + r.Path
	return normalizeHistorySearchText(raw) + " " + strings.ToLower(raw)
}

// historyFTSMatchExpr turns a query into a safe FTS5 OR expression over its
// stopword-filtered terms and identifiers. Each term is wrapped as an FTS5 string
// literal (double-quoted, embedded quotes doubled) so query operators and
// punctuation can never reach the FTS parser. Empty when the query reduces to no
// usable term, which the caller treats as "fall back to the substring scorer".
func historyFTSMatchExpr(query string) string {
	parts := make([]string, 0, 8)
	for _, t := range historyQueryAllTerms(query) {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		parts = append(parts, `"`+strings.ReplaceAll(t, `"`, `""`)+`"`)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " OR ")
}

type historyFTSIdentity struct {
	RecordsFingerprint string
	RecordCount        int
}

func historyFTSIdentityFromIndex(index historyIndex) historyFTSIdentity {
	// ONE key space with historyFTSIdentityFromSource: the payload fast path
	// derives the identity from the manifest without loading index.json, so an
	// index-derived key that differed (for example a generation-path hash)
	// would never match the persisted store and would silently disable that
	// path. The records fingerprint already discriminates two generations that
	// share a GeneratedAt and a record count but not their rows, and it is
	// populated from the manifest on load, so this stays O(1) in practice.
	fingerprint := index.recordsFingerprint
	if !validHistorySHA256(fingerprint) {
		fingerprint = historyRecordsFingerprint(index.Records)
	}
	return historyFTSIdentity{
		RecordsFingerprint: fingerprint,
		RecordCount:        len(index.Records),
	}
}

func historyFTSIdentityFromSource(source *historySourceManifest) (historyFTSIdentity, bool) {
	if source == nil || source.Records < 0 || !validHistorySHA256(source.RecordsFingerprint) {
		return historyFTSIdentity{}, false
	}
	return historyFTSIdentity{RecordsFingerprint: source.RecordsFingerprint, RecordCount: source.Records}, true
}

func validHistorySHA256(value string) bool {
	raw, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return strings.HasPrefix(value, "sha256:") && err == nil && len(raw) == 32
}

// openHistoryFTS opens (and rebuilds if stale) the derived BM25 index. The index
// is keyed by a fingerprint of the history index it was built from, so a refresh
// that regenerates history/index.json triggers a clean rebuild here.
//
// Locking is double-checked: the fresh-index fast path opens WITHOUT the
// exclusive brain lock — queries vastly outnumber rebuilds, and taking the
// write lock on the read path would serialize every concurrent search and
// time out (10s) against any long-running writer. Only a stale/missing index
// takes the lock, re-checking freshness under it because another process may
// have rebuilt while we waited. Concurrent readers of the fresh index are
// safe: the store is WAL-mode SQLite with a busy timeout.
func openHistoryFTS(brainDir string, index historyIndex) (*sql.DB, error) {
	identity := historyFTSIdentityFromIndex(index)
	if db, err := openHistoryFTSIfFreshIdentity(brainDir, identity); err == nil && db != nil {
		return db, nil
	}
	var db *sql.DB
	err := withBrainWriteLock(brainDir, func() error {
		var runErr error
		db, runErr = openHistoryFTSLockedIdentity(brainDir, index, identity)
		return runErr
	})
	return db, err
}

// openHistoryFTSIfFresh returns an open handle when the on-disk index already
// matches the current history index, (nil, nil) when it is stale or missing.
func openHistoryFTSIfFresh(brainDir string, index historyIndex) (*sql.DB, error) {
	return openHistoryFTSIfFreshIdentity(brainDir, historyFTSIdentityFromIndex(index))
}

func openHistoryFTSIfFreshIdentity(brainDir string, identity historyFTSIdentity) (*sql.DB, error) {
	path, err := prepareBrainRelativeSQLiteFile(brainDir, historyFTSDBRelPath())
	if err != nil {
		return nil, err
	}
	db, err := sql.Open(sqliteDriverName, path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{"PRAGMA busy_timeout=5000;", "PRAGMA journal_mode=WAL;"} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, err
		}
	}
	if historyFTSFreshIdentity(db, identity) {
		return db, nil
	}
	db.Close()
	return nil, nil
}

func openHistoryFTSLocked(brainDir string, index historyIndex) (*sql.DB, error) {
	return openHistoryFTSLockedIdentity(brainDir, index, historyFTSIdentityFromIndex(index))
}

func openHistoryFTSLockedIdentity(brainDir string, index historyIndex, identity historyFTSIdentity) (*sql.DB, error) {
	path, err := prepareBrainRelativeSQLiteFile(brainDir, historyFTSDBRelPath())
	if err != nil {
		return nil, err
	}
	db, err := sql.Open(sqliteDriverName, path)
	if err != nil {
		return nil, err
	}
	// Pin a single connection so connection-scoped PRAGMAs (busy_timeout) below
	// apply to every query/build, not just whichever pooled connection ran them.
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{"PRAGMA busy_timeout=5000;", "PRAGMA journal_mode=WAL;"} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, err
		}
	}
	if historyFTSFreshIdentity(db, identity) {
		return db, nil
	}
	if err := buildHistoryFTSIdentity(db, index, identity); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// historyFTSFresh reports whether the existing index matches the current history
// index. Any error (missing tables, unreadable meta) is treated as stale so the
// caller rebuilds rather than failing.
func historyFTSFresh(db *sql.DB, index historyIndex) bool {
	return historyFTSFreshIdentity(db, historyFTSIdentityFromIndex(index))
}

type historyFTSQueryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

func historyFTSFreshIdentity(q historyFTSQueryer, identity historyFTSIdentity) bool {
	if !validHistorySHA256(identity.RecordsFingerprint) || identity.RecordCount < 0 {
		return false
	}
	rows, err := q.Query(`SELECT key, value FROM history_fts_meta`)
	if err != nil {
		return false
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return false
		}
		got[k] = v
	}
	if rows.Err() != nil {
		return false // a driver error mid-iteration must read as stale, not fresh
	}
	return got["schema"] == historyFTSSchema &&
		got["payload_schema"] == historyFTSPayload &&
		got["records_fingerprint"] == identity.RecordsFingerprint &&
		got["record_count"] == strconv.Itoa(identity.RecordCount)
}

func buildHistoryFTS(db *sql.DB, index historyIndex) error {
	return buildHistoryFTSIdentity(db, index, historyFTSIdentityFromIndex(index))
}

func buildHistoryFTSIdentity(db *sql.DB, index historyIndex, identity historyFTSIdentity) error {
	actualIdentity := historyFTSIdentityFromIndex(index)
	if identity != actualIdentity {
		return errors.New("history fts source identity does not match records")
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS history_fts`,
		`DROP TABLE IF EXISTS history_records`,
		`DROP TABLE IF EXISTS history_fts_meta`,
		`CREATE TABLE history_fts_meta(key TEXT PRIMARY KEY, value TEXT)`,
		`CREATE TABLE history_records(
			fts_rowid INTEGER PRIMARY KEY,
			rec_order INTEGER NOT NULL UNIQUE,
			id TEXT NOT NULL,
			kind TEXT NOT NULL,
			branch TEXT NOT NULL,
			path TEXT NOT NULL,
			line INTEGER NOT NULL,
			summary TEXT NOT NULL,
			terms_json TEXT NOT NULL,
			CHECK(fts_rowid = rec_order + 1)
		)`,
		`CREATE VIRTUAL TABLE history_fts USING fts5(content, content='', tokenize='porter unicode61')`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("history fts schema: %w", err)
		}
	}
	recordIns, err := tx.Prepare(`INSERT INTO history_records(fts_rowid, rec_order, id, kind, branch, path, line, summary, terms_json) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer recordIns.Close()
	ftsIns, err := tx.Prepare(`INSERT INTO history_fts(rowid, content) VALUES (?, ?)`)
	if err != nil {
		return err
	}
	defer ftsIns.Close()
	for i, r := range index.Records {
		termsJSON := "null"
		if len(r.Terms) > 0 {
			encoded, err := json.Marshal(r.Terms)
			if err != nil {
				return fmt.Errorf("history fts terms: %w", err)
			}
			termsJSON = string(encoded)
		}
		ftsRowID := i + 1
		if _, err := recordIns.Exec(ftsRowID, i, r.ID, r.Kind, r.Branch, r.Path, r.Line, r.Summary, termsJSON); err != nil {
			return fmt.Errorf("history fts payload insert: %w", err)
		}
		if _, err := ftsIns.Exec(ftsRowID, historyFTSContent(r)); err != nil {
			return fmt.Errorf("history fts insert: %w", err)
		}
	}
	meta := map[string]string{
		"schema":              historyFTSSchema,
		"payload_schema":      historyFTSPayload,
		"records_fingerprint": identity.RecordsFingerprint,
		"record_count":        strconv.Itoa(identity.RecordCount),
	}
	keys := make([]string, 0, len(meta))
	for key := range meta {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := meta[k]
		if _, err := tx.Exec(`INSERT INTO history_fts_meta(key, value) VALUES (?, ?)`, k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// rebuildHistoryFTSFromTruth force-replaces a corrupt derived payload after the
// authoritative JSON index has been verified and loaded. Normal stale caches use
// openHistoryFTS's cheaper double-checked rebuild; this path is only for a v3
// database whose metadata claimed freshness but whose hydrated rows were invalid.
func rebuildHistoryFTSFromTruth(brainDir string, index historyIndex) error {
	return withBrainWriteLock(brainDir, func() error {
		path, err := prepareBrainRelativeSQLiteFile(brainDir, historyFTSDBRelPath())
		if err != nil {
			return err
		}
		db, err := sql.Open(sqliteDriverName, path)
		if err != nil {
			return err
		}
		defer db.Close()
		db.SetMaxOpenConns(1)
		for _, pragma := range []string{"PRAGMA busy_timeout=5000;", "PRAGMA journal_mode=WAL;"} {
			if _, err := db.Exec(pragma); err != nil {
				return err
			}
		}
		return buildHistoryFTS(db, index)
	})
}

// validateHistoryFTSDirectSource establishes the authoritative side of the
// no-JSON-read contract. The manifest must carry a strong record identity and
// exact index-file metadata produced by refresh. We require the declared truth
// to remain a safe regular file and compare its size without reading it. A size
// mismatch is resolved by the bounded publication-race logic in the caller:
// refresh writes the next JSON generation before committing its FTS transaction,
// so an old reader may use the matching old FTS only while the writer lock proves
// that publication is in progress. The exact checksum is verified on the full
// JSON path.
func validateHistoryFTSDirectSource(brainDir string, source *historySourceManifest) (historyFTSIdentity, bool, bool, error) {
	identity, ok := historyFTSIdentityFromSource(source)
	if !ok || source.IndexBytes <= 0 || !validHistorySHA256(source.IndexSHA256) {
		return historyFTSIdentity{}, false, false, nil // legacy source: use the verified full-index path
	}
	clean, err := validateHistoryIndexPath(source.IndexPath)
	if err != nil {
		return historyFTSIdentity{}, false, false, err
	}
	if err := rejectSymlinkPathComponents(brainDir, clean); err != nil {
		return historyFTSIdentity{}, false, false, err
	}
	path := filepath.Join(brainDir, clean)
	if err := rejectUnsafeExistingRegularFile(path, "history index"); err != nil {
		return historyFTSIdentity{}, false, false, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return historyFTSIdentity{}, false, false, err
	}
	if !info.Mode().IsRegular() {
		return historyFTSIdentity{}, false, false, fmt.Errorf("history index must be a regular file: %s", source.IndexPath)
	}
	return identity, true, info.Size() == source.IndexBytes, nil
}

// openHistoryFTSReadOnly opens only an already-present, safe derived cache. It
// never creates directories/files and never changes journal mode on the query
// path. Unsafe or absent SQLite artifacts are cache misses; the authoritative
// JSON fallback decides whether the user-visible request can still succeed.
func openHistoryFTSReadOnly(brainDir string) (*sql.DB, error) {
	if err := rejectSymlinkedBrainRoot(brainDir); err != nil {
		return nil, err
	}
	clean, err := cleanBrainRelativePath(historyFTSDBRelPath())
	if err != nil {
		return nil, err
	}
	if err := rejectSymlinkPathComponents(brainDir, clean); err != nil {
		return nil, err
	}
	path := filepath.Join(brainDir, clean)
	for _, candidate := range []string{path, path + "-wal", path + "-shm", path + "-journal"} {
		if err := rejectUnsafeExistingRegularFile(candidate, "history sqlite cache"); err != nil {
			return nil, err
		}
	}
	dsn := sqliteLiveReadOnlyDSN(path)
	db, err := sql.Open(sqliteDriverName, dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{"PRAGMA busy_timeout=5000;", "PRAGMA query_only=ON;"} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, err
		}
	}
	return db, nil
}

// rankHistoryViaFreshFTS hydrates BM25 matches directly from schema-v3 SQLite
// payload rows. Metadata validation and ranking share one read transaction, so
// a concurrent refresh cannot commit a new generation between the two reads.
// used=false is a soft cache miss and requires the caller to load index.json;
// used=true includes an honest empty result.
func rankHistoryViaFreshFTS(brainDir string, source *historySourceManifest, kind, query string, limit int) ([]scoredHistoryRecord, bool, error) {
	return rankHistoryViaFreshFTSCutoff(brainDir, source, kind, query, limit, historyFTSRelevanceCutoff)
}

type historyFTSDirectRetry int

var errHistoryFTSPayloadCorrupt = errors.New("history fts payload is corrupt")

const (
	historyFTSDirectNoRetry historyFTSDirectRetry = iota
	historyFTSDirectReloadManifest
	historyFTSDirectWaitForPublication
)

func rankHistoryViaFreshFTSCutoff(brainDir string, source *historySourceManifest, kind, query string, limit int, cutoff float64) ([]scoredHistoryRecord, bool, error) {
	out, used, err := rankHistoryViaFreshFTSCutoffDetailed(brainDir, source, kind, query, limit, cutoff)
	if errors.Is(err, errHistoryFTSPayloadCorrupt) {
		return nil, false, nil // derived-cache corruption is always a soft miss
	}
	return out, used, err
}

func rankHistoryViaFreshFTSCutoffDetailed(brainDir string, source *historySourceManifest, kind, query string, limit int, cutoff float64) ([]scoredHistoryRecord, bool, error) {
	out, used, retry, err := rankHistoryViaFreshFTSCutoffOnce(brainDir, source, kind, query, limit, cutoff)
	if err != nil || retry == historyFTSDirectNoRetry {
		return out, used, err
	}
	current, err := currentHistorySource(brainDir)
	if err != nil {
		return nil, false, err
	}
	if !sameHistoryDirectSource(source, current) {
		out, used, _, err = rankHistoryViaFreshFTSCutoffOnce(brainDir, current, kind, query, limit, cutoff)
		return out, used, err
	}
	if retry == historyFTSDirectReloadManifest {
		return nil, false, nil // ordinary stale/corrupt derived cache
	}

	// index.json is a different size while the caller still holds the old
	// manifest. This is either a refresh publication window or out-of-band
	// corruption. Wait once on the writer lock as a publication barrier, then
	// re-read the manifest. The normal long rebuild path never waits here: while
	// it runs, the old committed FTS metadata still matches and the read proceeds.
	unlock, lockErr := acquireBrainWriteLock(brainDir)
	if lockErr != nil {
		return nil, false, lockErr
	}
	unlock()
	current, err = currentHistorySource(brainDir)
	if err != nil {
		return nil, false, err
	}
	if !sameHistoryDirectSource(source, current) {
		out, used, _, err = rankHistoryViaFreshFTSCutoffOnce(brainDir, current, kind, query, limit, cutoff)
		return out, used, err
	}
	return nil, false, errors.New("history index size does not match manifest; run `entire brain refresh history` to rebuild it")
}

func currentHistorySource(brainDir string) (*historySourceManifest, error) {
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		return nil, err
	}
	if manifest.Sources == nil || manifest.Sources.History == nil {
		return nil, errors.New("history source missing after manifest reload")
	}
	return manifest.Sources.History, nil
}

func sameHistoryDirectSource(a, b *historySourceManifest) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.IndexPath == b.IndexPath && a.IndexBytes == b.IndexBytes && a.IndexSHA256 == b.IndexSHA256 &&
		a.RecordsFingerprint == b.RecordsFingerprint && a.Records == b.Records
}

func historyBrainWriteInProgress(brainDir string) (bool, error) {
	unlock, err := acquireBrainWriteLockTimeout(brainDir, 0)
	if err == nil {
		unlock()
		return false, nil
	}
	if strings.Contains(err.Error(), "brain_locked:") {
		return true, nil
	}
	return false, err
}

func rankHistoryViaFreshFTSCutoffOnce(brainDir string, source *historySourceManifest, kind, query string, limit int, cutoff float64) ([]scoredHistoryRecord, bool, historyFTSDirectRetry, error) {
	if limit <= 0 {
		return nil, false, historyFTSDirectNoRetry, nil
	}
	expr := historyFTSMatchExpr(query)
	if expr == "" {
		return nil, false, historyFTSDirectNoRetry, nil
	}
	identity, eligible, sizeMatches, err := validateHistoryFTSDirectSource(brainDir, source)
	if err != nil || !eligible {
		return nil, false, historyFTSDirectNoRetry, err
	}
	db, err := openHistoryFTSReadOnly(brainDir)
	if err != nil {
		if !sizeMatches {
			return nil, false, historyFTSDirectWaitForPublication, nil
		}
		return nil, false, historyFTSDirectNoRetry, nil
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		if !sizeMatches {
			return nil, false, historyFTSDirectWaitForPublication, nil
		}
		return nil, false, historyFTSDirectNoRetry, nil
	}
	defer tx.Rollback()
	if !historyFTSFreshIdentity(tx, identity) {
		if !sizeMatches {
			return nil, false, historyFTSDirectWaitForPublication, nil
		}
		return nil, false, historyFTSDirectReloadManifest, nil
	}
	if !sizeMatches {
		writing, lockErr := historyBrainWriteInProgress(brainDir)
		if lockErr != nil {
			return nil, false, historyFTSDirectNoRetry, lockErr
		}
		if !writing {
			return nil, false, historyFTSDirectWaitForPublication, nil
		}
	}

	args := []any{expr}
	var sb strings.Builder
	sb.WriteString(`SELECT r.fts_rowid, r.rec_order, r.id, r.kind, r.branch, r.path, r.line, r.summary, r.terms_json, bm25(history_fts)
		FROM history_fts JOIN history_records r ON r.fts_rowid = history_fts.rowid
		WHERE history_fts MATCH ?`)
	appendHistoryFTSKindFilter(&sb, &args, kind, "r.kind")
	sb.WriteString(" ORDER BY bm25(history_fts) LIMIT ?")
	args = append(args, limit*4)

	rows, err := tx.Query(sb.String(), args...)
	if err != nil {
		if !sizeMatches {
			return nil, false, historyFTSDirectWaitForPublication, nil
		}
		return nil, false, historyFTSDirectNoRetry, nil
	}
	out := make([]scoredHistoryRecord, 0, limit)
	seen := map[string]struct{}{}
	seenOrders := map[int]struct{}{}
	var topScore float64
	valid := true
	for rows.Next() {
		var rec historyRecord
		var ftsRowID int
		var order int
		var termsJSON string
		var bm float64
		if err := rows.Scan(&ftsRowID, &order, &rec.ID, &rec.Kind, &rec.Branch, &rec.Path, &rec.Line, &rec.Summary, &termsJSON, &bm); err != nil {
			valid = false
			break
		}
		if order < 0 || order >= identity.RecordCount || ftsRowID != order+1 || rec.ID == "" || len(termsJSON) > historyMaxLineBytes*4 {
			valid = false
			break
		}
		if _, duplicate := seenOrders[order]; duplicate {
			valid = false
			break
		}
		seenOrders[order] = struct{}{}
		if termsJSON != "null" {
			if err := json.Unmarshal([]byte(termsJSON), &rec.Terms); err != nil {
				valid = false
				break
			}
			if len(rec.Terms) == 0 {
				rec.Terms = nil
			}
		}
		score := -bm
		if topScore == 0 {
			topScore = score
		} else if score < cutoff*topScore {
			break
		}
		key := normalizeHistorySearchText(rec.Summary)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, scoredHistoryRecord{Record: rec, Score: int(score*1000 + 0.5), Order: order})
		if len(out) >= limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		valid = false
	}
	if err := rows.Close(); err != nil {
		valid = false
	}
	if !valid {
		if !sizeMatches {
			return nil, false, historyFTSDirectWaitForPublication, nil
		}
		return nil, false, historyFTSDirectNoRetry, errHistoryFTSPayloadCorrupt
	}
	if err := tx.Commit(); err != nil {
		if !sizeMatches {
			return nil, false, historyFTSDirectWaitForPublication, nil
		}
		return nil, false, historyFTSDirectNoRetry, nil
	}
	return out, true, historyFTSDirectNoRetry, nil
}

func appendHistoryFTSKindFilter(sb *strings.Builder, args *[]any, kind, column string) {
	if allowed := historyInspectKinds(kind); len(allowed) > 0 {
		kinds := make([]string, 0, len(allowed))
		for k := range allowed {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		placeholders := make([]string, len(kinds))
		for i, k := range kinds {
			placeholders[i] = "?"
			*args = append(*args, k)
		}
		sb.WriteString(" AND " + column + " IN (" + strings.Join(placeholders, ",") + ")")
		return
	}
	sb.WriteString(" AND " + column + " NOT IN ('request', ?)")
	*args = append(*args, conversationKind)
}

const (
	historyIndexAccessFTSPayload = "fts_payload"
	historyIndexAccessJSON       = "index_json"
)

type historyIndexLoadFunc func(string, *historySourceManifest) (historyIndex, error)

func rankHistoryLexicalFromSource(brainDir string, source *historySourceManifest, kind, query string, limit int) ([]scoredHistoryRecord, string, int, error) {
	return rankHistoryLexicalFromSourceWithLoader(brainDir, source, kind, query, limit, loadBrainHistoryIndex)
}

func rankHistoryLexicalFromSourceWithLoader(brainDir string, source *historySourceManifest, kind, query string, limit int, loader historyIndexLoadFunc) ([]scoredHistoryRecord, string, int, error) {
	scored, used, directErr := rankHistoryViaFreshFTSCutoffDetailed(brainDir, source, kind, query, limit, historyFTSRelevanceCutoff)
	derivedCorrupt := errors.Is(directErr, errHistoryFTSPayloadCorrupt)
	if directErr != nil && !derivedCorrupt {
		return nil, "", 0, directErr
	} else if used {
		return scored, historyIndexAccessFTSPayload, source.Records, nil
	}
	index, err := loader(brainDir, source)
	if err != nil {
		return nil, "", 0, err
	}
	if derivedCorrupt {
		if rebuildErr := rebuildHistoryFTSFromTruth(brainDir, index); rebuildErr == nil {
			if rebuilt, ok := rankHistoryViaFTS(brainDir, index, kind, query, limit); ok {
				scored = rebuilt
			} else {
				scored = rankHistoryRecordsScored(index, kind, query, limit, 0)
			}
		} else {
			scored = rankHistoryRecordsScored(index, kind, query, limit, 0)
		}
	} else {
		var ok bool
		scored, ok = rankHistoryViaFTS(brainDir, index, kind, query, limit)
		if !ok {
			scored = rankHistoryRecordsScored(index, kind, query, limit, 0)
		}
	}
	return scored, historyIndexAccessJSON, len(index.Records), nil
}

// rankHistoryViaFTS ranks records with the BM25 index, returning ok=false on any
// failure (or a degenerate query) so the caller falls back to the substring
// scorer. It dedups by normalized summary like the scorer does. NOTE on the sign:
// SQLite FTS5 bm25() is the BM25 score *negated* (per the FTS5 docs), so a better
// match is MORE NEGATIVE and `ORDER BY bm25()` ASC returns best-first. We negate it
// back to a positive display score (higher = better) for the cutoff and Score.
func rankHistoryViaFTS(brainDir string, index historyIndex, kind, query string, limit int) ([]scoredHistoryRecord, bool) {
	out, _, ok := rankHistoryViaFTSFiltered(brainDir, index, kind, query, limit, historyFTSRelevanceCutoff, nil)
	return out, ok
}

// rankHistoryViaFTSCutoff is rankHistoryViaFTS with the relevance cutoff as a
// parameter, so the history eval can sweep it (the shipped 0.30 was chosen by
// inspection, "tunable once a history eval lands"). Production callers go
// through rankHistoryViaFTS and always get the shipped constant.
func rankHistoryViaFTSCutoff(brainDir string, index historyIndex, kind, query string, limit int, cutoff float64) ([]scoredHistoryRecord, bool) {
	out, _, ok := rankHistoryViaFTSFiltered(brainDir, index, kind, query, limit, cutoff, nil)
	return out, ok
}

// historyFTSContainsPathPhrase reports whether the on-disk BM25 store holds
// any row mentioning the given transcript path (record content indexes the
// path text). It opens the store file directly, never through openHistoryFTS,
// so a privacy verify pass can inspect rows without ever triggering a
// rebuild. An absent or unreadable store returns an error; the caller falls
// back to the deletion and mtime checks.
func historyFTSContainsPathPhrase(brainDir, rel string) (bool, error) {
	path := filepath.Join(brainDir, filepath.FromSlash(historyFTSDBRelPath()))
	if _, err := os.Stat(path); err != nil {
		return false, err
	}
	return historyFTSContainsPathPhraseAt(path, rel)
}

func historyFTSContainsPathPhraseAt(path, rel string) (bool, error) {
	db, err := sql.Open(sqliteDriverName, path)
	if err != nil {
		return false, err
	}
	defer db.Close()
	base := filepath.Base(rel)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	fields := strings.FieldsFunc(strings.ToLower(base), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	if len(fields) == 0 {
		return false, nil
	}
	phrase := `"` + strings.Join(fields, " ") + `"`
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM history_fts WHERE history_fts MATCH ?`, phrase).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

// historyFTSFilteredScanCeiling bounds the exhaustive candidate scan behind a
// structured-filter predicate. A var so tests can lower it to prove the
// degraded state.
var historyFTSFilteredScanCeiling = 10000

// historyFTSExhaustiveRawScanCeiling is a resource guard for multi-concept complete
// lexical enumeration. It is deliberately much larger than the in-scope
// candidate ceiling: rows that fail a scope predicate must never recreate the
// old bounded-window false negative. Reaching this guard is a conservative
// typed refusal, never a partial result. A var keeps the boundary testable.
var historyFTSExhaustiveRawScanCeiling = 100000

type historyExhaustiveRankState uint8

const (
	historyExhaustiveRankComplete historyExhaustiveRankState = iota
	historyExhaustiveRankCandidateOverflow
	historyExhaustiveRankRawScanOverflow
)

// rankHistoryViaFTSFiltered pushes a structured-filter predicate into
// candidate generation: a matching row that fails pred is skipped
// before the relevance cutoff, the summary dedup, and the limit apply, so an
// in-scope hit can never be displaced out of a bounded candidate window by
// higher-ranked out-of-scope rows. The cutoff then compares in-scope rows
// only against the best in-scope row. With pred set, the SQL scan is
// exhausted up to historyFTSFilteredScanCeiling rows; complete=false reports
// a scan that hit the ceiling before filling the limit, and the caller must
// return a structured degraded state rather than a silently partial result.
// The index passed here must be the same index the shared FTS store was built
// from; filtering happens Go-side exactly so a filtered view can never
// rebuild the store.
func rankHistoryViaFTSFiltered(brainDir string, index historyIndex, kind, query string, limit int, cutoff float64, pred func(historyRecord) bool) (out []scoredHistoryRecord, complete bool, ok bool) {
	if limit <= 0 {
		return nil, false, false
	}
	expr := historyFTSMatchExpr(query)
	if expr == "" {
		return nil, false, false
	}
	db, err := openHistoryFTS(brainDir, index)
	if err != nil {
		return nil, false, false
	}
	defer db.Close()

	args := []any{expr}
	var sb strings.Builder
	sb.WriteString(`SELECT r.rec_order, bm25(history_fts)
		FROM history_fts JOIN history_records r ON r.fts_rowid = history_fts.rowid
		WHERE history_fts MATCH ?`)
	appendHistoryFTSKindFilter(&sb, &args, kind, "r.kind")
	sb.WriteString(" ORDER BY bm25(history_fts) LIMIT ?")
	scanLimit := limit * 4 // over-fetch so summary dedup still fills limit
	if pred != nil {
		// Exhaust the match set up to the ceiling: with a filter active, a
		// fixed multiplier is exactly the bounded-window defect being repaired.
		scanLimit = historyFTSFilteredScanCeiling
	}
	args = append(args, scanLimit)

	rows, err := db.Query(sb.String(), args...)
	if err != nil {
		return nil, false, false
	}
	defer rows.Close()
	out = make([]scoredHistoryRecord, 0, limit)
	seen := map[string]struct{}{}
	var topScore float64
	scanned := 0
	for rows.Next() {
		var order int
		var bm float64
		if err := rows.Scan(&order, &bm); err != nil {
			return nil, false, false
		}
		scanned++
		if order < 0 || order >= len(index.Records) {
			continue
		}
		rec := index.Records[order]
		if pred != nil && !pred(rec) {
			continue
		}
		// Rows arrive best-first (bm25 ascending), so the first kept row is the
		// top score and the cutoff can stop the scan: every later row is weaker.
		score := -bm // bm is SQLite's negated BM25 (<= 0); -bm is positive, higher = better
		if topScore == 0 {
			topScore = score
		} else if score < cutoff*topScore {
			break
		}
		key := normalizeHistorySearchText(rec.Summary)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, scoredHistoryRecord{Record: rec, Score: int(score*1000 + 0.5), Order: order})
		if len(out) >= limit {
			break
		}
	}
	if rows.Err() != nil {
		return nil, false, false
	}
	complete = pred == nil || scanned < scanLimit || len(out) >= limit
	return out, complete, true
}

// rankHistoryViaFTSExhaustiveFiltered is the complete-enumeration variant used
// by multi-concept lexical retrieval. Unlike the normal ranker, it does not
// impose a SQL candidate window: a record that is outside the caller's scope
// must not conceal a later in-scope match. It stops only after finding one more
// in-scope, deduplicated result than ceiling, so candidate overflow is exact
// for the ranking semantics rather than an artifact of FTS ordering. A
// separate high raw-row guard protects the process from an unbounded global
// match set and reports a conservative refusal.
//
// The normal single-concept path deliberately keeps its bounded candidate
// behavior; this more expensive mode is reserved for the explicit AND query
// whose contract promises complete lexical coverage or a typed refusal.
func rankHistoryViaFTSExhaustiveFiltered(brainDir string, index historyIndex, kind, query string, ceiling int, pred func(historyRecord) bool) (out []scoredHistoryRecord, state historyExhaustiveRankState, ok bool, scanned int) {
	if ceiling <= 0 {
		return nil, historyExhaustiveRankComplete, false, 0
	}
	expr := historyFTSMatchExpr(query)
	if expr == "" {
		return nil, historyExhaustiveRankComplete, false, 0
	}
	db, err := openHistoryFTS(brainDir, index)
	if err != nil {
		return nil, historyExhaustiveRankComplete, false, 0
	}
	defer db.Close()

	args := []any{expr}
	var sb strings.Builder
	sb.WriteString(`SELECT r.rec_order, bm25(history_fts)
		FROM history_fts JOIN history_records r ON r.fts_rowid = history_fts.rowid
		WHERE history_fts MATCH ?`)
	appendHistoryFTSKindFilter(&sb, &args, kind, "r.kind")
	sb.WriteString(" ORDER BY bm25(history_fts), r.rec_order")

	rows, err := db.Query(sb.String(), args...)
	if err != nil {
		return nil, historyExhaustiveRankComplete, false, 0
	}
	defer rows.Close()
	out = make([]scoredHistoryRecord, 0, ceiling)
	seen := map[historyRecordReplacementKey]struct{}{}
	for rows.Next() {
		scanned++
		if scanned > historyFTSExhaustiveRawScanCeiling {
			return nil, historyExhaustiveRankRawScanOverflow, true, scanned
		}
		var order int
		var bm float64
		if err := rows.Scan(&order, &bm); err != nil {
			return nil, historyExhaustiveRankComplete, false, scanned
		}
		if order < 0 || order >= len(index.Records) {
			continue
		}
		rec := index.Records[order]
		if pred != nil && !pred(rec) {
			continue
		}
		// FTS uses an OR expression to enumerate possible rows efficiently, but
		// The exact lexical match contract is the shared canonical scorer. Apply
		// it after scope filtering so SQLite and the pure-Go/overlay paths admit
		// the same exchanges (an any-one-term FTS hit is not sufficient).
		if historyRecordQueryScoreMin(rec, query, 0) == 0 {
			continue
		}
		// Multi-concept coverage is session/evidence exact. Two exchanges with identical
		// text but different stable identities are both candidates; only an
		// already-reconciled copy of the same identity may be collapsed.
		key := exhaustiveHistoryRecordIdentity(rec)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		if len(out) >= ceiling {
			return nil, historyExhaustiveRankCandidateOverflow, true, scanned
		}
		out = append(out, scoredHistoryRecord{Record: rec, Score: int((-bm)*1000 + 0.5), Order: order})
	}
	if rows.Err() != nil {
		return nil, historyExhaustiveRankComplete, false, scanned
	}
	return out, historyExhaustiveRankComplete, true, scanned
}
