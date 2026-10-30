package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/historystore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

// An external tool that hardlink-copies the brain's history tree (a backup, or
// a bench harness staging a cheap clone) raises nlink on the LIVE artifacts,
// and the alias guard then refuses every read: history context silently drops
// out of each brief. This test pins the two halves of the fix: the refusal
// names `memory repair`, and repair actually clears the state instead of
// failing on the same guard it is asked to repair.
func TestMemoryRepairRecoversHardlinkedHistoryProjection(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	now := time.Date(2026, 8, 9, 20, 0, 0, 0, time.UTC)
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	rel := "sessions/main/hardlink.jsonl"
	full := filepath.Join(storage.BrainDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hardlinked history"}]}}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Decision: rebuild from canonical sessions."}]}}` + "\n"
	if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := exportManifest{
		SchemaVersion: bmanifest.SchemaVersion, GeneratedAt: now.Add(-time.Hour), RepoKey: storage.Key, DefaultBranch: "main",
		Sources: &brainSources{Sessions: &sessionSourceManifest{GeneratedAt: now.Add(-time.Hour), DefaultBranch: "main", Sessions: []exportSession{
			{SessionID: "hardlink-session", Branch: "main", LatestCheckpoint: "cp", TranscriptPath: rel, CreatedAt: now.Add(-2 * time.Hour)},
		}}},
	}
	if err := writeBrainManifestAndReadme(storage.BrainDir, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := writeBrainHistoryIndexAndSource(storage.BrainDir, now.Add(-time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	before, _ := activeHistoryProjection(t, storage.BrainDir)
	// The real-world clone covers the whole history tree, so the short-term
	// overlay is hardlinked too. Its identity capture runs during prepare and
	// used to abort the very rebuild that supersedes it.
	if err := saveHistoryShortTerm(storage.BrainDir, shortTermIndex{Files: map[string]shortTermFile{}}); err != nil {
		t.Fatal(err)
	}
	cloneDir := t.TempDir()
	for _, artifact := range []string{before.IndexPath, before.ProjectionStatePath, historyShortTermPath} {
		source := filepath.Join(storage.BrainDir, filepath.FromSlash(artifact))
		if err := os.Link(source, filepath.Join(cloneDir, filepath.Base(artifact)+"-"+filepath.Base(filepath.Dir(artifact)))); err != nil {
			t.Skipf("hardlinks unavailable: %v", err)
		}
	}

	_, readErr := historystore.LoadIndex(storage.BrainDir, before)
	if readErr == nil {
		t.Fatal("hardlinked history index was readable")
	}
	for _, want := range []string{"must not be hardlinked", "memory repair"} {
		if !strings.Contains(readErr.Error(), want) {
			t.Fatalf("read error %q does not name %q", readErr, want)
		}
	}

	out, err := execute(t, newMemoryRepairCommand(opts), "--json")
	if err != nil {
		t.Fatalf("repair failed: %v\n%s", err, out)
	}
	var payload struct {
		Rebuilt      bool `json:"rebuilt"`
		IndexCurrent bool `json:"index_current"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Rebuilt || !payload.IndexCurrent {
		t.Fatalf("repair payload = %+v\n%s", payload, out)
	}
	after, _ := activeHistoryProjection(t, storage.BrainDir)
	if after.IndexPath == before.IndexPath {
		t.Fatalf("repair kept the hardlinked generation: %+v", after)
	}
	if _, err := historystore.LoadIndex(storage.BrainDir, after); err != nil {
		t.Fatalf("rebuilt history index unreadable: %v", err)
	}
	oldGeneration := filepath.Dir(filepath.Join(storage.BrainDir, filepath.FromSlash(before.IndexPath)))
	if _, err := os.Lstat(oldGeneration); !os.IsNotExist(err) {
		t.Fatalf("hardlinked generation was not pruned: lstat err=%v", err)
	}
	if _, err := os.Lstat(filepath.Join(storage.BrainDir, filepath.FromSlash(historyShortTermPath))); !os.IsNotExist(err) {
		t.Fatalf("hardlinked overlay was not cleared after the rebuild: lstat err=%v", err)
	}
	cloned, err := os.ReadDir(cloneDir)
	if err != nil || len(cloned) != 3 {
		t.Fatalf("external clone must keep its files: err=%v entries=%d", err, len(cloned))
	}
}
