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
	addRefreshSemanticFixture(runner, repoDir)
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

	out, err := execute(t, cmd, "refresh", "--entire-binary", "entire-test")
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	for _, want := range []string{
		"refresh: export sessions",
		"refresh: export sessions: unavailable, using seed baseline done",
		"refresh: seed baseline: 3 documents, 2 entrypoints, 4 commands done",
		"refresh: history index: 0 records, 0 decisions, 0 tool calls done",
		"refresh: semantic index: 1 symbol, 1 relation, 1 file done",
		"refreshed brain:",
		"brain: ",
		"sources: seed=true",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("refresh output missing %q:\n%s", want, out)
		}
	}
	brainDir := filepath.Join(dataDir, repoStoreDirName, "gh", "example", "repo")
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
	if manifest.Sources.History == nil {
		t.Fatalf("refresh did not create history index source: %+v", manifest.Sources)
	}
}

func TestRefreshHelpShowsSimplifiedFlags(t *testing.T) {
	cmd := NewRootCommand(Options{Version: "test-version"})
	out, err := execute(t, cmd, "refresh", "--help")
	if err != nil {
		t.Fatalf("refresh help: %v", err)
	}
	for _, want := range []string{"--agent", "--force", "--output"} {
		if !strings.Contains(out, want) {
			t.Fatalf("refresh help missing %q:\n%s", want, out)
		}
	}
	for _, hidden := range []string{"--history-index", "--force-seed", "--semantic", "--sem-binary"} {
		if strings.Contains(out, hidden) {
			t.Fatalf("refresh help exposed hidden flag %q:\n%s", hidden, out)
		}
	}
}

func TestRefreshOutputRequiresEmptyDirectoryUnlessForced(t *testing.T) {
	repoDir := seedFixtureRepo(t)
	outputDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outputDir, "existing.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatalf("write existing output file: %v", err)
	}
	runner := seedFixtureRunner(repoDir)
	cmd := NewRootCommand(Options{
		Version: "test-version",
		Env: EntireEnv{
			RepoRoot:        repoDir,
			PluginConfigDir: filepath.Join(t.TempDir(), "config"),
			PluginDataDir:   filepath.Join(t.TempDir(), "data"),
			PluginStateDir:  filepath.Join(t.TempDir(), "state"),
			PluginCacheDir:  filepath.Join(t.TempDir(), "cache"),
		},
		Runner: runner,
		Now:    time.Now,
	})
	out, err := execute(t, cmd, "refresh", "--output", outputDir)
	if err == nil || !strings.Contains(err.Error(), "output directory is not empty") {
		t.Fatalf("refresh --output err = %v\n%s", err, out)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("refresh touched runner before output validation: %+v", runner.calls)
	}
}

func TestHistoryIndexCurrentUsesSessionFingerprint(t *testing.T) {
	brainDir := t.TempDir()
	sessionSource := &sessionSourceManifest{
		GeneratedAt:        time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		TranscriptMode:     "compact",
		Scope:              exportScopeAll,
		LatestCheckpointID: "aaa111",
		Sessions: []exportSession{{
			SessionID:        "session-one",
			LatestCheckpoint: "aaa111",
			Branch:           "main",
			TranscriptPath:   "sessions/main/session-one.jsonl",
		}},
	}
	if err := os.MkdirAll(filepath.Join(brainDir, historyDirName), 0o700); err != nil {
		t.Fatalf("create history dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(brainDir, filepath.FromSlash(historyIndexPath)), []byte(`{"records":[]}`), 0o600); err != nil {
		t.Fatalf("write history index: %v", err)
	}
	manifest := &exportManifest{Sources: &brainSources{
		Sessions: sessionSource,
		History: &historySourceManifest{
			IndexPath:           historyIndexPath,
			SessionsFingerprint: sessionSourceFingerprint(sessionSource),
		},
	}}
	if !historyIndexCurrent(brainDir, manifest) {
		t.Fatalf("history index should be current for matching session fingerprint")
	}
	sessionSource.GeneratedAt = time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	if !historyIndexCurrent(brainDir, manifest) {
		t.Fatalf("history index should ignore session source timestamp-only changes")
	}
	sessionSource.Sessions[0].LatestCheckpoint = "bbb222"
	if historyIndexCurrent(brainDir, manifest) {
		t.Fatalf("history index should be stale when exported sessions change")
	}
}

func addRefreshSemanticFixture(runner *fakeCommandRunner, repoDir string) {
	runner.responses[fakeCommandKey("git", "rev-parse", "HEAD")] = fakeCommandResponse{stdout: "aaa111\n"}
	runner.responses[fakeCommandKey("git", "rev-parse", "HEAD^{tree}")] = fakeCommandResponse{stdout: "tree111\n"}
	runner.responses[fakeCommandKey("git", "branch", "--show-current")] = fakeCommandResponse{stdout: "main\n"}
	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{stdout: ""}
	runner.responses[fakeCommandKey("git", "status", "--porcelain", "--untracked-files=all")] = fakeCommandResponse{stdout: ""}
	runner.responses[fakeCommandKey("git", "diff", "--binary", "HEAD")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "diff", "--cached", "--binary", "HEAD")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("entire", "sem", "doctor", "--json")] = fakeCommandResponse{stdout: `{"no_egress":true}`}
	runner.responses[fakeCommandKey("entire", "sem", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network")] = fakeCommandResponse{stdout: semanticFixtureSnapshot("1.0")}
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

func TestRefreshSemanticWorktreePassesWorktreeToIndex(t *testing.T) {
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
	if _, err := execute(t, cmd, "refresh", "--semantic", "--semantic-worktree"); err != nil {
		t.Fatalf("refresh --semantic --semantic-worktree: %v", err)
	}
	if !fakeRunnerCalled(runner, "entire", "sem", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network", "--worktree") {
		t.Fatalf("semantic snapshot was not called with --worktree: %+v", runner.calls)
	}
}

func TestRefreshSeedWorktreeDoesNotPassSemanticWorktree(t *testing.T) {
	repoDir := seedFixtureRepo(t)
	dataDir := filepath.Join(t.TempDir(), "data")
	runner := seedFixtureRunner(repoDir)
	runner.responses[fakeCommandKey("git", "rev-parse", "HEAD")] = fakeCommandResponse{stdout: "aaa111\n"}
	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{stdout: ""}
	runner.responses[fakeCommandKey("git", "rev-parse", "HEAD^{tree}")] = fakeCommandResponse{stdout: "tree111\n"}
	runner.responses[fakeCommandKey("entire", "sem", "doctor", "--json")] = fakeCommandResponse{stdout: `{"no_egress":true}`}
	runner.responses[fakeCommandKey("entire", "sem", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network")] = fakeCommandResponse{stdout: semanticFixtureSnapshot("1.0")}

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
	if !fakeRunnerCalled(runner, "entire", "sem", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network") {
		t.Fatalf("semantic snapshot was not called without --worktree: %+v", runner.calls)
	}
	if fakeRunnerCalled(runner, "entire", "sem", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network", "--worktree") {
		t.Fatalf("seed --worktree leaked into semantic snapshot: %+v", runner.calls)
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
	brainDir := filepath.Join(dataDir, repoStoreDirName, "gh", "example", "repo")
	if out != brainDir+"\n" {
		t.Fatalf("path output = %q, want %q", out, brainDir+"\n")
	}
	if _, err := os.Stat(filepath.Join(brainDir, seedDirName, "repo-overview.md")); err != nil {
		t.Fatalf("path did not create seed: %v", err)
	}
}
