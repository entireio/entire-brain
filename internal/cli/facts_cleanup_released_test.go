package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
)

// This test intentionally invokes no migration or cleanup command. Both direct
// pre-flat upgrades and v0.5.0's already-parked copies must clean up on normal use.
func TestAutomaticMemoryCleanupReleased(t *testing.T) {
	f := newReleasedMemoryFixture(t)
	f.env = append(f.env, "HOME="+filepath.Join(f.root, "home"), "USERPROFILE="+filepath.Join(f.root, "home"), "ENTIRE_BRAIN_NO_EGRESS=1", "ENTIRE_BRAIN_DAEMON_NO_REGISTER=1")
	seed := f.old
	if os.Getenv("ENTIRE_BRAIN_TEST_LEGACY_VERSION") == "v0.5.0" {
		seed = os.Getenv("ENTIRE_BRAIN_TEST_PRE_FLAT_BINARY")
		if seed == "" {
			t.Fatal("v0.5.0 cleanup requires the real v0.4.0 binary to create its migration input")
		}
	}
	main := f.remember(seed, "Main memory survives automatic cleanup", "main")
	feature := f.remember(seed, "Feature memory survives automatic cleanup", "feature")
	retired := f.remember(seed, "Retracted memory stays retracted", "main")
	f.run(seed, "memories", "retract", retired.ID, "--branch", "main", "--json")
	root := f.brain()
	// A hosted/branch mirror stores the same released bytes in another branch.
	raw, err := os.ReadFile(f.legacyStorePath(root, "main"))
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(f.legacyStorePath(root, "feature"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(raw); err != nil {
		t.Fatal(err)
	}
	file.Close()
	f.run(seed, "remember", "Global memory survives automatic cleanup", "--global", "--path", "project.memory.storage", "--json")
	global := filepath.Join(f.root, "data", "global")
	if seed != f.old {
		f.remember(f.old, "v0.5.0 repository write", "main")
		f.run(f.old, "remember", "v0.5.0 global write", "--global", "--path", "project.memory.storage", "--json")
		for _, dir := range []string{root, global} {
			paths, _ := filepath.Glob(filepath.Join(dir, "facts", ".legacy-*"))
			if len(paths) == 0 {
				t.Fatal("v0.5.0 did not produce a real parked migration")
			}
		}
	}
	expected := map[string][]factRecord{}
	for _, dir := range []string{root, global} {
		records, err := factstore.Load(dir)
		if err != nil {
			t.Fatal(err)
		}
		expected[dir] = records
	}
	f.remember(f.current, "First normal repository write after upgrade", "main")
	f.run(f.current, "remember", "First normal global write after upgrade", "--global", "--path", "project.memory.storage", "--json")
	for _, dir := range []string{root, global} {
		paths, _ := filepath.Glob(filepath.Join(dir, "facts", ".legacy-*"))
		if len(paths) != 0 {
			t.Fatalf("upgrade left redundant copies: %v", paths)
		}
		got, err := factstore.Load(dir)
		if err != nil {
			t.Fatal(err)
		}
		byID := map[string]factRecord{}
		for _, rec := range got {
			byID[rec.ID] = rec
		}
		for _, rec := range expected[dir] {
			want, _ := json.Marshal(rec)
			actual, _ := json.Marshal(byID[rec.ID])
			if !bytes.Equal(want, actual) {
				t.Fatalf("memory changed during upgrade: %s", rec.ID)
			}
		}
	}
	f.requireVisible(f.current, "main", []factRecord{main, feature}, []factRecord{retired})
	// A repeated normal write must not recreate legacy directories.
	f.remember(f.current, "Second normal repository write after upgrade", "main")
	paths, _ := filepath.Glob(filepath.Join(root, "facts", ".legacy-*"))
	if len(paths) != 0 {
		t.Fatal("normal write recreated legacy storage")
	}
}
