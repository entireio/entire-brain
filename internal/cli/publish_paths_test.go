package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ashtom/entire-brain/internal/brainwire"
)

func TestPublishArtifactPathsRejectSymlinks(t *testing.T) {
	overlay := filepath.Join(semanticDirName, "overlays", testPublishCommit+".."+testPublishHead+".json")
	snapshot := filepath.Join(semanticDirName, semanticSnapshotsDir, testPublishCommit, semanticSnapshotName)
	facts := filepath.FromSlash(factsFileRelPath("main"))
	for _, tc := range []struct {
		name, rel string
		collect   func(*brainwire.BrainArtifact, string) ([]publishArtifact, error)
	}{
		{"facts-root", factsDirName, collectFactArtifacts},
		{"facts-leaf", facts, collectFactArtifacts},
		{"snapshot-semantic-parent", semanticDirName, collectSnapshotArtifacts},
		{"snapshot-root", filepath.Join(semanticDirName, semanticSnapshotsDir), collectSnapshotArtifacts},
		{"snapshot-leaf", snapshot, collectSnapshotArtifacts},
		{"overlay-semantic-parent", semanticDirName, collectOverlayArtifacts},
		{"overlay-root", filepath.Join(semanticDirName, "overlays"), collectOverlayArtifacts},
		{"overlay-leaf", overlay, collectOverlayArtifacts},
	} {
		t.Run(tc.name, func(t *testing.T) {
			brainDir, outside := t.TempDir(), t.TempDir()
			writePublishBrainFixture(t, brainDir)
			link := filepath.Join(brainDir, tc.rel)
			target := filepath.Join(outside, "external")
			if err := os.Rename(link, target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			art := brainwire.NewBrainArtifact("gh/example/repo", "main", time.Now())
			blobs, err := tc.collect(art, brainDir)
			if err == nil || !strings.Contains(err.Error(), "symlink") {
				t.Fatalf("expected symlink refusal, got %d artifacts, err=%v", len(blobs), err)
			}
			if len(blobs) != 0 {
				t.Fatalf("unsafe collection returned %d artifacts", len(blobs))
			}
		})
	}
}

func TestPublishBlobRejectsLeafSwapBeforeRead(t *testing.T) {
	brainDir := t.TempDir()
	rel := filepath.FromSlash(factsFileRelPath("main"))
	target := filepath.Join(brainDir, rel)
	outside := filepath.Join(t.TempDir(), "external.ndjson")
	writePublishFile(t, target, []byte("local"))
	writePublishFile(t, outside, []byte("outside-secret"))
	original := memoryStateOpenFile
	swapped := false
	memoryStateOpenFile = func(path string, flag int, perm os.FileMode) (*os.File, error) {
		if path == target && !swapped {
			swapped = true
			if err := os.Remove(target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, target); err != nil {
				t.Fatal(err)
			}
		}
		return original(path, flag, perm)
	}
	t.Cleanup(func() { memoryStateOpenFile = original })
	data, ok, err := readBrainBlob(brainDir, rel)
	if !swapped || err == nil || ok || len(data) != 0 {
		t.Fatalf("leaf swap returned data=%q, ok=%v, err=%v, swapped=%v", data, ok, err, swapped)
	}
}

func TestPublishBlobBoundsAndOptionalFiles(t *testing.T) {
	brainDir := t.TempDir()
	rel := filepath.FromSlash(factsFileRelPath("main"))
	if data, ok, err := readBrainBlob(brainDir, rel); err != nil || ok || len(data) != 0 {
		t.Fatalf("missing optional blob: data=%q ok=%v err=%v", data, ok, err)
	}
	writePublishFile(t, filepath.Join(brainDir, rel), []byte("content"))
	if data, ok, err := readBrainBlob(brainDir, rel); err != nil || !ok || string(data) != "content" {
		t.Fatalf("ordinary blob: data=%q ok=%v err=%v", data, ok, err)
	}
	t.Setenv("ENTIRE_BRAIN_MAX_SNAPSHOT_BYTES", "3")
	if data, ok, err := readBrainBlob(brainDir, rel); err == nil || ok || len(data) != 0 || !strings.Contains(err.Error(), "maximum size") {
		t.Fatalf("oversized blob: data=%q ok=%v err=%v", data, ok, err)
	}
	if data, ok, err := readBrainBlob(brainDir, "../external"); err == nil || ok || len(data) != 0 {
		t.Fatalf("escaping blob: data=%q ok=%v err=%v", data, ok, err)
	}
}

func TestPublishBlobSizeErrorUsesOpenedPath(t *testing.T) {
	brainDir := t.TempDir()
	path := filepath.Join(brainDir, "facts", "sample.ndjson")
	writePublishFile(t, path, []byte("content"))
	t.Setenv("ENTIRE_BRAIN_MAX_SNAPSHOT_BYTES", "3")
	_, _, err := readBrainBlob(brainDir, "  facts//sample.ndjson  ")
	bound, ok := err.(*readBoundExceededError)
	if !ok || bound.source != path {
		t.Fatalf("size error should name opened path %q: %v", path, err)
	}
}
