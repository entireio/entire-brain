package brainstore

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/internal/sqlitedriver"
)

func TestSQLiteSnapshotReplaysWALWithoutChangingSource(t *testing.T) {
	brainDir := t.TempDir()
	const rel = "source.sqlite"
	path := filepath.Join(brainDir, rel)
	db, err := sql.Open(sqlitedriver.Name, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA wal_autocheckpoint=0`,
		`CREATE TABLE evidence (value TEXT)`,
		`INSERT INTO evidence VALUES ('committed in WAL')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	before := map[string][]byte{}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		data, err := os.ReadFile(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		before[suffix] = data
	}
	if len(before["-wal"]) == 0 {
		t.Fatal("fixture must retain committed WAL frames")
	}
	snapshot, cleanup, err := SnapshotPrivacySQLiteStore(brainDir, rel)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if filepath.Dir(snapshot) == brainDir {
		t.Fatal("snapshot must live outside the source directory")
	}
	if err := ValidatePrivacySQLiteSnapshot(snapshot, rel); err != nil {
		t.Fatal(err)
	}
	reader, err := sql.Open(sqlitedriver.Name, SQLiteReadOnlyDSN(snapshot))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var value string
	if err := reader.QueryRow(`SELECT value FROM evidence`).Scan(&value); err != nil || value != "committed in WAL" {
		t.Fatalf("snapshot lost committed WAL data: value=%q err=%v", value, err)
	}
	if _, err := reader.Exec(`INSERT INTO evidence VALUES ('unexpected write')`); err == nil {
		t.Fatal("immutable snapshot reader accepted a write")
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	for suffix, want := range before {
		got, err := os.ReadFile(path + suffix)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("source %q changed during snapshot: %v", suffix, err)
		}
	}
	cleanup()
	if _, err := os.Stat(filepath.Dir(snapshot)); !os.IsNotExist(err) {
		t.Fatalf("snapshot cleanup left its directory: %v", err)
	}
}

func TestPrivacyReadersDistinguishAbsentUnsafeAndBounded(t *testing.T) {
	brainDir := t.TempDir()
	if _, present, err := ReadPrivacyArtifact(brainDir, "missing", "fixture", 10); err != nil || present {
		t.Fatalf("missing artifact: present=%v err=%v", present, err)
	}
	if err := os.Mkdir(filepath.Join(brainDir, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brainDir, "nested", "data"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, present, err := ReadPrivacyArtifact(brainDir, "nested/data", "fixture", 7)
	if err != nil || !present || string(data) != "payload" {
		t.Fatalf("valid artifact: data=%q present=%v err=%v", data, present, err)
	}
	if _, present, err := ReadPrivacyArtifact(brainDir, "nested/data", "fixture", 6); !present || err == nil || !strings.Contains(err.Error(), StateCorruptCode) {
		t.Fatalf("oversized artifact: present=%v err=%v", present, err)
	}
	for _, rel := range []string{"../outside", "nested"} {
		if _, _, err := ReadPrivacyArtifact(brainDir, rel, "fixture", 10); err == nil || !strings.Contains(err.Error(), StateUnsafeCode) {
			t.Fatalf("unsafe artifact %q: %v", rel, err)
		}
	}
}

func TestSQLiteSnapshotFailureCleansPrivateFiles(t *testing.T) {
	brainDir := t.TempDir()
	privateRoot := t.TempDir()
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, privateRoot)
	}
	const rel = "corrupt.sqlite"
	path := filepath.Join(brainDir, rel)
	canary := []byte("not a SQLite database")
	if err := os.WriteFile(path, canary, 0o600); err != nil {
		t.Fatal(err)
	}
	if snapshot, cleanup, err := SnapshotPrivacySQLiteStore(brainDir, rel); err == nil || snapshot != "" || cleanup != nil || !strings.Contains(err.Error(), StateCorruptCode) {
		if cleanup != nil {
			cleanup()
		}
		t.Fatalf("corrupt snapshot: path=%q err=%v", snapshot, err)
	}
	entries, err := os.ReadDir(privateRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed snapshot leaked private files: %v err=%v", entries, err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, canary) {
		t.Fatalf("failed snapshot modified source: %q err=%v", got, err)
	}
	if _, _, err := SnapshotPrivacySQLiteStore(brainDir, "missing.sqlite"); err == nil || !strings.Contains(err.Error(), StateCorruptCode) {
		t.Fatalf("missing required source was not an error: %v", err)
	}
}
