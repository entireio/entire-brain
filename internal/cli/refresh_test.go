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

func TestDefaultRefreshAgentHonorsNoEgressMode(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("codex", "--version"):  {stdout: "codex 1.0"},
		fakeCommandKey("claude", "--version"): {stdout: "claude 1.0"},
	}}
	if got := defaultRefreshAgent(context.Background(), runner, t.TempDir()); got != "none" {
		t.Fatalf("defaultRefreshAgent in no-egress mode = %q, want none", got)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("no-egress auto-selection should not probe hosted agents, calls=%+v", runner.calls)
	}
}

func TestRefreshSeedsWhenExportFindsNoSessions(t *testing.T) {
	repoDir := seedFixtureRepo(t)
	dataDir := filepath.Join(t.TempDir(), "data")
	stateDir := filepath.Join(t.TempDir(), "state")
	runner := seedFixtureRunner(repoDir)
	addRefreshSemanticFixture(runner, repoDir)
	runner.responses[fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef)] = fakeCommandResponse{err: os.ErrNotExist}
	runner.responses[fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1OriginRef)] = fakeCommandResponse{err: os.ErrNotExist}

	cmd := NewRootCommand(Options{
		Version: "test-version",
		Env: EntireEnv{
			RepoRoot:        repoDir,
			PluginConfigDir: filepath.Join(t.TempDir(), "config"),
			PluginDataDir:   dataDir,
			PluginStateDir:  stateDir,
			PluginCacheDir:  filepath.Join(t.TempDir(), "cache"),
		},
		Runner: runner,
		Now: func() time.Time {
			return time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
		},
	})

	out, err := execute(t, cmd, "refresh", "--entire-binary", "entire-test")
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	for _, want := range []string{
		"refresh: export sessions",
		"refresh: export sessions: unavailable, using seed baseline done",
		"refresh: seed baseline: 3 documents, 2 entrypoints, 4 commands done",
		"refresh: history index: 0 records, 0 decisions, 0 tool calls done",
		"refresh: semantic index: 1 symbol, 1 relation, 1 file done",
		"refreshed brain:",
		"brain: ",
		"sources: seed=true",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("refresh output missing %q:\n%s", want, out)
		}
	}
	brainDir := filepath.Join(dataDir, repoStoreDirName, "gh", "example", "repo")
	var manifest exportManifest
	data, err := os.ReadFile(filepath.Join(brainDir, exportManifestFileName))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if manifest.Sources == nil || manifest.Sources.Seed == nil {
		t.Fatalf("refresh did not create seed source: %+v", manifest)
	}
	if manifest.Sources.History == nil {
		t.Fatalf("refresh did not create history index source: %+v", manifest.Sources)
	}
}

func TestRefreshSkipsCurrentSemanticIndex(t *testing.T) {
	repoDir := seedFixtureRepo(t)
	dataDir := filepath.Join(t.TempDir(), "data")
	stateDir := filepath.Join(t.TempDir(), "state")
	env := EntireEnv{
		RepoRoot:        repoDir,
		PluginConfigDir: filepath.Join(t.TempDir(), "config"),
		PluginDataDir:   dataDir,
		PluginStateDir:  stateDir,
		PluginCacheDir:  filepath.Join(t.TempDir(), "cache"),
	}
	runner := seedFixtureRunner(repoDir)
	addRefreshSemanticFixture(runner, repoDir)
	opts := Options{
		Version: "test-version",
		Env:     env,
		Runner:  runner,
		Now: func() time.Time {
			return time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
		},
	}

	if out, err := execute(t, NewRootCommand(opts), "refresh", "--entire-binary", "entire-test"); err != nil {
		t.Fatalf("initial refresh: %v\n%s", err, out)
	}
	runner.calls = nil
	if out, err := execute(t, NewRootCommand(opts), "refresh", "--entire-binary", "entire-test"); err != nil {
		t.Fatalf("warm refresh: %v\n%s", err, out)
	}
	for _, call := range runner.calls {
		if call.name == "entire" && len(call.args) >= 3 && call.args[0] == "sem" && call.args[1] == "snapshot" {
			t.Fatalf("warm refresh reran semantic snapshot: %+v", runner.calls)
		}
	}
}

func TestRefreshHelpShowsSimplifiedFlags(t *testing.T) {
	cmd := NewRootCommand(Options{Version: "test-version"})
	out, err := execute(t, cmd, "refresh", "--help")
	if err != nil {
		t.Fatalf("refresh help: %v", err)
	}
	for _, want := range []string{"--agent", "--force", "--output"} {
		if !strings.Contains(out, want) {
			t.Fatalf("refresh help missing %q:\n%s", want, out)
		}
	}
	for _, hidden := range []string{"--history-index", "--force-seed", "--semantic", "--sem-binary"} {
		if strings.Contains(out, hidden) {
			t.Fatalf("refresh help exposed hidden flag %q:\n%s", hidden, out)
		}
	}
}

func TestRefreshOutputRequiresEmptyDirectoryUnlessForced(t *testing.T) {
	repoDir := seedFixtureRepo(t)
	outputDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outputDir, "existing.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatalf("write existing output file: %v", err)
	}
	runner := seedFixtureRunner(repoDir)
	cmd := NewRootCommand(Options{
		Version: "test-version",
		Env: EntireEnv{
			RepoRoot:        repoDir,
			PluginConfigDir: filepath.Join(t.TempDir(), "config"),
			PluginDataDir:   filepath.Join(t.TempDir(), "data"),
			PluginStateDir:  filepath.Join(t.TempDir(), "state"),
			PluginCacheDir:  filepath.Join(t.TempDir(), "cache"),
		},
		Runner: runner,
		Now:    time.Now,
	})
	out, err := execute(t, cmd, "refresh", "--output", outputDir)
	if err == nil || !strings.Contains(err.Error(), "output directory is not empty") {
		t.Fatalf("refresh --output err = %v\n%s", err, out)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("refresh touched runner before output validation: %+v", runner.calls)
	}
}

func TestRefreshForceOutputRefusesRepoRoot(t *testing.T) {
	repoDir := seedFixtureRepo(t)
	sentinel := filepath.Join(repoDir, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := seedFixtureRunner(repoDir)
	cmd := NewRootCommand(Options{
		Version: "test-version",
		Env: EntireEnv{
			RepoRoot:        repoDir,
			PluginConfigDir: filepath.Join(t.TempDir(), "config"),
			PluginDataDir:   filepath.Join(t.TempDir(), "data"),
			PluginStateDir:  filepath.Join(t.TempDir(), "state"),
			PluginCacheDir:  filepath.Join(t.TempDir(), "cache"),
		},
		Runner: runner,
		Now:    time.Now,
	})
	out, err := execute(t, cmd, "refresh", "--output", repoDir, "--force")
	if err == nil || !strings.Contains(err.Error(), "refuses to remove") {
		t.Fatalf("refresh --force --output repo root err = %v\n%s", err, out)
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "keep" {
		t.Fatalf("sentinel was removed or changed: %q err=%v", data, err)
	}
}

func TestHistoryIndexCurrentUsesSessionFingerprint(t *testing.T) {
	brainDir := t.TempDir()
	sessionSource := &sessionSourceManifest{
		GeneratedAt:        time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		TranscriptMode:     "compact",
		Scope:              exportScopeAll,
		LatestCheckpointID: "aaa111",
		Sessions: []exportSession{{
			SessionID:        "session-one",
			LatestCheckpoint: "aaa111",
			Branch:           "main",
			TranscriptPath:   "sessions/main/session-one.jsonl",
		}},
	}
	if err := os.MkdirAll(filepath.Join(brainDir, historyDirName), 0o700); err != nil {
		t.Fatalf("create history dir: %v", err)
	}
	indexData := []byte(`{"records":[]}`)
	if err := os.WriteFile(filepath.Join(brainDir, filepath.FromSlash(historyIndexPath)), indexData, 0o600); err != nil {
		t.Fatalf("write history index: %v", err)
	}
	sessionPath := filepath.Join(brainDir, filepath.FromSlash(sessionSource.Sessions[0].TranscriptPath))
	if err := os.MkdirAll(filepath.Dir(sessionPath), 0o700); err != nil {
		t.Fatalf("create session dir: %v", err)
	}
	if err := os.WriteFile(sessionPath, []byte("session\n"), 0o600); err != nil {
		t.Fatalf("write session: %v", err)
	}
	var warnings []string
	files, err := collectHistorySessionFiles(filepath.Join(brainDir, exportSessionsDirectory), &warnings)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("fingerprint sessions: warnings=%v err=%v", warnings, err)
	}
	manifest := &exportManifest{Sources: &brainSources{
		Sessions: sessionSource,
		History: &historySourceManifest{
			IndexPath:              historyIndexPath,
			IndexBytes:             int64(len(indexData)),
			IndexSHA256:            historyIndexBytesFingerprint(indexData),
			RecordsFingerprint:     historyRecordsFingerprint(nil),
			SessionsFingerprint:    sessionSourceFingerprint(sessionSource),
			TranscriptsFingerprint: historyTranscriptFilesFingerprint(brainDir, files),
		},
	}}
	if !historyIndexCurrent(brainDir, manifest) {
		t.Fatalf("history index should be current for matching session fingerprint")
	}
	manifest.Sources.History.TranscriptsFingerprint = ""
	if historyIndexCurrent(brainDir, manifest) {
		t.Fatal("legacy history source without transcript content identity should rebuild once")
	}
	manifest.Sources.History.TranscriptsFingerprint = historyTranscriptFilesFingerprint(brainDir, files)
	sessionSource.GeneratedAt = time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	if !historyIndexCurrent(brainDir, manifest) {
		t.Fatalf("history index should ignore session source timestamp-only changes")
	}
	future := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(sessionPath, future, future); err != nil {
		t.Fatalf("change transcript mtime: %v", err)
	}
	if !historyIndexCurrent(brainDir, manifest) {
		t.Fatal("history index should ignore transcript mtime-only changes")
	}
	sessionSource.Sessions[0].LatestCheckpoint = "bbb222"
	if historyIndexCurrent(brainDir, manifest) {
		t.Fatalf("history index should be stale when exported sessions change")
	}
}

type historyFreshnessFixture struct {
	brainDir    string
	sessionPath string
	manifest    *exportManifest
}

func newHistoryFreshnessFixture(t *testing.T, transcript []byte) historyFreshnessFixture {
	t.Helper()
	brainDir := t.TempDir()
	sessionRel := "sessions/main/session.jsonl"
	sessionPath := filepath.Join(brainDir, filepath.FromSlash(sessionRel))
	if err := os.MkdirAll(filepath.Dir(sessionPath), 0o700); err != nil {
		t.Fatalf("create session dir: %v", err)
	}
	if err := os.WriteFile(sessionPath, transcript, 0o600); err != nil {
		t.Fatalf("write session: %v", err)
	}
	sessionSource := &sessionSourceManifest{
		TranscriptMode: "compact",
		Scope:          exportScopeAll,
		Sessions: []exportSession{{
			SessionID:        "session-one",
			LatestCheckpoint: "aaa111",
			Branch:           "main",
			TranscriptPath:   sessionRel,
		}},
	}
	if err := writeBrainManifestAndReadme(brainDir, exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		Sources:       &brainSources{Sessions: sessionSource},
	}); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if _, err := writeBrainHistoryIndexAndSource(brainDir, time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC), nil); err != nil {
		t.Fatalf("build history: %v", err)
	}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if !historyIndexCurrent(brainDir, manifest) {
		t.Fatal("freshly built history index should be current")
	}
	return historyFreshnessFixture{brainDir: brainDir, sessionPath: sessionPath, manifest: manifest}
}

func TestHistoryIndexCurrentRejectsSamePathTranscriptMutation(t *testing.T) {
	original := []byte(`{"type":"agent_message","message":"Decision: keep alpha cache."}` + "\n")
	mutated := []byte(`{"type":"agent_message","message":"Decision: drop alpha cache."}` + "\n")
	if len(original) != len(mutated) {
		t.Fatalf("fixture lengths differ: %d != %d", len(original), len(mutated))
	}
	fixture := newHistoryFreshnessFixture(t, original)
	info, err := os.Stat(fixture.sessionPath)
	if err != nil {
		t.Fatalf("stat session: %v", err)
	}
	if err := os.WriteFile(fixture.sessionPath, mutated, 0o600); err != nil {
		t.Fatalf("mutate session: %v", err)
	}
	if err := os.Chtimes(fixture.sessionPath, info.ModTime(), info.ModTime()); err != nil {
		t.Fatalf("restore session mtime: %v", err)
	}
	if historyIndexCurrent(fixture.brainDir, fixture.manifest) {
		t.Fatal("same-path, same-size, same-mtime transcript mutation left history index current")
	}
	if _, err := writeBrainHistoryIndexAndSource(fixture.brainDir, time.Date(2026, 7, 17, 12, 0, 1, 0, time.UTC), nil); err != nil {
		t.Fatalf("rebuild history: %v", err)
	}
	rebuiltManifest, err := loadBrainManifest(fixture.brainDir)
	if err != nil {
		t.Fatalf("load rebuilt manifest: %v", err)
	}
	if !historyIndexCurrent(fixture.brainDir, rebuiltManifest) {
		t.Fatal("rebuilt history index should be current")
	}
	rebuilt, err := loadBrainHistoryIndex(fixture.brainDir, rebuiltManifest.Sources.History)
	if err != nil {
		t.Fatalf("load rebuilt history: %v", err)
	}
	if len(rebuilt.Records) != 1 || !strings.Contains(rebuilt.Records[0].Summary, "drop alpha") {
		t.Fatalf("rebuild reused stale transcript records: %+v", rebuilt.Records)
	}
	if err := os.WriteFile(fixture.sessionPath, []byte(`{"type":"agent_message","message":"Decision:`), 0o600); err != nil {
		t.Fatalf("corrupt session: %v", err)
	}
	if historyIndexCurrent(fixture.brainDir, rebuiltManifest) {
		t.Fatal("corrupted transcript content left history index current")
	}
	if _, err := writeBrainHistoryIndexAndSource(fixture.brainDir, time.Date(2026, 7, 17, 12, 0, 2, 0, time.UTC), nil); err != nil {
		t.Fatalf("rebuild corrupted history: %v", err)
	}
	corruptManifest, err := loadBrainManifest(fixture.brainDir)
	if err != nil {
		t.Fatalf("load corrupted-source manifest: %v", err)
	}
	corruptIndex, err := loadBrainHistoryIndex(fixture.brainDir, corruptManifest.Sources.History)
	if err != nil {
		t.Fatalf("load corrupted-source history: %v", err)
	}
	if len(corruptIndex.Records) != 0 {
		t.Fatalf("corrupted transcript retained stale records: %+v", corruptIndex.Records)
	}
	if !historyIndexCurrent(fixture.brainDir, corruptManifest) {
		t.Fatal("history should become current after safely rebuilding corrupted content")
	}
}

func TestHistoryIndexRejectsTranscriptSymlinkWithoutFollowingIt(t *testing.T) {
	fixture := newHistoryFreshnessFixture(t, []byte(`{"type":"agent_message","message":"Decision: safe local record."}`+"\n"))
	external := filepath.Join(t.TempDir(), "external.jsonl")
	secret := "Decision: external secret must never be indexed."
	if err := os.WriteFile(external, []byte(`{"type":"agent_message","message":"`+secret+`"}`+"\n"), 0o600); err != nil {
		t.Fatalf("write external transcript: %v", err)
	}
	if err := os.Remove(fixture.sessionPath); err != nil {
		t.Fatalf("remove session: %v", err)
	}
	if err := os.Symlink(external, fixture.sessionPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if historyIndexCurrent(fixture.brainDir, fixture.manifest) {
		t.Fatal("transcript symlink should make the history index stale")
	}
	index, source, err := buildBrainHistoryIndex(fixture.brainDir, time.Date(2026, 7, 17, 12, 0, 1, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("build with transcript symlink: %v", err)
	}
	for _, record := range index.Records {
		if strings.Contains(record.Summary, secret) {
			t.Fatalf("followed transcript symlink into external content: %+v", record)
		}
	}
	if len(source.Warnings) == 0 || !strings.Contains(strings.Join(source.Warnings, "\n"), "symlink") {
		t.Fatalf("symlink should be reported as a warning: %+v", source.Warnings)
	}
	if _, err := writeBrainHistoryIndexAndSource(fixture.brainDir, time.Date(2026, 7, 17, 12, 0, 2, 0, time.UTC), nil); err != nil {
		t.Fatalf("publish safe index with excluded symlink: %v", err)
	}
	stableManifest, err := loadBrainManifest(fixture.brainDir)
	if err != nil {
		t.Fatalf("load safe manifest: %v", err)
	}
	if !historyIndexCurrent(fixture.brainDir, stableManifest) {
		t.Fatal("unchanged safely-excluded symlink should not force an endless rebuild")
	}
}

func addRefreshSemanticFixture(runner *fakeCommandRunner, repoDir string) {
	runner.responses[fakeCommandKey("git", "rev-parse", "HEAD")] = fakeCommandResponse{stdout: "aaa111\n"}
	runner.responses[fakeCommandKey("git", "rev-parse", "HEAD^{tree}")] = fakeCommandResponse{stdout: "tree111\n"}
	runner.responses[fakeCommandKey("git", "branch", "--show-current")] = fakeCommandResponse{stdout: "main\n"}
	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{stdout: ""}
	runner.responses[fakeCommandKey("git", "status", "--porcelain", "--untracked-files=all")] = fakeCommandResponse{stdout: ""}
	runner.responses[fakeCommandKey("git", "diff", "--binary", "HEAD")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "diff", "--cached", "--binary", "HEAD")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("entire", "sem", "doctor", "--json")] = fakeCommandResponse{stdout: `{"no_egress":true}`}
	runner.responses[fakeCommandKey("entire", "sem", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network")] = fakeCommandResponse{stdout: semanticFixtureSnapshot("1.0")}
}

func TestRefreshAllBranchesRequiresSemanticBeforeMutation(t *testing.T) {
	runner := &fakeCommandRunner{
		responses: map[string]fakeCommandResponse{
			fakeCommandKey("git", "rev-parse", "--show-toplevel"): {err: errors.New("must not resolve repo")},
		},
	}
	cmd := &cobra.Command{Use: "refresh"}
	err := runRefresh(context.Background(), cmd, Options{
		Env: EntireEnv{
			RepoRoot:        t.TempDir(),
			PluginConfigDir: filepath.Join(t.TempDir(), "config"),
			PluginDataDir:   filepath.Join(t.TempDir(), "data"),
			PluginStateDir:  filepath.Join(t.TempDir(), "state"),
			PluginCacheDir:  filepath.Join(t.TempDir(), "cache"),
		},
		Runner: runner,
		Now:    time.Now,
	}, refreshCommandOptions{allBranches: true})
	if err == nil || !strings.Contains(err.Error(), "--all-branches requires --semantic") {
		t.Fatalf("refresh err = %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("refresh mutated before validation: %+v", runner.calls)
	}
}

func TestRefreshSemanticWorktreePassesWorktreeToIndex(t *testing.T) {
	repoDir := seedFixtureRepo(t)
	dataDir := filepath.Join(t.TempDir(), "data")
	runner := seedFixtureRunner(repoDir)
	runner.responses[fakeCommandKey("git", "rev-parse", "HEAD")] = fakeCommandResponse{stdout: "aaa111\n"}
	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{stdout: " M internal/cli/semantic.go\n"}
	runner.responses[fakeCommandKey("git", "diff", "--binary", "HEAD")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "diff", "--cached", "--binary", "HEAD")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "rev-parse", "HEAD^{tree}")] = fakeCommandResponse{stdout: "tree111\n"}
	runner.responses[fakeCommandKey("entire", "sem", "doctor", "--json")] = fakeCommandResponse{stdout: `{"no_egress":true}`}
	runner.responses[fakeCommandKey("entire", "sem", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network", "--worktree")] = fakeCommandResponse{stdout: semanticFixtureSnapshot("1.0")}

	cmd := NewRootCommand(Options{
		Version: "test-version",
		Env: EntireEnv{
			RepoRoot:        repoDir,
			PluginConfigDir: filepath.Join(t.TempDir(), "config"),
			PluginDataDir:   dataDir,
			PluginStateDir:  filepath.Join(t.TempDir(), "state"),
			PluginCacheDir:  filepath.Join(t.TempDir(), "cache"),
		},
		Runner: runner,
		Now:    time.Now,
	})
	if _, err := execute(t, cmd, "refresh", "--semantic", "--semantic-worktree"); err != nil {
		t.Fatalf("refresh --semantic --semantic-worktree: %v", err)
	}
	if !fakeRunnerCalled(runner, "entire", "sem", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network", "--worktree") {
		t.Fatalf("semantic snapshot was not called with --worktree: %+v", runner.calls)
	}
}

func TestRefreshSeedWorktreeDoesNotPassSemanticWorktree(t *testing.T) {
	repoDir := seedFixtureRepo(t)
	dataDir := filepath.Join(t.TempDir(), "data")
	runner := seedFixtureRunner(repoDir)
	runner.responses[fakeCommandKey("git", "rev-parse", "HEAD")] = fakeCommandResponse{stdout: "aaa111\n"}
	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{stdout: ""}
	runner.responses[fakeCommandKey("git", "rev-parse", "HEAD^{tree}")] = fakeCommandResponse{stdout: "tree111\n"}
	runner.responses[fakeCommandKey("entire", "sem", "doctor", "--json")] = fakeCommandResponse{stdout: `{"no_egress":true}`}
	runner.responses[fakeCommandKey("entire", "sem", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network")] = fakeCommandResponse{stdout: semanticFixtureSnapshot("1.0")}

	cmd := NewRootCommand(Options{
		Version: "test-version",
		Env: EntireEnv{
			RepoRoot:        repoDir,
			PluginConfigDir: filepath.Join(t.TempDir(), "config"),
			PluginDataDir:   dataDir,
			PluginStateDir:  filepath.Join(t.TempDir(), "state"),
			PluginCacheDir:  filepath.Join(t.TempDir(), "cache"),
		},
		Runner: runner,
		Now:    time.Now,
	})
	if _, err := execute(t, cmd, "refresh", "--semantic", "--worktree"); err != nil {
		t.Fatalf("refresh --semantic --worktree: %v", err)
	}
	if !fakeRunnerCalled(runner, "entire", "sem", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network") {
		t.Fatalf("semantic snapshot was not called without --worktree: %+v", runner.calls)
	}
	if fakeRunnerCalled(runner, "entire", "sem", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network", "--worktree") {
		t.Fatalf("seed --worktree leaked into semantic snapshot: %+v", runner.calls)
	}
}

func TestPathMaterializesSeedWhenExportUnavailable(t *testing.T) {
	repoDir := seedFixtureRepo(t)
	dataDir := filepath.Join(t.TempDir(), "data")
	runner := seedFixtureRunner(repoDir)
	runner.responses[fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef)] = fakeCommandResponse{err: os.ErrNotExist}
	runner.responses[fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1OriginRef)] = fakeCommandResponse{err: os.ErrNotExist}

	cmd := NewRootCommand(Options{
		Version: "test-version",
		Env: EntireEnv{
			PluginConfigDir: filepath.Join(t.TempDir(), "config"),
			PluginDataDir:   dataDir,
			PluginStateDir:  filepath.Join(t.TempDir(), "state"),
			PluginCacheDir:  filepath.Join(t.TempDir(), "cache"),
		},
		Runner: runner,
	})
	out, err := execute(t, cmd, "path", "--entire-binary", "entire-test", repoDir)
	if err != nil {
		t.Fatalf("path: %v\n%s", err, out)
	}
	brainDir := filepath.Join(dataDir, repoStoreDirName, "gh", "example", "repo")
	if out != brainDir+"\n" {
		t.Fatalf("path output = %q, want %q", out, brainDir+"\n")
	}
	if _, err := os.Stat(filepath.Join(brainDir, seedDirName, "repo-overview.md")); err != nil {
		t.Fatalf("path did not create seed: %v", err)
	}
}
