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
	"slices"
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
		CLIVersion: os.Getenv(envCLIVersion),
		// Keep the host-supplied spelling. Storage identity canonicalization is
		// intentionally local to repoStoragePaths; retaining an ambient hidden
		// alias here would leak repo A into per-repo Options copies for repo B.
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

const localRepoIdentityConflictCode = "repo_identity_conflict"

// localRepoIdentityConflictError refuses to guess when an upgrade finds state
// under both the canonical root identity and the exact pre-upgrade lexical
// alias identity. Choosing either would hide the other repository history.
type localRepoIdentityConflictError struct {
	Canonical repoStorage
	Legacy    repoStorage
}

func (e *localRepoIdentityConflictError) Error() string {
	return fmt.Sprintf(
		"%s: state exists for canonical local repo key %q and pre-upgrade alias key %q; move one brain/head store aside and retry (canonical brain: %s; legacy brain: %s)",
		localRepoIdentityConflictCode,
		e.Canonical.Key,
		e.Legacy.Key,
		e.Canonical.BrainDir,
		e.Legacy.BrainDir,
	)
}

type repoStorageIdentity struct {
	Key       string
	LegacyKey string
	// PhysicalKey is the key of the FULLY symlink-resolved repository root. It
	// differs from Key only when an ANCESTOR of the root is a symlink, where
	// Key deliberately preserves the caller's lexical spelling so an explicit
	// alias keeps addressing its established brain. A relative invocation from
	// inside the working tree can only ever produce the physical spelling, so
	// without carrying both, one repository would silently split across two
	// brains depending on how it was addressed. Treating the physical spelling
	// as an alternate identity means an existing physical-key brain is
	// recovered in place and a genuine both-exist collision is reported rather
	// than guessed, exactly like LegacyKey.
	PhysicalKey string
	Local       bool
}

func repoStoragePaths(ctx context.Context, runner CommandRunner, env EntireEnv, repoDir string) (repoStorage, error) {
	dirs, err := resolvePluginDirs(env)
	if err != nil {
		return repoStorage{}, err
	}
	identity, err := resolveRepoStorageIdentity(ctx, runner, dirs.Config, repoDir, repoDir)
	if err != nil {
		return repoStorage{}, err
	}
	brainRoot := filepath.Join(dirs.Data, repoStoreDirName)
	canonical := repoStorageForKey(dirs, identity.Key)
	if err := rejectBrainRootPathSymlinks(brainRoot, filepath.FromSlash(canonical.Key)); err != nil {
		return repoStorage{}, err
	}
	if !identity.Local {
		return canonical, nil
	}
	// Alternate spellings of the same local repository: the pre-upgrade lexical
	// alias and the fully symlink-resolved physical root. Either may already
	// hold this repository's brain.
	var alternateKeys []string
	for _, key := range []string{identity.LegacyKey, identity.PhysicalKey} {
		if key == "" || key == identity.Key || slices.Contains(alternateKeys, key) {
			continue
		}
		alternateKeys = append(alternateKeys, key)
	}
	if len(alternateKeys) == 0 {
		return canonical, nil
	}
	canonicalExists, err := repoStorageContainsState(canonical)
	if err != nil {
		return repoStorage{}, err
	}
	var populated []repoStorage
	for _, key := range alternateKeys {
		alternate := repoStorageForKey(dirs, key)
		if err := rejectBrainRootPathSymlinks(brainRoot, filepath.FromSlash(alternate.Key)); err != nil {
			return repoStorage{}, err
		}
		exists, err := repoStorageContainsState(alternate)
		if err != nil {
			return repoStorage{}, err
		}
		if exists {
			populated = append(populated, alternate)
		}
	}
	if len(populated) == 0 {
		return canonical, nil
	}
	// Refuse to guess whenever more than one spelling holds state: picking
	// either would hide the other repository history.
	if canonicalExists {
		return repoStorage{}, &localRepoIdentityConflictError{Canonical: canonical, Legacy: populated[0]}
	}
	if len(populated) > 1 {
		return repoStorage{}, &localRepoIdentityConflictError{Canonical: populated[0], Legacy: populated[1]}
	}
	// Preserve the exact established key in place. This is atomic because no
	// files move, and it avoids a cross-root brain/head migration that could
	// only be partially committed if the process or filesystem fails.
	return populated[0], nil
}

func repoStorageForKey(dirs pluginDirs, key string) repoStorage {
	return repoStorage{
		Key:      key,
		BrainDir: filepath.Join(dirs.Data, repoStoreDirName, filepath.FromSlash(key)),
		HeadPath: filepath.Join(dirs.State, repoStoreDirName, filepath.FromSlash(key), brainHeadFileName),
	}
}

func repoStorageContainsState(storage repoStorage) (bool, error) {
	for _, path := range []string{storage.BrainDir, storage.HeadPath} {
		_, err := os.Lstat(path)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, os.ErrNotExist):
			continue
		default:
			return false, fmt.Errorf("inspect repo identity state %s: %w", path, err)
		}
	}
	return false, nil
}

func brainDirForKey(env EntireEnv, key string) (string, error) {
	// "workspaces" is reserved in the repo keyspace: older builds stored
	// workspace manifests at repos/workspaces/<name> (since relocated to a
	// sibling of repos/), and a host slug or hand-edited DomainSlugs entry
	// claiming the segment would collide with any legacy data still awaiting
	// migration.
	if first, _, _ := strings.Cut(key, "/"); first == workspaceDirName {
		return "", fmt.Errorf("repo key uses the reserved %q segment: %s", workspaceDirName, key)
	}
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
	identity, err := resolveRepoStorageIdentity(ctx, runner, configDir, repoDir, repoDir)
	if err != nil {
		return "", err
	}
	return identity.Key, nil
}

func resolveRepoStorageIdentity(ctx context.Context, runner CommandRunner, configDir, repoDir, legacyRepoDir string) (repoStorageIdentity, error) {
	legacyRepoDir = filepath.Clean(legacyRepoDir)
	canonicalRepoDir, err := canonicalExistingLocalRepoDir(repoDir)
	if err != nil {
		return repoStorageIdentity{}, err
	}
	if runner != nil {
		stdout, _, err := runner.Run(ctx, canonicalRepoDir, "git", "remote", "get-url", "origin")
		if err == nil {
			if key, ok, keyErr := repoKeyFromRemote(configDir, strings.TrimSpace(string(stdout))); ok || keyErr != nil {
				return repoStorageIdentity{Key: key}, keyErr
			}
		}
	}
	identity := repoStorageIdentity{
		Key:       filepath.ToSlash(filepath.Join("local", localRepoKey(canonicalRepoDir))),
		LegacyKey: filepath.ToSlash(filepath.Join("local", localRepoKey(legacyRepoDir))),
		Local:     true,
	}
	if physical := physicalLocalRepoDir(canonicalRepoDir); physical != "" {
		identity.PhysicalKey = filepath.ToSlash(filepath.Join("local", localRepoKey(physical)))
	}
	return identity, nil
}

// physicalLocalRepoDir resolves EVERY path component, so a repository reached
// through a symlinked ancestor maps to the same spelling that a relative
// invocation from inside the working tree produces (git reports the physical
// root). It returns "" when the path does not exist or cannot be resolved:
// there is then no physical identity to compare against, and the durable
// missing-path hint must survive untouched.
func physicalLocalRepoDir(repoDir string) string {
	if repoDir == "" {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(repoDir)
	if err != nil {
		return ""
	}
	return filepath.Clean(resolved)
}

// canonicalExistingLocalRepoDir resolves only a symlink in the repository
// root's final path component. Resolving every component would change existing
// path-hashed identities when an ancestor is an operating-system alias (for
// example, macOS /var -> /private/var). A final-component link is different: it
// is the explicitly supplied repository alias, so following its declared target
// makes the alias and target share one identity without rewriting ancestors.
//
// Missing paths remain untouched because workspace manifests use them as
// durable hints that may become available again later. Broken final-component
// links likewise retain the original hint until their targets exist.
func canonicalExistingLocalRepoDir(repoDir string) (string, error) {
	if repoDir == "" {
		return "", nil
	}
	original := filepath.Clean(repoDir)
	current := original
	const maxRootSymlinkDepth = 40
	seen := make(map[string]struct{}, 1)
	for depth := 0; depth < maxRootSymlinkDepth; depth++ {
		if _, ok := seen[current]; ok {
			return "", fmt.Errorf("resolve local repo root symlinks: cycle at %q", current)
		}
		seen[current] = struct{}{}

		info, err := os.Lstat(current)
		if err == nil && info.Mode()&os.ModeSymlink == 0 {
			return current, nil
		}
		if errors.Is(err, os.ErrNotExist) {
			return original, nil
		}
		if err != nil {
			return "", fmt.Errorf("stat local repo root: %w", err)
		}

		target, err := os.Readlink(current)
		if err != nil {
			return "", fmt.Errorf("read local repo root symlink: %w", err)
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(current), target)
		}
		current = filepath.Clean(target)
	}
	return "", fmt.Errorf("resolve local repo root symlinks: exceeded %d links", maxRootSymlinkDepth)
}

func repoKeyFromRemote(configDir, remote string) (string, bool, error) {
	remote = strings.TrimSpace(remote)
	if parsed, err := url.Parse(remote); err == nil && strings.EqualFold(parsed.Scheme, "entire") {
		if parsed.Hostname() == "" {
			return "", false, nil
		}
		components := normalizeRepoPath(parsed.Path)
		if len(components) < 3 {
			return "", false, nil
		}
		key := strings.Join(components, "/")
		if components[0] == workspaceDirName {
			return "", true, fmt.Errorf("repo key uses the reserved %q segment: %s", workspaceDirName, key)
		}
		return key, true, nil
	}

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
