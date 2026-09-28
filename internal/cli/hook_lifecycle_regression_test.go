package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHookSessionEndRecordsRecoveryHintAfterExportFailure(t *testing.T) {
	for _, branch := range []string{"", "captured-branch"} {
		t.Run("branch="+branch, func(t *testing.T) {
			opts, brainDir := privacyRegressionCommandFixture(t, time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC))

			manifest, err := loadBrainManifest(brainDir)
			if err != nil {
				t.Fatal(err)
			}
			manifest.RepoKey = "gh/acme/privacy-regression"
			data, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(brainDir, exportManifestFileName), data, 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv(memoryWorkerOriginEnv, "")
			old := memoryWorkerLaunch
			launches := 0
			memoryWorkerLaunch = func(repo string) error {
				launches++
				if repo != opts.Env.RepoRoot {
					t.Errorf("launch repo=%q", repo)
				}
				return errors.New("test launch unavailable")
			}
			t.Cleanup(func() { memoryWorkerLaunch = old })
			args := []string{"hook", "session-end", "--session", "ended-session", "--agent", "none"}
			if branch != "" {
				args = append(args, "--branch", branch)
			}
			stdout, stderr, err := executeSplit(t, NewRootCommand(opts), args...)
			if err != nil || stdout != "" {
				t.Fatalf("hook must succeed silently on stdout: %q err=%v stderr=%s", stdout, err, stderr)
			}
			if launches != 1 || !strings.Contains(stderr, "worker launch failed") || !strings.Contains(stderr, "incremental export failed") {
				t.Fatalf("recovery routing: launches=%d stderr=%s", launches, stderr)
			}
			hints := loadMemoryHints(brainDir)
			wantBranch := branch
			if wantBranch == "" {
				wantBranch = "main"
			}
			if len(hints) != 1 || hints[0].SessionID != "ended-session" || hints[0].Branch != wantBranch || hints[0].LastEvent != "session_end" {
				t.Fatalf("durable recovery hint=%+v", hints)
			}
		})
	}
}

func TestHookSessionEndWorkerOriginDoesNotRecurse(t *testing.T) {
	opts, brainDir := privacyRegressionCommandFixture(t, time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC))
	t.Setenv(memoryWorkerOriginEnv, "1")
	old := memoryWorkerLaunch
	memoryWorkerLaunch = func(string) error { t.Error("worker-origin hook recursively launched worker"); return nil }
	t.Cleanup(func() { memoryWorkerLaunch = old })
	before := privacyTreeDigest(t, brainDir)
	stdout, stderr, err := executeSplit(t, NewRootCommand(opts), "hook", "session-end", "--session", "ended-session")
	if err != nil || stdout != "" || stderr != "" {
		t.Fatalf("worker-origin hook: stdout=%s stderr=%s err=%v", stdout, stderr, err)
	}
	if privacyTreeDigest(t, brainDir) != before {
		t.Fatal("worker-origin hook modified state")
	}
}
