//go:build brain_cgo

package cli

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"

	sqlitevec "github.com/asg017/sqlite-vec-go-bindings/cgo"
)

// vecStore is the Stage 1b vector store: fact vectors live in a sqlite-vec
// vec0 virtual table (cosine metric) inside facts/<branch>/embeddings/
// vectors.sqlite, replacing the vectors.bin flat file + brute-force cosine of
// the pure-Go build. The schema is the qmd/Librarian-local shape — vectors in
// the same SQLite engine as the FTS index — which is what makes a future
// shared local store a read/attach instead of an export.
//
// Semantics mirror embedStore exactly: the store is a regenerable derived
// artifact keyed by embedder model id + dim; load() returns empty on any
// mismatch or corruption (a cache miss the caller refills), and save() is a
// full atomic rewrite of the touched-and-present vector set, pruning departed
// facts. Every method opens the database per call — a CLI invocation does one
// load, at most one KNN query, and one save, so holding a handle across the
// process buys nothing and leaks on early exits.
type vecStore struct {
	path    string // absolute path to vectors.sqlite
	modelID string
	dim     int
}

const vecStoreFileName = "vectors.sqlite"

// newVectorStore on the brain_cgo build returns the vec0-backed store. The
// vectors.bin file of the pure-Go build is left untouched: each build
// maintains its own derived cache, so switching builds costs one re-embed of
// the branch, never corruption.
func newVectorStore(brainDir, branch, modelID string, dim int) vectorStore {
	dir := filepath.Join(brainDir, filepath.FromSlash(factsBranchRelDir(branch)), embedStoreDirName)
	return &vecStore{path: filepath.Join(dir, vecStoreFileName), modelID: modelID, dim: dim}
}

func (s *vecStore) vectorCacheBackend() string { return "sqlite_vec" }
func (s *vecStore) vectorCachePath() string    { return s.path }

func (s *vecStore) open() (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open(sqliteDriverName, s.path)
	if err != nil {
		return nil, err
	}
	// Pin one connection: the vec0 table and PRAGMAs are connection-scoped
	// concerns, and the per-call usage pattern never needs concurrency.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// metaMatches reports whether the store on disk was built for this embedder
// model + dim. Any error reads as a mismatch: the store is a cache, so the
// degraded path is always "rebuild", never "fail".
func (s *vecStore) metaMatches(db *sql.DB) bool {
	var modelID, dimStr string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = 'model_id'`).Scan(&modelID); err != nil {
		return false
	}
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = 'dim'`).Scan(&dimStr); err != nil {
		return false
	}
	dim, err := strconv.Atoi(dimStr)
	return err == nil && modelID == s.modelID && dim == s.dim && dim > 0
}

func (s *vecStore) load() map[string][]float32 {
	out := map[string][]float32{}
	if _, err := os.Stat(s.path); err != nil {
		return out
	}
	db, err := s.open()
	if err != nil {
		return out
	}
	defer db.Close()
	if !s.metaMatches(db) {
		return out // model/dim mismatch -> full rebuild
	}
	rows, err := db.Query(`SELECT f.fact_id, v.embedding FROM vec_facts v JOIN fact_ids f ON f.rowid = v.rowid`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil || len(blob) != 4*s.dim {
			return map[string][]float32{} // corrupt -> rebuild
		}
		vec := make([]float32, s.dim)
		for d := range vec {
			vec[d] = math.Float32frombits(binary.LittleEndian.Uint32(blob[4*d:]))
		}
		out[id] = vec
	}
	if rows.Err() != nil {
		return map[string][]float32{}
	}
	return out
}

func (s *vecStore) save(vecs map[string][]float32) error {
	return s.savePresent(vecs, nil)
}

func (s *vecStore) savePresent(vecs map[string][]float32, present map[string]struct{}) error {
	if present != nil {
		merged := s.load()
		for id := range merged {
			if _, ok := present[id]; !ok {
				delete(merged, id)
			}
		}
		for id, vec := range vecs {
			if _, ok := present[id]; ok {
				merged[id] = vec
			}
		}
		vecs = merged
	}
	db, err := s.open()
	if err != nil {
		return err
	}
	defer db.Close()
	// Recreate the schema from scratch on every save: a model or dim change
	// alters the vec0 column declaration itself, and save() is a full rewrite
	// anyway, so dropping is both the migration and the prune.
	ddl := []string{
		`DROP TABLE IF EXISTS vec_facts`,
		`DROP TABLE IF EXISTS fact_ids`,
		`CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT)`,
		fmt.Sprintf(`CREATE VIRTUAL TABLE vec_facts USING vec0(embedding float[%d] distance_metric=cosine)`, s.dim),
		`CREATE TABLE fact_ids(rowid INTEGER PRIMARY KEY, fact_id TEXT UNIQUE NOT NULL)`,
	}
	for _, stmt := range ddl {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, kv := range [][2]string{{"model_id", s.modelID}, {"dim", strconv.Itoa(s.dim)}} {
		if _, err := tx.Exec(`INSERT INTO meta(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, kv[0], kv[1]); err != nil {
			return err
		}
	}
	rowid := int64(0)
	for factID, vec := range vecs {
		if len(vec) != s.dim {
			continue // wrong-dim vector: skip, as embedStore.save does
		}
		blob, err := sqlitevec.SerializeFloat32(vec)
		if err != nil {
			return err
		}
		rowid++
		if _, err := tx.Exec(`INSERT INTO fact_ids(rowid, fact_id) VALUES (?, ?)`, rowid, factID); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO vec_facts(rowid, embedding) VALUES (?, ?)`, rowid, blob); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// historyVecStore is the vec0 store for history record vectors, at
// history/embeddings/vectors.sqlite (history is repo-level, so no branch in
// the path). It shares vecStore's regenerable-cache semantics but not its
// write strategy: savePresent's full rewrite is sized for hundreds of facts,
// while a large repo's history holds hundreds of thousands of records — so
// every write here is an incremental upsert/delete and the present set is
// never rewritten.
type historyVecStore struct {
	path    string
	modelID string
	dim     int
}

// newHistoryVectorStore on the brain_cgo build returns the vec0-backed history
// store. There is deliberately no pure-Go fallback (see embed_vec_purego.go).
func newHistoryVectorStore(brainDir, modelID string, dim int) (historyVectorStore, bool) {
	dir := filepath.Join(brainDir, historyDirName, embedStoreDirName)
	return &historyVecStore{path: filepath.Join(dir, vecStoreFileName), modelID: modelID, dim: dim}, true
}

func (s *historyVecStore) open() (*sql.DB, error) {
	return (&vecStore{path: s.path}).open()
}

func (s *historyVecStore) metaMatches(db *sql.DB) bool {
	return (&vecStore{path: s.path, modelID: s.modelID, dim: s.dim}).metaMatches(db)
}

// ensure makes the store writable for this model+dim: a fresh schema when the
// file is new, a drop-and-recreate when the on-disk meta mismatches (a model
// or dim change alters the vec0 column declaration, and the old vectors are
// another model's cache — useless, not migratable).
func (s *historyVecStore) ensure(db *sql.DB) error {
	if s.metaMatches(db) {
		return nil
	}
	ddl := []string{
		`DROP TABLE IF EXISTS vec_history`,
		`DROP TABLE IF EXISTS history_ids`,
		`CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT)`,
		fmt.Sprintf(`CREATE VIRTUAL TABLE vec_history USING vec0(embedding float[%d] distance_metric=cosine)`, s.dim),
		`CREATE TABLE history_ids(rowid INTEGER PRIMARY KEY, record_id TEXT UNIQUE NOT NULL)`,
	}
	for _, stmt := range ddl {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	for _, kv := range [][2]string{{"model_id", s.modelID}, {"dim", strconv.Itoa(s.dim)}} {
		if _, err := db.Exec(`INSERT INTO meta(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, kv[0], kv[1]); err != nil {
			return err
		}
	}
	return nil
}

func (s *historyVecStore) ids() (map[string]struct{}, bool) {
	if _, err := os.Stat(s.path); err != nil {
		return nil, false
	}
	db, err := s.open()
	if err != nil {
		return nil, false
	}
	defer db.Close()
	if !s.metaMatches(db) {
		return nil, false
	}
	rows, err := db.Query(`SELECT record_id FROM history_ids`)
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, false
		}
		out[id] = struct{}{}
	}
	if rows.Err() != nil {
		return nil, false
	}
	return out, true
}

func (s *historyVecStore) upsert(add map[string][]float32, drop []string) error {
	db, err := s.open()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := s.ensure(db); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range drop {
		var rowid int64
		err := tx.QueryRow(`SELECT rowid FROM history_ids WHERE record_id = ?`, id).Scan(&rowid)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM vec_history WHERE rowid = ?`, rowid); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM history_ids WHERE rowid = ?`, rowid); err != nil {
			return err
		}
	}
	var next int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(rowid), 0) FROM history_ids`).Scan(&next); err != nil {
		return err
	}
	for id, vec := range add {
		if len(vec) != s.dim {
			continue // wrong-dim vector: skip, as the facts stores do
		}
		// Re-adding a known id is a no-op, not a duplicate row: ids are
		// content-addressed, so the stored vector is already this vector.
		var existing int64
		switch err := tx.QueryRow(`SELECT rowid FROM history_ids WHERE record_id = ?`, id).Scan(&existing); err {
		case nil:
			continue
		case sql.ErrNoRows:
		default:
			return err
		}
		blob, err := sqlitevec.SerializeFloat32(vec)
		if err != nil {
			return err
		}
		next++
		if _, err := tx.Exec(`INSERT INTO history_ids(rowid, record_id) VALUES (?, ?)`, next, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO vec_history(rowid, embedding) VALUES (?, ?)`, next, blob); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// vec0KnnMaxK is sqlite-vec's MATCH k ceiling: a KNN with k above it errors,
// which read as "no semantic arm" until the history store (the first corpus
// big enough to hit it) made the cap explicit. Top-k requests clamp to it.
const vec0KnnMaxK = 4096

// knnCos mirrors vecStore.knnCos over the history tables, but top-k instead
// of all-rows: cosine similarity for the k nearest record ids, ok=false on
// any miss.
func (s *historyVecStore) knnCos(qvec []float32, k int) (map[string]float64, bool) {
	if len(qvec) != s.dim || k <= 0 {
		return nil, false
	}
	if _, err := os.Stat(s.path); err != nil {
		return nil, false
	}
	db, err := s.open()
	if err != nil {
		return nil, false
	}
	defer db.Close()
	if !s.metaMatches(db) {
		return nil, false
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM vec_history`).Scan(&count); err != nil || count == 0 {
		return nil, false
	}
	k = min(k, count, vec0KnnMaxK)
	qblob, err := sqlitevec.SerializeFloat32(qvec)
	if err != nil {
		return nil, false
	}
	rows, err := db.Query(
		`SELECT h.record_id, v.distance FROM vec_history v JOIN history_ids h ON h.rowid = v.rowid WHERE v.embedding MATCH ? AND k = ?`,
		qblob, k)
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	out := make(map[string]float64, count)
	for rows.Next() {
		var id string
		var dist float64
		if err := rows.Scan(&id, &dist); err != nil {
			return nil, false
		}
		out[id] = 1 - dist // vec0 cosine distance = 1 - cosine similarity
	}
	if rows.Err() != nil || len(out) == 0 {
		return nil, false
	}
	return out, true
}

// knnCos runs one vec0 KNN MATCH over the whole store and returns cosine
// similarity per fact id — the semantic arm ranks the full candidate set, so
// k is the store's row count, not a top-k. ok=false on any miss (no store,
// model/dim mismatch, empty, wrong query dim) sends rankFactsFused to the
// brute-force fallback.
func (s *vecStore) knnCos(qvec []float32) (map[string]float64, bool) {
	if len(qvec) != s.dim {
		return nil, false
	}
	if _, err := os.Stat(s.path); err != nil {
		return nil, false
	}
	db, err := s.open()
	if err != nil {
		return nil, false
	}
	defer db.Close()
	if !s.metaMatches(db) {
		return nil, false
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM vec_facts`).Scan(&count); err != nil || count == 0 {
		return nil, false
	}
	qblob, err := sqlitevec.SerializeFloat32(qvec)
	if err != nil {
		return nil, false
	}
	rows, err := db.Query(
		`SELECT f.fact_id, v.distance FROM vec_facts v JOIN fact_ids f ON f.rowid = v.rowid WHERE v.embedding MATCH ? AND k = ?`,
		qblob, count)
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	out := make(map[string]float64, count)
	for rows.Next() {
		var id string
		var dist float64
		if err := rows.Scan(&id, &dist); err != nil {
			return nil, false
		}
		out[id] = 1 - dist // vec0 cosine distance = 1 - cosine similarity
	}
	if rows.Err() != nil || len(out) == 0 {
		return nil, false
	}
	return out, true
}
