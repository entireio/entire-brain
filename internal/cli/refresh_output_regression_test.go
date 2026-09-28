package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRefreshExplicitOutputBuildsIndependentSeedAndDocs(t *testing.T) {
	repoDir := seedFixtureRepo(t)
	runner := seedFixtureRunner(repoDir)
	opts := Options{Version: "test", Env: EntireEnv{RepoRoot: repoDir, PluginConfigDir: t.TempDir(), PluginDataDir: t.TempDir(), PluginStateDir: t.TempDir(), PluginCacheDir: t.TempDir()}, Runner: runner, Now: func() time.Time { return time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC) }}
	output := filepath.Join(t.TempDir(), "custom-brain")
	stdout, stderr, err := executeSplit(t, NewRootCommand(opts), "refresh", "--output", output, "--agent", "none", "--semantic=false", "--history-index=false")
	if err != nil {
		t.Fatalf("explicit output: %v stdout=%s stderr=%s", err, stdout, stderr)
	}
	manifest, err := loadBrainManifest(output)
	if err != nil || manifest.Sources == nil || manifest.Sources.Seed == nil || manifest.Sources.Docs == nil || manifest.Sources.Docs.Records == 0 {
		t.Fatalf("missing materialized seed/docs: %+v err=%v", manifest, err)
	}
	if manifest.Sources.Semantic != nil || manifest.Sources.History != nil {
		t.Fatalf("disabled stages unexpectedly ran: %+v", manifest.Sources)
	}
	storage, err := repoStoragePaths(context.Background(), runner, opts.Env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(storage.BrainDir, exportManifestFileName)); !os.IsNotExist(err) {
		t.Fatalf("explicit output also wrote default brain: %v", err)
	}
	if !strings.Contains(stdout+stderr, output) {
		t.Fatalf("refresh did not identify output destination: %s%s", stdout, stderr)
	}
}

func TestRefreshEmptyOutputRejectedBeforeWrites(t *testing.T) {
	repoDir := seedFixtureRepo(t)
	opts := Options{Env: EntireEnv{RepoRoot: repoDir, PluginDataDir: t.TempDir()}, Runner: seedFixtureRunner(repoDir), Now: time.Now}
	before := privacyTreeDigest(t, repoDir)
	_, _, err := executeSplit(t, NewRootCommand(opts), "refresh", "--output=", "--agent", "none")
	if err == nil || !strings.Contains(err.Error(), "--output must not be empty") {
		t.Fatalf("empty output error=%v", err)
	}
	if privacyTreeDigest(t, repoDir) != before {
		t.Fatal("invalid refresh modified source repository")
	}
}
