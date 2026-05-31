package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestRefreshSeedsWhenExportFindsNoSessions(t *testing.T) {
	repoDir := seedFixtureRepo(t)
	dataDir := filepath.Join(t.TempDir(), "data")
	stateDir := filepath.Join(t.TempDir(), "state")
	runner := seedFixtureRunner(repoDir)
	runner.responses[fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef)] = fakeCommandResponse{err: os.ErrNotExist}
	runner.responses[fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1OriginRef)] = fakeCommandResponse{err: os.ErrNotExist}

	cmd := NewRootCommand(Options{
		Version: "test-version",
		Env: EntireEnv{
			RepoRoot:        repoDir,
			PluginConfigDir: filepath.Join(t.TempDir(), "config"),
			PluginDataDir:   dataDir,
			PluginStateDir:  stateDir,
			PluginCacheDir:  filepath.Join(t.TempDir(), "cache"),
		},
		Runner: runner,
		Now: func() time.Time {
			return time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
		},
	})

	if _, err := execute(t, cmd, "refresh", "--entire-binary", "entire-test"); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	brainDir := filepath.Join(dataDir, brainDirName, "gh", "example", "repo")
	var manifest exportManifest
	data, err := os.ReadFile(filepath.Join(brainDir, exportManifestFileName))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if manifest.Sources == nil || manifest.Sources.Seed == nil {
		t.Fatalf("refresh did not create seed source: %+v", manifest)
	}
}

func TestRefreshAllBranchesRequiresSemanticBeforeMutation(t *testing.T) {
	runner := &fakeCommandRunner{
		responses: map[string]fakeCommandResponse{
			fakeCommandKey("git", "rev-parse", "--show-toplevel"): {err: errors.New("must not resolve repo")},
		},
	}
	cmd := &cobra.Command{Use: "refresh"}
	err := runRefresh(context.Background(), cmd, Options{
		Env: EntireEnv{
			RepoRoot:        t.TempDir(),
			PluginConfigDir: filepath.Join(t.TempDir(), "config"),
			PluginDataDir:   filepath.Join(t.TempDir(), "data"),
			PluginStateDir:  filepath.Join(t.TempDir(), "state"),
			PluginCacheDir:  filepath.Join(t.TempDir(), "cache"),
		},
		Runner: runner,
		Now:    time.Now,
	}, refreshCommandOptions{allBranches: true})
	if err == nil || !strings.Contains(err.Error(), "--all-branches requires --semantic") {
		t.Fatalf("refresh err = %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("refresh mutated before validation: %+v", runner.calls)
	}
}

func TestRefreshSemanticPassesWorktreeToIndex(t *testing.T) {
	repoDir := seedFixtureRepo(t)
	dataDir := filepath.Join(t.TempDir(), "data")
	runner := seedFixtureRunner(repoDir)
	runner.responses[fakeCommandKey("git", "rev-parse", "HEAD")] = fakeCommandResponse{stdout: "aaa111\n"}
	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{stdout: " M internal/cli/semantic.go\n"}
	runner.responses[fakeCommandKey("git", "diff", "--binary", "HEAD")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "diff", "--cached", "--binary", "HEAD")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "rev-parse", "HEAD^{tree}")] = fakeCommandResponse{stdout: "tree111\n"}
	runner.responses[fakeCommandKey("entire", "sem", "doctor", "--json")] = fakeCommandResponse{stdout: `{"no_egress":true}`}
	runner.responses[fakeCommandKey("entire", "sem", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network", "--worktree")] = fakeCommandResponse{stdout: semanticFixtureSnapshot("1.0")}

	cmd := NewRootCommand(Options{
		Version: "test-version",
		Env: EntireEnv{
			RepoRoot:        repoDir,
			PluginConfigDir: filepath.Join(t.TempDir(), "config"),
			PluginDataDir:   dataDir,
			PluginStateDir:  filepath.Join(t.TempDir(), "state"),
			PluginCacheDir:  filepath.Join(t.TempDir(), "cache"),
		},
		Runner: runner,
		Now:    time.Now,
	})
	if _, err := execute(t, cmd, "refresh", "--semantic", "--worktree"); err != nil {
		t.Fatalf("refresh --semantic --worktree: %v", err)
	}
	if !fakeRunnerCalled(runner, "entire", "sem", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network", "--worktree") {
		t.Fatalf("semantic snapshot was not called with --worktree: %+v", runner.calls)
	}
}

func TestPathMaterializesSeedWhenExportUnavailable(t *testing.T) {
	repoDir := seedFixtureRepo(t)
	dataDir := filepath.Join(t.TempDir(), "data")
	runner := seedFixtureRunner(repoDir)
	runner.responses[fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef)] = fakeCommandResponse{err: os.ErrNotExist}
	runner.responses[fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1OriginRef)] = fakeCommandResponse{err: os.ErrNotExist}

	cmd := NewRootCommand(Options{
		Version: "test-version",
		Env: EntireEnv{
			PluginConfigDir: filepath.Join(t.TempDir(), "config"),
			PluginDataDir:   dataDir,
			PluginStateDir:  filepath.Join(t.TempDir(), "state"),
			PluginCacheDir:  filepath.Join(t.TempDir(), "cache"),
		},
		Runner: runner,
	})
	out, err := execute(t, cmd, "path", "--entire-binary", "entire-test", repoDir)
	if err != nil {
		t.Fatalf("path: %v\n%s", err, out)
	}
	brainDir := filepath.Join(dataDir, brainDirName, "gh", "example", "repo")
	if out != brainDir+"\n" {
		t.Fatalf("path output = %q, want %q", out, brainDir+"\n")
	}
	if _, err := os.Stat(filepath.Join(brainDir, seedDirName, "repo-overview.md")); err != nil {
		t.Fatalf("path did not create seed: %v", err)
	}
}
