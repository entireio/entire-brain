package cli

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
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
	// historyFTSSchema v2: conversation exchange rows are indexed (their bounded
	// search projection lives in Summary) and excluded from general ranking
	// unless the exchange kind is explicitly selected.
	historyFTSSchema = "2"
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

func historyFTSFingerprint(index historyIndex) string {
	return index.GeneratedAt.UTC().Format(time.RFC3339Nano) + ":" + strconv.Itoa(len(index.Records))
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
	if db, err := openHistoryFTSIfFresh(brainDir, index); err == nil && db != nil {
		return db, nil
	}
	var db *sql.DB
	err := withBrainWriteLock(brainDir, func() error {
		var runErr error
		db, runErr = openHistoryFTSLocked(brainDir, index)
		return runErr
	})
	return db, err
}

// openHistoryFTSIfFresh returns an open handle when the on-disk index already
// matches the current history index, (nil, nil) when it is stale or missing.
func openHistoryFTSIfFresh(brainDir string, index historyIndex) (*sql.DB, error) {
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
	if historyFTSFresh(db, index) {
		return db, nil
	}
	db.Close()
	return nil, nil
}

func openHistoryFTSLocked(brainDir string, index historyIndex) (*sql.DB, error) {
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
	if historyFTSFresh(db, index) {
		return db, nil
	}
	if err := buildHistoryFTS(db, index); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// historyFTSFresh reports whether the existing index matches the current history
// index. Any error (missing tables, unreadable meta) is treated as stale so the
// caller rebuilds rather than failing.
func historyFTSFresh(db *sql.DB, index historyIndex) bool {
	rows, err := db.Query(`SELECT key, value FROM history_fts_meta`)
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
	return got["schema"] == historyFTSSchema && got["fingerprint"] == historyFTSFingerprint(index)
}

func buildHistoryFTS(db *sql.DB, index historyIndex) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS history_fts`,
		`DROP TABLE IF EXISTS history_fts_meta`,
		`CREATE TABLE history_fts_meta(key TEXT PRIMARY KEY, value TEXT)`,
		`CREATE VIRTUAL TABLE history_fts USING fts5(content, kind UNINDEXED, rec_order UNINDEXED, tokenize='porter unicode61')`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("history fts schema: %w", err)
		}
	}
	ins, err := tx.Prepare(`INSERT INTO history_fts(content, kind, rec_order) VALUES (?, ?, ?)`)
	if err != nil {
		return err
	}
	defer ins.Close()
	for i, r := range index.Records {
		if _, err := ins.Exec(historyFTSContent(r), r.Kind, i); err != nil {
			return fmt.Errorf("history fts insert: %w", err)
		}
	}
	for k, v := range map[string]string{"schema": historyFTSSchema, "fingerprint": historyFTSFingerprint(index)} {
		if _, err := tx.Exec(`INSERT INTO history_fts_meta(key, value) VALUES (?, ?)`, k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
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

// historyFTSFilteredScanCeiling bounds the exhaustive candidate scan behind a
// structured-filter predicate. A var so tests can lower it to prove the
// degraded state.
var historyFTSFilteredScanCeiling = 10000

// rankHistoryViaFTSFiltered pushes a structured-filter predicate into
// candidate generation (R0-3): a matching row that fails pred is skipped
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
	sb.WriteString(`SELECT CAST(rec_order AS INTEGER), bm25(history_fts) FROM history_fts WHERE history_fts MATCH ?`)
	if allowed := historyInspectKinds(kind); len(allowed) > 0 {
		kinds := make([]string, 0, len(allowed))
		for k := range allowed {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		placeholders := make([]string, len(kinds))
		for i, k := range kinds {
			placeholders[i] = "?"
			args = append(args, k)
		}
		sb.WriteString(" AND kind IN (" + strings.Join(placeholders, ",") + ")")
	} else {
		// User-prompt records add noise to general ranking (measured: they displace
		// relevant content without improving recall); conversation exchanges are
		// opt-in via the explicit conversation source. Surface them only via their
		// explicit kinds, never the broad history/sessions sweep.
		sb.WriteString(" AND kind NOT IN ('request', '" + conversationKind + "')")
	}
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
