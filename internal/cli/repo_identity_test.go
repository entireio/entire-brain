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

// symlinkedRepoFixture builds the shape the defect actually occurs in: a
// repository whose ANCESTOR is a symlink, so the same directory has two
// absolute spellings. This is /tmp on macOS -- a symlink to /private/tmp --
// which is why a repository created under /tmp was the everyday trigger.
//
// It returns the physical root (what git and getcwd report) and the logical
// root (what an absolute ENTIRE_REPO_ROOT through the alias spells).
func symlinkedRepoFixture(t *testing.T) (physical, logical string) {
	t.Helper()
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
	resolved, err := filepath.EvalSymlinks(repoDir)
	if err != nil {
		t.Fatalf("resolve repo dir: %v", err)
	}
	logical = filepath.Join(aliasParent, "repo")
	if resolved == logical {
		t.Skip("platform collapsed the two spellings; there is nothing to split")
	}
	return resolved, logical
}

func repoIdentityTestEnv(t *testing.T) EntireEnv {
	t.Helper()
	return EntireEnv{
		PluginConfigDir: t.TempDir(),
		PluginDataDir:   t.TempDir(),
		PluginStateDir:  t.TempDir(),
		PluginCacheDir:  t.TempDir(),
	}
}

// TestConfigLaunchAndCwdLaunchShareOneBrain is the whole defect, end to end, on
// a real symlink.
//
// Two supported ways to start this binary against one repository:
//
//	a config-file launch  ENTIRE_REPO_ROOT=<absolute path>, what
//	                      `mcp --print-config` bakes into a host's MCP entry
//	a cwd launch          no ENTIRE_REPO_ROOT, the repository is simply the
//	                      working directory
//
// They used to disagree. The cwd route resolves symlinks (git reports the
// physical root; so does the getcwd syscall), while the config route kept
// whatever absolute string was printed -- and `--print-config` printed the
// UNRESOLVED path, because os.Getwd honours $PWD. So on macOS a repository
// under /tmp got one brain per route, and once both existed every repo-scoped
// command -- including brain_delete_project, the one that could have cleared
// them -- refused with repo_identity_conflict. The MCP surface had no way out.
//
// Both routes must land on one store, and the printed binding must be the
// spelling they agree on.
func TestConfigLaunchAndCwdLaunchShareOneBrain(t *testing.T) {
	physical, logical := symlinkedRepoFixture(t)
	env := repoIdentityTestEnv(t)
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		// git always answers with the physical root, whichever spelling it was
		// asked from. That asymmetry is what the two routes tripped over.
		fakeCommandKey("git", "rev-parse", "--show-toplevel"): {stdout: physical + "\n"},
		fakeCommandKey("git", "remote", "get-url", "origin"):  {err: os.ErrNotExist},
	}}

	// The cwd launch, with $PWD spelling the symlinked ancestor exactly as a
	// shell that cd'd through it would.
	t.Chdir(logical)
	cwdOpts := Options{Version: "test", Env: env, Runner: runner}
	printedRoot := mcpConfigRepoRoot(context.Background(), cwdOpts)
	if printedRoot == "" {
		t.Fatal("--print-config emitted no binding for a repository")
	}
	if resolved, err := filepath.EvalSymlinks(printedRoot); err != nil || resolved != printedRoot {
		t.Fatalf("--print-config emitted the unresolved spelling %q (resolves to %q, err %v); "+
			"a config launch would then key a different brain than a cwd launch",
			printedRoot, resolved, err)
	}

	cwdRepoDir, local, err := resolveLocalTargetRepoDir(context.Background(), runner, ".")
	if err != nil || !local {
		t.Fatalf("resolve cwd target: %v (local=%v)", err, local)
	}
	cwdStorage, err := repoStoragePaths(context.Background(), runner, env, cwdRepoDir)
	if err != nil {
		t.Fatalf("cwd launch storage: %v", err)
	}

	// The config-file launch, bound to the printed root.
	configRepoDir, local, err := resolveLocalTargetRepoDir(context.Background(), runner, printedRoot)
	if err != nil || !local {
		t.Fatalf("resolve bound target: %v (local=%v)", err, local)
	}
	configStorage, err := repoStoragePaths(context.Background(), runner, env, configRepoDir)
	if err != nil {
		t.Fatalf("config launch storage: %v", err)
	}

	if configStorage != cwdStorage {
		t.Fatalf("one repository, two brains:\n config launch: %+v\n    cwd launch: %+v", configStorage, cwdStorage)
	}

	// And an explicitly supplied LOGICAL root -- a hand-written MCP entry, or a
	// host that passes the path the user typed -- lands on the same store.
	logicalRepoDir, local, err := resolveLocalTargetRepoDir(context.Background(), runner, logical)
	if err != nil || !local {
		t.Fatalf("resolve logical target: %v (local=%v)", err, local)
	}
	logicalStorage, err := repoStoragePaths(context.Background(), runner, env, logicalRepoDir)
	if err != nil {
		t.Fatalf("logical launch storage: %v", err)
	}
	if logicalStorage != cwdStorage {
		t.Fatalf("the logical spelling opened a second brain:\n logical: %+v\n     cwd: %+v", logicalStorage, cwdStorage)
	}
}

// TestLocalRepoStorageKeyRefusesAnUnresolvableRoot pins the deliberate error
// path of resolving symlinks.
//
// EvalSymlinks fails for two very different reasons and they must not share an
// outcome. A root that is simply NOT THERE is a durable hint -- workspace
// manifests carry paths for checkouts that are not present -- so it keeps its
// lexical spelling and no error. A root that IS there but cannot be walked (a
// resolution loop here; in the field, an unreadable ancestor or a half-mounted
// network filesystem) has no determinable identity, and silently falling back
// to the lexical spelling would mint the second key this whole change exists to
// prevent: the repository's real brain would be invisible, a new one would be
// built beside it, and when the mount came back both would exist and every
// command would refuse.
func TestLocalRepoStorageKeyRefusesAnUnresolvableRoot(t *testing.T) {
	parent := t.TempDir()

	t.Run("missing root keeps its spelling", func(t *testing.T) {
		missing := filepath.Join(parent, "not-created-yet")
		root, err := canonicalLocalRepoRoot(missing)
		if err != nil {
			t.Fatalf("a missing root is a hint, not a failure: %v", err)
		}
		if root != missing {
			t.Fatalf("canonical root = %q, want the hint preserved as %q", root, missing)
		}
		key, err := localRepoStorageKey(missing)
		if err != nil {
			t.Fatalf("key for a missing root: %v", err)
		}
		if key != localRepoStorageKeyForSpelling(missing) {
			t.Fatalf("key = %q, want the lexical hint key %q", key, localRepoStorageKeyForSpelling(missing))
		}
	})

	t.Run("unresolvable root is refused", func(t *testing.T) {
		loopA := filepath.Join(parent, "loop-a")
		loopB := filepath.Join(parent, "loop-b")
		if err := os.Symlink(loopB, loopA); err != nil {
			t.Skipf("symlinks are unavailable on this platform: %v", err)
		}
		if err := os.Symlink(loopA, loopB); err != nil {
			t.Skipf("symlinks are unavailable on this platform: %v", err)
		}
		if _, err := filepath.EvalSymlinks(loopA); err == nil {
			t.Skip("platform resolved a symlink loop; nothing to refuse")
		} else if errors.Is(err, os.ErrNotExist) {
			t.Skip("platform reports a symlink loop as a missing path")
		}

		if _, err := canonicalLocalRepoRoot(loopA); err == nil {
			t.Fatal("an unresolvable root must be refused, not silently keyed on its lexical spelling")
		}
		key, err := localRepoStorageKey(loopA)
		if err == nil {
			t.Fatalf("localRepoStorageKey returned %q for an unresolvable root; want a refusal", key)
		}
		if key != "" {
			t.Fatalf("a refused key must be empty, got %q", key)
		}
	})
}

// conflictedRepoFixture leaves a repository with state under two keys: the shape
// anyone who used the tool before this fix is now stuck in.
func conflictedRepoFixture(t *testing.T) (env EntireEnv, logical string, canonical, legacy repoStorage) {
	t.Helper()
	physical, logical := symlinkedRepoFixture(t)
	env = repoIdentityTestEnv(t)
	dirs, err := resolvePluginDirs(env)
	if err != nil {
		t.Fatalf("plugin dirs: %v", err)
	}
	canonical = repoStorageForKey(dirs, testLocalRepoStorageKey(t, physical))
	legacy = repoStorageForKey(dirs, localRepoStorageKeyForSpelling(logical))
	if canonical.Key == legacy.Key {
		t.Skip("platform collapsed the two spellings; there is no conflict to build")
	}
	for _, storage := range []repoStorage{canonical, legacy} {
		if err := os.MkdirAll(storage.BrainDir, 0o700); err != nil {
			t.Fatalf("create brain %s: %v", storage.Key, err)
		}
		// A marker naming which history this is, so a repair can be shown to
		// have kept the one the caller asked for rather than merely to have
		// left A store standing.
		if err := os.WriteFile(filepath.Join(storage.BrainDir, "which-history"), []byte(storage.Key), 0o600); err != nil {
			t.Fatalf("mark brain %s: %v", storage.Key, err)
		}
		if err := os.MkdirAll(filepath.Dir(storage.HeadPath), 0o700); err != nil {
			t.Fatalf("create head dir %s: %v", storage.Key, err)
		}
		if err := os.WriteFile(storage.HeadPath, []byte(storage.Key), 0o600); err != nil {
			t.Fatalf("create head %s: %v", storage.Key, err)
		}
	}
	return env, logical, canonical, legacy
}

func repoIdentityFixtureRunner(physicalRoot string) *fakeCommandRunner {
	return &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "rev-parse", "--show-toplevel"): {stdout: physicalRoot + "\n"},
		fakeCommandKey("git", "remote", "get-url", "origin"):  {err: os.ErrNotExist},
	}}
}

// TestRepoIdentityConflictNamesACommandThatExists: the refusal every command
// gives while two stores exist used to end with "move one brain/head store
// aside and retry" -- an instruction to run `mv` by hand, with nothing behind
// it. Since the refusal is the only thing a locked-out user has, it must name a
// command.
func TestRepoIdentityConflictNamesACommandThatExists(t *testing.T) {
	env, logical, _, _ := conflictedRepoFixture(t)
	_, err := repoStoragePaths(context.Background(), nil, env, logical)
	var conflict *localRepoIdentityConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %v, want the identity conflict", err)
	}
	message := err.Error()
	if !strings.Contains(message, "repo-identity") {
		t.Fatalf("the refusal must name the command that resolves it, got: %s", message)
	}
	root := NewRootCommand(Options{Version: "test", Env: env})
	var found bool
	for _, sub := range root.Commands() {
		if sub.Name() == "repo-identity" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the refusal names `repo-identity`, but no such command is registered")
	}
}

// TestRepoIdentityResolvesTheConflictWithoutDeleting is the recovery path: the
// repair verb must read the storage set that every other verb refuses over, and
// it must not destroy the history the user did not pick.
func TestRepoIdentityResolvesTheConflictWithoutDeleting(t *testing.T) {
	env, logical, canonical, legacy := conflictedRepoFixture(t)
	physical, err := canonicalLocalRepoRoot(logical)
	if err != nil {
		t.Fatalf("canonical root: %v", err)
	}
	opts := Options{
		Version: "test",
		Env:     env,
		Runner:  repoIdentityFixtureRunner(physical),
		Now:     func() time.Time { return time.Date(2026, 9, 13, 10, 30, 0, 0, time.UTC) },
	}

	// Listing works while the conflict stands -- that is the point of it.
	listCmd := &cobra.Command{}
	var listed strings.Builder
	listCmd.SetOut(&listed)
	if err := runRepoIdentity(context.Background(), listCmd, opts, repoIdentityOptions{}, logical); err != nil {
		t.Fatalf("repo-identity must not be blocked by the conflict it repairs: %v", err)
	}
	for _, key := range []string{canonical.Key, legacy.Key} {
		if !strings.Contains(listed.String(), key) {
			t.Fatalf("listing omitted store %q:\n%s", key, listed.String())
		}
	}

	// Keep the NON-canonical store: the hard case, and the likely one, since
	// the store an existing user has been writing to is the one an older build
	// created under the old rule.
	keepCmd := &cobra.Command{}
	var kept strings.Builder
	keepCmd.SetOut(&kept)
	if err := runRepoIdentity(context.Background(), keepCmd, opts, repoIdentityOptions{keep: legacy.Key}, logical); err != nil {
		t.Fatalf("repo-identity --keep: %v", err)
	}

	// The history the caller chose is the one that survives, and it now lives
	// at the canonical key so that EVERY spelling of this repository finds it.
	// Leaving it at the old key would half-repair the repository: reachable
	// through the spelling that created it, invisible to a plain `cd`-and-run.
	surviving, err := os.ReadFile(filepath.Join(canonical.BrainDir, "which-history"))
	if err != nil {
		t.Fatalf("the kept history must live at the canonical key: %v", err)
	}
	if string(surviving) != legacy.Key {
		t.Fatalf("the canonical store holds %q, want the kept history %q", surviving, legacy.Key)
	}
	head, err := os.ReadFile(canonical.HeadPath)
	if err != nil {
		t.Fatalf("the kept head store must move with its brain: %v", err)
	}
	if string(head) != legacy.Key {
		t.Fatalf("the canonical head holds %q, want the kept history %q", head, legacy.Key)
	}
	if _, err := os.Stat(legacy.BrainDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the old key must be vacated, stat says: %v", err)
	}

	// Nothing was deleted: the history the caller did NOT pick is still on disk
	// under a dated name, so a wrong choice is undone with mv.
	suffix := repoIdentityConflictSuffix(opts.Now())
	retired, err := os.ReadFile(filepath.Join(canonical.BrainDir+suffix, "which-history"))
	if err != nil {
		t.Fatalf("the store that was not kept must be MOVED, not deleted: %v", err)
	}
	if string(retired) != canonical.Key {
		t.Fatalf("the retired store holds %q, want %q", retired, canonical.Key)
	}
	if _, err := os.Stat(filepath.Dir(canonical.HeadPath) + suffix); err != nil {
		t.Fatalf("the retired head store is missing: %v", err)
	}

	// And every route into the repository now opens that one store.
	for _, spelling := range []string{logical, physical} {
		storage, err := repoStoragePaths(context.Background(), opts.Runner, env, spelling)
		if err != nil {
			t.Fatalf("the repository is still locked out through %s: %v", spelling, err)
		}
		if storage.Key != canonical.Key {
			t.Fatalf("storage key through %s = %q, want the single repaired store %q", spelling, storage.Key, canonical.Key)
		}
	}
}

// TestRepoIdentityRefusesAKeyThatIsNotAStore: --keep is a destructive-looking
// choice made from a key the user copied from an error message. A typo must
// name the real keys back rather than move every store aside.
func TestRepoIdentityRefusesAKeyThatIsNotAStore(t *testing.T) {
	env, logical, canonical, legacy := conflictedRepoFixture(t)
	physical, err := canonicalLocalRepoRoot(logical)
	if err != nil {
		t.Fatalf("canonical root: %v", err)
	}
	opts := Options{Version: "test", Env: env, Runner: repoIdentityFixtureRunner(physical)}
	cmd := &cobra.Command{}
	cmd.SetOut(&strings.Builder{})
	err = runRepoIdentity(context.Background(), cmd, opts, repoIdentityOptions{keep: "local/not-a-real-key"}, logical)
	if err == nil {
		t.Fatal("--keep with an unknown key must be refused")
	}
	for _, key := range []string{canonical.Key, legacy.Key} {
		if !strings.Contains(err.Error(), key) {
			t.Fatalf("the refusal must name the real stores, got: %v", err)
		}
	}
	for _, storage := range []repoStorage{canonical, legacy} {
		if _, statErr := os.Stat(storage.BrainDir); statErr != nil {
			t.Fatalf("a refused --keep must move nothing; %s: %v", storage.Key, statErr)
		}
	}
}

// TestMCPDeleteProjectIsNotBlockedByTheIdentityConflict is the MCP half of the
// recovery.
//
// brain_delete_project resolved its target through the same refusal as every
// other tool, so the one call capable of clearing a duplicate store was blocked
// by the duplicate store. With no CLI in reach, an agent holding only the MCP
// surface had no recovery at all.
//
// A delete has nothing to disambiguate -- both stores ARE this repository -- so
// it erases them all, and the repository works again afterwards.
func TestMCPDeleteProjectIsNotBlockedByTheIdentityConflict(t *testing.T) {
	env, logical, canonical, legacy := conflictedRepoFixture(t)
	physical, err := canonicalLocalRepoRoot(logical)
	if err != nil {
		t.Fatalf("canonical root: %v", err)
	}
	env.RepoRoot = logical
	opts := Options{Version: "test", Env: env, Runner: repoIdentityFixtureRunner(physical)}

	// The conflict really is in force for ordinary tools.
	if _, _, err := mcpBoundRepoStorage(context.Background(), opts); err == nil {
		t.Fatal("fixture did not produce an identity conflict")
	}

	cmd := &cobra.Command{}
	var out strings.Builder
	cmd.SetOut(&out)
	if err := runMCPDeleteProject(context.Background(), cmd, opts, ""); err != nil {
		t.Fatalf("brain_delete_project must not be blocked by the conflict it can resolve: %v", err)
	}
	for _, storage := range []repoStorage{canonical, legacy} {
		if _, err := os.Stat(storage.BrainDir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("store %s survived the delete: %v", storage.Key, err)
		}
		// Both halves, not just the brain. repoStorageContainsState counts the
		// head store too, so a brain erased with its head left behind is still
		// a second identity and the conflict outlives the delete.
		if _, err := os.Stat(filepath.Dir(storage.HeadPath)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("head store for %s survived the delete: %v", storage.Key, err)
		}
	}

	var payload struct {
		DeletedRepoKey string   `json:"deleted_repo_key"`
		AlsoDeleted    []string `json:"also_deleted_repo_keys"`
	}
	if err := json.Unmarshal([]byte(out.String()), &payload); err != nil {
		t.Fatalf("decode delete result %q: %v", out.String(), err)
	}
	reported := append([]string{payload.DeletedRepoKey}, payload.AlsoDeleted...)
	for _, key := range []string{canonical.Key, legacy.Key} {
		var named bool
		for _, got := range reported {
			if got == key {
				named = true
			}
		}
		if !named {
			t.Fatalf("the delete erased %q without reporting it: %+v", key, payload)
		}
	}

	// The repository is usable through MCP again.
	if _, _, err := mcpBoundRepoStorage(context.Background(), opts); err != nil {
		t.Fatalf("the conflict survived the delete that was supposed to end it: %v", err)
	}
}
