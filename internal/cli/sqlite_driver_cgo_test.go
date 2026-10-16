//go:build brain_cgo

package cli

import (
	"database/sql"
	"testing"
)

// TestBrainCGOFTS5Compiled guards the build recipe: brain_cgo must be paired
// with mattn's sqlite_fts5 feature tag or every FTS index (history, facts,
// docs) silently degrades at runtime. A binary that fails this test was built
// with `-tags brain_cgo` alone.
func TestBrainCGOFTS5Compiled(t *testing.T) {
	db, err := sql.Open(sqliteDriverName, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE VIRTUAL TABLE probe USING fts5(text)`); err != nil {
		t.Fatalf("FTS5 unavailable — build with -tags \"brain_cgo sqlite_fts5\": %v", err)
	}
}
