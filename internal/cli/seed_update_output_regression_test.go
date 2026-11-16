package cli

// Regression tests for issue #637: `refresh seed --update --output <dir>`
// accepted any existing directory and then created or truncated lock files
// under <dir>/locks/, clobbering a foreign directory's own files.

import (
	"context"
	"encoding/json"
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

func TestSeedUpdateExplicitOutputRefusesForeignManifestJSON(t *testing.T) {
	repoDir := seedFixtureRepo(t)
	opts := seedUpdateTestOptions(t, repoDir)
	output := t.TempDir()
	foreign := []byte(`{"name":"my extension","manifest_version":3}`)
	manifestPath := filepath.Join(output, "manifest.json")
	if err := os.WriteFile(manifestPath, foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := executeSplit(t, NewRootCommand(opts), "refresh", "seed", "--agent", "none", "--update", "--output", output, repoDir)
	if err == nil || !strings.Contains(err.Error(), "must be an existing brain") {
		t.Fatalf("expected non-brain refusal, got err=%v stdout=%s stderr=%s", err, stdout, stderr)
	}
	got, readErr := os.ReadFile(manifestPath)
	if readErr != nil || string(got) != string(foreign) {
		t.Fatalf("foreign manifest.json modified: %q err=%v", got, readErr)
	}
	if _, statErr := os.Stat(filepath.Join(output, "locks")); !os.IsNotExist(statErr) {
		t.Fatalf("seed wrote locks/ into a foreign directory: %v", statErr)
	}
}

func TestSeedUpdateExplicitOutputAcceptsLargeBrainManifest(t *testing.T) {
	// Real manifests grow with the session catalog — a long-lived repo's is
	// over 1 MiB on disk — so the ownership probe must not cap the read at a
	// size a genuine brain exceeds.
	repoDir := seedFixtureRepo(t)
	output := filepath.Join(t.TempDir(), "out")
	if _, _, err := executeSplit(t, NewRootCommand(seedUpdateTestOptions(t, repoDir)), "refresh", "seed", "--agent", "none", "--update", "--output", output, repoDir); err != nil {
		t.Fatalf("first seed: %v", err)
	}
	manifestPath := filepath.Join(output, "manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	m["warnings"] = []string{strings.Repeat("x", 2<<20)}
	bloated, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, bloated, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := executeSplit(t, NewRootCommand(seedUpdateTestOptions(t, repoDir)), "refresh", "seed", "--agent", "none", "--update", "--output", output, repoDir); err != nil {
		t.Fatalf("update refused a brain with a large manifest: %v", err)
	}
}

func TestRefreshSeedPhaseBaselineAllowedWithoutManifest(t *testing.T) {
	// A failed export leaves sessions/ without manifest.json; refresh then runs
	// seed as the baseline (refresh.go needSeed on exportErr). The --update
	// output gate must not refuse that refresh-owned directory.
	repoDir := seedFixtureRepo(t)
	opts := seedUpdateTestOptions(t, repoDir)
	output := t.TempDir()
	if err := os.MkdirAll(filepath.Join(output, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	seed := seedCommandOptions{
		includeTests:       true,
		maxFileBytes:       defaultSeedMaxFileBytes,
		maxFiles:           defaultSeedMaxFiles,
		format:             "markdown+json",
		agent:              "none",
		agentTimeoutAction: "keep-quick",
		agentMaxInputBytes: defaultAgentMaxInput,
	}
	if err := runRefreshSeedPhase(context.Background(), opts, seed, repoDir, output, true, func(string) {}); err != nil {
		t.Fatalf("baseline seed after failed export refused: %v", err)
	}
	if !bmanifest.Exists(output) {
		t.Fatalf("baseline seed did not write a manifest in %s", output)
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
