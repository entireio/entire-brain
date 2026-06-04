package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ashtom/entire-brain/internal/config"
)

func TestEnvFromOS(t *testing.T) {
	t.Setenv(envCLIVersion, "cli-test")
	t.Setenv(envRepoRoot, "/tmp/repo")
	t.Setenv(envPluginConfigDir, "/tmp/config")
	t.Setenv(envPluginDataDir, "/tmp/data")
	t.Setenv(envPluginStateDir, "/tmp/state")
	t.Setenv(envPluginCacheDir, "/tmp/cache")

	env := EnvFromOS()
	if env.CLIVersion != "cli-test" {
		t.Fatalf("CLIVersion = %q", env.CLIVersion)
	}
	if env.RepoRoot != "/tmp/repo" {
		t.Fatalf("RepoRoot = %q", env.RepoRoot)
	}
	if env.PluginConfigDir != "/tmp/config" {
		t.Fatalf("PluginConfigDir = %q", env.PluginConfigDir)
	}
	if env.PluginDataDir != "/tmp/data" {
		t.Fatalf("PluginDataDir = %q", env.PluginDataDir)
	}
	if env.PluginStateDir != "/tmp/state" {
		t.Fatalf("PluginStateDir = %q", env.PluginStateDir)
	}
	if env.PluginCacheDir != "/tmp/cache" {
		t.Fatalf("PluginCacheDir = %q", env.PluginCacheDir)
	}
}

func TestResolvePluginDirsUsesXDGDefaultsUnderEntireRoot(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv(xdgConfigHome, filepath.Join(xdg, "config"))
	t.Setenv(xdgDataHome, filepath.Join(xdg, "data"))
	t.Setenv(xdgStateHome, filepath.Join(xdg, "state"))
	t.Setenv(xdgCacheHome, filepath.Join(xdg, "cache"))

	dirs, err := resolvePluginDirs(EntireEnv{})
	if err != nil {
		t.Fatalf("resolve plugin dirs: %v", err)
	}
	if dirs.Config != filepath.Join(xdg, "config", "entire") {
		t.Fatalf("Config = %q", dirs.Config)
	}
	if dirs.Data != filepath.Join(xdg, "data", "entire") {
		t.Fatalf("Data = %q", dirs.Data)
	}
	if dirs.State != filepath.Join(xdg, "state", "entire") {
		t.Fatalf("State = %q", dirs.State)
	}
	if dirs.Cache != filepath.Join(xdg, "cache", "entire") {
		t.Fatalf("Cache = %q", dirs.Cache)
	}
}

func TestRepoStoragePathsUseKnownOriginDomain(t *testing.T) {
	base := t.TempDir()
	env := EntireEnv{
		PluginConfigDir: filepath.Join(base, "entire-config"),
		PluginDataDir:   filepath.Join(base, "entire-data"),
		PluginStateDir:  filepath.Join(base, "entire-state"),
	}
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "remote", "get-url", "origin"): {
			stdout: "git@github.com:entireio/cli.git\n",
		},
	}}

	storage, err := repoStoragePaths(context.Background(), runner, env, "/repo/cli")
	if err != nil {
		t.Fatalf("repo storage paths: %v", err)
	}
	if storage.Key != "gh/entireio/cli" {
		t.Fatalf("key = %q", storage.Key)
	}
	if storage.BrainDir != filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "entireio", "cli") {
		t.Fatalf("brain dir = %q", storage.BrainDir)
	}
	if storage.HeadPath != filepath.Join(env.PluginStateDir, repoStoreDirName, "gh", "entireio", "cli", brainHeadFileName) {
		t.Fatalf("head path = %q", storage.HeadPath)
	}
}

func TestRepoStorageKeyStoresUnknownDomainSlug(t *testing.T) {
	configDir := t.TempDir()

	key, ok, err := repoKeyFromRemote(configDir, "ssh://git@git.example.test/acme/service.git")
	if err != nil {
		t.Fatalf("repo key from remote: %v", err)
	}
	if !ok {
		t.Fatal("repo key from remote returned ok=false")
	}
	parts := strings.Split(key, "/")
	if len(parts) != 3 || len(parts[0]) != 3 || parts[1] != "acme" || parts[2] != "service" {
		t.Fatalf("key = %q", key)
	}

	cfg, err := config.Load(configDir)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.DomainSlugs["git.example.test"] != parts[0] {
		t.Fatalf("stored domain slug = %q, want %q", cfg.DomainSlugs["git.example.test"], parts[0])
	}
}

func TestRepoStorageKeyParsesOneLetterSCPHostAlias(t *testing.T) {
	key, ok, err := repoKeyFromRemote(t.TempDir(), "g:org/repo.git")
	if err != nil {
		t.Fatalf("repo key from remote: %v", err)
	}
	if !ok {
		t.Fatal("repo key from one-letter SCP remote returned ok=false")
	}
	parts := strings.Split(key, "/")
	if len(parts) != 3 || parts[1] != "org" || parts[2] != "repo" {
		t.Fatalf("key = %q", key)
	}
}

func TestRepoStorageKeyTreatsWindowsDriveOriginAsLocal(t *testing.T) {
	repoDir := t.TempDir()
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "remote", "get-url", "origin"): {stdout: `C:\repos\upstream.git` + "\n"},
	}}
	key, err := repoStorageKey(context.Background(), runner, t.TempDir(), repoDir)
	if err != nil {
		t.Fatalf("repo storage key: %v", err)
	}
	if !strings.HasPrefix(key, "local/") {
		t.Fatalf("key = %q, want local fallback", key)
	}

	runner.responses[fakeCommandKey("git", "remote", "get-url", "origin")] = fakeCommandResponse{stdout: "C:upstream.git\n"}
	key, err = repoStorageKey(context.Background(), runner, t.TempDir(), repoDir)
	if err != nil {
		t.Fatalf("repo storage key: %v", err)
	}
	if !strings.HasPrefix(key, "local/") {
		t.Fatalf("key = %q, want local fallback", key)
	}
}
