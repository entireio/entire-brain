package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
)

// brainFileMtimes snapshots every file under brainDir except the lock
// directory, which the promote legitimately touches.
func brainFileMtimes(t *testing.T, brainDir string) map[string]time.Time {
	t.Helper()
	mtimes := map[string]time.Time{}
	err := filepath.WalkDir(brainDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == brainstore.BrainLockDirName {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mtimes[path] = info.ModTime()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return mtimes
}

// ageBrainFiles pushes every snapshotted mtime into the past so any rewrite —
// even one with identical content — is visible as a newer mtime. Returns the
// mtime every file was set to.
func ageBrainFiles(t *testing.T, mtimes map[string]time.Time) time.Time {
	t.Helper()
	past := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for path := range mtimes {
		if err := os.Chtimes(path, past, past); err != nil {
			t.Fatal(err)
		}
	}
	return past
}

// TestFactsPromoteNoOpSkipsAllWrites is the regression for the customer whose
// prompt hook ran `facts promote` on every prompt: a promote that changed
// nothing still rewrote the target store, the manifest, and the README inside
// the brain write lock — an O(all branches + all proposals) hold for zero
// work. A no-op promote must leave every brain file untouched.
func TestFactsPromoteNoOpSkipsAllWrites(t *testing.T) {
	f, _ := promoteFixtureWithSourceFacts(t)

	out, err := execute(t, NewRootCommand(f.opts), "facts", "promote", "--from", "feature", "--strategy", "keep-both")
	if err != nil {
		t.Fatalf("first promote failed: %v\n%s", err, out)
	}

	before := brainFileMtimes(t, f.storage.BrainDir)
	past := ageBrainFiles(t, before)

	out, err = execute(t, NewRootCommand(f.opts), "facts", "promote", "--from", "feature", "--strategy", "keep-both")
	if err != nil {
		t.Fatalf("repeat promote failed: %v\n%s", err, out)
	}
	// The output contract is unchanged: a no-op still reports like today.
	if !strings.Contains(out, "promoted 0 memory(ies) from feature into main (keep-both); 0 conflict(s) queued for review") {
		t.Fatalf("no-op promote summary changed:\n%s", out)
	}

	after := brainFileMtimes(t, f.storage.BrainDir)
	for path, mtime := range after {
		if _, existed := before[path]; !existed {
			t.Errorf("no-op promote created %s", path)
			continue
		}
		if !mtime.Equal(past) {
			t.Errorf("no-op promote rewrote %s", path)
		}
	}
	for path := range before {
		if _, kept := after[path]; !kept {
			t.Errorf("no-op promote deleted %s", path)
		}
	}
}

// TestFactsPromoteProvenanceUnionStillWrites pins the trap in the fast exit:
// promoted==0 is not enough evidence of a no-op, because promoting an
// identical fact with new provenance mutates the target without counting as
// promoted. That promote must still persist.
func TestFactsPromoteProvenanceUnionStillWrites(t *testing.T) {
	f, fact := promoteFixtureWithSourceFacts(t)
	fact.Provenance = []factAnchor{{SessionID: "s1"}}
	f.writeFacts(t, "feature", []factRecord{fact})

	out, err := execute(t, NewRootCommand(f.opts), "facts", "promote", "--from", "feature", "--strategy", "keep-both")
	if err != nil {
		t.Fatalf("first promote failed: %v\n%s", err, out)
	}

	fact.Provenance = []factAnchor{{SessionID: "s1"}, {SessionID: "s2"}}
	f.writeFacts(t, "feature", []factRecord{fact})

	out, err = execute(t, NewRootCommand(f.opts), "facts", "promote", "--from", "feature", "--strategy", "keep-both")
	if err != nil {
		t.Fatalf("provenance-union promote failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "promoted 0 memory(ies)") {
		t.Fatalf("provenance union should not count as promoted:\n%s", out)
	}

	facts, loadErr := factstore.Load(f.storage.BrainDir, "main")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	i := indexOfFact(facts, fact.ID)
	if i < 0 {
		t.Fatalf("promoted fact missing from main: %+v", facts)
	}
	if len(facts[i].Provenance) != 2 {
		t.Fatalf("provenance union was not persisted, got %+v", facts[i].Provenance)
	}
}
