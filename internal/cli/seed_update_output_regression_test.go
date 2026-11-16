package cli

// Regression tests for issue #637: `refresh seed --update --output <dir>`
// accepted any existing directory and then created or truncated lock files
// under <dir>/locks/, clobbering a foreign directory's own files.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

func seedUpdateTestOptions(t *testing.T, repoDir string) Options {
	t.Helper()
	return Options{
		Version: "test",
		Env:     EntireEnv{RepoRoot: repoDir, PluginConfigDir: t.TempDir(), PluginDataDir: t.TempDir(), PluginStateDir: t.TempDir(), PluginCacheDir: t.TempDir()},
		Runner:  seedFixtureRunner(repoDir),
		Now:     func() time.Time { return time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC) },
	}
}

func TestSeedUpdateExplicitOutputRefusesNonBrainDirectory(t *testing.T) {
	repoDir := seedFixtureRepo(t)
	opts := seedUpdateTestOptions(t, repoDir)
	output := filepath.Join(t.TempDir(), "out")
	ownerPath := filepath.Join(output, "locks", "manifest.lock.owner")
	if err := os.MkdirAll(filepath.Dir(ownerPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ownerPath, []byte("my own notes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := executeSplit(t, NewRootCommand(opts), "refresh", "seed", "--agent", "none", "--update", "--output", output, repoDir)
	if err == nil || !strings.Contains(err.Error(), "must be an existing brain") {
		t.Fatalf("expected non-brain refusal, got err=%v stdout=%s stderr=%s", err, stdout, stderr)
	}
	got, readErr := os.ReadFile(ownerPath)
	if readErr != nil || string(got) != "my own notes\n" {
		t.Fatalf("foreign lock owner file modified: %q err=%v", got, readErr)
	}
}

func TestSeedUpdateExplicitOutputAcceptsMissingAndBrainDirectory(t *testing.T) {
	repoDir := seedFixtureRepo(t)
	output := filepath.Join(t.TempDir(), "missing", "out")
	stdout, stderr, err := executeSplit(t, NewRootCommand(seedUpdateTestOptions(t, repoDir)), "refresh", "seed", "--agent", "none", "--update", "--output", output, repoDir)
	if err != nil {
		t.Fatalf("update into missing directory: %v stdout=%s stderr=%s", err, stdout, stderr)
	}
	if !bmanifest.Exists(output) {
		t.Fatalf("first seed did not write a manifest in %s", output)
	}
	stdout, stderr, err = executeSplit(t, NewRootCommand(seedUpdateTestOptions(t, repoDir)), "refresh", "seed", "--agent", "none", "--update", "--output", output, repoDir)
	if err != nil {
		t.Fatalf("update into existing brain: %v stdout=%s stderr=%s", err, stdout, stderr)
	}
}
