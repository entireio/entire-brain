package factstore

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/entireio/entire-brain/factmerge"
)

func TestRepoFactStoreReadOnlyMigrationPreservesData(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix directory mode test; Windows does not enforce these permission bits")
	}
	for _, migrated := range []bool{false, true} {
		name := "legacy"
		if migrated {
			name = "native"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			rec := repoStoreFact("read-only memory", "main")
			writeLegacyView(t, root, "main", []factmerge.Record{rec})
			if migrated {
				if err := MigrateRepository(root); err != nil {
					t.Fatal(err)
				}
			}
			legacyPath := filepath.Join(root, FileRelPath("main"))
			legacyBefore, err := os.ReadFile(legacyPath)
			if err != nil {
				t.Fatal(err)
			}
			snapshotBefore, _ := os.ReadFile(filepath.Join(root, RepositoryPath))
			for _, dir := range []string{filepath.Dir(legacyPath), filepath.Join(root, DirName)} {
				if err := os.Chmod(dir, 0o500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
			}
			probe := filepath.Join(root, DirName, "permission-probe")
			if err := os.WriteFile(probe, nil, 0o600); err == nil {
				_ = os.Remove(probe)
				t.Skip("process can bypass directory permissions")
			}
			requireRepoView(t, root, "main", []factmerge.Record{rec})
			if _, err := RepositoryStatus(root); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadStoreFiles(root); err != nil {
				t.Fatal(err)
			}
			for action, call := range map[string]func(string) error{"migrate": MigrateRepository, "rollback": RollbackRepository} {
				if err := call(root); err == nil {
					t.Fatalf("%s succeeded on read-only data", action)
				}
			}
			if err := Write(root, "main", nil); err == nil {
				t.Fatal("write succeeded on read-only data")
			}
			legacyAfter, err := os.ReadFile(legacyPath)
			if err != nil || !bytes.Equal(legacyBefore, legacyAfter) {
				t.Fatalf("failed operation changed legacy data: %v", err)
			}
			snapshotAfter, err := os.ReadFile(filepath.Join(root, RepositoryPath))
			if !bytes.Equal(snapshotBefore, snapshotAfter) || (!migrated && !os.IsNotExist(err)) {
				t.Fatalf("failed operation changed snapshot: %v", err)
			}
		})
	}
}
