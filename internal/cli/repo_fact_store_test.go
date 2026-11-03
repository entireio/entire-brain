package cli

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

func repoStoreFact(text, branch string) factRecord {
	paths := []string{"project.memory.storage"}
	at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	return factRecord{ID: factRecordID(text, paths), Text: text, Paths: paths, Branch: branch,
		Origin: factstore.OriginAuthored, Status: factStatusActive, CreatedAt: at, UpdatedAt: at,
		Provenance: []factAnchor{{SessionID: "session-" + branch, Commit: "commit-" + branch}}}
}

// The facts bundle layer carries the flat store itself and nothing private:
// crashed atomic-writer temporaries and parked legacy dirs stay home.
func TestRepoFactStoreBundleCarriesTheFlatStoreOnly(t *testing.T) {
	root := t.TempDir()
	if err := factstore.Write(root, []factRecord{repoStoreFact("bundle", "main")}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, factstore.DirName, ".store.json.tmp-crashed"), []byte("private partial native snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	parked := filepath.Join(root, factstore.DirName, ".legacy-1700000000", "main-legacy00")
	if err := os.MkdirAll(parked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parked, "facts.ndjson"), []byte(`{"id":"fact:old"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, layer := range brainBundleLayers {
		if layer.name != bundleLayerFacts {
			continue
		}
		paths, err := collectBrainBundleLayerPaths(root, layer)
		if err != nil {
			t.Fatal(err)
		}
		if len(paths) != 1 || filepath.ToSlash(paths[0]) != factstore.RepositoryPath {
			t.Fatalf("bundle must carry exactly the flat store: %v", paths)
		}
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		if err := addBundlePath(tw, root, paths[0]); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		tr := tar.NewReader(&buf)
		if _, err := tr.Next(); err != nil {
			t.Fatal(err)
		}
		payload, err := io.ReadAll(tr)
		if err != nil || !bytes.Contains(payload, []byte(`"text":"bundle"`)) {
			t.Fatalf("bundle exported wrong bytes: %s, %v", payload, err)
		}
	}
}

// Refresh is a write entry point: a legacy-only brain migrates into the flat
// store on the spot — records folded by content id, branch dirs parked.
func TestRepoFactStoreRefreshMigratesLegacyBranchFiles(t *testing.T) {
	root := t.TempDir()
	rec := repoStoreFact("stable observation", "main")
	rec.Kind = factKindConvention
	// Build a real legacy-only fixture. Using factstore.Write here would
	// store it flat already and would not test the upgrade entry point.
	for _, branch := range []string{"main", "feature"} {
		path := filepath.Join(root, filepath.FromSlash(testLegacyFactsRel(branch)))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		var data bytes.Buffer
		if err := factmerge.WriteNDJSON(&data, []factRecord{rec}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeBrainManifestAndReadme(root, exportManifest{SchemaVersion: bmanifest.SchemaVersion, GeneratedAt: rec.CreatedAt}); err != nil {
		t.Fatal(err)
	}
	if changed, err := reclassifyAllFacts(root, rec.CreatedAt); err != nil || changed != 0 {
		t.Fatalf("refresh: changed=%d, %v", changed, err)
	}
	report, err := factstore.RepositoryStatus(root)
	if err != nil || report.Format != "repository" || report.Views != 0 || report.Records != 1 {
		t.Fatalf("refresh did not migrate the legacy files: %+v, %v", report, err)
	}
	got, err := factstore.Load(root)
	if err != nil || len(got) != 1 || got[0].ID != rec.ID {
		t.Fatalf("migrated store = %+v, %v", got, err)
	}
	// The branch dirs are parked, not deleted.
	entries, err := os.ReadDir(filepath.Join(root, factstore.DirName))
	if err != nil {
		t.Fatal(err)
	}
	parked := false
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".legacy-") {
			parked = true
		}
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
			t.Fatalf("live legacy branch dir survived migration: %s", entry.Name())
		}
	}
	if !parked {
		t.Fatal("migration must park the legacy branch dirs")
	}
}
