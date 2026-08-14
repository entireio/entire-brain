//go:build !windows

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestMemoryGuardedVectorStoreRejectsLateUnsafeProgressAlias(t *testing.T) {
	brainDir, epoch, ownership, _ := memoryVectorOwnershipFixture(t)
	raw := &memoryVectorTestStore{vectors: map[string][]float32{"keep": {1, 0}}}
	guarded := &memoryGuardedVectorStore{brainDir: brainDir, epoch: epoch, ownership: ownership, store: raw}

	progressPath := filepath.Join(brainDir, filepath.FromSlash(memoryVectorProgressRel))
	outside := filepath.Join(t.TempDir(), "future-progress.json")
	canary := []byte(`{"schema_version":3,"future":true}` + "\n")
	if err := os.WriteFile(outside, canary, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(progressPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, progressPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	err := guarded.upsert(map[string][]float32{"late-add": {0, 1}}, nil)
	var typed *memoryVectorProgressLoadError
	if !errors.As(err, &typed) || typed.Code != memoryErrStateUnsafe || typed.State != memoryVectorProgressUnsafe {
		t.Fatalf("guarded unsafe mutation error = %#v, want typed unsafe", err)
	}
	if _, added := raw.vectors["late-add"]; added || len(raw.vectors) != 1 {
		t.Fatalf("unsafe takeover mutated vectors: %v", raw.vectors)
	}
	after, readErr := os.ReadFile(outside)
	if readErr != nil || string(after) != string(canary) {
		t.Fatalf("unsafe target bytes changed: after=%q err=%v", after, readErr)
	}
	if info, lstatErr := os.Lstat(progressPath); lstatErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("unsafe progress alias was replaced: info=%v err=%v", info, lstatErr)
	}
}
