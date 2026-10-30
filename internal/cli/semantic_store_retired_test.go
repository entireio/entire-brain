package cli

import (
	"errors"
	"github.com/entireio/entire-brain/internal/cli/internal/semstore"
	"os"
	"path/filepath"
	"testing"

	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

// A persisted source without the live graph handle is the pre-0.2.0 layout;
// since COR-2217 Brain has no reader for it, only a remedy.
func TestDeclaredStoreWithoutGraphHandleIsRetired(t *testing.T) {
	src := &semanticSourceManifest{
		SnapshotPath:   "semantic/snapshots/x/snapshot.ndjson",
		GenerationPath: "semantic/generations/x",
		StorePath:      "semantic/generations/x/semantic.sqlite",
	}
	_, err := semstore.ValidateDeclaredStore(t.TempDir(), src)
	if err == nil || !errors.Is(err, semstore.ErrStoreRetired) {
		t.Fatalf("want semstore.ErrStoreRetired, got %v", err)
	}
}

// The dash must not render a declared-but-unserveable index as a healthy empty
// one: a non-nil source without the live graph handle is unreadable, and the
// error is what flips the Sources row to "unreadable".
func TestDashSymbolsTreatRetiredSourceAsUnreadable(t *testing.T) {
	syms, err := semstore.LoadSymbols(t.TempDir(), &semanticSourceManifest{SnapshotPath: "semantic/snapshots/x/snapshot.ndjson"}, 0)
	if !errors.Is(err, semstore.ErrStoreRetired) {
		t.Fatalf("want semstore.ErrStoreRetired, got syms=%v err=%v", syms, err)
	}
	if syms, err := semstore.LoadSymbols(t.TempDir(), nil, 0); err != nil || syms != nil {
		t.Fatalf("a brain with no source stays silently empty, got syms=%v err=%v", syms, err)
	}
}

// A bundle's code-index layer is ignored on import (never restored, never
// persisted), so its schema version must not reject an otherwise valid bundle.
func TestBundleImportIgnoresUnsupportedSemanticSchema(t *testing.T) {
	root := t.TempDir()
	manifest := `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"schema_version":"99.0","snapshot_path":"semantic/snapshots/aaa/snapshot.ndjson"}}}`
	if err := os.WriteFile(filepath.Join(root, bmanifest.FileName), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	imported, err := validateImportedBundle(root, "gh/example/repo")
	if err != nil {
		t.Fatalf("unsupported ignored-layer schema rejected the bundle: %v", err)
	}
	if imported.Sources.Semantic == nil {
		t.Fatal("the layer must survive validation so the not-imported notice can fire")
	}
}
