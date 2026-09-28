package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/entire-brain/internal/config"
)

// A repo key is untrusted input that becomes a filesystem path, and the rules
// for turning one into a path live in brainDirForKey: refuse the reserved
// "workspaces" segment, refuse a symlinked store root, refuse a symlinked
// component. Three places built a store path WITHOUT going through it —
// `brain path` twice, and headPathForKey — so a key every other command refuses
// was still accepted there. These tests pin that every construction site now
// answers to the same rules.

func reserveWorkspacesSlug(t *testing.T, configDir, host string) {
	t.Helper()
	if _, err := config.Update(configDir, func(cfg *config.Config) error {
		if cfg.DomainSlugs == nil {
			cfg.DomainSlugs = make(map[string]string)
		}
		cfg.DomainSlugs[host] = workspaceDirName
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestRepoKeyFromRemoteRefusesReservedSegment mints the key where the bad slug
// enters, so the failure names the configuration that caused it rather than
// surfacing later in whichever consumer happens to build a path first.
func TestRepoKeyFromRemoteRefusesReservedSegment(t *testing.T) {
	t.Parallel()
	configDir := t.TempDir()
	reserveWorkspacesSlug(t, configDir, "git.example.test")

	key, ok, err := repoKeyFromRemote(configDir, "https://git.example.test/acme/repo.git")
	if err == nil {
		t.Fatalf("repoKeyFromRemote minted reserved key %q (ok=%v)", key, ok)
	}
	if !strings.Contains(err.Error(), workspaceDirName) {
		t.Fatalf("error does not name the reserved segment: %v", err)
	}

	// An ordinary host must still resolve.
	if _, ok, err := repoKeyFromRemote(configDir, "https://github.com/entireio/cli.git"); err != nil || !ok {
		t.Fatalf("ordinary remote rejected: ok=%v err=%v", ok, err)
	}
}

// TestPathRefusesReservedRepoKeySegment is the end-to-end shape: `brain path`
// joined the key onto the store root directly, so it printed a directory the
// reserved-segment rule exists to keep nothing in — a location no other verb
// will read or write.
func TestPathRefusesReservedRepoKeySegment(t *testing.T) {
	t.Parallel()
	configDir := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "data")
	reserveWorkspacesSlug(t, configDir, "git.example.test")

	cmd := NewRootCommand(Options{
		Version: "test-version",
		Env: EntireEnv{
			PluginConfigDir: configDir,
			PluginDataDir:   dataDir,
			PluginStateDir:  filepath.Join(t.TempDir(), "state"),
			PluginCacheDir:  filepath.Join(t.TempDir(), "cache"),
		},
		Runner: &fakeCommandRunner{responses: map[string]fakeCommandResponse{}},
	})

	out, err := execute(t, cmd, "path", "https://git.example.test/acme/repo.git")
	if err == nil {
		t.Fatalf("path printed a reserved store location: %q", out)
	}
	if strings.Contains(out, filepath.Join(dataDir, repoStoreDirName, workspaceDirName)) {
		t.Fatalf("path leaked the reserved location before failing: %q", out)
	}
}

// TestPathRefusesSymlinkedStoreRoot pins the other half: `brain path` must not
// advertise a location the commands that use it would refuse. Every verb that
// resolves a brain through brainDirForKey rejects a symlinked repos/ root, so a
// `path` that prints one hands the caller a directory the tool will not honour.
func TestPathRefusesSymlinkedStoreRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	elsewhere := filepath.Join(root, "elsewhere")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(dataDir, repoStoreDirName)); err != nil {
		t.Fatal(err)
	}

	cmd := NewRootCommand(Options{
		Version: "test-version",
		Env: EntireEnv{
			PluginConfigDir: t.TempDir(),
			PluginDataDir:   dataDir,
			PluginStateDir:  filepath.Join(t.TempDir(), "state"),
			PluginCacheDir:  filepath.Join(t.TempDir(), "cache"),
		},
		Runner: &fakeCommandRunner{responses: map[string]fakeCommandResponse{}},
	})

	if out, err := execute(t, cmd, "path", "https://github.com/entireio/cli.git"); err == nil {
		t.Fatalf("path printed a brain dir under a symlinked store root: %q", out)
	}
}

// TestHeadPathForKeyAppliesTheSameGuardsAsBrainDirForKey: the head file is
// built from the same untrusted key and lands in the same per-key directory
// shape under state/. Accepting a key here that brainDirForKey refuses would
// put the two halves of a repository's storage under different rules.
func TestHeadPathForKeyAppliesTheSameGuardsAsBrainDirForKey(t *testing.T) {
	t.Parallel()
	stateDir := filepath.Join(t.TempDir(), "state")
	env := EntireEnv{
		PluginConfigDir: t.TempDir(),
		PluginDataDir:   filepath.Join(t.TempDir(), "data"),
		PluginStateDir:  stateDir,
		PluginCacheDir:  filepath.Join(t.TempDir(), "cache"),
	}

	if head, err := headPathForKey(env, workspaceDirName+"/acme/repo"); err == nil {
		t.Fatalf("headPathForKey accepted a reserved repo key: %s", head)
	}
	// Traversal out of the head store, with the store root already on disk.
	// The absent-root case is covered separately in path_guard_dotdot_test.go.
	if err := os.MkdirAll(filepath.Join(stateDir, repoStoreDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	if head, err := headPathForKey(env, ".."); err == nil {
		t.Fatalf("headPathForKey resolved %q to %s, outside the head store", "..", head)
	}

	// A symlinked component must be refused, exactly as brainDirForKey refuses it.
	linked := filepath.Join(stateDir, repoStoreDirName, "linked")
	if err := os.Symlink(t.TempDir(), linked); err != nil {
		t.Fatal(err)
	}
	if head, err := headPathForKey(env, "linked/acme/repo"); err == nil {
		t.Fatalf("headPathForKey followed a symlinked component to %s", head)
	}

	// A legitimate key must still resolve, under the head store root.
	head, err := headPathForKey(env, "gh/entireio/cli")
	if err != nil {
		t.Fatalf("legitimate key rejected: %v", err)
	}
	want := filepath.Join(stateDir, repoStoreDirName, "gh", "entireio", "cli", brainHeadFileName)
	if head != want {
		t.Fatalf("head path = %s, want %s", head, want)
	}
}
