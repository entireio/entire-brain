package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/entireio/entire-brain/internal/config"
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

// Error names the command that resolves the conflict.
//
// It used to end with "move one brain/head store aside and retry" -- a manual
// filesystem instruction with nothing behind it. Since every repo-scoped
// command refuses while the conflict stands, and that includes the deletes a
// reader would reach for, the message was the ONLY thing standing between the
// user and a dead repository, and it pointed at `mv`. Naming a command that
// exists is the difference between a diagnosis and a recovery path.
func (e *localRepoIdentityConflictError) Error() string {
	return fmt.Sprintf(
		"%s: state exists for canonical local repo key %q and pre-upgrade alias key %q; "+
			"run `%s repo-identity` to see both stores, then `%s repo-identity --keep <repo-key>` "+
			"to keep one and move the other aside (canonical brain: %s; legacy brain: %s)",
		localRepoIdentityConflictCode,
		e.Canonical.Key,
		e.Legacy.Key,
		setupCommandPrefix(os.LookupEnv),
		setupCommandPrefix(os.LookupEnv),
		e.Canonical.BrainDir,
		e.Legacy.BrainDir,
	)
}

type repoStorageIdentity struct {
	// Key is the identity every spelling of the same repository root must
	// agree on: the root with EVERY path component resolved through symlinks.
	//
	// It used to be the caller's lexical spelling, which made the key a
	// property of HOW the repository was addressed rather than of WHICH
	// repository it is. A relative invocation from inside the working tree can
	// only ever produce the physical spelling (git and the getcwd syscall both
	// report it), while an absolute ENTIRE_REPO_ROOT keeps whatever the caller
	// typed -- so on any host where a repository is reachable through a
	// symlinked ancestor (macOS /tmp -> /private/tmp being the everyday case)
	// the two routes minted two keys, built two brains, and then refused every
	// command with a repo_identity_conflict once both existed. Only the fully
	// resolved root is the same string no matter which route asked.
	Key string

	// AlternateKeys are the other spellings of this same root that an earlier
	// build may already have keyed this repository's brain under: the lexical
	// spelling the caller supplied, and the spelling with only the root's final
	// component resolved (the pre-fix canonical form). They are not separate
	// repositories -- they are the same directory under a different name -- so
	// an existing brain found under one of them is adopted in place rather than
	// orphaned, and state under more than one is reported rather than guessed.
	AlternateKeys []string

	Local bool
}

// repoStorageSet is every store one repository could be using: the canonical
// one its identity names, and every spelling of that same root that currently
// holds state.
//
// It exists so the repair paths -- the ones whose whole job is to clear a
// duplicate store -- can see the duplicate instead of being refused by it.
// repoStoragePaths, which every ordinary command uses, turns the same facts
// into the single store to read and write, or into the refusal.
type repoStorageSet struct {
	// Canonical is the store this repository's identity names, whether or not
	// anything has been written there yet.
	Canonical repoStorage
	// Populated is every store holding state, canonical first when it holds
	// any. More than one entry is the identity conflict.
	Populated []repoStorage
	// Local reports whether the identity came from the repository root's path
	// (as opposed to a remote), which is the only case that can have
	// alternates at all.
	Local bool
}

// Conflicted reports whether this repository's brain is split across more than
// one store.
func (s repoStorageSet) Conflicted() bool { return len(s.Populated) > 1 }

// Active is the store ordinary commands read and write: the one established
// store when exactly one spelling holds state (adopted in place, so no files
// move), otherwise the canonical one.
func (s repoStorageSet) Active() repoStorage {
	if len(s.Populated) == 1 {
		return s.Populated[0]
	}
	return s.Canonical
}

// conflictError renders the split as the refusal ordinary commands report.
func (s repoStorageSet) conflictError() error {
	if !s.Conflicted() {
		return nil
	}
	return &localRepoIdentityConflictError{Canonical: s.Populated[0], Legacy: s.Populated[1]}
}

func resolveRepoStorageSet(ctx context.Context, runner CommandRunner, env EntireEnv, repoDir string) (repoStorageSet, error) {
	return resolveRepoStorageSetWithSlug(ctx, runner, env, repoDir, repoDomainSlug)
}

func resolveRepoStorageSetWithSlug(ctx context.Context, runner CommandRunner, env EntireEnv, repoDir string, slugForHost func(string, string) (string, error)) (repoStorageSet, error) {
	dirs, err := resolvePluginDirs(env)
	if err != nil {
		return repoStorageSet{}, err
	}
	identity, err := resolveRepoStorageIdentityWithSlug(ctx, runner, dirs.Config, repoDir, repoDir, slugForHost)
	if err != nil {
		return repoStorageSet{}, err
	}
	brainRoot := filepath.Join(dirs.Data, repoStoreDirName)
	canonical := repoStorageForKey(dirs, identity.Key)
	if err := rejectBrainRootPathSymlinks(brainRoot, filepath.FromSlash(canonical.Key)); err != nil {
		return repoStorageSet{}, err
	}
	set := repoStorageSet{Canonical: canonical, Local: identity.Local}
	if !identity.Local {
		return set, nil
	}
	canonicalExists, err := repoStorageContainsState(canonical)
	if err != nil {
		return repoStorageSet{}, err
	}
	if canonicalExists {
		set.Populated = append(set.Populated, canonical)
	}
	for _, key := range identity.AlternateKeys {
		alternate := repoStorageForKey(dirs, key)
		if err := rejectBrainRootPathSymlinks(brainRoot, filepath.FromSlash(alternate.Key)); err != nil {
			return repoStorageSet{}, err
		}
		exists, err := repoStorageContainsState(alternate)
		if err != nil {
			return repoStorageSet{}, err
		}
		if exists {
			set.Populated = append(set.Populated, alternate)
		}
	}
	return set, nil
}

func repoStoragePaths(ctx context.Context, runner CommandRunner, env EntireEnv, repoDir string) (repoStorage, error) {
	set, err := resolveRepoStorageSet(ctx, runner, env, repoDir)
	if err != nil {
		return repoStorage{}, err
	}
	// Refuse to guess whenever more than one spelling holds state: picking
	// either would hide the other repository history. The refusal names the
	// command that resolves it -- see localRepoIdentityConflictError.
	if err := set.conflictError(); err != nil {
		return repoStorage{}, err
	}
	// Otherwise preserve the exact established key in place. This is atomic
	// because no files move, and it avoids a cross-root brain/head migration
	// that could only be partially committed if the process or filesystem
	// fails.
	return set.Active(), nil
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

// rejectReservedRepoKeySegment refuses a repo key whose first segment is
// reserved in the repo keyspace.
//
// "workspaces" is reserved: older builds stored workspace manifests at
// repos/workspaces/<name> (since relocated to a sibling of repos/), and a host
// slug or hand-edited DomainSlugs entry claiming the segment would collide with
// any legacy data still awaiting migration.
//
// This lives in one function because it is a property of the KEY, not of any one
// consumer of it. It used to be inline in brainDirForKey only, which left the two
// places that build a store path without that helper — `brain path`, and
// headPathForKey — accepting a key every other command refuses.
func rejectReservedRepoKeySegment(key string) error {
	if first, _, _ := strings.Cut(key, "/"); first == workspaceDirName {
		return fmt.Errorf("repo key uses the reserved %q segment: %s", workspaceDirName, key)
	}
	return nil
}

func brainDirForKey(env EntireEnv, key string) (string, error) {
	if err := rejectReservedRepoKeySegment(key); err != nil {
		return "", err
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
	// Same guards as brainDirForKey. The head path is built from the same
	// untrusted key and joined onto the same per-key directory, so accepting a
	// key here that brainDirForKey refuses would put the two halves of a repo's
	// storage under different rules.
	if err := rejectReservedRepoKeySegment(key); err != nil {
		return "", err
	}
	dirs, err := resolvePluginDirs(env)
	if err != nil {
		return "", err
	}
	headRoot := filepath.Join(dirs.State, repoStoreDirName)
	if err := rejectBrainRootPathSymlinks(headRoot, filepath.FromSlash(key)); err != nil {
		return "", err
	}
	return filepath.Join(headRoot, filepath.FromSlash(key), brainHeadFileName), nil
}

func repoStorageKey(ctx context.Context, runner CommandRunner, configDir, repoDir string) (string, error) {
	identity, err := resolveRepoStorageIdentity(ctx, runner, configDir, repoDir, repoDir)
	if err != nil {
		return "", err
	}
	return identity.Key, nil
}

func resolveRepoStorageIdentity(ctx context.Context, runner CommandRunner, configDir, repoDir, legacyRepoDir string) (repoStorageIdentity, error) {
	return resolveRepoStorageIdentityWithSlug(ctx, runner, configDir, repoDir, legacyRepoDir, repoDomainSlug)
}

func resolveRepoStorageIdentityWithSlug(ctx context.Context, runner CommandRunner, configDir, repoDir, legacyRepoDir string, slugForHost func(string, string) (string, error)) (repoStorageIdentity, error) {
	legacyRepoDir = filepath.Clean(legacyRepoDir)
	rootLinkResolved, err := rootSymlinkResolvedLocalRepoDir(repoDir)
	if err != nil {
		return repoStorageIdentity{}, err
	}
	if runner != nil {
		stdout, _, err := runner.Run(ctx, rootLinkResolved, "git", "remote", "get-url", "origin")
		if err == nil {
			if key, ok, keyErr := repoKeyFromRemoteWithSlug(configDir, strings.TrimSpace(string(stdout)), slugForHost); ok || keyErr != nil {
				return repoStorageIdentity{Key: key}, keyErr
			}
		}
	}
	canonicalKey, err := localRepoStorageKey(rootLinkResolved)
	if err != nil {
		return repoStorageIdentity{}, err
	}
	identity := repoStorageIdentity{Key: canonicalKey, Local: true}
	// The spellings this same root used to be keyed under, newest rule first:
	// the final-component-resolved form (the pre-fix canonical), then the raw
	// lexical form the caller supplied. Either may already hold the brain.
	for _, spelling := range []string{rootLinkResolved, legacyRepoDir} {
		key := localRepoStorageKeyForSpelling(spelling)
		if key == identity.Key || slices.Contains(identity.AlternateKeys, key) {
			continue
		}
		identity.AlternateKeys = append(identity.AlternateKeys, key)
	}
	return identity, nil
}

// localRepoStorageKey is the ONE derivation of a local repository's storage key,
// and the only one any caller should use: the repository root with every path
// component resolved through symlinks, hashed.
//
// Full resolution is what makes the key a property of the repository rather
// than of the route taken to it. Every other spelling -- an absolute
// ENTIRE_REPO_ROOT through a symlinked ancestor, a `cd` through an alias, a
// relative invocation -- collapses onto this one, so a repository cannot end up
// with two brains and then be locked out of both.
//
// The deliberate error path: EvalSymlinks fails on a path that does not exist
// yet, and workspace manifests carry exactly such paths as durable hints for
// checkouts that are not present. A missing root therefore keeps its lexical
// spelling and no error. Any OTHER failure -- a resolution loop, an unreadable
// ancestor, a network mount that is up enough to stat but not to walk -- is
// REPORTED rather than silently degraded to the lexical spelling: degrading
// would mint exactly the second key this function exists to prevent, and the
// brain the repository already has would be invisible until the mount came
// back, at which point both would exist and every command would refuse.
func localRepoStorageKey(repoDir string) (string, error) {
	root, err := canonicalLocalRepoRoot(repoDir)
	if err != nil {
		return "", err
	}
	return localRepoStorageKeyForSpelling(root), nil
}

// localRepoStorageKeyForSpelling hashes the spelling it is handed, verbatim.
// It is for ALTERNATE identities only -- the historical spellings a brain may
// already be stored under. Deriving the identity of a repository with it is the
// defect; use localRepoStorageKey.
func localRepoStorageKeyForSpelling(repoDir string) string {
	return filepath.ToSlash(filepath.Join("local", localRepoKey(repoDir)))
}

// canonicalLocalRepoRoot resolves every component of a repository root. See
// localRepoStorageKey for why, and for the error contract.
func canonicalLocalRepoRoot(repoDir string) (string, error) {
	if repoDir == "" {
		return "", nil
	}
	cleaned := filepath.Clean(repoDir)
	resolved, err := filepath.EvalSymlinks(cleaned)
	switch {
	case err == nil:
		return filepath.Clean(resolved), nil
	case errors.Is(err, os.ErrNotExist):
		// A root that is not here yet is a durable hint, not an identity to
		// refuse; keep the caller's spelling so the hint still resolves to the
		// same key when the checkout appears.
		return cleaned, nil
	default:
		return "", fmt.Errorf("resolve local repo root %s: %w", cleaned, err)
	}
}

// rootSymlinkResolvedLocalRepoDir resolves only a symlink in the repository
// root's FINAL path component.
//
// This was the canonical identity until full resolution replaced it, and it is
// kept for exactly two jobs. It is the directory git is invoked in, so a
// repository addressed through an alias is interrogated at its target. And it
// is one of the alternate key spellings a brain may already be stored under --
// see repoStorageIdentity.AlternateKeys -- which is why the rule it implements
// must not drift: an old store is only found again if this reproduces the
// spelling that created it.
//
// Missing paths remain untouched because workspace manifests use them as
// durable hints that may become available again later. Broken final-component
// links likewise retain the original hint until their targets exist.
func rootSymlinkResolvedLocalRepoDir(repoDir string) (string, error) {
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
	return repoKeyFromRemoteWithSlug(configDir, remote, repoDomainSlug)
}

func repoKeyFromRemoteWithSlug(configDir, remote string, slugForHost func(string, string) (string, error)) (string, bool, error) {
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
	slug, err := slugForHost(configDir, host)
	if err != nil {
		return "", true, err
	}
	components := []string{slug}
	components = append(components, repoPath...)
	key := strings.Join(components, "/")
	// Mint no key the store will not accept. The slug is generated as three
	// letters, so this can only fire for a hand-edited DomainSlugs entry — the
	// case brainDirForKey's reserved-segment refusal exists for. Catching it here
	// reports the config that produced the key rather than failing later in
	// whichever consumer happens to build a path first.
	if err := rejectReservedRepoKeySegment(key); err != nil {
		return "", true, err
	}
	return key, true, nil
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

// localRepoKey hashes one path spelling into a store directory name. It is the
// raw primitive: reach it through localRepoStorageKey (which resolves the root
// first, and is what a repository's identity is) or, for a historical spelling,
// localRepoStorageKeyForSpelling.
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

// lookupRepoDomainSlug reads an established binding without allocating one or
// quarantining malformed configuration. A getter must not redefine identity.
func lookupRepoDomainSlug(configDir, host string) (string, error) {
	if slug, ok := knownRepoDomainSlugs[host]; ok {
		return slug, nil
	}
	path, err := config.Path(configDir)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read host bindings: %w", err)
	}
	if err == nil {
		var cfg config.Config
		if err := json.Unmarshal(data, &cfg); err != nil {
			return "", fmt.Errorf("read host bindings: %w", err)
		}
		if slug := cfg.DomainSlugs[host]; slug != "" {
			return slug, nil
		}
	}
	return "", fmt.Errorf("unresolved_repo_host: %q has no stored host binding; run setup or refresh in its checkout, or use path --ensure to allocate it", host)
}
