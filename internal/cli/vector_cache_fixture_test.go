package cli

import (
	"bytes"
	"database/sql"
	"os"
	"testing"
)

// Inspect persisted rows without the production reader's invalid-vector filter.
func persistedVectorBytes(t *testing.T, store vectorStore, id string) ([]byte, bool) {
	t.Helper()
	descriptor := store.(interface {
		vectorCacheBackend() string
		vectorCachePath() string
	})
	if descriptor.vectorCacheBackend() == "sqlite_vec" {
		db, err := sql.Open(sqliteDriverName, descriptor.vectorCachePath())
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var raw []byte
		err = db.QueryRow("SELECT v.embedding FROM vec_facts v JOIN fact_ids f ON f.rowid=v.rowid WHERE f.fact_id=?", id).Scan(&raw)
		if err == sql.ErrNoRows {
			return nil, false
		}
		if err != nil {
			t.Fatal(err)
		}
		return raw, true
	}
	raw, err := os.ReadFile(descriptor.vectorCachePath())
	if err != nil {
		t.Fatal(err)
	}
	r := &byteReader{b: raw}
	r.take(4)
	r.take(int(r.u16()))
	dim := int(r.u32())
	count := int(r.u32())
	for i := 0; i < count; i++ {
		key := string(r.take(int(r.u16())))
		value := r.take(dim * 4)
		if r.err != nil {
			t.Fatal(r.err)
		}
		if key == id {
			return value, true
		}
	}
	return nil, false
}

func assertPersistedZeroVector(t *testing.T, store vectorStore, id string) {
	t.Helper()
	raw, ok := persistedVectorBytes(t, store, id)
	if !ok || len(raw) == 0 || !bytes.Equal(raw, make([]byte, len(raw))) {
		t.Fatalf("fixture did not persist a zero vector for %s: present=%v bytes=%x", id, ok, raw)
	}
}

// Persist a valid single row, then corrupt its bytes below the validated writer.
// This models old/corrupt cache data rather than asking savePresent to reject it.
func seedPersistedZeroVector(t *testing.T, store vectorStore, id string, dim int) {
	t.Helper()
	vector := make([]float32, dim)
	vector[0] = 1
	if err := store.savePresent(map[string][]float32{id: vector}, map[string]struct{}{id: {}}); err != nil {
		t.Fatal(err)
	}
	descriptor := store.(interface {
		vectorCacheBackend() string
		vectorCachePath() string
	})
	if descriptor.vectorCacheBackend() == "sqlite_vec" {
		db, err := sql.Open(sqliteDriverName, descriptor.vectorCachePath())
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec("UPDATE vec_facts SET embedding=? WHERE rowid=(SELECT rowid FROM fact_ids WHERE fact_id=?)", make([]byte, dim*4), id); err != nil {
			t.Fatal(err)
		}
	} else {
		raw, err := os.ReadFile(descriptor.vectorCachePath())
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) < dim*4 {
			t.Fatal("short vector fixture")
		}
		clear(raw[len(raw)-dim*4:])
		if err := os.WriteFile(descriptor.vectorCachePath(), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	assertPersistedZeroVector(t, store, id)
}
