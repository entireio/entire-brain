package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/command/watch"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	"github.com/spf13/cobra"
)

func writeCleanupFixture(t *testing.T, dir string, alreadyFlat bool) string {
	t.Helper()
	record := factmerge.Record{ID: factmerge.RecordID("automatic cleanup memory", []string{"project.cleanup.test"}), Text: "automatic cleanup memory", Paths: []string{"project.cleanup.test"}, Branch: "main", Origin: "authored", Status: "active", CreatedAt: setupTestNow, UpdatedAt: setupTestNow, Provenance: []factmerge.Anchor{{SessionID: "source"}}}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	rel := "facts/main-old/facts.ndjson"
	if alreadyFlat {
		rel = "facts/.legacy-v050/main-old/facts.ndjson"
		data, _ := json.Marshal(map[string]any{"schema_version": 3, "records": map[string]json.RawMessage{record.ID: raw}})
		if err := brainstore.WriteBrainRelativeFileAtomic(dir, "facts/store.json", data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := brainstore.WriteBrainRelativeFileAtomic(dir, rel, append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	return record.ID
}

func assertCleanupFixture(t *testing.T, dir, id string) {
	t.Helper()
	records, err := factstore.Load(dir)
	if err != nil || len(records) != 1 || records[0].ID != id {
		t.Fatalf("memory lost: %v %v", records, err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "facts", ".legacy-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("cleanup did not finish: %v %v", matches, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "facts", "main-old")); !os.IsNotExist(err) {
		t.Fatal("legacy source remains active")
	}
}

func TestAutomaticMemoryCleanupMaintainsRepositoryAndGlobal(t *testing.T) {
	for _, alreadyFlat := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "v050"}[alreadyFlat], func(t *testing.T) {
			f := newSetupTestFixture(t)
			f.writeSessions(t)
			global, err := factstore.GlobalBrainDir(f.env)
			if err != nil {
				t.Fatal(err)
			}
			repoID := writeCleanupFixture(t, f.storage.BrainDir, alreadyFlat)
			globalID := writeCleanupFixture(t, global, alreadyFlat)
			cmd := &cobra.Command{}
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			steps := watchStepsForRepo(cmd, f.opts, watch.DefaultFlags(), f.repoDir, f.opts.Now)
			if err := steps.Maintain(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertCleanupFixture(t, f.storage.BrainDir, repoID)
			assertCleanupFixture(t, global, globalID)
		})
	}
}

func TestAutomaticMemoryCleanupDoesNotBlockOrCreateGlobal(t *testing.T) {
	f := newSetupTestFixture(t)
	f.writeSessions(t)
	writeCleanupFixture(t, f.storage.BrainDir, true)
	unlock, err := brainstore.AcquireBrainWriteLock(f.storage.BrainDir)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	maintainFactsStorage(context.Background(), io.Discard, f.opts, f.storage.BrainDir)
	unlock()
	if time.Since(start) > time.Second {
		t.Fatal("maintenance queued behind foreground writer")
	}
	global, _ := factstore.GlobalBrainDir(f.env)
	if _, err := os.Stat(global); !os.IsNotExist(err) {
		t.Fatal("maintenance created unused global store")
	}
}

func TestAutomaticMemoryCleanupRunsOnForegroundDelta(t *testing.T) {
	f := newSetupTestFixture(t)
	f.writeSessions(t)
	id := writeCleanupFixture(t, f.storage.BrainDir, true)
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if _, err := runRefreshDeltaStats(cmd, f.opts, false, false); err != nil {
		t.Fatal(err)
	}
	assertCleanupFixture(t, f.storage.BrainDir, id)
}
