package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/entireio/entire-brain/internal/cli/internal/semstore"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// gitSpawnCountingRunner counts Run invocations so the test can assert the
// all-branches refresh spawns a fixed number of subprocesses regardless of how
// many local branches exist.
type gitSpawnCountingRunner struct {
	ExecRunner
	mu   sync.Mutex
	runs []string
}

func (r *gitSpawnCountingRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
	r.mu.Lock()
	r.runs = append(r.runs, name+" "+strings.Join(args, " "))
	r.mu.Unlock()
	return r.ExecRunner.Run(ctx, dir, name, args...)
}

func (r *gitSpawnCountingRunner) reset() {
	r.mu.Lock()
	r.runs = nil
	r.mu.Unlock()
}

func TestSemanticRefreshAllBranchesSpawnsConstantGitProcesses(t *testing.T) {
	const sameHeadBranches = 60
	repoDir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-b", "main")
	git("config", "user.email", "test@example.com")
	git("config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(repoDir, "f.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "f.txt")
	git("commit", "-m", "one")
	for i := range sameHeadBranches {
		git("branch", fmt.Sprintf("same-%03d", i))
	}
	git("switch", "-c", "feature")
	if err := os.WriteFile(filepath.Join(repoDir, "f.txt"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("commit", "-am", "two")
	git("switch", "main")

	runner := &gitSpawnCountingRunner{}
	opts := Options{
		Version: "test", Runner: runner,
		Env: EntireEnv{RepoRoot: repoDir, PluginConfigDir: t.TempDir(), PluginDataDir: t.TempDir(), PluginStateDir: t.TempDir(), PluginCacheDir: t.TempDir()},
		Now: func() time.Time { return time.Now().UTC() },
	}
	ctx := context.Background()
	storage, err := repoStoragePaths(ctx, runner, opts.Env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(storage.BrainDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeBrainSemanticSource(storage.BrainDir, storage.Key, &semanticSourceManifest{StoreOwner: "graph"}); err != nil {
		t.Fatal(err)
	}

	runner.reset()
	start := time.Now()
	if err := runSemanticRefreshAllBranches(ctx, opts, refreshCommandOptions{semantic: true, allBranches: true}, repoDir); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	t.Logf("branches=%d subprocess Runs=%d elapsed=%s", sameHeadBranches+2, len(runner.runs), elapsed)

	// The refresh must not shell out once per branch: everything it needs per
	// branch travels in the single for-each-ref. The fixed overhead (storage
	// resolution, default-branch detection, for-each-ref) is well under this
	// bound; a per-branch spawn regression lands at 60+.
	if len(runner.runs) > 15 {
		t.Fatalf("subprocess count scales with branch count: %d Runs for %d branches:\n%s",
			len(runner.runs), sameHeadBranches+2, strings.Join(runner.runs, "\n"))
	}

	data, err := os.ReadFile(filepath.Join(storage.BrainDir, semstore.DirName, "overlays", "all-branches.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Branches []struct {
			Branch string `json:"branch"`
			State  string `json:"state"`
			Reason string `json:"reason"`
		} `json:"branches"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, b := range report.Branches {
		states[b.Branch] = b.State + "/" + b.Reason
	}
	if got := states["feature"]; got != "overlay_written/" {
		t.Fatalf("feature branch = %q, want overlay_written", got)
	}
	if got := states["same-000"]; got != "skipped/no_branch_delta" {
		t.Fatalf("same-head branch = %q, want skipped/no_branch_delta", got)
	}
	if got := states["main"]; got != "skipped/no_branch_delta" {
		t.Fatalf("default branch = %q, want skipped/no_branch_delta", got)
	}
}
