package factstore

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
)

func TestRepoFactStoreRollbackPartialPublicationCanRetry(t *testing.T) {
	for name, action := range map[string]func(string) error{"rollback": RollbackRepository, "migrate": MigrateRepository} {
		t.Run(name, func(t *testing.T) {
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
			writeSchema1Snapshot(t, root, s)
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
			if err := action(root); err == nil {
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
			if err := action(root); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(root, RepositoryPath)); name == "rollback" && !os.IsNotExist(err) {
				t.Fatalf("snapshot remains: %v", err)
			}
			views, err := LoadStoreFiles(root)
			if err != nil || len(views["a"]) != 1 || len(views["z"]) != 1 || views["a"][0].ID != a.ID || views["z"][0].ID != b.ID {
				t.Fatalf("rollback lost records: %+v, %v", views, err)
			}
			if err := MigrateRepository(root); err != nil {
				t.Fatal(err)
			}
			if err := action(root); err != nil {
				t.Fatal(err)
			}
		})
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

func TestRepoFactStoreSchema1ConversionPreservesLegacyChanges(t *testing.T) {
	for _, action := range []string{"migrate", "rollback", "write"} {
		t.Run(action, func(t *testing.T) {
			root := t.TempDir()
			a, b, c := repoStoreFact("private old body", "main"), repoStoreFact("surviving view", "feature"), repoStoreFact("snapshot only", "missing")
			for branch, rec := range map[string]factmerge.Record{"main": a, "feature": b, "missing": c} {
				writeLegacyView(t, root, branch, []factmerge.Record{rec})
			}
			writeSchema1FromLegacy(t, root)
			// Old purge plus old retraction after the snapshot was written.
			writeLegacyView(t, root, "main", nil)
			b.Status = factmerge.StatusRetracted
			b.UpdatedAt = b.UpdatedAt.Add(time.Hour)
			b.Provenance = []factmerge.Anchor{{SessionID: "new-source"}}
			writeLegacyView(t, root, "feature", []factmerge.Record{b})
			if err := os.Remove(filepath.Join(root, FileRelPath("missing"))); err != nil {
				t.Fatal(err)
			}
			var err error
			switch action {
			case "migrate":
				err = MigrateRepository(root)
			case "rollback":
				err = RollbackRepository(root)
			case "write":
				err = Write(root, "target", []factmerge.Record{repoStoreFact("new write", "target")})
			}
			if err != nil {
				t.Fatal(err)
			}
			requireRepoView(t, root, "main", nil)
			requireRepoView(t, root, "feature", []factmerge.Record{b})
			requireRepoView(t, root, "missing", []factmerge.Record{c})
			raw, err := os.ReadFile(filepath.Join(root, RepositoryPath))
			if action == "rollback" {
				if !os.IsNotExist(err) {
					t.Fatalf("rollback retained snapshot: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if string(raw) != "{\"schema_version\":2,\"storage\":\"legacy_views\"}\n" {
					t.Fatalf("marker contains unexpected data: %s", raw)
				}
			}
			if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() {
					return nil
				}
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				if bytes.Contains(data, []byte(a.Text)) {
					t.Errorf("private body retained: %s", path)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRepoFactStoreSchema2MarkerValidation(t *testing.T) {
	for _, data := range []string{`{"schema_version":2}`, `{"schema_version":2,"storage":"future"}`, `{"schema_version":2,"storage":"legacy_views","records":{}}`, `{"schema_version":2,"storage":"legacy_views"} {}`, `{"schema_version":3,"storage":"legacy_views"}`} {
		t.Run(data, func(t *testing.T) {
			root := t.TempDir()
			a := repoStoreFact("unchanged", "main")
			writeLegacyView(t, root, "main", []factmerge.Record{a})
			if err := os.WriteFile(filepath.Join(root, RepositoryPath), []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			for _, action := range []func(string) error{MigrateRepository, RollbackRepository, func(root string) error { return Write(root, "main", nil) }} {
				if err := action(root); err == nil {
					t.Fatal("accepted invalid marker")
				}
			}
			raw, err := os.ReadFile(filepath.Join(root, FileRelPath("main")))
			if err != nil || !bytes.Contains(raw, []byte(a.Text)) {
				t.Fatalf("invalid marker changed data: %v", err)
			}
		})
	}
}

func TestRepoFactStoreSchema1ConversionRejectsUntrustedPrivacyState(t *testing.T) {
	for _, state := range []string{`{`, `{"version":999,"excluded":{}}`} {
		t.Run(state, func(t *testing.T) {
			root := t.TempDir()
			a := repoStoreFact("must not change", "main")
			writeLegacyView(t, root, "main", []factmerge.Record{a})
			writeSchema1FromLegacy(t, root)
			if err := brainstore.WriteBrainRelativeFileAtomic(root, brainstore.SessionTombstonesPath, []byte(state), 0600); err != nil {
				t.Fatal(err)
			}
			snapshot, _ := os.ReadFile(filepath.Join(root, RepositoryPath))
			legacy, _ := os.ReadFile(filepath.Join(root, FileRelPath("main")))
			for _, action := range []func(string) error{MigrateRepository, RollbackRepository, func(root string) error { return Write(root, "main", nil) }} {
				if err := action(root); err == nil {
					t.Fatal("conversion ignored invalid privacy state")
				}
				after, _ := os.ReadFile(filepath.Join(root, RepositoryPath))
				if !bytes.Equal(snapshot, after) {
					t.Fatal("failed conversion changed snapshot")
				}
				after, _ = os.ReadFile(filepath.Join(root, FileRelPath("main")))
				if !bytes.Equal(legacy, after) {
					t.Fatal("failed conversion changed legacy payload")
				}
			}
		})
	}
}
