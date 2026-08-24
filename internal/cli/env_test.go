package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
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
	// Data mirrors the host's plugin-data namespace so a standalone/dev run hits
	// the real brain store; config/state/cache stay at the bare XDG root (the host
	// leaves those env vars unset too).
	if dirs.Data != filepath.Join(xdg, "data", "entire", "plugins", "data", pluginDataName) {
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

func TestRepoStorageKeyUsesCanonicalEntireProxyPath(t *testing.T) {
	configDir := t.TempDir()
	tests := map[string]string{
		"entire://cluster.example/gh/entirehq/entire-api": "gh/entirehq/entire-api",
		"entire://cluster.example/et/project/repo":        "et/project/repo",
		"entire://cluster.example/gt/owner/repo":          "gt/owner/repo",
	}
	for remote, want := range tests {
		got, ok, err := repoKeyFromRemote(configDir, remote)
		if err != nil || !ok || got != want {
			t.Fatalf("repoKeyFromRemote(%q) = %q, %v, %v; want %q, true, nil", remote, got, ok, err, want)
		}
	}

	cfg, err := config.Load(configDir)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if len(cfg.DomainSlugs) != 0 {
		t.Fatalf("Entire proxy created domain slugs: %#v", cfg.DomainSlugs)
	}
}

func TestRepoStorageKeyTreatsEntireProxyAsOriginEquivalent(t *testing.T) {
	configDir := t.TempDir()
	originKey, originOK, originErr := repoKeyFromRemote(configDir, "https://github.com/entirehq/entire-api.git")
	proxyKey, proxyOK, proxyErr := repoKeyFromRemote(configDir, "entire://cluster.example/gh/entirehq/entire-api")
	if originErr != nil || proxyErr != nil || !originOK || !proxyOK || proxyKey != originKey {
		t.Fatalf("proxy key = %q, %v, %v; origin key = %q, %v, %v", proxyKey, proxyOK, proxyErr, originKey, originOK, originErr)
	}
}

func TestRepoStorageKeyRejectsHostlessEntireProxy(t *testing.T) {
	key, ok, err := repoKeyFromRemote(t.TempDir(), "entire://:443/gh/owner/repo")
	if err != nil || ok || key != "" {
		t.Fatalf("repoKeyFromRemote() = %q, %v, %v; want \"\", false, nil", key, ok, err)
	}
}

func TestRepoStorageKeyTreatsMalformedEntireProxyAsLocal(t *testing.T) {
	repoDir := t.TempDir()
	configDir := t.TempDir()
	want := filepath.ToSlash(filepath.Join("local", localRepoKey(repoDir)))
	for _, remote := range []string{
		"entire://cluster.example/gh/owner",
		"entire://cluster.example/gh",
	} {
		runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
			fakeCommandKey("git", "remote", "get-url", "origin"): {stdout: remote + "\n"},
		}}
		key, err := repoStorageKey(context.Background(), runner, configDir, repoDir)
		if err != nil {
			t.Fatalf("repo storage key for %q: %v", remote, err)
		}
		if key != want {
			t.Fatalf("repo storage key for %q = %q, want local fallback %q", remote, key, want)
		}
	}

	cfg, err := config.Load(configDir)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if len(cfg.DomainSlugs) != 0 {
		t.Fatalf("malformed Entire proxy created domain slugs: %#v", cfg.DomainSlugs)
	}
}

func TestRepoStoragePathsRejectsEntireProxyWorkspacesNamespace(t *testing.T) {
	base := t.TempDir()
	env := EntireEnv{
		PluginConfigDir: filepath.Join(base, "config"),
		PluginDataDir:   filepath.Join(base, "data"),
		PluginStateDir:  filepath.Join(base, "state"),
	}
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "remote", "get-url", "origin"): {stdout: "entire://cluster.example/workspaces/acme/api\n"},
	}}

	_, err := repoStoragePaths(context.Background(), runner, env, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), `repo key uses the reserved "workspaces" segment: workspaces/acme/api`) {
		t.Fatalf("repo storage paths error = %v", err)
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

func TestRepoStoragePathsCanonicalizesExistingLocalSymlink(t *testing.T) {
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
	}
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "remote", "get-url", "origin"): {err: os.ErrNotExist},
	}}

	physical, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatalf("physical storage: %v", err)
	}
	viaAlias, err := repoStoragePaths(context.Background(), runner, env, alias)
	if err != nil {
		t.Fatalf("symlink storage: %v", err)
	}
	if viaAlias != physical {
		t.Fatalf("symlink storage = %+v, want %+v", viaAlias, physical)
	}
}

func TestCanonicalLocalRepoDirFollowsFinalSymlinkChain(t *testing.T) {
	parent := t.TempDir()
	repoDir := filepath.Join(parent, "repo")
	if err := os.Mkdir(repoDir, 0o700); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	relativeAlias := filepath.Join(parent, "relative-alias")
	if err := os.Symlink(filepath.Base(repoDir), relativeAlias); err != nil {
		t.Skipf("directory symlinks are unavailable on this platform: %v", err)
	}
	chainedAlias := filepath.Join(parent, "chained-alias")
	if err := os.Symlink(filepath.Base(relativeAlias), chainedAlias); err != nil {
		t.Skipf("directory symlinks are unavailable on this platform: %v", err)
	}
	absoluteAlias := filepath.Join(parent, "absolute-alias")
	if err := os.Symlink(repoDir, absoluteAlias); err != nil {
		t.Skipf("directory symlinks are unavailable on this platform: %v", err)
	}

	for _, alias := range []string{relativeAlias, chainedAlias, absoluteAlias} {
		got, err := canonicalExistingLocalRepoDir(alias)
		if err != nil {
			t.Fatalf("canonicalize %q: %v", alias, err)
		}
		if got != repoDir {
			t.Errorf("canonicalize %q = %q, want %q", alias, got, repoDir)
		}
	}
}

func TestRepoStorageKeyPreservesSymlinkedAncestorSpelling(t *testing.T) {
	parent := t.TempDir()
	realParent := filepath.Join(parent, "real-parent")
	repoDir := filepath.Join(realParent, "repo")
	if err := os.MkdirAll(repoDir, 0o700); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	aliasParent := filepath.Join(parent, "alias-parent")
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Skipf("directory symlinks are unavailable on this platform: %v", err)
	}
	logicalRepoDir := filepath.Join(aliasParent, "repo")

	canonical, err := canonicalExistingLocalRepoDir(logicalRepoDir)
	if err != nil {
		t.Fatalf("canonical local repo: %v", err)
	}
	if canonical != logicalRepoDir {
		t.Fatalf("canonical root = %q, want ancestor spelling preserved as %q", canonical, logicalRepoDir)
	}
	key, err := repoStorageKey(context.Background(), nil, t.TempDir(), logicalRepoDir)
	if err != nil {
		t.Fatalf("local key: %v", err)
	}
	sum := sha256.Sum256([]byte(filepath.Clean(logicalRepoDir)))
	want := "local/repo-" + hex.EncodeToString(sum[:])[:12]
	if key != want {
		t.Fatalf("local key = %q, want established lexical key %q", key, want)
	}
}

func TestRepoStorageKeyPreservesMissingPathHint(t *testing.T) {
	parent := t.TempDir()
	realParent := filepath.Join(parent, "real")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatalf("mkdir real parent: %v", err)
	}
	aliasParent := filepath.Join(parent, "alias")
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Skipf("directory symlinks are unavailable on this platform: %v", err)
	}
	missing := filepath.Join(aliasParent, "not-created")

	key, err := repoStorageKey(context.Background(), nil, t.TempDir(), missing)
	if err != nil {
		t.Fatalf("missing path key: %v", err)
	}
	clean := filepath.Clean(missing)
	sum := sha256.Sum256([]byte(clean))
	want := "local/not-created-" + hex.EncodeToString(sum[:])[:12]
	if key != want {
		t.Fatalf("missing path key = %q, want preserved hint key %q", key, want)
	}
}

func TestRepoStorageKeyKeepsRemoteIdentityAcrossLocalSymlink(t *testing.T) {
	parent := t.TempDir()
	repoDir := filepath.Join(parent, "repo")
	if err := os.Mkdir(repoDir, 0o700); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	alias := filepath.Join(parent, "repo-alias")
	if err := os.Symlink(repoDir, alias); err != nil {
		t.Skipf("directory symlinks are unavailable on this platform: %v", err)
	}
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "remote", "get-url", "origin"): {stdout: "git@github.com:entireio/cli.git\n"},
	}}

	physical, err := repoStorageKey(context.Background(), runner, t.TempDir(), repoDir)
	if err != nil {
		t.Fatalf("physical key: %v", err)
	}
	viaAlias, err := repoStorageKey(context.Background(), runner, t.TempDir(), alias)
	if err != nil {
		t.Fatalf("symlink key: %v", err)
	}
	if physical != "gh/entireio/cli" || viaAlias != physical {
		t.Fatalf("remote keys = %q, %q; want gh/entireio/cli", physical, viaAlias)
	}
}

func TestRepoStoragePathsCanonicalIdentityUpgradeCompatibility(t *testing.T) {
	tests := []struct {
		name          string
		canonicalData bool
		legacyData    bool
		wantLegacy    bool
		wantConflict  bool
	}{
		{name: "legacy only", legacyData: true, wantLegacy: true},
		{name: "canonical only", canonicalData: true},
		{name: "neither"},
		{name: "both conflict", canonicalData: true, legacyData: true, wantConflict: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
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
			}
			dirs, err := resolvePluginDirs(env)
			if err != nil {
				t.Fatalf("plugin dirs: %v", err)
			}
			canonicalKey := filepath.ToSlash(filepath.Join("local", localRepoKey(repoDir)))
			legacyKey := filepath.ToSlash(filepath.Join("local", localRepoKey(alias)))
			canonical := repoStorageForKey(dirs, canonicalKey)
			legacy := repoStorageForKey(dirs, legacyKey)
			if tc.canonicalData {
				if err := os.MkdirAll(canonical.BrainDir, 0o700); err != nil {
					t.Fatalf("create canonical brain: %v", err)
				}
				if err := os.MkdirAll(filepath.Dir(canonical.HeadPath), 0o700); err != nil {
					t.Fatalf("create canonical head dir: %v", err)
				}
				if err := os.WriteFile(canonical.HeadPath, []byte("{}\n"), 0o600); err != nil {
					t.Fatalf("create canonical head: %v", err)
				}
			}
			if tc.legacyData {
				if err := os.MkdirAll(legacy.BrainDir, 0o700); err != nil {
					t.Fatalf("create legacy brain: %v", err)
				}
				if err := os.MkdirAll(filepath.Dir(legacy.HeadPath), 0o700); err != nil {
					t.Fatalf("create legacy head dir: %v", err)
				}
				if err := os.WriteFile(legacy.HeadPath, []byte("{}\n"), 0o600); err != nil {
					t.Fatalf("create legacy head: %v", err)
				}
			}

			got, err := repoStoragePaths(context.Background(), nil, env, alias)
			if tc.wantConflict {
				var conflict *localRepoIdentityConflictError
				if !errors.As(err, &conflict) {
					t.Fatalf("error = %v, want typed identity conflict", err)
				}
				if commandErrorCode(err) != localRepoIdentityConflictCode {
					t.Fatalf("error code = %q, want %q", commandErrorCode(err), localRepoIdentityConflictCode)
				}
				if conflict.Canonical.Key != canonicalKey || conflict.Legacy.Key != legacyKey {
					t.Fatalf("conflict = %+v, want canonical %q legacy %q", conflict, canonicalKey, legacyKey)
				}
				return
			}
			if err != nil {
				t.Fatalf("repo storage paths: %v", err)
			}
			want := canonical
			if tc.wantLegacy {
				want = legacy
			}
			if got != want {
				t.Fatalf("storage = %+v, want %+v", got, want)
			}
		})
	}
}

func TestRepoStoragePathsRemoteSymlinkBypassesLocalLegacyState(t *testing.T) {
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
	}
	dirs, err := resolvePluginDirs(env)
	if err != nil {
		t.Fatalf("plugin dirs: %v", err)
	}
	legacyKey := filepath.ToSlash(filepath.Join("local", localRepoKey(alias)))
	legacy := repoStorageForKey(dirs, legacyKey)
	if err := os.MkdirAll(legacy.BrainDir, 0o700); err != nil {
		t.Fatalf("create legacy local brain: %v", err)
	}
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "remote", "get-url", "origin"): {stdout: "git@github.com:entireio/cli.git\n"},
	}}

	got, err := repoStoragePaths(context.Background(), runner, env, alias)
	if err != nil {
		t.Fatalf("remote storage: %v", err)
	}
	want := repoStorageForKey(dirs, "gh/entireio/cli")
	if got != want {
		t.Fatalf("remote storage = %+v, want %+v", got, want)
	}
}

func TestEnvFromOSKeepsLexicalAliasForStorageCompatibility(t *testing.T) {
	parent := t.TempDir()
	repoDir := filepath.Join(parent, "repo")
	if err := os.Mkdir(repoDir, 0o700); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	alias := filepath.Join(parent, "repo-alias")
	if err := os.Symlink(repoDir, alias); err != nil {
		t.Skipf("directory symlinks are unavailable on this platform: %v", err)
	}
	configDir := t.TempDir()
	dataDir := t.TempDir()
	stateDir := t.TempDir()
	t.Setenv(envRepoRoot, alias)
	t.Setenv(envPluginConfigDir, configDir)
	t.Setenv(envPluginDataDir, dataDir)
	t.Setenv(envPluginStateDir, stateDir)

	env := EnvFromOS()
	if env.RepoRoot != alias {
		t.Fatalf("lexical RepoRoot = %q, want %q", env.RepoRoot, alias)
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

	got, err := repoStoragePaths(context.Background(), nil, env, env.RepoRoot)
	if err != nil {
		t.Fatalf("repo storage paths: %v", err)
	}
	if got != legacy {
		t.Fatalf("storage = %+v, want legacy alias storage %+v", got, legacy)
	}
}
