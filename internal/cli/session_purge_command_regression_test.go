package cli

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSessionPurgeRootDryRunApplyAndRetry(t *testing.T) {
	opts, brainDir := privacyRegressionCommandFixture(t, time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC))
	cleanPath := filepath.Join(brainDir, "sessions/main/20260801T000000Z_clean.jsonl")
	cleanBefore, err := os.ReadFile(cleanPath)
	if err != nil {
		t.Fatal(err)
	}
	before := purgeCommandDataDigest(t, brainDir)
	for _, jsonOut := range []bool{false, true} {
		args := []string{"privacy", "purge", "secret-sess", "--dry-run"}
		if jsonOut {
			args = append(args, "--json")
		}
		out, err := execute(t, NewRootCommand(opts), args...)
		if err != nil {
			t.Fatal(err)
		}
		if jsonOut {
			var plan sessionPurgePlan
			if err := json.Unmarshal([]byte(out), &plan); err != nil {
				t.Fatal(err)
			}
			if !plan.DryRun || plan.SessionID != "secret-sess" || len(plan.Transcripts) != 1 || plan.Records == 0 {
				t.Fatalf("dry-run plan: %+v", plan)
			}
		} else if !strings.Contains(out, "purge (dry-run) would remove session secret-sess") || !strings.Contains(out, "20260802T000000Z_secret.jsonl") {
			t.Fatalf("dry-run text: %s", out)
		}
		if purgeCommandDataDigest(t, brainDir) != before {
			t.Fatal("dry-run changed persisted state")
		}
	}
	out, err := execute(t, NewRootCommand(opts), "privacy", "purge", "secret-sess", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var plan sessionPurgePlan
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.DryRun || plan.SessionID != "secret-sess" || len(plan.Transcripts) != 1 {
		t.Fatalf("apply plan: %+v", plan)
	}
	assertCanaryAbsent(t, brainDir)
	cleanAfter, err := os.ReadFile(cleanPath)
	if err != nil || string(cleanAfter) != string(cleanBefore) {
		t.Fatalf("unrelated transcript changed: %q err=%v", cleanAfter, err)
	}
	if _, ok := loadSessionTombstones(brainDir).Excluded["secret-sess"]; !ok {
		t.Fatal("purge did not persist tombstone")
	}
	out, err = execute(t, NewRootCommand(opts), "privacy", "purge", "secret-sess")
	if err != nil || !strings.Contains(out, "purged session secret-sess") || !strings.Contains(out, "0 derived index records") {
		t.Fatalf("idempotent retry: %s err=%v", out, err)
	}
}

func TestSessionPurgeRootRejectsInvalidStateWithoutWrites(t *testing.T) {
	for _, tc := range []struct {
		name, id string
		corrupt  bool
	}{{"blank", " ", false}, {"corrupt manifest", "secret-sess", true}} {
		t.Run(tc.name, func(t *testing.T) {
			opts, brainDir := privacyRegressionCommandFixture(t, time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC))
			if tc.corrupt {
				if err := os.WriteFile(filepath.Join(brainDir, "manifest.json"), []byte("broken"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := purgeCommandDataDigest(t, brainDir)
			stdout, _, err := executeSplit(t, NewRootCommand(opts), "privacy", "purge", tc.id, "--json")
			if err == nil || stdout != "" {
				t.Fatalf("invalid purge reported success: %q err=%v", stdout, err)
			}
			if purgeCommandDataDigest(t, brainDir) != before {
				t.Fatal("failed purge modified persisted state")
			}
		})
	}
}

// Operational lock files may be created even for dry runs. Every data artifact
// is hashed, including its path, so additions, deletions, and edits are detected.
func purgeCommandDataDigest(t *testing.T, root string) [32]byte {
	t.Helper()
	h := sha256.New()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == filepath.Join(root, brainLockDirName) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", filepath.ToSlash(rel), len(data))
		h.Write(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return digest
}
