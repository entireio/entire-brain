package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// watchCommandFixture supplies the real refresh route with a local repository,
// while keeping every command it can invoke inside the fake runner.  In
// particular, the semantic snapshot is local fixture data and the default
// watch flags leave both agent steps disabled.
func watchCommandFixture(t *testing.T) (Options, string, *fakeCommandRunner, repoStorage, *[]string) {
	t.Helper()
	oldMemoryWorkerLaunch := memoryWorkerLaunch
	var memoryWorkerLaunches []string
	memoryWorkerLaunch = func(repoDir string) error {
		memoryWorkerLaunches = append(memoryWorkerLaunches, repoDir)
		return nil
	}
	t.Cleanup(func() { memoryWorkerLaunch = oldMemoryWorkerLaunch })
	repoDir := seedFixtureRepo(t)
	runner := seedFixtureRunner(repoDir)
	for key, response := range semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.1")).responses {
		runner.responses[key] = response
	}
	env := EntireEnv{
		RepoRoot: repoDir, PluginConfigDir: t.TempDir(), PluginDataDir: t.TempDir(),
		PluginStateDir: t.TempDir(), PluginCacheDir: t.TempDir(),
	}
	opts := Options{
		Version: "test", Env: env, Runner: runner,
		Now: func() time.Time { return time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC) },
	}
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatalf("repo storage: %v", err)
	}
	return opts, repoDir, runner, storage, &memoryWorkerLaunches
}

func assertMemoryWorkerLaunches(t *testing.T, launches []string, repoDir string, want int) {
	t.Helper()
	if len(launches) != want {
		t.Fatalf("memory worker launch count = %d, want %d (%v)", len(launches), want, launches)
	}
	for _, got := range launches {
		if got != repoDir {
			t.Fatalf("memory worker launch repo = %q, want %q", got, repoDir)
		}
	}
}

func assertWatchDefaultDidNotSpend(t *testing.T, cursor watchCursor, output string) {
	t.Helper()
	if cursor.LastRefreshAt.IsZero() || cursor.LastFingerprint == "" {
		t.Fatalf("deterministic refresh was not persisted: %+v", cursor)
	}
	if !cursor.LastAgentSpendAt.IsZero() {
		t.Fatalf("default watch must not invoke an agent: %+v", cursor)
	}
	if !strings.Contains(output, "refreshed (deterministic, no agent tokens)") {
		t.Fatalf("missing deterministic refresh result:\n%s", output)
	}
}

func TestWatchRootCommandOnceRefreshesThenRepeatsAsNoop(t *testing.T) {
	opts, repoDir, _, storage, launches := watchCommandFixture(t)
	cursorPath := filepath.Join(filepath.Dir(storage.HeadPath), "watch.json")

	out, err := execute(t, NewRootCommand(opts), "watch", repoDir, "--once")
	if err != nil {
		t.Fatalf("root watch once: %v\n%s", err, out)
	}
	first := readWatchCursor(t, cursorPath)
	assertWatchDefaultDidNotSpend(t, first, out)

	out, err = execute(t, NewRootCommand(opts), "watch", repoDir, "--once")
	if err != nil {
		t.Fatalf("second root watch once: %v\n%s", err, out)
	}
	if got := readWatchCursor(t, cursorPath); got != first {
		t.Fatalf("unchanged root watch rewrote cursor: got=%+v want=%+v", got, first)
	}
	if !strings.Contains(out, "no change; nothing to do") {
		t.Fatalf("second root watch did not take no-op path:\n%s", out)
	}
	assertMemoryWorkerLaunches(t, *launches, repoDir, 3)
}

func TestWorkspaceWatchCommandOnceUsesMemberCursorWithoutAgent(t *testing.T) {
	opts, repoDir, _, storage, launches := watchCommandFixture(t)
	manifest := workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "watch-fixture",
		Repos:         []workspaceRepo{{RepoKey: storage.Key, LocalPathHint: repoDir}},
	}
	if err := writeWorkspaceManifest(opts.Env, manifest); err != nil {
		t.Fatal(err)
	}
	cursorPath := filepath.Join(filepath.Dir(storage.HeadPath), "watch.json")

	out, err := execute(t, NewRootCommand(opts), "workspace", "watch", manifest.Name, "--once")
	if err != nil {
		t.Fatalf("workspace watch once: %v\n%s", err, out)
	}
	assertWatchDefaultDidNotSpend(t, readWatchCursor(t, cursorPath), out)
	if !strings.Contains(out, "workspace "+manifest.Name+" — 1 repos") || !strings.Contains(out, storage.Key) {
		t.Fatalf("workspace member was not routed through the real watch pass:\n%s", out)
	}
	assertMemoryWorkerLaunches(t, *launches, repoDir, 2)
}

func TestSupervisedWorkspaceWatchOnceReadsPlanAndRefreshes(t *testing.T) {
	opts, repoDir, _, storage, launches := watchCommandFixture(t)
	manifest := workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "supervised-fixture",
		Repos:         []workspaceRepo{{RepoKey: storage.Key, LocalPathHint: repoDir}},
	}
	if err := writeWorkspaceManifest(opts.Env, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := recordSetupWatchPlan(opts.Env, "test-daemon", setupWatchPlanEntry{Workspace: manifest.Name, Interval: "1h"}); err != nil {
		t.Fatalf("record watch plan: %v", err)
	}
	cursorPath := filepath.Join(filepath.Dir(storage.HeadPath), "watch.json")

	out, err := execute(t, NewRootCommand(opts), "workspace", "watch", "--once")
	if err != nil {
		t.Fatalf("supervised workspace watch once: %v\n%s", err, out)
	}
	assertWatchDefaultDidNotSpend(t, readWatchCursor(t, cursorPath), out)
	if !strings.Contains(out, "workspace "+manifest.Name+" — 1 repos") {
		t.Fatalf("supervised route did not dispatch planned workspace:\n%s", out)
	}
	assertMemoryWorkerLaunches(t, *launches, repoDir, 2)
}

func TestWatchRootCommandRejectsMissingTargetBeforeWritingCursor(t *testing.T) {
	opts, _, _, _, launches := watchCommandFixture(t)
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	out, err := execute(t, NewRootCommand(opts), "watch", missing, "--once")
	if err == nil || !strings.Contains(err.Error(), "requires a local repository path") {
		t.Fatalf("missing target error = %v\n%s", err, out)
	}
	var cursors []string
	if walkErr := filepath.WalkDir(opts.Env.PluginDataDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && entry.Name() == "watch.json" {
			cursors = append(cursors, path)
		}
		return nil
	}); walkErr != nil && !os.IsNotExist(walkErr) {
		t.Fatalf("inspect refused target writes: %v", walkErr)
	}
	if len(cursors) != 0 {
		t.Fatalf("refused target wrote watch cursors: %v", cursors)
	}
	assertMemoryWorkerLaunches(t, *launches, "", 0)
}
