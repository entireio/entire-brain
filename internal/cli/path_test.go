package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPathPrintsBrainDirForRepoURL(t *testing.T) {
	configDir := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "data")
	stateDir := filepath.Join(t.TempDir(), "state")
	cacheDir := filepath.Join(t.TempDir(), "cache")

	cmd := NewRootCommand(Options{
		Version: "test-version",
		Env: EntireEnv{
			PluginConfigDir: configDir,
			PluginDataDir:   dataDir,
			PluginStateDir:  stateDir,
			PluginCacheDir:  cacheDir,
		},
		Runner: &fakeCommandRunner{responses: map[string]fakeCommandResponse{}},
	})

	out, err := execute(t, cmd, "path", "https://github.com/entireio/cli.git")
	if err != nil {
		t.Fatalf("path: %v\n%s", err, out)
	}
	want := filepath.Join(dataDir, repoStoreDirName, "gh", "entireio", "cli") + "\n"
	if out != want {
		t.Fatalf("path output = %q, want %q", out, want)
	}
}

func TestPathPrintsBrainDirForOneLetterSCPHostAlias(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	cmd := NewRootCommand(Options{
		Version: "test-version",
		Env: EntireEnv{
			PluginConfigDir: t.TempDir(),
			PluginDataDir:   dataDir,
			PluginStateDir:  t.TempDir(),
			PluginCacheDir:  t.TempDir(),
		},
		Runner: &fakeCommandRunner{responses: map[string]fakeCommandResponse{}},
	})

	out, err := execute(t, cmd, "path", "g:org/repo.git")
	if err != nil {
		t.Fatalf("path: %v\n%s", err, out)
	}
	wantSuffix := filepath.Join("org", "repo") + "\n"
	if !strings.HasSuffix(out, wantSuffix) {
		t.Fatalf("path output = %q, want suffix %q", out, wantSuffix)
	}
}

func TestPathPrefersExistingLocalPathThatLooksLikeRepoRemote(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "github.com", "entireio", "cli")
	if err := os.MkdirAll(repoDir, 0o700); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	dataDir := filepath.Join(t.TempDir(), "data")
	brainDir := filepath.Join(dataDir, repoStoreDirName, "gh", "local", "repo")
	if err := os.MkdirAll(brainDir, 0o700); err != nil {
		t.Fatalf("mkdir brain: %v", err)
	}
	if err := os.WriteFile(filepath.Join(brainDir, exportManifestFileName), []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	cmd := NewRootCommand(Options{
		Version: "test-version",
		Env: EntireEnv{
			PluginConfigDir: t.TempDir(),
			PluginDataDir:   dataDir,
			PluginStateDir:  t.TempDir(),
			PluginCacheDir:  t.TempDir(),
		},
		Runner: &fakeCommandRunner{responses: map[string]fakeCommandResponse{
			fakeCommandKey("git", "rev-parse", "--show-toplevel"): {stdout: repoDir + "\n"},
			fakeCommandKey("git", "remote", "get-url", "origin"):  {stdout: "git@github.com:local/repo.git\n"},
		}},
	})

	out, err := execute(t, cmd, "path", repoDir)
	if err != nil {
		t.Fatalf("path: %v\n%s", err, out)
	}
	want := filepath.Join(dataDir, repoStoreDirName, "gh", "local", "repo") + "\n"
	if out != want {
		t.Fatalf("path output = %q, want %q", out, want)
	}
}

func TestPathRejectsMissingWindowsDrivePathAsRemote(t *testing.T) {
	cmd := NewRootCommand(Options{
		Version: "test-version",
		Env: EntireEnv{
			PluginConfigDir: t.TempDir(),
			PluginDataDir:   t.TempDir(),
			PluginStateDir:  t.TempDir(),
			PluginCacheDir:  t.TempDir(),
		},
		Runner: &fakeCommandRunner{responses: map[string]fakeCommandResponse{}},
	})
	out, err := execute(t, cmd, "path", `C:\missing\repo`)
	if err == nil {
		t.Fatalf("path succeeded for missing Windows drive path:\n%s", out)
	}
	if !strings.Contains(err.Error(), "neither an existing path") && !strings.Contains(err.Error(), "stat target path") {
		t.Fatalf("path err = %v", err)
	}
	out, err = execute(t, cmd, "path", `C:missing`)
	if err == nil {
		t.Fatalf("path succeeded for drive-relative Windows path:\n%s", out)
	}
	if !strings.Contains(err.Error(), "neither an existing path") && !strings.Contains(err.Error(), "stat target path") {
		t.Fatalf("path err = %v", err)
	}
}

func TestResolveLocalTargetRepoDirPreservesExplicitSymlinkSpelling(t *testing.T) {
	parent := t.TempDir()
	repoDir := filepath.Join(parent, "repo")
	if err := os.Mkdir(repoDir, 0o700); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	alias := filepath.Join(parent, "repo-alias")
	if err := os.Symlink(repoDir, alias); err != nil {
		t.Skipf("directory symlinks are unavailable on this platform: %v", err)
	}

	resolved, local, err := resolveLocalTargetRepoDir(context.Background(), nil, alias)
	if err != nil {
		t.Fatalf("resolve symlink target: %v", err)
	}
	if !local {
		t.Fatal("symlink target was not local")
	}
	want := filepath.Clean(alias)
	if resolved != want {
		t.Fatalf("resolved lexical root = %q, want %q", resolved, want)
	}
}

func TestPathExplicitAliasRecoversLegacyLocalBrain(t *testing.T) {
	parent := t.TempDir()
	repoDir := filepath.Join(parent, "repo")
	if err := os.Mkdir(repoDir, 0o700); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	alias := filepath.Join(parent, "repo-alias")
	if err := os.Symlink(repoDir, alias); err != nil {
		t.Skipf("directory symlinks are unavailable on this platform: %v", err)
	}
	env := EntireEnv{
		PluginConfigDir: t.TempDir(),
		PluginDataDir:   t.TempDir(),
		PluginStateDir:  t.TempDir(),
		PluginCacheDir:  t.TempDir(),
	}
	dirs, err := resolvePluginDirs(env)
	if err != nil {
		t.Fatalf("plugin dirs: %v", err)
	}
	legacyKey := filepath.ToSlash(filepath.Join("local", localRepoKey(alias)))
	legacy := repoStorageForKey(dirs, legacyKey)
	if err := os.MkdirAll(legacy.BrainDir, 0o700); err != nil {
		t.Fatalf("create legacy brain: %v", err)
	}
	if err := os.WriteFile(filepath.Join(legacy.BrainDir, exportManifestFileName), []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write legacy manifest: %v", err)
	}
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "rev-parse", "--show-toplevel"): {stdout: repoDir + "\n"},
		fakeCommandKey("git", "remote", "get-url", "origin"):  {err: os.ErrNotExist},
	}}
	cmd := NewRootCommand(Options{Version: "test", Env: env, Runner: runner})

	out, err := execute(t, cmd, "path", alias)
	if err != nil {
		t.Fatalf("path alias: %v\n%s", err, out)
	}
	if out != legacy.BrainDir+"\n" {
		t.Fatalf("path alias output = %q, want legacy brain %q", out, legacy.BrainDir+"\n")
	}
	for _, call := range runner.calls {
		if call.name != "git" {
			t.Fatalf("legacy brain should avoid export, calls: %+v", runner.calls)
		}
	}
}

func TestResolveLocalTargetRepoDirPreservesLexicalAncestorAgainstGitPhysicalRoot(t *testing.T) {
	parent := t.TempDir()
	realParent := filepath.Join(parent, "real-parent")
	realRepo := filepath.Join(realParent, "repo")
	subdir := filepath.Join(realRepo, "subdir")
	if err := os.MkdirAll(subdir, 0o700); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	aliasParent := filepath.Join(parent, "alias-parent")
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Skipf("directory symlinks are unavailable on this platform: %v", err)
	}
	logicalRepo := filepath.Join(aliasParent, "repo")
	logicalSubdir := filepath.Join(logicalRepo, "subdir")
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "rev-parse", "--show-toplevel"): {stdout: realRepo + "\n"},
	}}

	resolved, local, err := resolveLocalTargetRepoDir(context.Background(), runner, logicalSubdir)
	if err != nil {
		t.Fatalf("resolve logical subdir: %v", err)
	}
	if !local {
		t.Fatal("logical subdir was not local")
	}
	if resolved != logicalRepo {
		t.Fatalf("resolved root = %q, want lexical root %q", resolved, logicalRepo)
	}
}

func TestPathUsesExistingLocalBrainWithoutExport(t *testing.T) {
	repoDir := t.TempDir()
	subDir := filepath.Join(repoDir, "internal", "pkg")
	if err := os.MkdirAll(subDir, 0o700); err != nil {
		t.Fatalf("create subdir: %v", err)
	}
	filePath := filepath.Join(subDir, "file.go")
	if err := os.WriteFile(filePath, []byte("package pkg\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	dataDir := filepath.Join(t.TempDir(), "data")
	brainDir := filepath.Join(dataDir, repoStoreDirName, "gh", "entireio", "cli")
	if err := os.MkdirAll(brainDir, 0o700); err != nil {
		t.Fatalf("create brain dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(brainDir, exportManifestFileName), []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "rev-parse", "--show-toplevel"): {
			stdout: repoDir + "\n",
		},
		fakeCommandKey("git", "remote", "get-url", "origin"): {
			stdout: "git@github.com:entireio/cli.git\n",
		},
	}}
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

	out, err := execute(t, cmd, "path", filePath)
	if err != nil {
		t.Fatalf("path: %v\n%s", err, out)
	}
	if out != brainDir+"\n" {
		t.Fatalf("path output = %q, want %q", out, brainDir+"\n")
	}
	for _, call := range runner.calls {
		if call.name != "git" {
			t.Fatalf("path should not export when manifest exists, calls: %+v", runner.calls)
		}
	}
}

// pathBuildFixtureRunner scripts the git calls `path` legitimately makes to
// identify a repository, and NOTHING else. Any brain-building subprocess --
// the worktree probe seed runs, the `entire` checkpoint calls export runs --
// is therefore an unexpected command and fails the run loudly, which is what
// makes the "path builds nothing" tests below fail without the fix.
func pathBuildFixtureRunner(repoDir string) *fakeCommandRunner {
	return &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "rev-parse", "--show-toplevel"): {stdout: repoDir + "\n"},
		fakeCommandKey("git", "remote", "get-url", "origin"):  {stdout: "https://github.com/entireio/cli.git\n"},
	}}
}

func TestPathBuildsNothingForRepoWithoutBrain(t *testing.T) {
	repoDir := t.TempDir()
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	stateDir := filepath.Join(root, "state")
	cacheDir := filepath.Join(root, "cache")
	brainDir := filepath.Join(dataDir, repoStoreDirName, "gh", "entireio", "cli")

	runner := pathBuildFixtureRunner(repoDir)
	cmd := NewRootCommand(Options{
		Version: "test-version",
		Env: EntireEnv{
			PluginConfigDir: filepath.Join(root, "config"),
			PluginDataDir:   dataDir,
			PluginStateDir:  stateDir,
			PluginCacheDir:  cacheDir,
		},
		Runner: runner,
		Now:    func() time.Time { return time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC) },
	})

	out, err := execute(t, cmd, "path", repoDir)
	if err != nil {
		t.Fatalf("path: %v\n%s", err, out)
	}
	if out != brainDir+"\n" {
		t.Fatalf("path output = %q, want %q", out, brainDir+"\n")
	}
	// The getter contract: asking where the brain lives must leave the store
	// exactly as it found it. Before the fix this single call wrote seed
	// markdown, a docs FTS index, a pattern corpus, a manifest and a lock dir.
	for _, dir := range []string{dataDir, stateDir, cacheDir} {
		if _, err := os.Lstat(dir); !os.IsNotExist(err) {
			t.Fatalf("path created %s (lstat err = %v); path must not build a brain", dir, err)
		}
	}
	for _, call := range runner.calls {
		if call.name != "git" {
			t.Fatalf("path ran a non-git command, so it was building: %+v", runner.calls)
		}
	}
}

func TestPathRefusesDirectoryGitCannotAnswerFor(t *testing.T) {
	plainDir := t.TempDir()
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")

	// No scripted responses at all: every git call fails, exactly as it does in
	// a directory that is not inside a work tree.
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	cmd := NewRootCommand(Options{
		Version: "test-version",
		Env: EntireEnv{
			PluginConfigDir: filepath.Join(root, "config"),
			PluginDataDir:   dataDir,
			PluginStateDir:  filepath.Join(root, "state"),
			PluginCacheDir:  filepath.Join(root, "cache"),
		},
		Runner: runner,
	})

	out, err := execute(t, cmd, "path", plainDir)
	if err == nil {
		t.Fatalf("path succeeded in a non-repository:\n%s", out)
	}
	// Before the fix this failed from INSIDE a build nobody asked for, with a
	// raw `fatal: not a git repository` leaked from a seed subprocess.
	if !strings.Contains(err.Error(), "not a git repository: "+plainDir) {
		t.Fatalf("path err = %v, want a clear not-a-repository refusal naming %s", err, plainDir)
	}
	if strings.Contains(err.Error(), "seed refresh") || strings.Contains(err.Error(), "worktree") {
		t.Fatalf("path leaked a build failure instead of refusing up front: %v", err)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("path printed something for a non-repository: %q", out)
	}
	if _, err := os.Lstat(dataDir); !os.IsNotExist(err) {
		t.Fatalf("path created %s while refusing (lstat err = %v)", dataDir, err)
	}
}

func TestPathPrintsStoredBrainWhenWorktreeLostItsGitDir(t *testing.T) {
	plainDir := t.TempDir()
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	env := EntireEnv{
		PluginConfigDir: filepath.Join(root, "config"),
		PluginDataDir:   dataDir,
		PluginStateDir:  filepath.Join(root, "state"),
		PluginCacheDir:  filepath.Join(root, "cache"),
	}
	dirs, err := resolvePluginDirs(env)
	if err != nil {
		t.Fatalf("plugin dirs: %v", err)
	}
	storage := repoStorageForKey(dirs, filepath.ToSlash(filepath.Join("local", localRepoKey(plainDir))))
	if err := os.MkdirAll(storage.BrainDir, 0o700); err != nil {
		t.Fatalf("create stored brain: %v", err)
	}
	if err := os.WriteFile(filepath.Join(storage.BrainDir, exportManifestFileName), []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	cmd := NewRootCommand(Options{
		Version: "test-version",
		Env:     env,
		Runner:  &fakeCommandRunner{responses: map[string]fakeCommandResponse{}},
	})

	out, err := execute(t, cmd, "path", plainDir)
	if err != nil {
		t.Fatalf("path: %v\n%s", err, out)
	}
	if out != storage.BrainDir+"\n" {
		t.Fatalf("path output = %q, want stored brain %q", out, storage.BrainDir+"\n")
	}
}

func TestPathEnsureBuildsMissingBrain(t *testing.T) {
	repoDir := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "data")
	stateDir := filepath.Join(t.TempDir(), "state")
	brainDir := filepath.Join(dataDir, repoStoreDirName, "gh", "entireio", "cli")

	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "rev-parse", "--show-toplevel"): {
			stdout: repoDir + "\n",
		},
		fakeCommandKey("git", "remote", "get-url", "origin"): {
			stdout: "https://github.com/entireio/cli.git\n",
		},
		fakeCommandKey("git", "status", "--porcelain", "--untracked-files=all"): {},
		fakeCommandKey("entire-test", "checkpoint", "explain", "--json", "--search-all"): {
			stdout: `[{"checkpoint_id":"aaa111aaa111","date":"2026-01-01T00:00:00Z","is_logs_only":true}]`,
		},
		fakeCommandKey("entire-test", "checkpoint", "explain", "--json", "aaa111aaa111"): {
			stdout: `{
  "checkpoint_id": "aaa111aaa111",
  "checkpoints_count": 1,
  "session_count": 1,
  "sessions": [
    {
      "index": 0,
      "session_id": "session-one",
      "agent": "Codex",
      "created_at": "2026-01-01T00:00:00Z"
    }
  ]
}`,
		},
		fakeCommandKey("entire-test", "checkpoint", "explain", "--transcript", "--session-index", "0", "aaa111aaa111"): {
			stdout: "{\"type\":\"message\",\"text\":\"session one\"}\n",
		},
	}}
	cmd := NewRootCommand(Options{
		Version: "test-version",
		Env: EntireEnv{
			PluginConfigDir: filepath.Join(t.TempDir(), "config"),
			PluginDataDir:   dataDir,
			PluginStateDir:  stateDir,
			PluginCacheDir:  filepath.Join(t.TempDir(), "cache"),
		},
		Runner: runner,
		Now: func() time.Time {
			return time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
		},
	})

	out, err := execute(t, cmd, "path", "--ensure", "--entire-binary", "entire-test", repoDir)
	if err != nil {
		t.Fatalf("path --ensure: %v\n%s", err, out)
	}
	if out != brainDir+"\n" {
		t.Fatalf("path output = %q, want %q", out, brainDir+"\n")
	}
	if _, err := os.Stat(filepath.Join(brainDir, exportManifestFileName)); err != nil {
		t.Fatalf("path --ensure did not export manifest: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, repoStoreDirName, "gh", "entireio", "cli", brainHeadFileName)); err != nil {
		t.Fatalf("path --ensure did not write cursor: %v", err)
	}

	var exportedTranscript bool
	for _, call := range runner.calls {
		if call.name == "entire-test" && len(call.args) > 2 && call.args[0] == "checkpoint" && call.args[1] == "explain" && call.args[2] == "--transcript" {
			exportedTranscript = true
		}
	}
	if !exportedTranscript {
		t.Fatalf("path --ensure did not build the missing brain, calls: %+v", runner.calls)
	}
}

func TestPathEnsureRefusesDirectoryGitCannotAnswerFor(t *testing.T) {
	plainDir := t.TempDir()
	root := t.TempDir()
	cmd := NewRootCommand(Options{
		Version: "test-version",
		Env: EntireEnv{
			PluginConfigDir: filepath.Join(root, "config"),
			PluginDataDir:   filepath.Join(root, "data"),
			PluginStateDir:  filepath.Join(root, "state"),
			PluginCacheDir:  filepath.Join(root, "cache"),
		},
		Runner: &fakeCommandRunner{responses: map[string]fakeCommandResponse{}},
	})

	out, err := execute(t, cmd, "path", "--ensure", plainDir)
	if err == nil {
		t.Fatalf("path --ensure succeeded in a non-repository:\n%s", out)
	}
	if !strings.Contains(err.Error(), "not a git repository: "+plainDir) {
		t.Fatalf("path --ensure err = %v, want a clear not-a-repository refusal", err)
	}
}
