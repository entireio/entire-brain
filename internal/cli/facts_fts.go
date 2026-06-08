package cli

import (
	"database/sql"
	"os"
	"strings"

	_ "modernc.org/sqlite"
)

// facts_fts.go scores facts with FTS5 BM25 as an *opt-in* alternative lexical arm
// of rankFactsFused. Unlike history (tens of thousands of records, a persisted
// disk index), a branch holds at most a few hundred facts, so the index is built
// in-memory from the candidate set each query — still derived from the NDJSON
// truth, but cheap enough that persistence and staleness tracking aren't worth
// it. Any failure or a degenerate query returns ok=false so rankFactsFused falls
// back to the hand-rolled token-overlap scorer.
//
// Off by default: on the frozen facts-eval baseline BM25 measured at parity with
// the hand-rolled scorer (1.43 vs 1.50 useful/1k, within noise on 16 tasks), not
// better, so it does not displace the default. It is kept behind a toggle to A/B
// the lexical engine — most usefully against the Stage 1b transformer embedder,
// where the facts quality lift is expected to come from the semantic arm.

// factsBM25Enabled reports whether the experimental BM25 lexical arm is on.
// Enable with ENTIRE_BRAIN_FACTS_BM25=1 (also accepts true/yes/on).
func factsBM25Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ENTIRE_BRAIN_FACTS_BM25"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// factsFTSContent indexes the dual normalized/raw form (like history) so both
// spaced and camelCase queries hit identifiers, and folds in the taxonomy paths
// so a path term ("preferences coding style") is searchable too.
func factsFTSContent(f factRecord) string {
	raw := f.Text + " " + strings.Join(f.Paths, " ")
	return normalizeHistorySearchText(raw) + " " + strings.ToLower(raw)
}

func factsFTSScores(facts []factRecord, query string) (map[string]float64, bool) {
	expr := historyFTSMatchExpr(query)
	if expr == "" {
		return nil, false
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, false
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE VIRTUAL TABLE facts_fts USING fts5(content, id UNINDEXED, tokenize='porter unicode61')`); err != nil {
		return nil, false
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, false
	}
	ins, err := tx.Prepare(`INSERT INTO facts_fts(content, id) VALUES (?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return nil, false
	}
	for _, f := range facts {
		if _, err := ins.Exec(factsFTSContent(f), f.ID); err != nil {
			_ = ins.Close()
			_ = tx.Rollback()
			return nil, false
		}
	}
	_ = ins.Close()
	if err := tx.Commit(); err != nil {
		return nil, false
	}
	rows, err := db.Query(`SELECT id, bm25(facts_fts) FROM facts_fts WHERE facts_fts MATCH ?`, expr)
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	scores := make(map[string]float64)
	for rows.Next() {
		var id string
		var bm float64
		if err := rows.Scan(&id, &bm); err != nil {
			return nil, false
		}
		scores[id] = -bm // bm25 is negative (lower = better) -> positive, higher = better
	}
	if rows.Err() != nil {
		return nil, false
	}
	return scores, true
}
