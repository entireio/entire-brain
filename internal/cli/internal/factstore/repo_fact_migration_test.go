package factstore

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/entire-brain/factmerge"
)

func TestRepoFactStoreRollbackPartialPublicationCanRetry(t *testing.T) {
	root := t.TempDir()
	// Directory names determine publication order. Both records exist only in
	// the native snapshot when rollback starts.
	a, b := repoStoreFact("first view", "a"), repoStoreFact("second view", "z")
	s := emptyRepoFactStore()
	for dir, rec := range map[string]factmerge.Record{"a": a, "z": b} {
		if err := s.replaceView(dir, []factmerge.Record{rec}, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeRepoFactStore(root, &s); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(root, RepositoryPath))
	if err != nil {
		t.Fatal(err)
	}
	// Inventory ignores a plain file at the directory level, but publishing
	// z/facts.ndjson must fail after a/facts.ndjson has succeeded.
	blocker := filepath.Join(root, DirName, "z")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RollbackRepository(root); err == nil {
		t.Fatal("rollback ignored publication failure")
	}
	if _, err := os.Stat(filepath.Join(root, legacyFactViewRel("a"))); err != nil {
		t.Fatalf("first view was not published: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(root, RepositoryPath))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("failed rollback changed snapshot: %v", err)
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if err := RollbackRepository(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, RepositoryPath)); !os.IsNotExist(err) {
		t.Fatalf("snapshot remains: %v", err)
	}
	views, err := LoadStoreFiles(root)
	if err != nil || len(views["a"]) != 1 || len(views["z"]) != 1 || views["a"][0].ID != a.ID || views["z"][0].ID != b.ID {
		t.Fatalf("rollback lost records: %+v, %v", views, err)
	}
	if err := MigrateRepository(root); err != nil {
		t.Fatal(err)
	}
	if err := RollbackRepository(root); err != nil {
		t.Fatal(err)
	}
}

func TestRepoFactStoreEmptyMigrationCycle(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent")
	if _, err := Load(root, "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := RepositoryStatus(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("read created absent store: %v", err)
	}
	for _, action := range []func(string) error{MigrateRepository, MigrateRepository, RollbackRepository, RollbackRepository, MigrateRepository} {
		if err := action(root); err != nil {
			t.Fatal(err)
		}
		report, err := RepositoryStatus(root)
		if err != nil || report.Records != 0 || report.Views != 0 {
			t.Fatalf("empty migration changed data: %+v, %v", report, err)
		}
	}
}
