package cli

import (
	"database/sql"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSQLiteReadOnlyDSNOpensNativeAbsolutePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "brain store", "semantic.sqlite")
	if err := createSQLiteFixture(path); err != nil {
		t.Fatalf("create sqlite fixture: %v", err)
	}

	dsn := sqliteReadOnlyDSN(path)
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse read-only DSN %q: %v", dsn, err)
	}
	if got := u.Query().Get("immutable"); got != "1" {
		t.Fatalf("immutable query parameter = %q, want 1", got)
	}
	if got := u.Query().Get("mode"); got != "ro" {
		t.Fatalf("mode query parameter = %q, want ro", got)
	}
	if runtime.GOOS == "windows" {
		if !strings.HasPrefix(dsn, "file:///") {
			t.Fatalf("Windows read-only DSN = %q, want an absolute file URI", dsn)
		}
		if strings.Contains(dsn, "%5C") {
			t.Fatalf("Windows read-only DSN = %q, want forward slashes", dsn)
		}
	}

	db, err := sql.Open(sqliteDriverName, dsn)
	if err != nil {
		t.Fatalf("open sqlite fixture read-only: %v", err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master`).Scan(&count); err != nil {
		t.Fatalf("query sqlite fixture read-only with DSN %q: %v", dsn, err)
	}
}

func TestSQLiteLiveReadOnlyDSNUsesPlatformSafeURIWithoutImmutable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "brain store", "history.sqlite")
	if err := createSQLiteFixture(path); err != nil {
		t.Fatalf("create sqlite fixture: %v", err)
	}

	dsn := sqliteLiveReadOnlyDSN(path)
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse live read-only DSN %q: %v", dsn, err)
	}
	if got := u.Query().Get("immutable"); got != "" {
		t.Fatalf("immutable query parameter = %q, want omitted", got)
	}
	if got := u.Query().Get("mode"); got != "ro" {
		t.Fatalf("mode query parameter = %q, want ro", got)
	}
	if runtime.GOOS == "windows" {
		if !strings.HasPrefix(dsn, "file:///") || strings.Contains(dsn, "%5C") {
			t.Fatalf("Windows live read-only DSN is not a normalized absolute file URI: %q", dsn)
		}
	}

	db, err := sql.Open(sqliteDriverName, dsn)
	if err != nil {
		t.Fatalf("open sqlite fixture live read-only: %v", err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master`).Scan(&count); err != nil {
		t.Fatalf("query sqlite fixture live read-only with DSN %q: %v", dsn, err)
	}
}

func createSQLiteFixture(path string) error {
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return err
	}
	db, err := sql.Open(sqliteDriverName, path)
	if err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE TABLE fixture (id INTEGER PRIMARY KEY)`); err != nil {
		_ = db.Close()
		return err
	}
	return db.Close()
}
