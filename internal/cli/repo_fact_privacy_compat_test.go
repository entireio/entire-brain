package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
)

func TestRepoFactStorePrivacyDetectsAndRepairsHiddenSchema1Bodies(t *testing.T) {
	for _, kind := range []string{"empty-projection", "orphan-body", "override", "temporary"} {
		t.Run(kind, func(t *testing.T) {
			root := writePrivacyFixture(t)
			plan, err := buildSessionPurgePlan(root, "secret-sess")
			if err != nil {
				t.Fatal(err)
			}
			if err := executeSessionCleanup(root, "secret-sess", plan, time.Now(), "test", true); err != nil {
				t.Fatal(err)
			}
			rec := repoStoreFact("hidden private memory canary", "main")
			rec.Provenance = []factAnchor{{SessionID: "secret-sess"}}
			if err := factstore.Write(root, "main", []factRecord{rec}); err != nil {
				t.Fatal(err)
			}
			writeSchema1Fixture(t, root)
			path := filepath.Join(root, factstore.RepositoryPath)
			if kind == "orphan-body" || kind == "override" {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var snapshot map[string]json.RawMessage
				if err := json.Unmarshal(data, &snapshot); err != nil {
					t.Fatal(err)
				}
				if kind == "orphan-body" {
					snapshot["views"] = json.RawMessage(`{}`)
				} else {
					// The representative has no private anchor; only the override does.
					rec.Provenance = nil
					snapshot["records"], _ = json.Marshal(map[string]factRecord{rec.ID: rec})
					dir := filepath.Base(factstore.BranchRelDir("main"))
					snapshot["views"], _ = json.Marshal(map[string]any{dir: map[string]any{"entries": []any{map[string]any{"id": rec.ID, "override": map[string]any{"provenance": []factAnchor{{SessionID: "secret-sess"}}}}}}})
				}
				data, err = json.Marshal(snapshot)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			// Simulate a successful released-client purge, bypassing the new writer.
			if err := os.WriteFile(filepath.Join(root, factstore.FileRelPath("main")), nil, 0600); err != nil {
				t.Fatal(err)
			}
			if kind == "temporary" {
				if err := factstore.MigrateRepository(root); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "facts", ".store.json.tmp-interrupted"), []byte(rec.Text), 0600); err != nil {
					t.Fatal(err)
				}
			}
			report, err := verifySessionPrivacy(root)
			if err != nil {
				t.Fatal(err)
			}
			if report.Clean {
				t.Fatal("verification ignored physically retained private content")
			}
			if _, _, err := purgeSessionFacts(root, "secret-sess", nil); err != nil {
				t.Fatal(err)
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
				if bytes.Contains(data, []byte(rec.Text)) {
					t.Errorf("private body remains in %s", path)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			report, err = verifySessionPrivacy(root)
			if err != nil || !report.Clean {
				t.Fatalf("repair not clean: %+v, %v", report, err)
			}
		})
	}
}

func TestRepoFactStoreConversionDoesNotRestorePurgedMissingView(t *testing.T) {
	for _, action := range []string{"migrate", "rollback", "write"} {
		t.Run(action, func(t *testing.T) {
			root := writePrivacyFixture(t)
			plan, err := buildSessionPurgePlan(root, "secret-sess")
			if err != nil {
				t.Fatal(err)
			}
			if err := executeSessionCleanup(root, "secret-sess", plan, time.Now(), "test", true); err != nil {
				t.Fatal(err)
			}
			private := repoStoreFact("do not restore private body", "main")
			private.Provenance = []factAnchor{{SessionID: "secret-sess"}}
			shared := repoStoreFact("retain independent source", "main")
			shared.Provenance = []factAnchor{{SessionID: "secret-sess"}, {SessionID: "kept-sess"}}
			pathOnly := repoStoreFact("do not restore transcript-only anchor", "main")
			// Use the durable scope written by the real cleanup transaction.
			if len(plan.Transcripts) == 0 {
				t.Fatal("fixture has no transcript scope")
			}
			pathOnly.Provenance = []factAnchor{{Transcript: plan.Transcripts[0].Path}}
			if err := factstore.Write(root, "main", []factRecord{private, shared, pathOnly}); err != nil {
				t.Fatal(err)
			}
			writeSchema1Fixture(t, root)
			if err := os.Remove(filepath.Join(root, factstore.FileRelPath("main"))); err != nil {
				t.Fatal(err)
			}
			switch action {
			case "migrate":
				err = factstore.MigrateRepository(root)
			case "rollback":
				err = factstore.RollbackRepository(root)
			case "write":
				err = factstore.Write(root, "other", []factRecord{repoStoreFact("new record", "other")})
			}
			if err != nil {
				t.Fatal(err)
			}
			shared.Provenance = []factAnchor{{SessionID: "kept-sess"}}
			requireRepoView(t, root, "main", []factRecord{shared})
			raw, err := os.ReadFile(filepath.Join(root, factstore.FileRelPath("main")))
			if err != nil {
				t.Fatal(err)
			}
			for _, needle := range []string{private.Text, pathOnly.Text, "secret-sess"} {
				if bytes.Contains(raw, []byte(needle)) {
					t.Fatalf("conversion restored %s", needle)
				}
			}
		})
	}
}
