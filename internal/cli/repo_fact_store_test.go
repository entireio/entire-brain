package cli

import (
	"archive/tar"
	"bytes"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func repoStoreFact(text, branch string) factRecord {
	paths := []string{"project.memory.storage"}
	at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	return factRecord{ID: factRecordID(text, paths), Text: text, Paths: paths, Branch: branch,
		Origin: factstore.OriginAuthored, Status: factStatusActive, CreatedAt: at, UpdatedAt: at,
		Provenance: []factAnchor{{SessionID: "session-" + branch, Commit: "commit-" + branch}}}
}

func requireRepoView(t *testing.T, root, branch string, want []factRecord) {
	t.Helper()
	got, err := factstore.Load(root, branch)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("%s: got %d records, want %d", branch, len(got), len(want))
	}
	if len(want) > 0 && !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: got %#v, want %#v", branch, got, want)
	}
}

func TestRepoFactStorePrivacyPurgesMirrorsAndMissingProjections(t *testing.T) {
	root := t.TempDir()
	a := repoStoreFact("private unique statement", "main")
	for _, view := range []string{"main", "feature"} {
		if err := factstore.Write(root, view, []factRecord{a}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(root, factstore.FileRelPath("feature"))); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(root, factstore.DirName, ".store.json.tmp-crashed")
	if err := os.WriteFile(orphan, []byte(a.Text), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := purgeSessionFacts(root, "session-main", nil); err != nil {
		t.Fatal(err)
	}
	for _, view := range []string{"main", "feature"} {
		requireRepoView(t, root, view, nil)
	}
	raw, err := os.ReadFile(filepath.Join(root, factstore.RepositoryPath))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(a.Text)) || bytes.Contains(raw, []byte("session-main")) {
		t.Fatal("privacy deletion left content in native representatives/overrides")
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("purge retained interrupted snapshot: %v", err)
	}
}

func TestRepoFactStoreBundleKeepsLegacyContract(t *testing.T) {
	root := t.TempDir()
	if err := factstore.Write(root, "main", []factRecord{repoStoreFact("bundle", "main")}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, factstore.DirName, ".store.json.tmp-crashed"), []byte("private partial native snapshot"), 0o600); err != nil {
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
		if len(paths) != 1 || filepath.ToSlash(paths[0]) != factstore.FileRelPath("main") {
			t.Fatalf("bundle leaks native storage or drops old data: %v", paths)
		}
		if err := os.Remove(filepath.Join(root, paths[0])); err != nil {
			t.Fatal(err)
		}
		paths, err = collectBrainBundleLayerPaths(root, layer)
		if err != nil || len(paths) != 1 {
			t.Fatalf("missing projection lost export: %v, %v", paths, err)
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
			t.Fatalf("missing projection exported wrong bytes: %s, %v", payload, err)
		}
	}
}
