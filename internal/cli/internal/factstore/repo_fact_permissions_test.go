package factstore

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/entireio/entire-brain/factmerge"
)

// Reads stay available on read-only data; every write path fails without
// changing a byte.
func TestFlatStoreReadOnlyDataPreserved(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix directory mode test; Windows does not enforce these permission bits")
	}
	for _, migrated := range []bool{false, true} {
		name := "legacy"
		if migrated {
			name = "flat"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			rec := repoStoreFact("read-only memory", "main")
			writeLegacyFile(t, root, "main-abc123", []factmerge.Record{rec})
			if migrated {
				if _, err := MigrateRepository(root); err != nil {
					t.Fatal(err)
				}
			}
			var frozen [][2]string // path, contents
			err := filepath.Walk(filepath.Join(root, DirName), func(path string, info os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				if info.IsDir() {
					if chmodErr := os.Chmod(path, 0o500); chmodErr != nil {
						return chmodErr
					}
					dir := path
					t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
					return nil
				}
				data, readErr := os.ReadFile(path)
				if readErr != nil {
					return readErr
				}
				frozen = append(frozen, [2]string{path, string(data)})
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			probe := filepath.Join(root, DirName, "permission-probe")
			if writeErr := os.WriteFile(probe, nil, 0o600); writeErr == nil {
				_ = os.Remove(probe)
				t.Skip("process can bypass directory permissions")
			}

			if got := loadByID(t, root); len(got) != 1 || got[rec.ID].Text != rec.Text {
				t.Fatalf("read-only load lost data: %+v", got)
			}
			if _, err := RepositoryStatus(root); err != nil {
				t.Fatal(err)
			}
			if err := Write(root, []factmerge.Record{rec}); err == nil {
				t.Fatal("write succeeded on read-only data")
			}
			if !migrated {
				// Legacy sources present: the pass must write, so it must fail.
				if _, err := MigrateRepository(root); err == nil {
					t.Fatal("migration succeeded on read-only data")
				}
			} else {
				// Nothing legacy left: the pass is a no-op and succeeds without
				// touching the read-only tree.
				if summary, err := MigrateRepository(root); err != nil || summary.Changed() {
					t.Fatalf("no-op migration on read-only data: %+v, %v", summary, err)
				}
			}
			for _, entry := range frozen {
				after, err := os.ReadFile(entry[0])
				if err != nil || !bytes.Equal(after, []byte(entry[1])) {
					t.Fatalf("failed operation changed %s: %v", entry[0], err)
				}
			}
		})
	}
}
