package cli

import (
	"context"
	"fmt"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"github.com/entireio/entire-brain/internal/cli/workspace"
	"github.com/spf13/cobra"
)

// workerPrepassSweepFixture establishes a checkpoint-backed persistent brain
// the same way a real foreground export does, then drops a stale cached
// transcript that is eligible for the sweep (it is not a live session, so
// nothing retains it).
func workerPrepassSweepFixture(t *testing.T) (Options, string, repoStorage, string) {
	t.Helper()
	repoDir, runner := compactV1Fixture(t, true)
	runner.responses[fakeCommandKey("git", "cat-file", "-t", v1MainRef+":aa/a111aaa111/0/transcript.jsonl")] = fakeCommandResponse{stdout: "blob\n"}
	runner.responses[fakeCommandKey("git", "remote", "get-url", "origin")] = fakeCommandResponse{stdout: "https://github.com/entireio/cli.git\n"}
	opts := Options{
		Version: "test",
		Runner:  runner,
		Env:     EntireEnv{RepoRoot: repoDir, PluginDataDir: t.TempDir(), PluginStateDir: t.TempDir(), PluginCacheDir: t.TempDir()},
		Now:     func() time.Time { return time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC) },
	}
	storage, err := repoStoragePaths(context.Background(), runner, opts.Env, repoDir)
	if err != nil {
		t.Fatal(err)
	}

	if out, err := execute(t, NewRootCommand(opts), "refresh", "sessions", "--entire-binary", "entire-test"); err != nil {
		t.Fatalf("export: %v\n%s", err, out)
	}

	sweepable := "sessions/main/stale.jsonl"
	if err := writeTranscriptFile(storage.BrainDir, sweepable, []byte("stale")); err != nil {
		t.Fatal(err)
	}
	return opts, repoDir, storage, sweepable
}

// A v3 manifest is what every 0.1.0-era brain has on disk. Bumping
// bmanifest.SchemaVersion must not turn those brains unreadable.
func TestManifestV3StillReadable(t *testing.T) {
	dir := writeTestManifest(t, `{"schema_version":3}`)
	if _, err := bmanifest.Load(dir); err != nil {
		t.Fatalf("v3 manifest must still be readable: %v", err)
	}
}

// A manifest from a newer build must be refused, not misread, and the
// message must tell the reader what to do about it.
func TestManifestNewerRefusedWithRemedy(t *testing.T) {
	// Derived, not a literal: a hardcoded "5" stops meaning "newer than
	// supported" the moment bmanifest.SchemaVersion is bumped to 5, and
	// silently starts testing the wrong thing instead of failing.
	dir := writeTestManifest(t, fmt.Sprintf(`{"schema_version":%d}`, bmanifest.SchemaVersion+1))
	_, err := bmanifest.Load(dir)
	if err == nil {
		t.Fatal("newer manifest must be refused")
	}
	if !strings.Contains(err.Error(), brainstore.UnsupportedVersionCode) {
		t.Fatalf("error code: %v", err)
	}
	if !strings.Contains(err.Error(), "upgrade entire-brain") {
		t.Fatalf("remedy missing from error: %v", err)
	}
}

// Every manifest this build writes must declare the current schema version,
// so a downgraded 0.1.0 binary can detect the skew instead of misreading
// fields it was never built to understand.
func TestManifestWrittenAtV4(t *testing.T) {
	dir := t.TempDir()
	if err := writeBrainManifestAndReadme(dir, exportManifest{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, bmanifest.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"schema_version": 4`) {
		t.Fatalf("manifest was not written at schema version 4: %s", data)
	}
	manifest, err := bmanifest.Load(dir)
	if err != nil || manifest.SchemaVersion != 4 {
		t.Fatalf("round trip: manifest=%+v err=%v", manifest, err)
	}
}

// The background memory worker is not a user-invoked export: nobody asked for
// the destructive transcript cache sweep, and nobody is watching stderr to
// learn that files were removed. The worker-driven delta export must leave
// the cache alone; user-invoked export/refresh still sweeps it.
func TestWorkerPrepassSkipsTranscriptSweep(t *testing.T) {
	opts, repoDir, storage, sweepable := workerPrepassSweepFixture(t)

	if err := memoryWorkerPrepass(context.Background(), opts, repoDir); err != nil {
		t.Fatalf("worker prepass: %v", err)
	}

	if _, err := os.Stat(filepath.Join(storage.BrainDir, filepath.FromSlash(sweepable))); err != nil {
		t.Fatalf("background worker prepass swept the transcript cache: %v", err)
	}
}

// The watch daemon's per-tick delta is just as unattended as the memory
// worker prepass: it runs as a launchd daemon with nobody watching its
// (discarded) stderr, so it must never run the destructive transcript sweep
// either. Only user-invoked export/refresh may sweep.
func TestWatchTickSkipsTranscriptSweep(t *testing.T) {
	opts, repoDir, storage, sweepable := workerPrepassSweepFixture(t)

	if _, err := watchShortTermDelta(context.Background(), &cobra.Command{}, opts, repoDir); err != nil {
		t.Fatalf("watch tick: %v", err)
	}

	if _, err := os.Stat(filepath.Join(storage.BrainDir, filepath.FromSlash(sweepable))); err != nil {
		t.Fatalf("background watch tick swept the transcript cache: %v", err)
	}
}

// The watch daemon's full refresh (consolidation) runs in the SAME unattended
// launchd tick as the delta: on the first tick, on every --consolidate-every
// elapsed tick, and immediately whenever the short-term buffer overflows.
// TestWatchTickSkipsTranscriptSweep alone over-claims coverage here -- it
// drives only the delta half. This drives watchDeterministicRefresh (the
// consolidation step itself, watch.go's steps.refresh) directly, after a real
// tick has already established the persistent brain, and proves it leaves a
// stale cached transcript alone too.
func TestWatchConsolidationRefreshSkipsTranscriptSweep(t *testing.T) {
	opts, repoDir, storage, _ := watchCommandFixture(t)
	// An empty, successful routed checkpoint catalog: the fixture's local git
	// refs are unmocked, so every export phase falls through to routed
	// discovery via the "entire" binary. Without this, the FIRST tick's export
	// failure is silently papered over as "degraded, fall back to seed" (no
	// manifest exists yet to make it fatal), while the SECOND direct refresh
	// call below has a manifest by then and the identical failure turns fatal
	// -- hiding what this test means to exercise behind an unrelated fixture gap.
	opts.Runner.(*fakeCommandRunner).responses[fakeRoutedCheckpointListKey("entire", true)] = fakeCommandResponse{stdout: `[]`}

	// Establish the persistent brain the way the real daemon's first-ever tick
	// does (the first refresh always runs).
	if out, err := execute(t, NewRootCommand(opts), "watch", repoDir, "--once"); err != nil {
		t.Fatalf("watch once: %v\n%s", err, out)
	}

	sweepable := "sessions/main/stale.jsonl"
	if err := writeTranscriptFile(storage.BrainDir, sweepable, []byte("stale")); err != nil {
		t.Fatal(err)
	}

	cmd := &cobra.Command{}
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	// true: this models the daemon's own tick (watch.go's watchStepsForRepo),
	// which is unattended and always skips the sweep.
	if err := watchDeterministicRefresh(context.Background(), cmd, opts, repoDir, true); err != nil {
		t.Fatalf("watch consolidation refresh: %v", err)
	}

	if _, err := os.Stat(filepath.Join(storage.BrainDir, filepath.FromSlash(sweepable))); err != nil {
		t.Fatalf("watch consolidation refresh swept the transcript cache: %v", err)
	}
}

// `entire brain workspace refresh <name> --full` is user-invoked, not a
// background daemon tick, even though it shares watchDeterministicRefresh
// with the watch daemon's unattended consolidation step. The classification
// is per-invoker: this path must keep sweeping, with the notice, like
// `refresh`, `path --ensure`, `export`, and `setup`.
func TestWorkspaceFullRefreshSweepsTranscriptCache(t *testing.T) {
	opts, repoDir, storage, _ := watchCommandFixture(t)
	opts.Runner.(*fakeCommandRunner).responses[fakeRoutedCheckpointListKey("entire", true)] = fakeCommandResponse{stdout: `[]`}

	// Establish the persistent brain the same way a real first refresh does.
	if out, err := execute(t, NewRootCommand(opts), "watch", repoDir, "--once"); err != nil {
		t.Fatalf("watch once: %v\n%s", err, out)
	}

	manifest := workspace.Manifest{
		SchemaVersion: workspace.SchemaVersion,
		Name:          "sweep-fixture",
		Repos:         []workspace.Repo{{RepoKey: storage.Key, LocalPathHint: repoDir}},
	}
	if err := workspace.DefaultStore(opts.Env).WriteManifest(manifest); err != nil {
		t.Fatal(err)
	}

	sweepable := "sessions/main/stale.jsonl"
	if err := writeTranscriptFile(storage.BrainDir, sweepable, []byte("stale")); err != nil {
		t.Fatal(err)
	}

	out, err := execute(t, NewRootCommand(opts), "workspace", "refresh", manifest.Name, "--full")
	if err != nil {
		t.Fatalf("workspace refresh --full: %v\n%s", err, out)
	}

	if _, err := os.Stat(filepath.Join(storage.BrainDir, filepath.FromSlash(sweepable))); !os.IsNotExist(err) {
		t.Fatalf("user-invoked workspace refresh --full did not sweep the transcript cache: %v", err)
	}
}
