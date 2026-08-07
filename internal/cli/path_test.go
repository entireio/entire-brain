package cli

import (
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

func TestPathExportsExistingRepoWhenMissing(t *testing.T) {
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
		fakeCommandKey("entire-test", "checkpoint", "explain", "--json", "--limit", "10000"): {
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

	out, err := execute(t, cmd, "path", "--entire-binary", "entire-test", repoDir)
	if err != nil {
		t.Fatalf("path: %v\n%s", err, out)
	}
	if out != brainDir+"\n" {
		t.Fatalf("path output = %q, want %q", out, brainDir+"\n")
	}
	if strings.Contains(out, "exported") {
		t.Fatalf("path output should only contain the brain path:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(brainDir, exportManifestFileName)); err != nil {
		t.Fatalf("path did not export manifest: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, repoStoreDirName, "gh", "entireio", "cli", brainHeadFileName)); err != nil {
		t.Fatalf("path did not write cursor: %v", err)
	}

	var exportedTranscript bool
	for _, call := range runner.calls {
		if call.name == "entire-test" && len(call.args) > 2 && call.args[0] == "checkpoint" && call.args[1] == "explain" && call.args[2] == "--transcript" {
			exportedTranscript = true
		}
	}
	if !exportedTranscript {
		t.Fatalf("path did not export missing brain, calls: %+v", runner.calls)
	}
}
