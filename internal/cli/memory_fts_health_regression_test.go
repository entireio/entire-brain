package cli

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestMemoryHistoryFTSHealthClassifiesSchemaAndReadOnlyStates(t *testing.T) {
	for _, tc := range []struct {
		name, schema, fingerprint, want string
		source                          *historySourceManifest
	}{
		{"missing fingerprint", historyFTSSchema, "", "corrupt", nil},
		{"older schema", "0", "old", memoryErrMigrationRequired, nil},
		{"newer schema", "999", "new", "unsupported", nil},
		{"current without source is stale", historyFTSSchema, "current", "stale", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			brainDir := t.TempDir()
			writeHealthFTSMetaFixture(t, brainDir, tc.schema, tc.fingerprint)
			path := filepath.Join(brainDir, filepath.FromSlash(historyFTSDBRelPath()))
			if tc.schema == historyFTSSchema && tc.fingerprint != "" {
				db, err := sql.Open(sqliteDriverName, path)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`INSERT INTO history_fts_meta(key, value) VALUES ('records_fingerprint', ?)`, tc.fingerprint); err != nil {
					_ = db.Close()
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			dataBefore := privacyTreeDigest(t, brainDir)
			health := memoryHistoryFTSHealth(brainDir, tc.source)
			if privacyTreeDigest(t, brainDir) != dataBefore {
				t.Fatal("FTS health changed data or created sidecars")
			}
			if health["state"] != tc.want || health["action"] == "none" {
				t.Fatalf("health=%#v", health)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(before) {
				t.Fatalf("health mutated FTS bytes err=%v", err)
			}
		})
	}
}

func TestMemoryHistoryFTSHealthComparesCanonicalHistoryFingerprint(t *testing.T) {
	brainDir := writePrivacyFixture(t)
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	index, err := loadBrainHistoryIndex(brainDir, manifest.Sources.History)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(brainDir, filepath.FromSlash(historyFTSDBRelPath()))
	write := func(fingerprint string) {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		writeHealthFTSMetaFixture(t, brainDir, historyFTSSchema, "legacy")
		db, err := sql.Open(sqliteDriverName, path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO history_fts_meta(key, value) VALUES ('records_fingerprint', ?)`, fingerprint); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	write(historyFTSIdentityFromIndex(index).RecordsFingerprint)
	if health := memoryHistoryFTSHealth(brainDir, manifest.Sources.History); health["state"] != "current" {
		t.Fatalf("current health=%#v", health)
	}
	write("different")
	if health := memoryHistoryFTSHealth(brainDir, manifest.Sources.History); health["state"] != "stale" || health["error_code"] != memoryErrSourceStale {
		t.Fatalf("stale health=%#v", health)
	}
	bad := *manifest.Sources.History
	bad.IndexPath = "history/missing.json"
	if health := memoryHistoryFTSHealth(brainDir, &bad); health["state"] != "stale" || health["error_code"] != memoryErrSourceStale {
		t.Fatalf("unreadable source health=%#v", health)
	}
}

func TestMemoryHistoryFTSHealthRejectsMissingAndMalformedSchemaMetadata(t *testing.T) {
	for _, tc := range []struct{ name, schema, want string }{{"missing schema", "", "corrupt"}, {"malformed schema", "future", "unsupported"}} {
		t.Run(tc.name, func(t *testing.T) {
			brainDir := t.TempDir()
			path := filepath.Join(brainDir, filepath.FromSlash(historyFTSDBRelPath()))
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open(sqliteDriverName, path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`CREATE TABLE history_fts_meta(key TEXT PRIMARY KEY, value TEXT)`); err != nil {
				_ = db.Close()
				t.Fatal(err)
			}
			if tc.schema != "" {
				if _, err := db.Exec(`INSERT INTO history_fts_meta(key, value) VALUES ('schema', ?)`, tc.schema); err != nil {
					_ = db.Close()
					t.Fatal(err)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			health := memoryHistoryFTSHealth(brainDir, nil)
			if health["state"] != tc.want {
				t.Fatalf("health=%#v", health)
			}
		})
	}
}
