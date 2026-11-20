package base

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// EntireEnv captures the environment variables supplied by the parent Entire
// CLI when this binary is dispatched as an external command.
type EntireEnv struct {
	CLIVersion      string
	RepoRoot        string
	PluginConfigDir string
	PluginDataDir   string
	PluginStateDir  string
	PluginCacheDir  string
}

type RepoStorage struct {
	Key      string
	BrainDir string
	HeadPath string
}

// StdinIsTerminal reports whether the process's stdin is a character device —
// a person's terminal rather than a pipe or redirect. Shared by the seed
// prompt and the hook command's would-block guard (COR-2268: one home).
func StdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	return err == nil && (info.Mode()&os.ModeCharDevice) != 0
}

// RepoIdentityConflictCode is the error code every repo-scoped command reports
// while one repository's brain is split across more than one store. The
// repo-identity command repairs that state and prints the same code.
const RepoIdentityConflictCode = "repo_identity_conflict"

const (
	EnvCLIVersion      = "ENTIRE_CLI_VERSION"
	EnvRepoRoot        = "ENTIRE_REPO_ROOT"
	EnvPluginConfigDir = "ENTIRE_PLUGIN_CONFIG_DIR"
	EnvPluginDataDir   = "ENTIRE_PLUGIN_DATA_DIR"
	EnvPluginStateDir  = "ENTIRE_PLUGIN_STATE_DIR"
	EnvPluginCacheDir  = "ENTIRE_PLUGIN_CACHE_DIR"

	XDGConfigHome = "XDG_CONFIG_HOME"
	XDGDataHome   = "XDG_DATA_HOME"
	XDGStateHome  = "XDG_STATE_HOME"
	XDGCacheHome  = "XDG_CACHE_HOME"

	XDGRootDir        = "entire"
	RepoStoreDirName  = "repos"
	BrainHeadFileName = "head.json"

	// PluginDataName is this plugin's namespace under the host's plugin data
	// root. The host dispatches the brain with ENTIRE_PLUGIN_DATA_DIR set to
	// <xdg_data>/entire/plugins/data/brain; run standalone (dev), we mirror that
	// suffix so the binary targets the real brain store, not a shadow one.
	PluginDataName = "brain"

	// WorkspacesDirName is the reserved sibling of the repo store that holds
	// workspace manifests and the per-workspace aggregate Brains.
	WorkspacesDirName = "workspaces"
)

func EnvFromOS() EntireEnv {
	return EntireEnv{
		CLIVersion: os.Getenv(EnvCLIVersion),
		// Keep the host-supplied spelling. Storage identity canonicalization is
		// intentionally local to repoStoragePaths; retaining an ambient hidden
		// alias here would leak repo A into per-repo Options copies for repo B.
		RepoRoot:        os.Getenv(EnvRepoRoot),
		PluginConfigDir: os.Getenv(EnvPluginConfigDir),
		PluginDataDir:   os.Getenv(EnvPluginDataDir),
		PluginStateDir:  os.Getenv(EnvPluginStateDir),
		PluginCacheDir:  os.Getenv(EnvPluginCacheDir),
	}
}

type PluginDirs struct {
	Config string
	Data   string
	State  string
	Cache  string
}

func ResolvePluginDirs(env EntireEnv) (PluginDirs, error) {
	configDir, err := ResolveXDGDir(env.PluginConfigDir, EnvPluginConfigDir, XDGConfigHome, filepath.Join(".config", XDGRootDir))
	if err != nil {
		return PluginDirs{}, fmt.Errorf("resolve plugin config dir: %w", err)
	}
	dataDir, err := resolvePluginDataRoot(env.PluginDataDir, runtime.GOOS)
	if err != nil {
		return PluginDirs{}, fmt.Errorf("resolve plugin data dir: %w", err)
	}
	if env.PluginDataDir == "" {
		// No host-supplied data dir (standalone/dev run). Mirror the host's
		// plugin-data layout — <data_root>/entire/plugins/data/brain — so the brain
		// resolves to the same store the installed plugin uses rather than a shadow
		// store. Only the data dir is namespaced; the host leaves config/state/
		// cache unset, so their XDG fallbacks already match.
		dataDir = filepath.Join(dataDir, "plugins", "data", PluginDataName)
	}
	stateDir, err := ResolveXDGDir(env.PluginStateDir, EnvPluginStateDir, XDGStateHome, filepath.Join(".local", "state", XDGRootDir))
	if err != nil {
		return PluginDirs{}, fmt.Errorf("resolve plugin state dir: %w", err)
	}
	cacheDir, err := ResolveXDGDir(env.PluginCacheDir, EnvPluginCacheDir, XDGCacheHome, filepath.Join(".cache", XDGRootDir))
	if err != nil {
		return PluginDirs{}, fmt.Errorf("resolve plugin cache dir: %w", err)
	}
	return PluginDirs{
		Config: configDir,
		Data:   dataDir,
		State:  stateDir,
		Cache:  cacheDir,
	}, nil
}

func ResolveXDGDir(pluginDir, pluginEnv, xdgEnv, homeFallback string) (string, error) {
	if pluginDir != "" {
		if !filepath.IsAbs(pluginDir) {
			return "", fmt.Errorf("%s must be absolute, got %q", pluginEnv, pluginDir)
		}
		return pluginDir, nil
	}
	if xdg := os.Getenv(xdgEnv); xdg != "" {
		if filepath.IsAbs(xdg) {
			return filepath.Join(xdg, XDGRootDir), nil
		}
		// XDG says relative paths are invalid; ignore and fall back to HOME.
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, homeFallback), nil
}

// windowsGOOS is the runtime.GOOS value the data-root rule below branches on.
// It is a named constant so the Windows branch can be exercised from a darwin
// or linux machine by passing it explicitly.
const windowsGOOS = "windows"

// EnvLocalAppData is the Windows per-user application data root. The host CLI
// reads it to place the managed plugin tree; the brain reads it only to agree
// with the host (see resolvePluginDataRoot).
const EnvLocalAppData = "LOCALAPPDATA"

// resolvePluginDataRoot resolves the directory that holds the host's plugin
// tree, which the caller then namespaces with plugins/data/<PluginDataName>.
//
// An explicit ENTIRE_PLUGIN_DATA_DIR always wins, on every OS: that is the
// dispatched path, and the host sets it on every plugin invocation.
//
// The fallback is where the two implementations disagreed. The host
// (entireio/cli, pluginParentDir) does NOT use XDG on Windows — it reads
// %LOCALAPPDATA%, falls back to %USERPROFILE%\AppData\Local, and ignores
// XDG_DATA_HOME entirely. Resolving the brain's fallback through the XDG rule
// therefore landed a standalone run on %USERPROFILE%\.local\share\entire while
// the installed plugin's store sat under %LOCALAPPDATA%\entire: a different,
// empty store, read with no error and no warning. PluginDataName's comment
// above already promises we mirror the host here; on Windows we did not.
//
// Unix is deliberately untouched. There the host and the brain both resolve
// XDG_DATA_HOME and then ~/.local/share, so the two already agree exactly, and
// only the data root is affected either way — config, state and cache are left
// unset by the host and their XDG fallbacks match on every OS.
func resolvePluginDataRoot(pluginDir, goos string) (string, error) {
	if pluginDir != "" || goos != windowsGOOS {
		return ResolveXDGDir(pluginDir, EnvPluginDataDir, XDGDataHome, filepath.Join(".local", "share", XDGRootDir))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// Report the miss rather than silently resolving somewhere else: a wrong
		// store reads as an empty brain, which is worse than a failure.
		home = ""
	}
	return windowsPluginDataRoot(os.Getenv(EnvLocalAppData), home)
}

// windowsPluginDataRoot is the pure half of the Windows rule, taking the two
// inputs as arguments so it is testable from any machine.
//
// A relative %LOCALAPPDATA% is discarded rather than joined: the host requires
// an absolute one (requireAbsPluginParent) and would not have placed the store
// under a relative path, so honouring it here would invent a third location
// rather than agree with anybody.
func windowsPluginDataRoot(localAppData, home string) (string, error) {
	if localAppData != "" && filepath.IsAbs(localAppData) {
		return filepath.Join(localAppData, XDGRootDir), nil
	}
	if home == "" {
		return "", fmt.Errorf("resolve plugin data root: %s is unset or relative and the home directory is unknown", EnvLocalAppData)
	}
	return filepath.Join(home, "AppData", "Local", XDGRootDir), nil
}

// RejectBrainRootPathSymlinks refuses a brain root behind a symlink and any
// existing symlinked component under it for the given relative path.
func RejectBrainRootPathSymlinks(brainRoot, rel string) error {
	if err := RejectSymlinkedBrainRoot(brainRoot); err != nil {
		return err
	}
	return RejectExistingSymlinkPathComponents(brainRoot, rel)
}

// RejectReservedRepoKeySegment refuses a repo key whose first segment is
// reserved in the repo keyspace.
//
// "workspaces" is reserved: older builds stored workspace manifests at
// repos/workspaces/<name> (since relocated to a sibling of repos/), and a host
// slug or hand-edited DomainSlugs entry claiming the segment would collide with
// any legacy data still awaiting migration.
//
// This lives in one function because it is a property of the KEY, not of any one
// consumer of it. It used to be inline in BrainDirForKey only, which left the two
// places that build a store path without that helper — `brain path`, and
// HeadPathForKey — accepting a key every other command refuses.
func RejectReservedRepoKeySegment(key string) error {
	if first, _, _ := strings.Cut(key, "/"); first == WorkspacesDirName {
		return fmt.Errorf("repo key uses the reserved %q segment: %s", WorkspacesDirName, key)
	}
	return nil
}

func BrainDirForKey(env EntireEnv, key string) (string, error) {
	if err := RejectReservedRepoKeySegment(key); err != nil {
		return "", err
	}
	dirs, err := ResolvePluginDirs(env)
	if err != nil {
		return "", err
	}
	brainRoot := filepath.Join(dirs.Data, RepoStoreDirName)
	if err := RejectBrainRootPathSymlinks(brainRoot, filepath.FromSlash(key)); err != nil {
		return "", err
	}
	return filepath.Join(brainRoot, filepath.FromSlash(key)), nil
}

func HeadPathForKey(env EntireEnv, key string) (string, error) {
	// Same guards as BrainDirForKey. The head path is built from the same
	// untrusted key and joined onto the same per-key directory, so accepting a
	// key here that BrainDirForKey refuses would put the two halves of a repo's
	// storage under different rules.
	if err := RejectReservedRepoKeySegment(key); err != nil {
		return "", err
	}
	dirs, err := ResolvePluginDirs(env)
	if err != nil {
		return "", err
	}
	headRoot := filepath.Join(dirs.State, RepoStoreDirName)
	if err := RejectBrainRootPathSymlinks(headRoot, filepath.FromSlash(key)); err != nil {
		return "", err
	}
	return filepath.Join(headRoot, filepath.FromSlash(key), BrainHeadFileName), nil
}

func RepoStorageForKey(dirs PluginDirs, key string) RepoStorage {
	return RepoStorage{
		Key:      key,
		BrainDir: filepath.Join(dirs.Data, RepoStoreDirName, filepath.FromSlash(key)),
		HeadPath: filepath.Join(dirs.State, RepoStoreDirName, filepath.FromSlash(key), BrainHeadFileName),
	}
}

var KnownRepoDomainSlugs = map[string]string{
	"github.com":    "gh",
	"gitlab.com":    "gl",
	"bitbucket.org": "bb",
	"entire.io":     "et",
	"tangled.org":   "tg",
	"code.storage":  "cs",
}
