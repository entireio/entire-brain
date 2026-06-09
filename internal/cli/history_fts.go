package cli

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
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
	historyFTSSchema   = "1"
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
func openHistoryFTS(brainDir string, index historyIndex) (*sql.DB, error) {
	path := historyFTSDBPath(brainDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
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
// scorer. It dedups by normalized summary like the scorer does and maps BM25's
// negative score (lower = better) to a positive display score (higher = better).
func rankHistoryViaFTS(brainDir string, index historyIndex, kind, query string, limit int) ([]scoredHistoryRecord, bool) {
	if limit <= 0 {
		return nil, false
	}
	expr := historyFTSMatchExpr(query)
	if expr == "" {
		return nil, false
	}
	db, err := openHistoryFTS(brainDir, index)
	if err != nil {
		return nil, false
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
		// relevant content without improving recall). Surface them only via the
		// explicit `requests` kind, not the broad history/sessions sweep.
		sb.WriteString(" AND kind != 'request'")
	}
	sb.WriteString(" ORDER BY bm25(history_fts) LIMIT ?")
	args = append(args, limit*4) // over-fetch so summary dedup still fills limit

	rows, err := db.Query(sb.String(), args...)
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	out := make([]scoredHistoryRecord, 0, limit)
	seen := map[string]struct{}{}
	var topScore float64
	for rows.Next() {
		var order int
		var bm float64
		if err := rows.Scan(&order, &bm); err != nil {
			return nil, false
		}
		if order < 0 || order >= len(index.Records) {
			continue
		}
		// Rows arrive best-first (bm25 ascending), so the first kept row is the
		// top score and the cutoff can stop the scan: every later row is weaker.
		score := -bm
		if topScore == 0 {
			topScore = score
		} else if score < historyFTSRelevanceCutoff*topScore {
			break
		}
		rec := index.Records[order]
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
		return nil, false
	}
	return out, true
}
