//go:build brain_cgo

package cli

import (
	"database/sql"
	"testing"

	sqlitevec "github.com/asg017/sqlite-vec-go-bindings/cgo"
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

// TestBrainCGOVec0Available proves the sqlite-vec auto-extension registration
// took effect (sqlite3_auto_extension is deprecated on Apple platforms, so
// this cannot be assumed from a clean compile) and that the vec0 KNN contract
// the vecStore depends on — cosine metric, `k = ?` constraint, BLOB match —
// holds for the bundled sqlite-vec version.
func TestBrainCGOVec0Available(t *testing.T) {
	db, err := sql.Open(sqliteDriverName, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version string
	if err := db.QueryRow(`SELECT vec_version()`).Scan(&version); err != nil {
		t.Fatalf("sqlite-vec not loaded (auto-extension registration failed?): %v", err)
	}
	t.Logf("sqlite-vec %s", version)
	if _, err := db.Exec(`CREATE VIRTUAL TABLE v USING vec0(embedding float[3] distance_metric=cosine)`); err != nil {
		t.Fatalf("vec0 table with cosine metric: %v", err)
	}
	for i, vec := range [][]float32{{1, 0, 0}, {0.9, 0.1, 0}, {0, 1, 0}} {
		blob, err := sqlitevec.SerializeFloat32(vec)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO v(rowid, embedding) VALUES (?, ?)`, i+1, blob); err != nil {
			t.Fatal(err)
		}
	}
	qblob, err := sqlitevec.SerializeFloat32([]float32{1, 0, 0})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT rowid, distance FROM v WHERE embedding MATCH ? AND k = ? ORDER BY distance`, qblob, 3)
	if err != nil {
		t.Fatalf("vec0 KNN query: %v", err)
	}
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var rowid int64
		var dist float64
		if err := rows.Scan(&rowid, &dist); err != nil {
			t.Fatal(err)
		}
		got = append(got, rowid)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// Cosine distance to (1,0,0): row 1 = 0, row 2 ≈ 0.006, row 3 = 1.
	if len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("KNN order = %v, want [1 2 3]", got)
	}
}
