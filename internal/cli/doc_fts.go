package cli

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// doc_fts.go builds a derived FTS5 BM25 index over the doc records and ranks
// queries with it, mirroring history_fts.go. Rebuildable: deleting the .sqlite
// forces a clean rebuild, and any failure falls back to the in-memory lexical
// scorer (rankDocsLexical) so the doc source still contributes. Heading + text are
// both indexed so a query matches either.

const (
	docFTSFileName        = "index-fts.sqlite"
	docFTSSchema          = "1"
	docFTSRelevanceCutoff = 0.30
)

type scoredDocRecord struct {
	Record docRecord
	Score  int
}

func docFTSDBPath(brainDir string) string {
	return filepath.Join(brainDir, docDirName, docFTSFileName)
}

func docFTSDBRelPath() string {
	return filepath.ToSlash(filepath.Join(docDirName, docFTSFileName))
}

func docFTSContent(r docRecord) string {
	raw := r.Heading + " " + r.Text + " " + r.Path
	return normalizeHistorySearchText(raw) + " " + strings.ToLower(raw)
}

func docFTSFingerprint(index docIndex) string {
	return index.GeneratedAt.UTC().Format(time.RFC3339Nano) + ":" + strconv.Itoa(len(index.Records))
}

func openDocFTS(brainDir string, index docIndex) (*sql.DB, error) {
	var db *sql.DB
	err := withBrainWriteLock(brainDir, func() error {
		var runErr error
		db, runErr = openDocFTSLocked(brainDir, index)
		return runErr
	})
	return db, err
}

func openDocFTSLocked(brainDir string, index docIndex) (*sql.DB, error) {
	path, err := prepareBrainRelativeSQLiteFile(brainDir, docFTSDBRelPath())
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
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
	if docFTSFresh(db, index) {
		return db, nil
	}
	if err := buildDocFTS(db, index); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func docFTSFresh(db *sql.DB, index docIndex) bool {
	rows, err := db.Query(`SELECT key, value FROM doc_fts_meta`)
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
	return got["schema"] == docFTSSchema && got["fingerprint"] == docFTSFingerprint(index)
}

func buildDocFTS(db *sql.DB, index docIndex) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS doc_fts`,
		`DROP TABLE IF EXISTS doc_fts_meta`,
		`CREATE TABLE doc_fts_meta(key TEXT PRIMARY KEY, value TEXT)`,
		`CREATE VIRTUAL TABLE doc_fts USING fts5(content, rec_order UNINDEXED, tokenize='porter unicode61')`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("doc fts schema: %w", err)
		}
	}
	ins, err := tx.Prepare(`INSERT INTO doc_fts(content, rec_order) VALUES (?, ?)`)
	if err != nil {
		return err
	}
	defer ins.Close()
	for i, r := range index.Records {
		if _, err := ins.Exec(docFTSContent(r), i); err != nil {
			return fmt.Errorf("doc fts insert: %w", err)
		}
	}
	for k, v := range map[string]string{"schema": docFTSSchema, "fingerprint": docFTSFingerprint(index)} {
		if _, err := tx.Exec(`INSERT INTO doc_fts_meta(key, value) VALUES (?, ?)`, k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// rankDocsViaFTS ranks doc chunks by BM25, trimming the weak tail at 30% of the
// top score (like history). ok=false on any failure or a degenerate query.
// rankDocsLexical is the in-memory fallback for when the doc FTS index can't be
// built or opened: it scores each doc by how many distinct query terms appear in
// its indexed content, so the `doc` source still contributes to search/query when
// SQLite/FTS is unavailable — mirroring history's substring fallback. Best-effort:
// the only floor is "shares at least one query term".
func rankDocsLexical(index docIndex, query string, limit int) []scoredDocRecord {
	terms := historyQueryAllTerms(query)
	if len(terms) == 0 || limit <= 0 {
		return nil
	}
	type sc struct {
		rec   docRecord
		score int
	}
	scored := make([]sc, 0, len(index.Records))
	for _, r := range index.Records {
		content := docFTSContent(r)
		n := 0
		for _, t := range terms {
			if t != "" && strings.Contains(content, strings.ToLower(t)) {
				n++
			}
		}
		if n > 0 {
			scored = append(scored, sc{r, n})
		}
	}
	sort.SliceStable(scored, func(a, b int) bool {
		if scored[a].score != scored[b].score {
			return scored[a].score > scored[b].score
		}
		return scored[a].rec.ID < scored[b].rec.ID
	})
	out := make([]scoredDocRecord, 0, min(limit, len(scored)))
	for _, s := range scored {
		out = append(out, scoredDocRecord{Record: s.rec, Score: s.score})
		if len(out) >= limit {
			break
		}
	}
	return out
}

func rankDocsViaFTS(brainDir string, index docIndex, query string, limit int) ([]scoredDocRecord, bool) {
	if limit <= 0 {
		return nil, false
	}
	expr := historyFTSMatchExpr(query)
	if expr == "" {
		return nil, false
	}
	db, err := openDocFTS(brainDir, index)
	if err != nil {
		return nil, false
	}
	defer db.Close()
	rows, err := db.Query(`SELECT CAST(rec_order AS INTEGER), bm25(doc_fts) FROM doc_fts WHERE doc_fts MATCH ? ORDER BY bm25(doc_fts) LIMIT ?`, expr, limit*4)
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	out := make([]scoredDocRecord, 0, limit)
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
		score := -bm // bm is SQLite's negated BM25 (<= 0); -bm is positive, higher = better
		if topScore == 0 {
			topScore = score
		} else if score < docFTSRelevanceCutoff*topScore {
			break
		}
		rec := index.Records[order]
		if _, ok := seen[rec.ID]; ok {
			continue
		}
		seen[rec.ID] = struct{}{}
		out = append(out, scoredDocRecord{Record: rec, Score: int(score*1000 + 0.5)})
		if len(out) >= limit {
			break
		}
	}
	if rows.Err() != nil {
		return nil, false
	}
	return out, true
}
