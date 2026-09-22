package cli

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// The memo must cache the verdict, never soften it. These pin the two things
// that would make it dangerous: a corrupt store passing because a good one was
// checked earlier, and a different artefact inheriting another's verdict.

func TestVerifiedSQLiteStoreStillRejectsAMismatchedStore(t *testing.T) {
	good := newMemoStoreFixture(t, 3, 2)
	if err := verifiedSQLiteStore(good, 3, 2); err != nil {
		t.Fatalf("a store matching its manifest was rejected: %v", err)
	}
	// A different store, wrong counts. It must be verified on its own terms
	// rather than riding the earlier pass.
	bad := newMemoStoreFixture(t, 1, 1)
	if err := verifiedSQLiteStore(bad, 3, 2); err == nil {
		t.Fatal("a store that does not match its manifest was accepted")
	}
}

func TestVerifiedSQLiteStoreKeysOnTheDeclaredCounts(t *testing.T) {
	// Same file, different claim. A manifest claiming different numbers is a
	// different question and must not reuse the previous answer.
	store := newMemoStoreFixture(t, 4, 4)
	if err := verifiedSQLiteStore(store, 4, 4); err != nil {
		t.Fatalf("matching counts rejected: %v", err)
	}
	if err := verifiedSQLiteStore(store, 99, 99); err == nil {
		t.Fatal("a mismatched claim reused the verdict from a matching one")
	}
}

func TestVerifiedSQLiteStoreCachesTheFailureToo(t *testing.T) {
	// A refusal is as much a verdict as a pass; repeating it must stay a refusal
	// rather than falling through to a second, unchecked attempt.
	bad := newMemoStoreFixture(t, 1, 0)
	first := verifiedSQLiteStore(bad, 7, 7)
	second := verifiedSQLiteStore(bad, 7, 7)
	if first == nil || second == nil {
		t.Fatalf("a mismatched store was accepted: first=%v second=%v", first, second)
	}
}

func TestVerifiedSnapshotSummaryRejectsADifferentRepoKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.ndjson")
	header := `{"record_type":"header","schema_version":"1.0","repo_key":"gh/example/repo"}` + "\n"
	if err := os.WriteFile(path, []byte(header), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := verifiedSnapshotSummary(path, "gh/example/repo"); err != nil {
		t.Fatalf("a snapshot matching its repo key was rejected: %v", err)
	}
	// Same file, different repo. The key includes the repo, so this is a fresh
	// question and must be refused rather than inheriting the pass above.
	if _, _, err := verifiedSnapshotSummary(path, "gh/other/repo"); err == nil {
		t.Fatal("a snapshot belonging to another repository was accepted")
	}
}

func newMemoStoreFixture(t *testing.T, symbols, relations int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "semantic.sqlite")
	db, err := sql.Open(sqliteDriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := initializeSemanticSQLite(db); err != nil {
		t.Skipf("semantic sqlite schema unavailable: %v", err)
	}
	for i := 0; i < symbols; i++ {
		if _, err := db.Exec(
			`INSERT INTO symbols(id, kind, name, qualified_name, file_path, start_line, end_line, signature, language, stable_id_version)
			 VALUES (?, 'function', ?, ?, 'a.go', 1, 2, 'func x()', 'Go', 1)`,
			fmt.Sprintf("sym:%d", i), fmt.Sprintf("n%d", i), fmt.Sprintf("q.n%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < relations; i++ {
		if _, err := db.Exec(
			`INSERT INTO relations(from_id, to_id, type) VALUES (?, ?, 'CALLS')`,
			fmt.Sprintf("sym:%d", i), fmt.Sprintf("sym:%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// The memo's first version keyed only on path and declared counts, reasoning
// that a generation directory is immutable. It is not: a file can be truncated
// or rewritten under a running process, and the repository's own
// TestTheInstantLineDoesNotCallAnUnreadableSemanticIndexBuilt caught a
// corrupted store still being reported as built. The key carries file identity
// for exactly this case.
func TestVerifiedSQLiteStoreRecheckesAStoreThatChangedOnDisk(t *testing.T) {
	store := newMemoStoreFixture(t, 3, 2)
	if err := verifiedSQLiteStore(store, 3, 2); err != nil {
		t.Fatalf("a healthy store was rejected: %v", err)
	}
	// Truncate it the way a partial write or a shred would.
	if err := os.Truncate(store, 0); err != nil {
		t.Skipf("cannot truncate the fixture: %v", err)
	}
	if err := verifiedSQLiteStore(store, 3, 2); err == nil {
		t.Fatal("a store corrupted after it was verified still passed from cache")
	}
}

func TestVerifiedSnapshotSummaryRecheckesASnapshotThatChangedOnDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.ndjson")
	good := `{"record_type":"header","schema_version":"1.0","repo_key":"gh/example/repo"}` + "\n"
	if err := os.WriteFile(path, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := verifiedSnapshotSummary(path, "gh/example/repo"); err != nil {
		t.Fatalf("a healthy snapshot was rejected: %v", err)
	}
	if err := os.WriteFile(path, []byte("not json at all\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := verifiedSnapshotSummary(path, "gh/example/repo"); err == nil {
		t.Fatal("a snapshot corrupted after it was verified still passed from cache")
	}
}
