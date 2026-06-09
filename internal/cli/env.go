package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/ashtom/entire-brain/internal/config"
)

const (
	envCLIVersion      = "ENTIRE_CLI_VERSION"
	envRepoRoot        = "ENTIRE_REPO_ROOT"
	envPluginConfigDir = "ENTIRE_PLUGIN_CONFIG_DIR"
	envPluginDataDir   = "ENTIRE_PLUGIN_DATA_DIR"
	envPluginStateDir  = "ENTIRE_PLUGIN_STATE_DIR"
	envPluginCacheDir  = "ENTIRE_PLUGIN_CACHE_DIR"

	xdgConfigHome = "XDG_CONFIG_HOME"
	xdgDataHome   = "XDG_DATA_HOME"
	xdgStateHome  = "XDG_STATE_HOME"
	xdgCacheHome  = "XDG_CACHE_HOME"

	xdgRootDir        = "entire"
	repoStoreDirName  = "repos"
	brainHeadFileName = "head.json"

	// pluginDataName is this plugin's namespace under the host's plugin data
	// root. The host dispatches the brain with ENTIRE_PLUGIN_DATA_DIR set to
	// <xdg_data>/entire/plugins/data/brain; run standalone (dev), we mirror that
	// suffix so the binary targets the real brain store, not a shadow one.
	pluginDataName = "brain"
)

var repoKeyUnsafeChars = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)
var repoRemoteSCPRegex = regexp.MustCompile(`^(?:[^@]+@)?([^:/]+):(.+)$`)
var knownRepoDomainSlugs = map[string]string{
	"github.com":    "gh",
	"gitlab.com":    "gl",
	"bitbucket.org": "bb",
	"entire.io":     "et",
	"tangled.org":   "tg",
	"code.storage":  "cs",
}

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

func EnvFromOS() EntireEnv {
	return EntireEnv{
		CLIVersion:      os.Getenv(envCLIVersion),
		RepoRoot:        os.Getenv(envRepoRoot),
		PluginConfigDir: os.Getenv(envPluginConfigDir),
		PluginDataDir:   os.Getenv(envPluginDataDir),
		PluginStateDir:  os.Getenv(envPluginStateDir),
		PluginCacheDir:  os.Getenv(envPluginCacheDir),
	}
}

type pluginDirs struct {
	Config string
	Data   string
	State  string
	Cache  string
}

func resolvePluginDirs(env EntireEnv) (pluginDirs, error) {
	configDir, err := resolveXDGDir(env.PluginConfigDir, envPluginConfigDir, xdgConfigHome, filepath.Join(".config", xdgRootDir))
	if err != nil {
		return pluginDirs{}, fmt.Errorf("resolve plugin config dir: %w", err)
	}
	dataDir, err := resolveXDGDir(env.PluginDataDir, envPluginDataDir, xdgDataHome, filepath.Join(".local", "share", xdgRootDir))
	if err != nil {
		return pluginDirs{}, fmt.Errorf("resolve plugin data dir: %w", err)
	}
	if env.PluginDataDir == "" {
		// No host-supplied data dir (standalone/dev run). Mirror the host's
		// plugin-data layout — <xdg_data>/entire/plugins/data/brain — so the brain
		// resolves to the same store the installed plugin uses rather than a shadow
		// XDG store. Only the data dir is namespaced; the host leaves config/state/
		// cache unset, so their XDG fallbacks already match.
		dataDir = filepath.Join(dataDir, "plugins", "data", pluginDataName)
	}
	stateDir, err := resolveXDGDir(env.PluginStateDir, envPluginStateDir, xdgStateHome, filepath.Join(".local", "state", xdgRootDir))
	if err != nil {
		return pluginDirs{}, fmt.Errorf("resolve plugin state dir: %w", err)
	}
	cacheDir, err := resolveXDGDir(env.PluginCacheDir, envPluginCacheDir, xdgCacheHome, filepath.Join(".cache", xdgRootDir))
	if err != nil {
		return pluginDirs{}, fmt.Errorf("resolve plugin cache dir: %w", err)
	}
	return pluginDirs{
		Config: configDir,
		Data:   dataDir,
		State:  stateDir,
		Cache:  cacheDir,
	}, nil
}

func resolveXDGDir(pluginDir, pluginEnv, xdgEnv, homeFallback string) (string, error) {
	if pluginDir != "" {
		if !filepath.IsAbs(pluginDir) {
			return "", fmt.Errorf("%s must be absolute, got %q", pluginEnv, pluginDir)
		}
		return pluginDir, nil
	}
	if xdg := os.Getenv(xdgEnv); xdg != "" {
		if filepath.IsAbs(xdg) {
			return filepath.Join(xdg, xdgRootDir), nil
		}
		// XDG says relative paths are invalid; ignore and fall back to HOME.
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, homeFallback), nil
}

type repoStorage struct {
	Key      string
	BrainDir string
	HeadPath string
}

func repoStoragePaths(ctx context.Context, runner CommandRunner, env EntireEnv, repoDir string) (repoStorage, error) {
	dirs, err := resolvePluginDirs(env)
	if err != nil {
		return repoStorage{}, err
	}
	key, err := repoStorageKey(ctx, runner, dirs.Config, repoDir)
	if err != nil {
		return repoStorage{}, err
	}
	brainRoot := filepath.Join(dirs.Data, repoStoreDirName)
	if err := rejectBrainRootPathSymlinks(brainRoot, filepath.FromSlash(key)); err != nil {
		return repoStorage{}, err
	}
	return repoStorage{
		Key:      key,
		BrainDir: filepath.Join(brainRoot, filepath.FromSlash(key)),
		HeadPath: filepath.Join(dirs.State, repoStoreDirName, filepath.FromSlash(key), brainHeadFileName),
	}, nil
}

func brainDirForKey(env EntireEnv, key string) (string, error) {
	dirs, err := resolvePluginDirs(env)
	if err != nil {
		return "", err
	}
	brainRoot := filepath.Join(dirs.Data, repoStoreDirName)
	if err := rejectBrainRootPathSymlinks(brainRoot, filepath.FromSlash(key)); err != nil {
		return "", err
	}
	return filepath.Join(brainRoot, filepath.FromSlash(key)), nil
}

func headPathForKey(env EntireEnv, key string) (string, error) {
	dirs, err := resolvePluginDirs(env)
	if err != nil {
		return "", err
	}
	return filepath.Join(dirs.State, repoStoreDirName, filepath.FromSlash(key), brainHeadFileName), nil
}

func repoStorageKey(ctx context.Context, runner CommandRunner, configDir, repoDir string) (string, error) {
	if runner != nil {
		stdout, _, err := runner.Run(ctx, repoDir, "git", "remote", "get-url", "origin")
		if err == nil {
			if key, ok, keyErr := repoKeyFromRemote(configDir, strings.TrimSpace(string(stdout))); ok || keyErr != nil {
				return key, keyErr
			}
		}
	}
	return filepath.ToSlash(filepath.Join("local", localRepoKey(repoDir))), nil
}

func repoKeyFromRemote(configDir, remote string) (string, bool, error) {
	host, repoPath, ok := parseRepoRemote(remote)
	if !ok {
		return "", false, nil
	}
	slug, err := repoDomainSlug(configDir, host)
	if err != nil {
		return "", true, err
	}
	components := []string{slug}
	components = append(components, repoPath...)
	return strings.Join(components, "/"), true, nil
}

func parseRepoRemote(remote string) (string, []string, bool) {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return "", nil, false
	}

	var host string
	var path string
	if parsed, ok := parseURLRemote(remote); ok {
		host = parsed.host
		path = parsed.path
	} else if match := repoRemoteSCPRegex.FindStringSubmatch(remote); len(match) == 3 {
		if looksLikeWindowsDriveRemotePath(remote, match[2]) {
			return "", nil, false
		}
		host = match[1]
		path = match[2]
	} else if before, after, ok := strings.Cut(remote, "/"); ok && before != "" && after != "" && strings.Contains(before, ".") {
		host = before
		path = after
	} else {
		return "", nil, false
	}

	host = normalizeRepoHost(host)
	components := normalizeRepoPath(path)
	if host == "" || len(components) == 0 {
		return "", nil, false
	}
	return host, components, true
}

func looksLikeWindowsDriveRemotePath(remote, scpPath string) bool {
	return looksLikeWindowsDrivePath(remote) &&
		(strings.HasPrefix(scpPath, "/") || strings.Contains(scpPath, "\\") || !strings.Contains(scpPath, "/"))
}

type parsedRemote struct {
	host string
	path string
}

func parseURLRemote(remote string) (parsedRemote, bool) {
	if !strings.Contains(remote, "://") {
		return parsedRemote{}, false
	}
	parsed, err := url.Parse(remote)
	if err != nil || parsed.Host == "" {
		return parsedRemote{}, false
	}
	return parsedRemote{host: parsed.Hostname(), path: parsed.Path}, true
}

func normalizeRepoHost(host string) string {
	host = strings.TrimSpace(strings.ToLower(host))
	if h, _, ok := strings.Cut(host, ":"); ok {
		host = h
	}
	return strings.Trim(host, "[]")
}

func normalizeRepoPath(path string) []string {
	path = strings.TrimSpace(path)
	path = strings.Trim(path, "/")
	path = strings.TrimSuffix(path, ".git")
	var components []string
	for _, component := range strings.Split(path, "/") {
		component = safeRepoPathComponent(strings.ToLower(component))
		if component != "" {
			components = append(components, component)
		}
	}
	return components
}

func repoDomainSlug(configDir, host string) (string, error) {
	if slug, ok := knownRepoDomainSlugs[host]; ok {
		return slug, nil
	}

	var slug string
	_, err := config.Update(configDir, func(cfg *config.Config) error {
		if cfg.DomainSlugs == nil {
			cfg.DomainSlugs = make(map[string]string)
		}
		if existing := cfg.DomainSlugs[host]; existing != "" {
			slug = existing
			return nil
		}
		slug = generateDomainSlug(host, cfg.DomainSlugs)
		cfg.DomainSlugs[host] = slug
		return nil
	})
	if err != nil {
		return "", err
	}
	return slug, nil
}

func generateDomainSlug(host string, existing map[string]string) string {
	used := make(map[string]struct{}, len(existing))
	for _, slug := range existing {
		used[slug] = struct{}{}
	}
	for i := 0; ; i++ {
		sum := sha256.Sum256([]byte(host + ":" + strconv.Itoa(i)))
		slug := threeLetterSlug(sum)
		if _, ok := used[slug]; !ok {
			return slug
		}
	}
}

func threeLetterSlug(sum [sha256.Size]byte) string {
	n := int(sum[0])<<16 | int(sum[1])<<8 | int(sum[2])
	var b [3]byte
	for i := range b {
		b[i] = byte('a' + n%26)
		n /= 26
	}
	return string(b[:])
}

func safeRepoPathComponent(value string) string {
	value = strings.Trim(repoKeyUnsafeChars.ReplaceAllString(value, "-"), "-._")
	return value
}

func localRepoKey(repoDir string) string {
	repoDir = filepath.Clean(repoDir)
	base := filepath.Base(repoDir)
	base = safeRepoPathComponent(base)
	if base == "" || base == "." || base == string(filepath.Separator) {
		base = "repo"
	}
	sum := sha256.Sum256([]byte(repoDir))
	return base + "-" + hex.EncodeToString(sum[:])[:12]
}

func ensureDir(path string) error {
	if path == "" {
		return errors.New("directory path is empty")
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return nil
}

func valueOrUnset(value string) string {
	if value == "" {
		return "<unset>"
	}
	return value
}
