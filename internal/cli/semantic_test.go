package cli

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestSemanticIndexStoresProviderSnapshotAndManifest(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}

	if err := runSemanticIndex(cmd.Context(), cmd, Options{
		Version: "test",
		Env:     env,
		Runner:  runner,
		Now:     func() time.Time { return time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC) },
	}, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}

	manifestPath := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo", exportManifestFileName)
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest exportManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	semantic := manifest.Sources.Semantic
	if semantic == nil {
		t.Fatalf("semantic source missing: %+v", manifest.Sources)
	}
	if semantic.Provider != "entire-sem" || semantic.SchemaVersion != "1.0" {
		t.Fatalf("provider metadata = %+v", semantic)
	}
	if semantic.Symbols != 1 || semantic.Relations != 1 || semantic.Files != 1 {
		t.Fatalf("counts = files %d symbols %d relations %d", semantic.Files, semantic.Symbols, semantic.Relations)
	}
	if !semantic.NoEgressVerified {
		t.Fatalf("no-egress was not verified")
	}
	if _, err := os.Stat(filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo", filepath.FromSlash(semantic.SnapshotPath))); err != nil {
		t.Fatalf("snapshot not written: %v", err)
	}
	if semantic.StorePath == "" || semantic.GenerationPath == "" || semantic.MetricsPath == "" || semantic.ParseCachePath == "" {
		t.Fatalf("semantic store metadata missing: %+v", semantic)
	}
	storePath := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo", filepath.FromSlash(semantic.StorePath))
	if _, err := os.Stat(storePath); err != nil {
		t.Fatalf("semantic sqlite not written: %v", err)
	}
	if got := semanticTestSQLCount(t, storePath, "symbols"); got != 1 {
		t.Fatalf("sqlite symbols = %d, want 1", got)
	}
	if got := semanticTestSQLCount(t, storePath, "relations"); got != 1 {
		t.Fatalf("sqlite relations = %d, want 1", got)
	}
	if got := semanticTestSQLCount(t, storePath, "reverse_relations"); got != 1 {
		t.Fatalf("sqlite reverse_relations = %d, want 1", got)
	}
	if _, err := os.Stat(filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo", filepath.FromSlash(semantic.MetricsPath))); err != nil {
		t.Fatalf("metrics not written: %v", err)
	}
	if !strings.Contains(semantic.SnapshotPath, "aaa111-") {
		t.Fatalf("snapshot path is not generation-addressed: %s", semantic.SnapshotPath)
	}
	if !fakeRunnerCalled(runner, "entire", "sem", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network") {
		t.Fatalf("semantic snapshot was not invoked with --no-network: %+v", runner.calls)
	}
}

func TestBuildSemanticGenerationLeavesIncompleteTargetInPlace(t *testing.T) {
	repoDir := t.TempDir()
	brainDir := t.TempDir()
	raw := []byte(semanticFixtureSnapshot("1.0"))
	header, counts, filtered, err := filterSemanticSnapshot(raw, brainIgnore{}, repoDir)
	if err != nil {
		t.Fatalf("filter snapshot: %v", err)
	}
	targetRel := filepath.ToSlash(filepath.Join(semanticDirName, semanticGenerationsDir, "aaa111"))
	targetDir := filepath.Join(brainDir, filepath.FromSlash(targetRel))
	if err := os.MkdirAll(targetDir, 0o700); err != nil {
		t.Fatalf("create incomplete target: %v", err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, semanticSQLiteName), []byte("not sqlite"), 0o600); err != nil {
		t.Fatalf("write invalid sqlite: %v", err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, semanticMetricsName), []byte(`{"generation_id":"aaa111"}`), 0o600); err != nil {
		t.Fatalf("write metrics: %v", err)
	}
	sentinel := filepath.Join(targetDir, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	generation, _, err := buildSemanticGeneration(brainDir, repoDir, "aaa111", filtered, header, counts, time.Now().UTC())
	if err != nil {
		t.Fatalf("build generation: %v", err)
	}
	if generation == targetRel {
		t.Fatalf("generation reused incomplete target path %s", generation)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("incomplete target was moved or removed: %v", err)
	}
	storePath := filepath.Join(brainDir, filepath.FromSlash(generation), semanticSQLiteName)
	if err := validateSemanticSQLiteStore(storePath, counts.Files, counts.Symbols, counts.Relations); err != nil {
		t.Fatalf("new generation store invalid: %v", err)
	}
}

func TestValidateLiveSemanticHeaderAcceptsRepoKeyCaseOnlyDifference(t *testing.T) {
	err := validateLiveSemanticHeader(
		semanticHeader{RepoKey: "gh/suhaanthayyil/Ultron", Commit: "aaa111", Tree: "tree111"},
		"gh/suhaanthayyil/ultron",
		"aaa111",
		"tree111",
		false,
	)
	if err != nil {
		t.Fatalf("case-only repo key mismatch should be accepted: %v", err)
	}
}

func TestSemanticIndexRunsProviderCommandsWithTimeouts(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}

	if err := runSemanticIndex(cmd.Context(), cmd, Options{
		Version: "test",
		Env:     env,
		Runner:  runner,
		Now:     time.Now,
	}, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}

	for _, args := range [][]string{
		{"sem", "doctor", "--json"},
		{"sem", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network"},
	} {
		var found bool
		for _, call := range runner.calls {
			if call.name == "entire" && semanticArgsEqual(call.args, args) {
				found = true
				if !call.hasDeadline {
					t.Fatalf("provider call %q did not use a timeout context", strings.Join(args, " "))
				}
			}
		}
		if !found {
			t.Fatalf("provider call %q was not recorded: %+v", strings.Join(args, " "), runner.calls)
		}
	}
}

func TestSemanticIndexPassesBrainignoreToProvider(t *testing.T) {
	repoDir := t.TempDir()
	brainignorePath := filepath.Join(repoDir, ".brainignore")
	if err := os.WriteFile(brainignorePath, []byte("generated/\n"), 0o600); err != nil {
		t.Fatalf("write .brainignore: %v", err)
	}
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}

	if err := runSemanticIndex(cmd.Context(), cmd, Options{
		Version: "test",
		Env:     env,
		Runner:  runner,
		Now:     time.Now,
	}, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}

	if !fakeRunnerCalled(runner, "entire", "sem", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network", "--ignore-file", brainignorePath) {
		t.Fatalf("semantic provider was not called with .brainignore: %+v", runner.calls)
	}
}

func TestSemanticIndexWritesBranchOverlayForFeatureBranch(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	runner.responses[fakeCommandKey("git", "rev-parse", "refs/heads/main")] = fakeCommandResponse{stdout: "base111\n"}
	cmd := &cobra.Command{Use: "index"}
	if err := runSemanticIndex(cmd.Context(), cmd, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	manifest, err := loadBrainManifest(filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo"))
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if manifest.Sources.Semantic.OverlayPath == "" {
		t.Fatalf("overlay path missing: %+v", manifest.Sources.Semantic)
	}
	data, err := os.ReadFile(filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo", filepath.FromSlash(manifest.Sources.Semantic.OverlayPath)))
	if err != nil {
		t.Fatalf("read overlay: %v", err)
	}
	if !strings.Contains(string(data), `"base": "base111"`) || !strings.Contains(string(data), `"head": "aaa111"`) {
		t.Fatalf("overlay missing base/head:\n%s", data)
	}
}

func TestSemanticRefreshAllBranchesWritesBoundedReport(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	now := time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC)
	runner.responses[fakeCommandKey("git", "rev-parse", "refs/heads/main")] = fakeCommandResponse{stdout: "base111\n"}
	runner.responses[fakeCommandKey("git", "rev-parse", "refs/heads/recent")] = fakeCommandResponse{stdout: "recent111\n"}
	runner.responses[fakeCommandKey("git", "for-each-ref", "--format=%(refname:short)%00%(committerdate:unix)", "refs/heads")] = fakeCommandResponse{
		stdout: "recent\x001779840000\nold\x001700000000\n",
	}
	opts := Options{Env: env, Runner: runner, Now: func() time.Time { return now }}
	if err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	if err := runSemanticRefreshAllBranches((&cobra.Command{}).Context(), opts, refreshCommandOptions{semantic: true}, repoDir); err != nil {
		t.Fatalf("refresh all branches: %v", err)
	}
	reportPath := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo", semanticDirName, "overlays", "all-branches.json")
	data, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read branch report: %v", err)
	}
	if !strings.Contains(string(data), `"branch": "recent"`) || !strings.Contains(string(data), `"state": "overlay_written"`) {
		t.Fatalf("recent branch missing from report:\n%s", data)
	}
	if !strings.Contains(string(data), `"overlay_path": "semantic/overlays/base111..recent111.json"`) {
		t.Fatalf("recent branch overlay missing from report:\n%s", data)
	}
	if !strings.Contains(string(data), `"branch": "old"`) || !strings.Contains(string(data), `"reason": "older_than_30d"`) {
		t.Fatalf("old branch skip missing from report:\n%s", data)
	}
	if _, err := os.Stat(filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo", semanticDirName, "overlays", "base111..recent111.json")); err != nil {
		t.Fatalf("branch overlay was not written: %v", err)
	}
	overlayData, err := os.ReadFile(filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo", semanticDirName, "overlays", "base111..recent111.json"))
	if err != nil {
		t.Fatalf("read branch overlay: %v", err)
	}
	if strings.Contains(string(overlayData), "snapshot_path") {
		t.Fatalf("non-current branch overlay should not reuse current snapshot:\n%s", overlayData)
	}
}

func TestSemanticIndexRejectsUnsupportedSchemaMajor(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("2.0"))
	cmd := &cobra.Command{Use: "index"}

	err := runSemanticIndex(cmd.Context(), cmd, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire"}, repoDir)
	if err == nil || !strings.Contains(err.Error(), "unsupported semantic schema major version") {
		t.Fatalf("index err = %v", err)
	}
}

func TestSemanticIndexRejectsMismatchedProviderHeader(t *testing.T) {
	for name, snapshot := range map[string]string{
		"repo-key": strings.Replace(semanticFixtureSnapshot("1.0"), `"repo_key":"gh/example/repo"`, `"repo_key":"gh/other/repo"`, 1),
		"commit":   strings.Replace(semanticFixtureSnapshot("1.0"), `"commit":"aaa111"`, `"commit":"bbb222"`, 1),
		"tree":     strings.Replace(semanticFixtureSnapshot("1.0"), `"tree":"tree111"`, `"tree":"tree222"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			repoDir := t.TempDir()
			env := semanticTestEnv(t, repoDir)
			runner := semanticFixtureRunner(repoDir, snapshot)
			err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire"}, repoDir)
			if err == nil || !strings.Contains(err.Error(), "does not match") {
				t.Fatalf("index err = %v", err)
			}
		})
	}
}

func TestSemanticIndexRequiresProviderCommitAndTree(t *testing.T) {
	for name, snapshot := range map[string]string{
		"commit":   strings.Replace(semanticFixtureSnapshot("1.0"), `"commit":"aaa111",`, "", 1),
		"tree":     strings.Replace(semanticFixtureSnapshot("1.0"), `"tree":"tree111",`, "", 1),
		"repo_key": strings.Replace(semanticFixtureSnapshot("1.0"), `"repo_key":"gh/example/repo",`, "", 1),
	} {
		t.Run(name, func(t *testing.T) {
			repoDir := t.TempDir()
			env := semanticTestEnv(t, repoDir)
			runner := semanticFixtureRunner(repoDir, snapshot)
			err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire"}, repoDir)
			if err == nil || !strings.Contains(err.Error(), "missing "+name) {
				t.Fatalf("index err = %v", err)
			}
		})
	}
}

func TestSemanticIndexFailsClosedWhenGitMetadataUnavailable(t *testing.T) {
	for name, key := range map[string]string{
		"head": fakeCommandKey("git", "rev-parse", "HEAD"),
		"tree": fakeCommandKey("git", "rev-parse", "HEAD^{tree}"),
	} {
		t.Run(name, func(t *testing.T) {
			repoDir := t.TempDir()
			env := semanticTestEnv(t, repoDir)
			runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
			runner.responses[key] = fakeCommandResponse{err: errors.New("git unavailable")}
			err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire"}, repoDir)
			if err == nil || !strings.Contains(err.Error(), "resolve HEAD") {
				t.Fatalf("index err = %v", err)
			}
		})
	}
}

func TestSemanticIndexFailsBeforeSnapshotWhenProviderRequiresNetwork(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	runner.responses[fakeCommandKey("entire", "sem", "doctor", "--json")] = fakeCommandResponse{stdout: `{"requires_network":true}`}
	delete(runner.responses, fakeCommandKey("entire", "sem", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network"))

	err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire"}, repoDir)
	if err == nil || !strings.Contains(err.Error(), "provider_network_required") {
		t.Fatalf("index err = %v", err)
	}
	for _, call := range runner.calls {
		if call.name == "entire" && len(call.args) > 1 && call.args[0] == "sem" && call.args[1] == "snapshot" {
			t.Fatalf("snapshot was called after network-required diagnostics")
		}
	}
}

func TestSemanticIndexFailsClosedWhenProviderNoEgressUnknown(t *testing.T) {
	for name, response := range map[string]fakeCommandResponse{
		"doctor-failed":    {err: os.ErrNotExist},
		"doctor-malformed": {stdout: `{not-json`},
		"doctor-unknown":   {stdout: `{}`},
	} {
		t.Run(name, func(t *testing.T) {
			repoDir := t.TempDir()
			env := semanticTestEnv(t, repoDir)
			runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
			runner.responses[fakeCommandKey("entire", "sem", "doctor", "--json")] = response
			delete(runner.responses, fakeCommandKey("entire", "sem", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network"))

			err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire"}, repoDir)
			if err == nil || !strings.Contains(err.Error(), "no-egress") {
				t.Fatalf("index err = %v", err)
			}
			for _, call := range runner.calls {
				if call.name == "entire" && len(call.args) > 1 && call.args[0] == "sem" && call.args[1] == "snapshot" {
					t.Fatalf("snapshot was called without verified no-egress")
				}
			}
		})
	}
}

func TestSemanticIndexRejectsDirtyWorktreeWithoutWorktreeFlag(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{stdout: " M file.go\n"}
	delete(runner.responses, fakeCommandKey("entire", "sem", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network"))

	err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire"}, repoDir)
	if err == nil || !strings.Contains(err.Error(), "dirty_worktree") {
		t.Fatalf("index err = %v", err)
	}
}

func TestSemanticIndexFailsClosedWhenWorktreeStatusUnavailable(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{err: errors.New("status failed")}
	delete(runner.responses, fakeCommandKey("entire", "sem", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network"))

	err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire"}, repoDir)
	if err == nil || !strings.Contains(err.Error(), "worktree dirtiness") {
		t.Fatalf("index err = %v", err)
	}
}

func TestSemanticIndexFailsIfCleanWorktreeChangesDuringSnapshot(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	runner.sequences = map[string][]fakeCommandResponse{}
	runner.sequences[fakeCommandKey("git", "status", "--porcelain")] = []fakeCommandResponse{
		{stdout: ""},
		{stdout: " M file.go\n"},
	}
	err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire"}, repoDir)
	if err == nil || !strings.Contains(err.Error(), "worktree_changed") {
		t.Fatalf("index err = %v", err)
	}
}

func TestSemanticIndexFailsIfDirtyWorktreeChangesDuringSnapshot(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	runner.sequences = map[string][]fakeCommandResponse{}
	runner.sequences[fakeCommandKey("git", "status", "--porcelain")] = []fakeCommandResponse{
		{stdout: " M file.go\n"},
		{stdout: " M file.go\n"},
		{stdout: " M file.go\n"},
		{stdout: " M file.go\n"},
	}
	runner.sequences[fakeCommandKey("git", "diff", "--binary", "HEAD")] = []fakeCommandResponse{
		{stdout: "before"},
		{stdout: "after"},
	}
	runner.sequences[fakeCommandKey("git", "diff", "--cached", "--binary", "HEAD")] = []fakeCommandResponse{
		{stdout: ""},
		{stdout: ""},
	}
	runner.responses[fakeCommandKey("entire", "sem", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network", "--worktree")] = fakeCommandResponse{stdout: semanticFixtureSnapshot("1.0")}
	err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire", worktree: true}, repoDir)
	if err == nil || !strings.Contains(err.Error(), "worktree_changed") {
		t.Fatalf("index err = %v", err)
	}
}

func TestSemanticIndexFailsIfWorktreeFingerprintUnavailable(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{stdout: " M file.go\n"}
	runner.responses[fakeCommandKey("git", "diff", "--binary", "HEAD")] = fakeCommandResponse{err: errors.New("diff failed")}
	runner.responses[fakeCommandKey("entire", "sem", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network", "--worktree")] = fakeCommandResponse{stdout: semanticFixtureSnapshot("1.0")}
	err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire", worktree: true}, repoDir)
	if err == nil || !strings.Contains(err.Error(), "fingerprint worktree") {
		t.Fatalf("index err = %v", err)
	}
}

func TestSemanticIndexWorktreeFlagCreatesDirtyOverlay(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{stdout: " M file.go\n"}
	runner.responses[fakeCommandKey("git", "diff", "--binary", "HEAD")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "diff", "--cached", "--binary", "HEAD")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("entire", "sem", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network", "--worktree")] = fakeCommandResponse{stdout: semanticFixtureSnapshot("1.0")}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, opts, semanticIndexOptions{semBinary: "entire", worktree: true}, repoDir); err != nil {
		t.Fatalf("index --worktree: %v", err)
	}
	manifest, err := loadBrainManifest(filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo"))
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if manifest.Sources.Semantic.WorktreeMode != "worktree" || manifest.Sources.Semantic.WorktreeHash == "" {
		t.Fatalf("worktree overlay metadata missing: %+v", manifest.Sources.Semantic)
	}
	report, err := semanticStaleReport((&cobra.Command{}).Context(), opts, repoDir)
	if err != nil {
		t.Fatalf("stale: %v", err)
	}
	if report.Axes["snapshot"].State != "ok" {
		t.Fatalf("worktree snapshot freshness = %+v", report.Axes["snapshot"])
	}
	if report.Axes["worktree"].State != "dirty-indexed" {
		t.Fatalf("worktree freshness = %+v", report.Axes["worktree"])
	}
}

func TestSemanticIndexWorktreeFlagAllowsCleanWorktree(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	runner.responses[fakeCommandKey("git", "diff", "--binary", "HEAD")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "diff", "--cached", "--binary", "HEAD")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("entire", "sem", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network", "--worktree")] = fakeCommandResponse{stdout: semanticFixtureSnapshot("1.0")}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, opts, semanticIndexOptions{semBinary: "entire", worktree: true}, repoDir); err != nil {
		t.Fatalf("index --worktree clean: %v", err)
	}
	report, err := semanticStaleReport((&cobra.Command{}).Context(), opts, repoDir)
	if err != nil {
		t.Fatalf("stale: %v", err)
	}
	if report.Severity != "ok" || report.Axes["worktree"].State != "clean" {
		t.Fatalf("clean worktree report = %+v", report)
	}
}

func TestSemanticIndexWorktreeCleanRejectsProviderTreeMismatch(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	snapshot := strings.Replace(semanticFixtureSnapshot("1.0"), `"tree":"tree111"`, `"tree":"dirtytree"`, 1)
	runner := semanticFixtureRunner(repoDir, snapshot)
	runner.responses[fakeCommandKey("git", "diff", "--binary", "HEAD")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "diff", "--cached", "--binary", "HEAD")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("entire", "sem", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network", "--worktree")] = fakeCommandResponse{stdout: snapshot}
	err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire", worktree: true}, repoDir)
	if err == nil || !strings.Contains(err.Error(), "tree") {
		t.Fatalf("index err = %v", err)
	}
}

func TestSemanticIndexWorktreeAllowsProviderTreeMismatch(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	snapshot := strings.Replace(semanticFixtureSnapshot("1.0"), `"tree":"tree111"`, `"tree":"dirtytree"`, 1)
	runner := semanticFixtureRunner(repoDir, snapshot)
	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{stdout: " M file.go\n"}
	runner.responses[fakeCommandKey("git", "diff", "--binary", "HEAD")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "diff", "--cached", "--binary", "HEAD")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("entire", "sem", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network", "--worktree")] = fakeCommandResponse{stdout: snapshot}
	if err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire", worktree: true}, repoDir); err != nil {
		t.Fatalf("index --worktree dirty tree: %v", err)
	}
}

func TestWorktreeFingerprintIncludesUntrackedContent(t *testing.T) {
	repoDir := t.TempDir()
	path := filepath.Join(repoDir, "notes.txt")
	if err := os.WriteFile(path, []byte("one"), 0o600); err != nil {
		t.Fatalf("write untracked: %v", err)
	}
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "status", "--porcelain"):                {stdout: "?? notes.txt\n"},
		fakeCommandKey("git", "diff", "--binary", "HEAD"):             {},
		fakeCommandKey("git", "diff", "--cached", "--binary", "HEAD"): {},
	}}
	first, err := worktreeFingerprint((&cobra.Command{}).Context(), runner, repoDir)
	if err != nil {
		t.Fatalf("first fingerprint: %v", err)
	}
	if err := os.WriteFile(path, []byte("two"), 0o600); err != nil {
		t.Fatalf("rewrite untracked: %v", err)
	}
	second, err := worktreeFingerprint((&cobra.Command{}).Context(), runner, repoDir)
	if err != nil {
		t.Fatalf("second fingerprint: %v", err)
	}
	if first == second {
		t.Fatalf("fingerprint did not change for untracked content: %s", first)
	}
}

func TestWorktreeFingerprintSkipsSymlinksInsideUntrackedDirectory(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, "notes"), 0o700); err != nil {
		t.Fatalf("mkdir notes: %v", err)
	}
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("one"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	if err := os.Symlink(secret, filepath.Join(repoDir, "notes", "secret.txt")); err != nil {
		t.Fatalf("symlink secret: %v", err)
	}
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "status", "--porcelain"):                {stdout: "?? notes/\n"},
		fakeCommandKey("git", "diff", "--binary", "HEAD"):             {},
		fakeCommandKey("git", "diff", "--cached", "--binary", "HEAD"): {},
	}}
	first, err := worktreeFingerprint((&cobra.Command{}).Context(), runner, repoDir)
	if err != nil {
		t.Fatalf("first fingerprint: %v", err)
	}
	if err := os.WriteFile(secret, []byte("two"), 0o600); err != nil {
		t.Fatalf("rewrite secret: %v", err)
	}
	second, err := worktreeFingerprint((&cobra.Command{}).Context(), runner, repoDir)
	if err != nil {
		t.Fatalf("second fingerprint: %v", err)
	}
	if first != second {
		t.Fatalf("fingerprint depended on symlink target: %s != %s", first, second)
	}
}

func TestWorktreeFingerprintIgnoresBrainignoredUntrackedContent(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoDir, ".brainignore"), []byte("ignored/\n"), 0o600); err != nil {
		t.Fatalf("write brainignore: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "ignored"), 0o700); err != nil {
		t.Fatalf("mkdir ignored: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "ignored", "secret.txt"), []byte("one"), 0o600); err != nil {
		t.Fatalf("write ignored: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "keep.txt"), []byte("one"), 0o600); err != nil {
		t.Fatalf("write keep: %v", err)
	}
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "status", "--porcelain"):                {stdout: "?? keep.txt\n"},
		fakeCommandKey("git", "diff", "--binary", "HEAD"):             {},
		fakeCommandKey("git", "diff", "--cached", "--binary", "HEAD"): {},
	}}
	first, err := worktreeFingerprint((&cobra.Command{}).Context(), runner, repoDir)
	if err != nil {
		t.Fatalf("first fingerprint: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "ignored", "secret.txt"), []byte("two"), 0o600); err != nil {
		t.Fatalf("rewrite ignored: %v", err)
	}
	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{stdout: "?? ignored/\n?? keep.txt\n"}
	second, err := worktreeFingerprint((&cobra.Command{}).Context(), runner, repoDir)
	if err != nil {
		t.Fatalf("second fingerprint: %v", err)
	}
	if first != second {
		t.Fatalf("fingerprint changed for ignored content: %s != %s", first, second)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "keep.txt"), []byte("two"), 0o600); err != nil {
		t.Fatalf("rewrite keep: %v", err)
	}
	third, err := worktreeFingerprint((&cobra.Command{}).Context(), runner, repoDir)
	if err != nil {
		t.Fatalf("third fingerprint: %v", err)
	}
	if third == second {
		t.Fatalf("fingerprint did not change for unignored content")
	}
}

func TestWorktreeFingerprintReadsQuotedUntrackedPath(t *testing.T) {
	repoDir := t.TempDir()
	path := filepath.Join(repoDir, "new file.go")
	if err := os.WriteFile(path, []byte("one"), 0o600); err != nil {
		t.Fatalf("write untracked: %v", err)
	}
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "status", "--porcelain", "--untracked-files=all"): {stdout: "?? \"new file.go\"\n"},
		fakeCommandKey("git", "diff", "--binary", "HEAD"):                       {},
		fakeCommandKey("git", "diff", "--cached", "--binary", "HEAD"):           {},
	}}
	first, err := worktreeFingerprint((&cobra.Command{}).Context(), runner, repoDir)
	if err != nil {
		t.Fatalf("first fingerprint: %v", err)
	}
	if err := os.WriteFile(path, []byte("two"), 0o600); err != nil {
		t.Fatalf("rewrite untracked: %v", err)
	}
	second, err := worktreeFingerprint((&cobra.Command{}).Context(), runner, repoDir)
	if err != nil {
		t.Fatalf("second fingerprint: %v", err)
	}
	if first == second {
		t.Fatalf("fingerprint did not include quoted untracked path")
	}
}

func TestWorktreeFingerprintUsesIgnoredDiffPathspecs(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoDir, ".brainignore"), []byte("ignored/\n"), 0o600); err != nil {
		t.Fatalf("write brainignore: %v", err)
	}
	ignore := brainIgnore{patterns: []string{"ignored/"}}
	diffArgs := append([]string{"diff", "--binary", "HEAD", "--", "."}, ignore.gitPathspecExclusions()...)
	cachedArgs := append([]string{"diff", "--cached", "--binary", "HEAD", "--", "."}, ignore.gitPathspecExclusions()...)
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "status", "--porcelain"):                {stdout: ""},
		fakeCommandKey("git", diffArgs...):                            {},
		fakeCommandKey("git", cachedArgs...):                          {},
		fakeCommandKey("git", "diff", "--binary", "HEAD"):             {stdout: "ignored diff should not be used"},
		fakeCommandKey("git", "diff", "--cached", "--binary", "HEAD"): {stdout: "ignored cached diff should not be used"},
	}}
	if _, err := worktreeFingerprint((&cobra.Command{}).Context(), runner, repoDir); err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	if !fakeRunnerCalled(runner, "git", diffArgs...) || !fakeRunnerCalled(runner, "git", cachedArgs...) {
		t.Fatalf("ignored pathspec diff was not used: %+v", runner.calls)
	}
}

func TestSemanticIndexAndStaleIgnoreBrainignoredDirtyFiles(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoDir, ".brainignore"), []byte("ignored/\n"), 0o600); err != nil {
		t.Fatalf("write brainignore: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "ignored"), 0o700); err != nil {
		t.Fatalf("mkdir ignored: %v", err)
	}
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{stdout: "?? ignored/\n"}
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: semanticTestEnv(t, repoDir), Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index ignored dirty worktree: %v", err)
	}
	report, err := semanticStaleReport(cmd.Context(), opts, repoDir)
	if err != nil {
		t.Fatalf("stale: %v", err)
	}
	if report.Axes["worktree"].State != "clean" {
		t.Fatalf("worktree axis = %+v", report.Axes["worktree"])
	}
}

func TestWorktreeDirtyKeepsRenameFromIgnoredToPublic(t *testing.T) {
	repoDir := t.TempDir()
	ignore := brainIgnore{patterns: []string{"ignored/"}}
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "status", "--porcelain"): {stdout: "R  ignored/file.go -> public/file.go\n"},
	}}
	dirty, err := worktreeDirtyWithIgnore((&cobra.Command{}).Context(), runner, repoDir, ignore)
	if err != nil {
		t.Fatalf("dirty: %v", err)
	}
	if !dirty {
		t.Fatalf("rename into public path was filtered as clean")
	}
	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{stdout: "R  ignored/file.go -> ignored/other.go\n"}
	dirty, err = worktreeDirtyWithIgnore((&cobra.Command{}).Context(), runner, repoDir, ignore)
	if err != nil {
		t.Fatalf("dirty ignored: %v", err)
	}
	if dirty {
		t.Fatalf("rename within ignored paths was not filtered")
	}
}

func TestWorktreeDirtyUsesExpandedUntrackedStatus(t *testing.T) {
	repoDir := t.TempDir()
	ignore := brainIgnore{patterns: []string{"secrets/*.pem"}}
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "status", "--porcelain", "--untracked-files=all"): {stdout: "?? secrets/token.pem\n"},
		fakeCommandKey("git", "status", "--porcelain"):                          {stdout: "?? secrets/\n"},
	}}
	dirty, err := worktreeDirtyWithIgnore((&cobra.Command{}).Context(), runner, repoDir, ignore)
	if err != nil {
		t.Fatalf("dirty: %v", err)
	}
	if dirty {
		t.Fatalf("expanded ignored untracked file was treated as dirty")
	}
	if !fakeRunnerCalled(runner, "git", "status", "--porcelain", "--untracked-files=all") {
		t.Fatalf("expanded status was not used: %+v", runner.calls)
	}
}

func TestSemanticStaleMarksSkipSemUnsafe(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{skipSem: true}, repoDir); err != nil {
		t.Fatalf("index --skip-sem: %v", err)
	}
	report, err := semanticStaleReport(cmd.Context(), opts, repoDir)
	if err != nil {
		t.Fatalf("stale: %v", err)
	}
	if report.Severity != "unsafe" || report.Axes["semantic_completeness"].State != "unsafe" {
		t.Fatalf("skip-sem report = %+v", report)
	}
}

func TestSemanticStaleAndQueryRejectCorruptDeclaredStore(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	storePath := filepath.Join(brainDir, filepath.FromSlash(manifest.Sources.Semantic.StorePath))
	if err := os.WriteFile(storePath, []byte("not sqlite"), 0o600); err != nil {
		t.Fatalf("corrupt store: %v", err)
	}
	report, err := semanticStaleReport(cmd.Context(), opts, repoDir)
	if err != nil {
		t.Fatalf("stale: %v", err)
	}
	if report.Axes["store"].State != "unsafe" {
		t.Fatalf("store axis = %+v", report.Axes["store"])
	}
	err = runSemanticQuery(cmd.Context(), cmd, opts, semanticQueryOptions{limit: 10}, "ValidateToken")
	if err == nil {
		t.Fatalf("query succeeded with corrupt declared store")
	}
}

func TestSemanticStaleMissingStoreDoesNotCreateSQLite(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	storePath := filepath.Join(brainDir, filepath.FromSlash(manifest.Sources.Semantic.StorePath))
	if err := os.Remove(storePath); err != nil {
		t.Fatalf("remove store: %v", err)
	}
	report, err := semanticStaleReport(cmd.Context(), opts, repoDir)
	if err != nil {
		t.Fatalf("stale: %v", err)
	}
	if report.Axes["store"].State != "unsafe" {
		t.Fatalf("store axis = %+v", report.Axes["store"])
	}
	if _, err := os.Stat(storePath); !os.IsNotExist(err) {
		t.Fatalf("missing store was recreated: %v", err)
	}
}

func TestSemanticQueryAndContextMissingStoreDoNotCreateSQLite(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	storePath := filepath.Join(brainDir, filepath.FromSlash(manifest.Sources.Semantic.StorePath))
	if err := os.Remove(storePath); err != nil {
		t.Fatalf("remove store: %v", err)
	}
	if err := runSemanticQuery(cmd.Context(), cmd, opts, semanticQueryOptions{limit: 10}, "ValidateToken"); err == nil {
		t.Fatalf("query succeeded with missing store")
	}
	if _, err := os.Stat(storePath); !os.IsNotExist(err) {
		t.Fatalf("query recreated missing store: %v", err)
	}
	if err := runSemanticContext(cmd.Context(), cmd, opts, semanticContextOptions{limit: 10}, "ValidateToken"); err == nil {
		t.Fatalf("context succeeded with missing store")
	}
	if _, err := os.Stat(storePath); !os.IsNotExist(err) {
		t.Fatalf("context recreated missing store: %v", err)
	}
}

func TestSemanticIndexRequiresForceWhenIndexExists(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("first index: %v", err)
	}
	err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir)
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("second index err = %v", err)
	}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire", force: true}, repoDir); err != nil {
		t.Fatalf("force index: %v", err)
	}
}

func TestSemanticRepairRebuildsMissingStoreFromActiveSnapshot(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
	if _, err := execute(t, cmd, "refresh", "index", "--sem-binary", "entire"); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	storePath := filepath.Join(brainDir, filepath.FromSlash(manifest.Sources.Semantic.StorePath))
	if err := os.Remove(storePath); err != nil {
		t.Fatalf("remove store: %v", err)
	}
	out, err := execute(t, cmd, "repair")
	if err != nil {
		t.Fatalf("repair: %v\n%s", err, out)
	}
	if !strings.Contains(out, "repaired semantic brain") {
		t.Fatalf("repair output = %q", out)
	}
	repairedManifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("load repaired manifest: %v", err)
	}
	repairedStorePath := filepath.Join(brainDir, filepath.FromSlash(repairedManifest.Sources.Semantic.StorePath))
	if repairedStorePath == storePath {
		t.Fatalf("repair reused missing active store path: %s", repairedStorePath)
	}
	if _, err := os.Stat(repairedStorePath); err != nil {
		t.Fatalf("store was not rebuilt at repaired path: %v", err)
	}
	queryOut, err := execute(t, cmd, "inspect", "code", "ValidateToken", "--json")
	if err != nil {
		t.Fatalf("query after repair: %v", err)
	}
	if !strings.Contains(queryOut, `"ValidateToken"`) {
		t.Fatalf("query output missing repaired symbol:\n%s", queryOut)
	}
}

func TestSemanticResetRequiresForceAndSemanticOnlyPreservesManifest(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
	if _, err := execute(t, cmd, "refresh", "index", "--sem-binary", "entire"); err != nil {
		t.Fatalf("index: %v", err)
	}
	if _, err := execute(t, cmd, "reset", "--semantic-only"); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("reset without force err = %v", err)
	}
	out, err := execute(t, cmd, "reset", "--semantic-only", "--force")
	if err != nil {
		t.Fatalf("reset semantic-only: %v", err)
	}
	if !strings.Contains(out, "reset semantic brain") {
		t.Fatalf("reset output = %q", out)
	}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	if _, err := os.Stat(filepath.Join(brainDir, semanticDirName)); !os.IsNotExist(err) {
		t.Fatalf("semantic dir still exists: %v", err)
	}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if manifest.Sources != nil && manifest.Sources.Semantic != nil {
		t.Fatalf("semantic source still present: %+v", manifest.Sources.Semantic)
	}
}

func TestSemanticResetForceLeavesEmptyBrainDirectory(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
	if _, err := execute(t, cmd, "refresh", "index", "--sem-binary", "entire"); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	if _, err := execute(t, cmd, "reset", "--force"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	info, err := os.Stat(brainDir)
	if err != nil {
		t.Fatalf("brain dir missing after reset: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("brain path is not a directory after reset")
	}
	entries, err := os.ReadDir(brainDir)
	if err != nil {
		t.Fatalf("read brain dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != brainLockDirName {
		t.Fatalf("brain dir entries after reset = %+v, want only %s", entries, brainLockDirName)
	}
	if _, err := os.Stat(filepath.Join(brainDir, brainLockDirName, brainWriteLockName)); err != nil {
		t.Fatalf("write lock metadata should remain after reset: %v", err)
	}
}

func TestSemanticJSONErrorsUseStructuredEnvelope(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	for name, args := range map[string][]string{
		"runtime":        {"inspect", "code", "ValidateToken", "--json", "--limit", "0"},
		"args":           {"inspect", "code", "--json"},
		"flags":          {"inspect", "code", "--json", "--bogus"},
		"flags-reversed": {"inspect", "code", "--bogus", "--json"},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
			out, err := execute(t, cmd, args...)
			if err == nil {
				t.Fatalf("search succeeded unexpectedly:\n%s", out)
			}
			var envelope commandJSONError
			if decodeErr := json.Unmarshal([]byte(out), &envelope); decodeErr != nil {
				t.Fatalf("error output was not JSON: %v\n%s", decodeErr, out)
			}
			if envelope.Code != "command_failed" || envelope.Message == "" {
				t.Fatalf("unexpected JSON error envelope: %+v", envelope)
			}
		})
	}
}

func TestSemanticFlagErrorsStayPlainTextWithoutJSON(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	for name, args := range map[string][]string{
		"omitted":    {"inspect", "code", "ValidateToken", "--bogus"},
		"false-flag": {"inspect", "code", "ValidateToken", "--json=false", "--limit", "0"},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
			out, err := execute(t, cmd, args...)
			if err == nil {
				t.Fatalf("query succeeded unexpectedly:\n%s", out)
			}
			if json.Valid([]byte(out)) {
				t.Fatalf("non-json error was rendered as JSON:\n%s", out)
			}
		})
	}
}

func TestSanitizeSemanticWarningTextPreservesColonDiagnostics(t *testing.T) {
	for _, text := range []string{
		"parse failed: unexpected token",
		"warning: skipped optional metadata",
		"file.go:12: unexpected token",
		"GET /tokens/{id}",
	} {
		if got := sanitizeSemanticWarningText(text, ""); got != text {
			t.Fatalf("sanitizeSemanticWarningText(%q) = %q, want unchanged", text, got)
		}
	}
	if got := sanitizeSemanticWarningText("failed at D:/work/repo/file.go", ""); got != "<redacted>" {
		t.Fatalf("Windows absolute path was not redacted: %q", got)
	}
	if got := sanitizeSemanticWarningText("failed at /opt/build/repo/secret.go", ""); got != "<redacted>" {
		t.Fatalf("Unix absolute path was not redacted: %q", got)
	}
	if got := sanitizeSemanticWarningText("failed at /builds/acme/repo/private", ""); got != "<redacted>" {
		t.Fatalf("extensionless Unix absolute path was not redacted: %q", got)
	}
}

func TestSemanticIndexReportsPluginDataWriteFailures(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	dataFile := filepath.Join(t.TempDir(), "plugin-data-file")
	if err := os.WriteFile(dataFile, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write plugin data file: %v", err)
	}
	env.PluginDataDir = dataFile
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
	if _, err := execute(t, cmd, "refresh", "index", "--sem-binary", "entire"); err == nil {
		t.Fatalf("index succeeded with file plugin data dir")
	}
}

func TestSemanticIndexReportsReadOnlyBrainDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod read-only directory semantics are not portable on Windows")
	}
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	brainRoot := filepath.Join(env.PluginDataDir, repoStoreDirName)
	if err := os.MkdirAll(brainRoot, 0o500); err != nil {
		t.Fatalf("mkdir brain root: %v", err)
	}
	defer func() { _ = os.Chmod(brainRoot, 0o700) }()
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
	if _, err := execute(t, cmd, "refresh", "index", "--sem-binary", "entire"); err == nil {
		t.Fatalf("index succeeded with read-only brain root")
	}
}

func TestSemanticIndexRejectsProviderPathEscape(t *testing.T) {
	absoluteProviderPath := "/tmp/secret.txt"
	if runtime.GOOS == "windows" {
		absoluteProviderPath = "C:/tmp/secret.txt"
	}
	for name, snapshot := range map[string]string{
		"parent": strings.Replace(semanticFixtureSnapshot("1.0"), `"file_path":"internal/auth/token.go"`, `"file_path":"../../secret.txt"`, 1),
		"windows-drive-relative": `{"schema_version":"1.0","provider":"entire-sem","provider_version":"0.1.0","repo_key":"gh/example/repo","commit":"aaa111","tree":"tree111","capabilities":["go"],"warnings":[],"partial_failures":[]}
{"record_type":"file","path":"C:secret.txt"}
`,
		"windows-drive": `{"schema_version":"1.0","provider":"entire-sem","provider_version":"0.1.0","repo_key":"gh/example/repo","commit":"aaa111","tree":"tree111","capabilities":["go"],"warnings":[],"partial_failures":[]}
{"record_type":"file","path":"C:/tmp/secret.txt"}
`,
		"absolute": `{"schema_version":"1.0","provider":"entire-sem","provider_version":"0.1.0","repo_key":"gh/example/repo","commit":"aaa111","tree":"tree111","capabilities":["go"],"warnings":[],"partial_failures":[]}
{"record_type":"file","path":"` + absoluteProviderPath + `"}
`,
	} {
		t.Run(name, func(t *testing.T) {
			repoDir := t.TempDir()
			env := semanticTestEnv(t, repoDir)
			runner := semanticFixtureRunner(repoDir, snapshot)
			err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire"}, repoDir)
			if err == nil || !strings.Contains(err.Error(), "semantic provider path") {
				t.Fatalf("index err = %v", err)
			}
		})
	}
}

func TestSemanticIndexRedactsBrainignoredRecordsFromSnapshotAndQuery(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoDir, ".brainignore"), []byte("secret/**\n"), 0o600); err != nil {
		t.Fatalf("write .brainignore: %v", err)
	}
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, strings.ReplaceAll(semanticFixtureSnapshotWithIgnoredSecret(), "secret/config.go", "secret/nested/config.go"))
	cmd := &cobra.Command{Use: "index"}
	if err := runSemanticIndex(cmd.Context(), cmd, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if manifest.Sources.Semantic.Symbols != 1 || manifest.Sources.Semantic.Relations != 0 {
		t.Fatalf("counts = symbols %d relations %d", manifest.Sources.Semantic.Symbols, manifest.Sources.Semantic.Relations)
	}
	snapshotPath := filepath.Join(brainDir, filepath.FromSlash(manifest.Sources.Semantic.SnapshotPath))
	data, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if strings.Contains(string(data), "SECRET_TOKEN") || strings.Contains(string(data), "secret/nested/config.go") {
		t.Fatalf("ignored semantic record persisted:\n%s", data)
	}
	results, err := findSemanticSymbols(snapshotPath, "SECRET_TOKEN", 10, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("ignored symbol was queryable: %+v", results)
	}
}

func TestSemanticIndexRedactsBrainignoredWarningsFromHeaderAndManifest(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoDir, ".brainignore"), []byte("secret/\n"), 0o600); err != nil {
		t.Fatalf("write .brainignore: %v", err)
	}
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshotWithIgnoredWarnings())
	cmd := &cobra.Command{Use: "index"}
	if err := runSemanticIndex(cmd.Context(), cmd, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if len(manifest.Sources.Semantic.Warnings) != 1 || manifest.Sources.Semantic.Warnings[0].Path == "secret/config.go" {
		t.Fatalf("manifest warnings were not redacted: %+v", manifest.Sources.Semantic.Warnings)
	}
	if len(manifest.Sources.Semantic.PartialFailures) != 0 {
		t.Fatalf("manifest partial failures were not redacted: %+v", manifest.Sources.Semantic.PartialFailures)
	}
	data, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(manifest.Sources.Semantic.SnapshotPath)))
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if strings.Contains(string(data), "secret/config.go") || strings.Contains(string(data), "SECRET_TOKEN") {
		t.Fatalf("ignored warning leaked into snapshot:\n%s", data)
	}
}

func TestSemanticIndexSanitizesAbsoluteProviderWarnings(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	absPath := filepath.ToSlash(filepath.Join(repoDir, "secret", "config.go"))
	snapshot := strings.Replace(semanticFixtureSnapshot("1.0"), `"warnings":[],"partial_failures":[]`, `"warnings":[{"code":"absolute_path","severity":"warning","path":"`+absPath+`","effect":"read `+absPath+`","detail":"failed at D:/work/repo/file.go and `+absPath+`"}],"partial_failures":[{"code":"partial_absolute","severity":"warning","path":"`+absPath+`","detail":"`+absPath+`"}]`, 1)
	runner := semanticFixtureRunner(repoDir, snapshot)
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	manifestData, err := os.ReadFile(filepath.Join(brainDir, exportManifestFileName))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if strings.Contains(string(manifestData), repoDir) || strings.Contains(string(manifestData), absPath) {
		t.Fatalf("manifest leaked absolute warning path:\n%s", manifestData)
	}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if got := manifest.Sources.Semantic.Warnings[0].Path; got != "<redacted>" {
		t.Fatalf("warning path = %q, want redacted", got)
	}
	snapshotData, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(manifest.Sources.Semantic.SnapshotPath)))
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if strings.Contains(string(snapshotData), repoDir) || strings.Contains(string(snapshotData), absPath) {
		t.Fatalf("snapshot leaked absolute warning path:\n%s", snapshotData)
	}
	output := filepath.Join(t.TempDir(), "brain.tar")
	if err := runSemanticBundleExport(cmd.Context(), cmd, opts, output); err != nil {
		t.Fatalf("bundle export: %v", err)
	}
	bundleManifest := readTestBundleFile(t, output, exportManifestFileName)
	if strings.Contains(string(bundleManifest), repoDir) || strings.Contains(string(bundleManifest), absPath) {
		t.Fatalf("bundle manifest leaked absolute warning path:\n%s", bundleManifest)
	}
}

func TestBundleExportSanitizesLegacyManifestWarnings(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	absPath := filepath.ToSlash(filepath.Join(repoDir, "legacy", "secret.go"))
	manifest.Sources.Semantic.Warnings = []semanticWarning{{
		Code:     "legacy_absolute",
		Severity: "warning",
		Path:     absPath,
		Effect:   "read " + absPath,
		Detail:   "failed at " + absPath,
	}}
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	output := filepath.Join(t.TempDir(), "brain.tar")
	if err := runSemanticBundleExport(cmd.Context(), cmd, opts, output); err != nil {
		t.Fatalf("bundle export: %v", err)
	}
	bundleManifest := readTestBundleFile(t, output, exportManifestFileName)
	if strings.Contains(string(bundleManifest), repoDir) || strings.Contains(string(bundleManifest), absPath) {
		t.Fatalf("bundle manifest leaked legacy absolute warning path:\n%s", bundleManifest)
	}
}

func TestSemanticIndexDoesNotDefaultIgnoreGitHubPaths(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshotWithGitHubWorkflow())
	cmd := &cobra.Command{Use: "index"}
	if err := runSemanticIndex(cmd.Context(), cmd, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	manifest, err := loadBrainManifest(filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo"))
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if manifest.Sources.Semantic.Symbols != 1 {
		t.Fatalf(".github symbol was unexpectedly ignored: %+v", manifest.Sources.Semantic)
	}
}

func TestBrainIgnoreDoesNotOvermatchDefaultDirectoryPrefixes(t *testing.T) {
	ignore := brainIgnore{}
	for _, path := range []string{"vendorized/pkg.go", "distill/main.go", "buildkite/pipeline.yml", "targeted/file.rs"} {
		if ignore.Ignored(path) {
			t.Fatalf("path %s was unexpectedly ignored", path)
		}
	}
	for _, path := range []string{"vendor/pkg.go", "web/dist/app.js", "pkg/build/out.go", "target/debug/app"} {
		if !ignore.Ignored(path) {
			t.Fatalf("path %s was not ignored", path)
		}
	}
}

func TestBrainIgnoreMatchesRecursivePatterns(t *testing.T) {
	ignore := brainIgnore{patterns: []string{"secrets/**", "generated/**/private-*.go"}}
	for _, path := range []string{"secrets/token.go", "secrets/nested/key.go", "generated/private-token.go", "generated/deep/private-token.go"} {
		if !ignore.Ignored(path) {
			t.Fatalf("path %s was not ignored", path)
		}
	}
	for _, path := range []string{"secret/token.go", "generated/deep/public.go"} {
		if ignore.Ignored(path) {
			t.Fatalf("path %s was unexpectedly ignored", path)
		}
	}
}

func TestSemanticIndexRedactsIgnoredPathsEmbeddedInRelationIDs(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoDir, ".brainignore"), []byte("secret/\n"), 0o600); err != nil {
		t.Fatalf("write .brainignore: %v", err)
	}
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshotWithIgnoredRelationID())
	cmd := &cobra.Command{Use: "index"}
	if err := runSemanticIndex(cmd.Context(), cmd, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if manifest.Sources.Semantic.Relations != 0 {
		t.Fatalf("ignored relation was counted: %+v", manifest.Sources.Semantic)
	}
	data, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(manifest.Sources.Semantic.SnapshotPath)))
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if strings.Contains(string(data), "secret/config.go") {
		t.Fatalf("ignored relation endpoint leaked:\n%s", data)
	}
}

func TestSemanticIndexKeepsRelationsWhenIgnorePatternMatchesRepoKey(t *testing.T) {
	repoDir := t.TempDir()
	// "repo" is a substring of the repo key (gh/example/repo) and of every
	// symbol ID, but it must only ignore files literally named "repo".
	if err := os.WriteFile(filepath.Join(repoDir, ".brainignore"), []byte("repo\n"), 0o600); err != nil {
		t.Fatalf("write .brainignore: %v", err)
	}
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshotWithIgnoredRelationID())
	cmd := &cobra.Command{Use: "index"}
	if err := runSemanticIndex(cmd.Context(), cmd, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	manifest, err := loadBrainManifest(filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo"))
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if manifest.Sources.Semantic.Relations != 1 {
		t.Fatalf("relation dropped because ignore pattern matched the repo key substring: %+v", manifest.Sources.Semantic)
	}
}

func TestSemanticEndpointPath(t *testing.T) {
	cases := map[string]string{
		"gh/ashtom/entire-brain:Go:cmd/entire-brain/main.go:function:main": "cmd/entire-brain/main.go",
		"gh/example/repo:go:secret/config.go:function:Secret":              "secret/config.go",
		"external:import:archive/tar":                                      "",
		"external:route:/repo":                                             "",
		"public":                                                           "",
		"":                                                                 "",
	}
	for id, want := range cases {
		if got := semanticEndpointPath(id); got != want {
			t.Fatalf("semanticEndpointPath(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestSemanticIndexDoesNotTreatSecretDirectoryAsDefaultIgnore(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshotWithIgnoredRelationID())
	cmd := &cobra.Command{Use: "index"}
	if err := runSemanticIndex(cmd.Context(), cmd, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	manifest, err := loadBrainManifest(filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo"))
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if manifest.Sources.Semantic.Relations != 1 {
		t.Fatalf("secret/ relation was treated as default ignored: %+v", manifest.Sources.Semantic)
	}
}

func TestSemanticFileContentHashSkipsOversizedFiles(t *testing.T) {
	repoDir := t.TempDir()
	path := filepath.Join(repoDir, "large.bin")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create large file: %v", err)
	}
	if err := f.Truncate(semanticParseCacheMaxFile + 1); err != nil {
		_ = f.Close()
		t.Fatalf("truncate large file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close large file: %v", err)
	}
	if got := semanticFileContentHash(repoDir, "large.bin"); got != "" {
		t.Fatalf("oversized content hash = %q", got)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "small.go"), []byte("package small\n"), 0o600); err != nil {
		t.Fatalf("write small file: %v", err)
	}
	if got := semanticFileContentHash(repoDir, "small.go"); !strings.HasPrefix(got, "sha256:") {
		t.Fatalf("small content hash = %q", got)
	}
}

func TestSemanticStaleReportsDirtyUnindexedWorktree(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	if err := runSemanticIndex(cmd.Context(), cmd, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}

	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{stdout: " M internal/cli/semantic.go\n"}
	report, err := semanticStaleReport(cmd.Context(), Options{Env: env, Runner: runner}, repoDir)
	if err != nil {
		t.Fatalf("stale: %v", err)
	}
	if report.Severity != "degraded" {
		t.Fatalf("severity = %s, want degraded", report.Severity)
	}
	if report.Axes["worktree"].State != "dirty-unindexed" {
		t.Fatalf("worktree axis = %+v", report.Axes["worktree"])
	}
}

func TestSemanticStaleReportsWorktreeStatusErrorUnsafe(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	if err := runSemanticIndex(cmd.Context(), cmd, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{err: errors.New("status failed")}
	report, err := semanticStaleReport(cmd.Context(), Options{Env: env, Runner: runner}, repoDir)
	if err != nil {
		t.Fatalf("stale: %v", err)
	}
	if report.Severity != "unsafe" || report.Axes["worktree"].State != "unsafe" {
		t.Fatalf("worktree status error was not unsafe: %+v", report)
	}
}

func TestSemanticStaleReportsHeadLookupErrorUnsafe(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	if err := runSemanticIndex(cmd.Context(), cmd, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	runner.responses[fakeCommandKey("git", "rev-parse", "HEAD")] = fakeCommandResponse{err: errors.New("head failed")}
	report, err := semanticStaleReport(cmd.Context(), Options{Env: env, Runner: runner}, repoDir)
	if err != nil {
		t.Fatalf("stale: %v", err)
	}
	axis := report.Axes["head"]
	if report.Severity != "unsafe" || axis.State != "unsafe" || !strings.Contains(axis.Detail, "HEAD unavailable") {
		t.Fatalf("head lookup error was not unsafe: %+v", report)
	}
}

func TestSemanticStaleReportsBranchLookupErrorUnsafe(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	if err := runSemanticIndex(cmd.Context(), cmd, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	runner.responses[fakeCommandKey("git", "branch", "--show-current")] = fakeCommandResponse{err: errors.New("branch failed")}
	report, err := semanticStaleReport(cmd.Context(), Options{Env: env, Runner: runner}, repoDir)
	if err != nil {
		t.Fatalf("stale: %v", err)
	}
	axis := report.Axes["branch_tip"]
	if report.Severity != "unsafe" || axis.State != "unsafe" || !strings.Contains(axis.Detail, "branch unavailable") {
		t.Fatalf("branch lookup error was not unsafe: %+v", report)
	}
}

func TestSemanticStaleReportsMissingOrCorruptSnapshotUnsafe(t *testing.T) {
	for name, mutate := range map[string]func(t *testing.T, path string){
		"missing": func(t *testing.T, path string) {
			t.Helper()
			if err := os.Remove(path); err != nil {
				t.Fatalf("remove snapshot: %v", err)
			}
		},
		"corrupt": func(t *testing.T, path string) {
			t.Helper()
			if err := os.WriteFile(path, []byte("{not-json}\n"), 0o600); err != nil {
				t.Fatalf("corrupt snapshot: %v", err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			repoDir := t.TempDir()
			env := semanticTestEnv(t, repoDir)
			runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
			cmd := &cobra.Command{Use: "index"}
			opts := Options{Env: env, Runner: runner, Now: time.Now}
			if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
				t.Fatalf("index: %v", err)
			}
			brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
			manifest, err := loadBrainManifest(brainDir)
			if err != nil {
				t.Fatalf("load manifest: %v", err)
			}
			mutate(t, filepath.Join(brainDir, filepath.FromSlash(manifest.Sources.Semantic.SnapshotPath)))
			report, err := semanticStaleReport(cmd.Context(), opts, repoDir)
			if err != nil {
				t.Fatalf("stale: %v", err)
			}
			if report.Severity != "unsafe" || report.Axes["snapshot"].State != "unsafe" {
				t.Fatalf("snapshot report = %+v", report)
			}
		})
	}
}

func TestSemanticStaleReportsSymlinkedSnapshotUnsafe(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	snapshotPath := filepath.Join(brainDir, filepath.FromSlash(manifest.Sources.Semantic.SnapshotPath))
	snapshotDir := filepath.Dir(snapshotPath)
	externalDir := filepath.Join(t.TempDir(), "external-snapshot")
	if err := os.MkdirAll(externalDir, 0o700); err != nil {
		t.Fatalf("mkdir external: %v", err)
	}
	if err := os.WriteFile(filepath.Join(externalDir, semanticSnapshotName), []byte(semanticFixtureSnapshot("1.0")), 0o600); err != nil {
		t.Fatalf("write external snapshot: %v", err)
	}
	if err := os.RemoveAll(snapshotDir); err != nil {
		t.Fatalf("remove snapshot dir: %v", err)
	}
	if err := os.Symlink(externalDir, snapshotDir); err != nil {
		t.Fatalf("symlink snapshot dir: %v", err)
	}
	report, err := semanticStaleReport(cmd.Context(), opts, repoDir)
	if err != nil {
		t.Fatalf("stale: %v", err)
	}
	if report.Severity != "unsafe" || report.Axes["snapshot"].State != "unsafe" {
		t.Fatalf("stale did not reject symlinked snapshot: %+v", report)
	}
}

func TestSemanticQueryFindsExactSymbol(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	if err := runSemanticIndex(cmd.Context(), cmd, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}

	manifest, err := loadBrainManifest(filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo"))
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	results, err := findSemanticSymbols(filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo", filepath.FromSlash(manifest.Sources.Semantic.SnapshotPath)), "ValidateToken", 10, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(results) != 1 || results[0].QualifiedName != "auth.ValidateToken" {
		t.Fatalf("results = %+v", results)
	}
}

func TestSemanticIndexNormalizesSymbolPathIntoSQLiteFilePath(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	snapshot := strings.Replace(semanticFixtureSnapshot("1.0"), `"file_path":"internal/auth/token.go"`, `"path":"internal/auth/token.go"`, 1)
	runner := semanticFixtureRunner(repoDir, snapshot)
	cmd := &cobra.Command{Use: "index"}
	if err := runSemanticIndex(cmd.Context(), cmd, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	manifest, err := loadBrainManifest(filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo"))
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	results, err := findSemanticSymbolsInSQLite(filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo", filepath.FromSlash(manifest.Sources.Semantic.StorePath)), "ValidateToken", 10, 0)
	if err != nil {
		t.Fatalf("query sqlite: %v", err)
	}
	if len(results) != 1 || results[0].FilePath != "internal/auth/token.go" {
		t.Fatalf("results = %+v", results)
	}
}

func TestSemanticQueryTreatsSQLiteSearchTextLiterally(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	if err := runSemanticIndex(cmd.Context(), cmd, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	manifest, err := loadBrainManifest(filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo"))
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	results, err := findSemanticSymbolsInSQLite(filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo", filepath.FromSlash(manifest.Sources.Semantic.StorePath)), "%", 10, 0)
	if err != nil {
		t.Fatalf("query sqlite: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("LIKE wildcard matched literal query: %+v", results)
	}
}

func TestSemanticQueryJSONIncludesPagination(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	var out bytes.Buffer
	queryCmd := &cobra.Command{Use: "query"}
	queryCmd.SetOut(&out)
	if err := runSemanticQuery(queryCmd.Context(), queryCmd, opts, semanticQueryOptions{limit: 1, offset: 0, json: true}, "ValidateToken"); err != nil {
		t.Fatalf("query: %v", err)
	}
	if !strings.Contains(out.String(), `"pagination"`) || !strings.Contains(out.String(), `"count": 1`) {
		t.Fatalf("query JSON missing pagination:\n%s", out.String())
	}
}

func TestSemanticContextJSONIncludesRelations(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	var out bytes.Buffer
	contextCmd := &cobra.Command{Use: "context"}
	contextCmd.SetOut(&out)
	if err := runSemanticContext(contextCmd.Context(), contextCmd, opts, semanticContextOptions{limit: 10, json: true}, "ValidateToken"); err != nil {
		t.Fatalf("context: %v", err)
	}
	if !strings.Contains(out.String(), `"relations"`) || !strings.Contains(out.String(), `"CALLS"`) {
		t.Fatalf("context JSON missing relations:\n%s", out.String())
	}
}

func TestSemanticImpactTraversesRelations(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	var out bytes.Buffer
	impactCmd := &cobra.Command{Use: "impact"}
	impactCmd.SetOut(&out)
	if err := runSemanticImpact(impactCmd.Context(), impactCmd, opts, semanticImpactOptions{limit: 10, depth: 1, json: true}, "ValidateToken"); err != nil {
		t.Fatalf("impact: %v", err)
	}
	if !strings.Contains(out.String(), `"relations"`) || !strings.Contains(out.String(), `"CALLS"`) {
		t.Fatalf("impact JSON missing relation:\n%s", out.String())
	}
}

func TestSemanticImpactReturnsRelationsWhenRootsFillLimit(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	roots, symbols, relations, err := semanticImpactFacts(brainDir, mustSemanticSource(t, env), "ValidateToken", 1, 1)
	if err != nil {
		t.Fatalf("impact facts: %v", err)
	}
	if len(roots) != 1 || len(symbols) != 1 || len(relations) != 1 {
		t.Fatalf("impact = roots %+v symbols %+v relations %+v", roots, symbols, relations)
	}
}

func TestSemanticImpactFallsBackToSnapshot(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	source := *mustSemanticSource(t, env)
	source.GenerationPath = ""
	source.StorePath = ""
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	roots, symbols, relations, err := semanticImpactFacts(brainDir, &source, "ValidateToken", 1, 10)
	if err != nil {
		t.Fatalf("impact facts: %v", err)
	}
	if len(roots) != 1 || len(symbols) != 1 || len(relations) != 1 {
		t.Fatalf("snapshot impact = roots %+v symbols %+v relations %+v", roots, symbols, relations)
	}
}

func TestSemanticChangesMapsChangedFilesToSymbols(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	runner.responses[fakeCommandKey("git", "diff", "--name-status", "-M", "-C", "HEAD")] = fakeCommandResponse{stdout: "M\tinternal/auth/token.go\n"}
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	var out bytes.Buffer
	changesCmd := &cobra.Command{Use: "changes"}
	changesCmd.SetOut(&out)
	if err := runSemanticChanges(changesCmd.Context(), changesCmd, opts, semanticChangesOptions{limit: 10, json: true}); err != nil {
		t.Fatalf("changes: %v", err)
	}
	if !strings.Contains(out.String(), `"internal/auth/token.go"`) || !strings.Contains(out.String(), `"ValidateToken"`) {
		t.Fatalf("changes JSON missing symbol:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo", semanticDirName, "changes", "latest.json")); err != nil {
		t.Fatalf("changes report missing: %v", err)
	}
}

func TestSemanticChangesIncludesRenamedOldPath(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	runner.responses[fakeCommandKey("git", "diff", "--name-status", "-M", "-C", "HEAD")] = fakeCommandResponse{stdout: "R100\tinternal/auth/token.go\tinternal/auth/token_new.go\n"}
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	var out bytes.Buffer
	changesCmd := &cobra.Command{Use: "changes"}
	changesCmd.SetOut(&out)
	if err := runSemanticChanges(changesCmd.Context(), changesCmd, opts, semanticChangesOptions{limit: 10, json: true}); err != nil {
		t.Fatalf("changes: %v", err)
	}
	if !strings.Contains(out.String(), `"internal/auth/token.go"`) || !strings.Contains(out.String(), `"ValidateToken"`) {
		t.Fatalf("changes JSON missing renamed old-path symbol:\n%s", out.String())
	}
}

func TestSemanticChangesFiltersBrainignoredFiles(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoDir, ".brainignore"), []byte("ignored/\n"), 0o600); err != nil {
		t.Fatalf("write brainignore: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "ignored"), 0o700); err != nil {
		t.Fatalf("mkdir ignored: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "ignored", "secret.go"), []byte("package ignored\n"), 0o600); err != nil {
		t.Fatalf("write ignored: %v", err)
	}
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "diff", "--name-status", "-M", "-C", "HEAD"): {stdout: "M\tignored/tracked.go\nM\tinternal/auth/token.go\n"},
		fakeCommandKey("git", "status", "--porcelain"):                     {stdout: "?? ignored/\n?? .env\n"},
	}}
	files, err := changedSemanticFiles((&cobra.Command{}).Context(), runner, repoDir)
	if err != nil {
		t.Fatalf("changed files: %v", err)
	}
	if len(files) != 1 || files[0] != "internal/auth/token.go" {
		t.Fatalf("files = %+v", files)
	}
}

func TestSemanticChangesDecodesQuotedUntrackedPath(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoDir, "new file.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatalf("write untracked: %v", err)
	}
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "diff", "--name-status", "-M", "-C", "HEAD"): {},
		fakeCommandKey("git", "status", "--porcelain"):                     {stdout: "?? \"new file.go\"\n"},
	}}
	files, err := changedSemanticFiles((&cobra.Command{}).Context(), runner, repoDir)
	if err != nil {
		t.Fatalf("changed files: %v", err)
	}
	if len(files) != 1 || files[0] != "new file.go" {
		t.Fatalf("files = %+v", files)
	}
}

func TestSemanticChangesExpandsUntrackedDirectory(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, "newpkg"), 0o700); err != nil {
		t.Fatalf("mkdir newpkg: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "newpkg", "foo.go"), []byte("package newpkg\n"), 0o600); err != nil {
		t.Fatalf("write new file: %v", err)
	}
	env := semanticTestEnv(t, repoDir)
	snapshot := strings.Replace(semanticFixtureSnapshot("1.0"), "internal/auth/token.go", "newpkg/foo.go", -1)
	runner := semanticFixtureRunner(repoDir, snapshot)
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	runner.responses[fakeCommandKey("git", "diff", "--name-status", "-M", "-C", "HEAD")] = fakeCommandResponse{stdout: ""}
	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{stdout: "?? newpkg/\n"}
	files, err := changedSemanticFiles(cmd.Context(), runner, repoDir)
	if err != nil {
		t.Fatalf("changed files: %v", err)
	}
	if len(files) != 1 || files[0] != "newpkg/foo.go" {
		t.Fatalf("files = %+v", files)
	}
}

func TestSemanticChangesIncludesUntrackedAndFailsOnDiffError(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	runner.responses[fakeCommandKey("git", "diff", "--name-status", "-M", "-C", "HEAD")] = fakeCommandResponse{stdout: ""}
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{stdout: "?? internal/auth/token.go\n"}
	var out bytes.Buffer
	changesCmd := &cobra.Command{Use: "changes"}
	changesCmd.SetOut(&out)
	if err := runSemanticChanges(changesCmd.Context(), changesCmd, opts, semanticChangesOptions{limit: 10, json: true}); err != nil {
		t.Fatalf("changes: %v", err)
	}
	if !strings.Contains(out.String(), `"ValidateToken"`) {
		t.Fatalf("changes did not include untracked symbol:\n%s", out.String())
	}
	runner.responses[fakeCommandKey("git", "diff", "--name-status", "-M", "-C", "HEAD")] = fakeCommandResponse{err: errors.New("diff failed")}
	if err := runSemanticChanges(changesCmd.Context(), changesCmd, opts, semanticChangesOptions{limit: 10, json: true}); err == nil {
		t.Fatalf("changes succeeded after diff failure")
	}
}

func TestSemanticBoundaryCommandsListRoutesToolsAndWorkflows(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticBoundaryFixtureSnapshot())
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
	if _, err := execute(t, cmd, "refresh", "index", "--sem-binary", "entire"); err != nil {
		t.Fatalf("index: %v", err)
	}
	for _, tc := range []struct {
		kind string
		want string
	}{
		{kind: "route", want: `"GET /tokens/{id}"`},
		{kind: "tool", want: `"brain refresh"`},
		{kind: "workflow", want: `"token validation"`},
	} {
		out, err := execute(t, cmd, "inspect", "boundaries", "--kind", tc.kind, "--json")
		if err != nil {
			t.Fatalf("%s: %v", tc.kind, err)
		}
		if !strings.Contains(out, tc.want) {
			t.Fatalf("%s output missing %s:\n%s", tc.kind, tc.want, out)
		}
	}
}

func TestSemanticBoundaryLimitFiltersRelationsAndHandlers(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticBoundaryFixtureSnapshotWithExtraRoute())
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
	if _, err := execute(t, cmd, "refresh", "index", "--sem-binary", "entire"); err != nil {
		t.Fatalf("index: %v", err)
	}
	out, err := execute(t, cmd, "inspect", "boundaries", "--kind", "route", "--limit", "1", "--json")
	if err != nil {
		t.Fatalf("routes: %v", err)
	}
	if !strings.Contains(out, `"GET /tokens/{id}"`) {
		t.Fatalf("routes output missing first route:\n%s", out)
	}
	if strings.Contains(out, `"POST /sessions"`) || strings.Contains(out, `"CreateSession"`) {
		t.Fatalf("routes limit leaked omitted boundary relation:\n%s", out)
	}
}

func TestSemanticTestsSuggestsRelevantTests(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticBoundaryFixtureSnapshot())
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
	if _, err := execute(t, cmd, "refresh", "index", "--sem-binary", "entire"); err != nil {
		t.Fatalf("index: %v", err)
	}
	out, err := execute(t, cmd, "inspect", "tests", "ValidateToken", "--json")
	if err != nil {
		t.Fatalf("tests: %v", err)
	}
	if !strings.Contains(out, `"TestValidateToken"`) || !strings.Contains(out, `"reason"`) {
		t.Fatalf("tests output missing relevant suggestion:\n%s", out)
	}
}

func TestSemanticBoundaryFactsFallBackToSnapshot(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticBoundaryFixtureSnapshot())
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	source := *mustSemanticSource(t, env)
	source.GenerationPath = ""
	source.StorePath = ""
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	result, err := semanticBoundaryFacts(brainDir, &source, semanticBoundarySpec{
		SymbolKinds:   []string{"route", "http_route"},
		RelationTypes: []string{"HANDLES_ROUTE"},
	}, 10)
	if err != nil {
		t.Fatalf("boundary facts: %v", err)
	}
	if len(result.Boundaries) != 1 || result.Boundaries[0].Name != "GET /tokens/{id}" || len(result.Handlers) != 1 {
		t.Fatalf("snapshot boundary result = %+v", result)
	}
}

func TestSemanticContextSQLiteFiltersRelationsBeforeLimit(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshotWithDelayedRelevantRelation())
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	symbols, relations, err := semanticContextFacts(filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo"), mustSemanticSource(t, env), "ValidateToken", 1, 0)
	if err != nil {
		t.Fatalf("context facts: %v", err)
	}
	if len(symbols) != 1 || len(relations) != 1 || relations[0].FromID != "caller" {
		t.Fatalf("context = symbols %+v relations %+v", symbols, relations)
	}
}

func TestSemanticContextContentSkipsSymlinkedRepoPath(t *testing.T) {
	repoDir := t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	if err := os.Symlink(secret, filepath.Join(repoDir, "link.go")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	content := semanticContextContent(repoDir, []semanticRecord{{FilePath: "link.go", StartLine: 1, EndLine: 1}})
	if len(content) != 0 {
		t.Fatalf("symlinked content was included: %+v", content)
	}
	if hash := semanticFileContentHash(repoDir, "link.go"); hash != "" {
		t.Fatalf("symlinked content was hashed: %s", hash)
	}
}

func TestSemanticContextIncludeContentRequiresFreshness(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, "internal", "auth"), 0o700); err != nil {
		t.Fatalf("mkdir repo file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "internal", "auth", "token.go"), []byte("package auth\n"), 0o600); err != nil {
		t.Fatalf("write repo file: %v", err)
	}
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{stdout: " M internal/auth/token.go\n"}
	err := runSemanticContext(cmd.Context(), cmd, opts, semanticContextOptions{limit: 10, includeContent: true}, "ValidateToken")
	if err == nil || !strings.Contains(err.Error(), "requires fresh semantic data") {
		t.Fatalf("context err = %v", err)
	}
}

func TestSemanticContextSnippetReadsRequestedRange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.go")
	var b strings.Builder
	for i := 1; i <= 5000; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write large file: %v", err)
	}
	text, truncated, err := readSemanticContextSnippet(path, 4999, 5000)
	if err != nil {
		t.Fatalf("snippet: %v", err)
	}
	if truncated {
		t.Fatalf("snippet was truncated")
	}
	if text != "line 4999\nline 5000" {
		t.Fatalf("snippet = %q", text)
	}
}

func TestSemanticContextSnippetTruncatesLargeRanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.go")
	var b strings.Builder
	for i := 1; i <= semanticContextMaxLines+20; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write large file: %v", err)
	}
	text, truncated, err := readSemanticContextSnippet(path, 1, semanticContextMaxLines+20)
	if err != nil {
		t.Fatalf("snippet: %v", err)
	}
	if !truncated {
		t.Fatalf("snippet was not truncated")
	}
	if strings.Contains(text, fmt.Sprintf("line %d", semanticContextMaxLines+1)) {
		t.Fatalf("snippet exceeded max lines")
	}
}

func TestSemanticQueryJSONIncludesFreshness(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	runner.responses[fakeCommandKey("git", "rev-parse", "HEAD")] = fakeCommandResponse{stdout: "bbb222\n"}
	var out bytes.Buffer
	queryCmd := &cobra.Command{Use: "query"}
	queryCmd.SetOut(&out)
	if err := runSemanticQuery(queryCmd.Context(), queryCmd, opts, semanticQueryOptions{limit: 10, json: true}, "ValidateToken"); err != nil {
		t.Fatalf("query: %v", err)
	}
	if !strings.Contains(out.String(), `"freshness"`) || !strings.Contains(out.String(), `"severity": "unsafe"`) {
		t.Fatalf("query JSON missing unsafe freshness:\n%s", out.String())
	}
}

func TestSemanticQueryRejectsUnsafeManifestSnapshotPath(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	manifest.Sources.Semantic.SnapshotPath = "../outside.ndjson"
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	err = runSemanticQuery(cmd.Context(), cmd, opts, semanticQueryOptions{limit: 10}, "ValidateToken")
	if err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("query err = %v", err)
	}
}

func TestSemanticIndexLockFailsFast(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	writeSemanticTestLock(t, brainDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire"}, repoDir)
	if err == nil || !strings.Contains(err.Error(), "index_locked") {
		t.Fatalf("lock err = %v", err)
	}
}

func TestSemanticIndexIgnoresStaleLockFile(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	lockDir := filepath.Join(brainDir, semanticLockDir)
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		t.Fatalf("mkdir lock: %v", err)
	}
	if err := os.WriteFile(filepath.Join(lockDir, semanticIndexLockName), []byte("stale\n"), 0o600); err != nil {
		t.Fatalf("write stale lock: %v", err)
	}
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	if err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index with stale lock file: %v", err)
	}
}

func TestSemanticIndexRejectsSymlinkedLockDirectory(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	external := t.TempDir()
	if err := os.MkdirAll(brainDir, 0o700); err != nil {
		t.Fatalf("mkdir brain: %v", err)
	}
	if err := os.Symlink(external, filepath.Join(brainDir, semanticLockDir)); err != nil {
		t.Fatalf("symlink lock dir: %v", err)
	}
	err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire"}, repoDir)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("index err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(external, semanticIndexLockName)); !os.IsNotExist(err) {
		t.Fatalf("external lock was created: %v", err)
	}
}

func TestSemanticIndexRejectsSymlinkedBrainRoot(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	if err := os.MkdirAll(filepath.Dir(brainDir), 0o700); err != nil {
		t.Fatalf("mkdir brain parent: %v", err)
	}
	external := t.TempDir()
	if err := os.Symlink(external, brainDir); err != nil {
		t.Fatalf("symlink brain root: %v", err)
	}
	err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire"}, repoDir)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("index err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(external, semanticDirName)); !os.IsNotExist(err) {
		t.Fatalf("semantic directory was created through symlinked brain root: %v", err)
	}
}

func TestSemanticIndexRejectsSymlinkedBrainParent(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	parent := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatalf("mkdir brain parent: %v", err)
	}
	external := t.TempDir()
	if err := os.Symlink(external, filepath.Join(parent, "example")); err != nil {
		t.Fatalf("symlink brain parent: %v", err)
	}
	err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire"}, repoDir)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("index err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(external, "repo")); !os.IsNotExist(err) {
		t.Fatalf("repo directory was created through symlinked brain parent: %v", err)
	}
}

func TestSemanticIndexRejectsSymlinkedSnapshotDestination(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	snapshotsParent := filepath.Join(brainDir, semanticDirName)
	if err := os.MkdirAll(snapshotsParent, 0o700); err != nil {
		t.Fatalf("mkdir semantic dir: %v", err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(snapshotsParent, semanticSnapshotsDir)); err != nil {
		t.Fatalf("symlink snapshots dir: %v", err)
	}

	err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire"}, repoDir)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("index symlink err = %v", err)
	}
}

func TestBundleExportLockFailsFast(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	writeSemanticTestLock(t, brainDir)

	err := runSemanticBundleExport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle export"}, Options{Env: env, Runner: runner, Now: time.Now}, filepath.Join(t.TempDir(), "brain.tar"))
	if err == nil || !strings.Contains(err.Error(), "index_locked") {
		t.Fatalf("export lock err = %v", err)
	}
}

func TestSemanticReadCommandsDoNotRequireExclusiveIndexLock(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticBoundaryFixtureSnapshot())
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	writeSemanticTestLock(t, brainDir)

	for _, tc := range []struct {
		name string
		run  func(*cobra.Command) error
	}{
		{
			name: "query",
			run: func(readCmd *cobra.Command) error {
				return runSemanticQuery(readCmd.Context(), readCmd, opts, semanticQueryOptions{limit: 10, json: true}, "ValidateToken")
			},
		},
		{
			name: "context",
			run: func(readCmd *cobra.Command) error {
				return runSemanticContext(readCmd.Context(), readCmd, opts, semanticContextOptions{limit: 10, json: true}, "ValidateToken")
			},
		},
		{
			name: "impact",
			run: func(readCmd *cobra.Command) error {
				return runSemanticImpact(readCmd.Context(), readCmd, opts, semanticImpactOptions{limit: 10, depth: 1, json: true}, "ValidateToken")
			},
		},
		{
			name: "routes",
			run: func(readCmd *cobra.Command) error {
				return runSemanticBoundary(readCmd.Context(), readCmd, opts, semanticBoundaryOptions{limit: 10, json: true}, semanticBoundarySpec{
					SymbolKinds:   []string{"route", "http_route"},
					RelationTypes: []string{"HANDLES_ROUTE"},
				})
			},
		},
		{
			name: "tests",
			run: func(readCmd *cobra.Command) error {
				return runSemanticTests(readCmd.Context(), readCmd, opts, semanticTestsOptions{limit: 10, json: true}, "ValidateToken")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			readCmd := &cobra.Command{Use: tc.name}
			readCmd.SetOut(&out)
			if err := tc.run(readCmd); err != nil {
				t.Fatalf("%s with index lock: %v", tc.name, err)
			}
		})
	}
}

func TestSemanticGCLockFailsFast(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	writeSemanticTestLock(t, brainDir)

	err := runSemanticGC((&cobra.Command{}).Context(), &cobra.Command{Use: "gc"}, Options{Env: env, Runner: runner, Now: time.Now}, repoDir, "24h")
	if err == nil || !strings.Contains(err.Error(), "index_locked") {
		t.Fatalf("gc lock err = %v", err)
	}
}

func TestSemanticGCRejectsSymlinkedSnapshotsRoot(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	if err := os.MkdirAll(filepath.Join(brainDir, semanticDirName), 0o700); err != nil {
		t.Fatalf("mkdir semantic dir: %v", err)
	}
	externalSnapshots := t.TempDir()
	externalOld := filepath.Join(externalSnapshots, "old")
	if err := os.MkdirAll(externalOld, 0o700); err != nil {
		t.Fatalf("mkdir external old snapshot: %v", err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(externalOld, old, old); err != nil {
		t.Fatalf("chtimes external snapshot: %v", err)
	}
	if err := os.Symlink(externalSnapshots, filepath.Join(brainDir, semanticDirName, semanticSnapshotsDir)); err != nil {
		t.Fatalf("symlink snapshots root: %v", err)
	}

	err := runSemanticGC((&cobra.Command{}).Context(), &cobra.Command{Use: "gc"}, Options{Env: env, Runner: runner, Now: time.Now}, repoDir, "24h")
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("gc symlink err = %v", err)
	}
	if _, statErr := os.Stat(externalOld); statErr != nil {
		t.Fatalf("external snapshot was removed: %v", statErr)
	}
}

func TestSemanticGCPrunesOldGenerationsAndKeepsActiveGeneration(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	now := time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC)
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: func() time.Time { return now }}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	oldSnapshot := filepath.Join(brainDir, semanticDirName, semanticSnapshotsDir, "old")
	oldGeneration := filepath.Join(brainDir, semanticDirName, semanticGenerationsDir, "old")
	if err := os.MkdirAll(oldSnapshot, 0o700); err != nil {
		t.Fatalf("mkdir old snapshot: %v", err)
	}
	if err := os.MkdirAll(oldGeneration, 0o700); err != nil {
		t.Fatalf("mkdir old generation: %v", err)
	}
	if err := os.WriteFile(filepath.Join(oldGeneration, semanticSQLiteName), []byte("old"), 0o600); err != nil {
		t.Fatalf("write old generation: %v", err)
	}
	old := now.Add(-48 * time.Hour)
	for _, path := range []string{
		oldSnapshot,
		oldGeneration,
		filepath.Join(brainDir, filepath.FromSlash(manifest.Sources.Semantic.GenerationPath)),
	} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatalf("chtimes %s: %v", path, err)
		}
	}
	if err := runSemanticGC((&cobra.Command{}).Context(), &cobra.Command{Use: "gc"}, opts, repoDir, "24h"); err != nil {
		t.Fatalf("gc: %v", err)
	}
	if _, err := os.Stat(oldSnapshot); !os.IsNotExist(err) {
		t.Fatalf("old snapshot still exists or stat failed differently: %v", err)
	}
	if _, err := os.Stat(oldGeneration); !os.IsNotExist(err) {
		t.Fatalf("old generation still exists or stat failed differently: %v", err)
	}
	if _, err := os.Stat(filepath.Join(brainDir, filepath.FromSlash(manifest.Sources.Semantic.GenerationPath))); err != nil {
		t.Fatalf("active generation was pruned: %v", err)
	}
}

func TestBundleImportLockFailsFast(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	writeSemanticTestLock(t, brainDir)
	archive := filepath.Join(t.TempDir(), "semantic.tar")
	writeTestBundle(t, archive, map[string]string{
		exportManifestFileName:                      `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"generated_at":"2026-05-31T00:00:00Z","provider":"entire-sem","schema_version":"1.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson","symbols":1}}}`,
		"semantic/snapshots/aaa111/snapshot.ndjson": semanticFixtureSnapshot("1.0"),
	})

	err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive))
	if err == nil || !strings.Contains(err.Error(), "index_locked") {
		t.Fatalf("import lock err = %v", err)
	}
}

func TestBundleImportRejectsSymlinkedSnapshotDestination(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	snapshotParent := filepath.Join(brainDir, semanticDirName, semanticSnapshotsDir)
	if err := os.MkdirAll(snapshotParent, 0o700); err != nil {
		t.Fatalf("mkdir snapshots dir: %v", err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(snapshotParent, "aaa111")); err != nil {
		t.Fatalf("symlink snapshot destination: %v", err)
	}
	archive := filepath.Join(t.TempDir(), "semantic.tar")
	writeTestBundle(t, archive, map[string]string{
		exportManifestFileName:                      `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"schema_version":"1.0","commit":"aaa111","tree":"tree111","provider":"entire-sem","provider_version":"0.1.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson","symbols":1,"relations":1}}}`,
		"semantic/snapshots/aaa111/snapshot.ndjson": semanticFixtureSnapshot("1.0"),
	})

	err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive))
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("import symlink err = %v", err)
	}
}

func TestSemanticGCFailsClosedOnCorruptManifest(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	snapshotDir := filepath.Join(brainDir, semanticDirName, semanticSnapshotsDir, "old")
	if err := os.MkdirAll(snapshotDir, 0o700); err != nil {
		t.Fatalf("mkdir snapshot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(snapshotDir, semanticSnapshotName), []byte("old"), 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(snapshotDir, old, old); err != nil {
		t.Fatalf("chtimes snapshot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(brainDir, exportManifestFileName), []byte("{bad-json"), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	err := runSemanticGC((&cobra.Command{}).Context(), &cobra.Command{Use: "gc"}, Options{Env: env, Runner: runner, Now: time.Now}, repoDir, "24h")
	if err == nil {
		t.Fatalf("gc succeeded with corrupt manifest")
	}
	if _, statErr := os.Stat(snapshotDir); statErr != nil {
		t.Fatalf("snapshot was pruned despite corrupt manifest: %v", statErr)
	}
}

func TestBundleRejectsRemotePaths(t *testing.T) {
	for _, path := range []string{"https://example.com/brain.tar", "s3://bucket/brain.tar", "git+ssh://host/repo"} {
		if err := rejectRemoteBundlePath(path); err == nil {
			t.Fatalf("expected remote path rejection for %s", path)
		}
	}
}

func TestBundleExportCreatesPrivateArchive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose Unix group/other permission bits for this assertion")
	}
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	output := filepath.Join(t.TempDir(), "brain.tar")
	if err := runSemanticBundleExport(cmd.Context(), cmd, opts, output); err != nil {
		t.Fatalf("bundle export: %v", err)
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatalf("stat bundle: %v", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("bundle mode = %o, want no group/other permissions", info.Mode().Perm())
	}
	entries := readTestBundleEntries(t, output)
	if !semanticTestContainsEntrySuffix(entries, semanticSQLiteName) {
		t.Fatalf("bundle missing semantic sqlite store: %+v", entries)
	}
	if !semanticTestContainsEntrySuffix(entries, semanticMetricsName) {
		t.Fatalf("bundle missing semantic metrics: %+v", entries)
	}
}

func TestSemanticOnlyBrainReadmeDescribesSemanticIndex(t *testing.T) {
	brainDir := t.TempDir()
	source := &semanticSourceManifest{
		GeneratedAt:   time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC),
		Commit:        "aaa111",
		Provider:      "entire-sem",
		SchemaVersion: "1.0",
		SnapshotPath:  "semantic/snapshots/aaa111/snapshot.ndjson",
		Symbols:       1,
	}
	if err := writeBrainSemanticSource(brainDir, "gh/example/repo", source); err != nil {
		t.Fatalf("write semantic source: %v", err)
	}
	readme, err := os.ReadFile(filepath.Join(brainDir, exportReadmeFileName))
	if err != nil {
		t.Fatalf("read readme: %v", err)
	}
	if !strings.Contains(string(readme), "Semantic Index") || strings.Contains(string(readme), "Session Export") {
		t.Fatalf("semantic README not rendered correctly:\n%s", readme)
	}
}

func TestBundleExportDoesNotTruncateExistingOutputWhenSemanticMissing(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	output := filepath.Join(t.TempDir(), "brain.tar")
	if err := os.WriteFile(output, []byte("old archive"), 0o600); err != nil {
		t.Fatalf("write output: %v", err)
	}
	err := runSemanticBundleExport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle export"}, Options{Env: env, Runner: runner, Now: time.Now}, output)
	if err == nil || !strings.Contains(err.Error(), "semantic index missing") {
		t.Fatalf("export err = %v", err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if string(data) != "old archive" {
		t.Fatalf("output was truncated: %q", data)
	}
}

func TestBundleExportFailsWhenActiveSnapshotMissing(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if err := os.Remove(filepath.Join(brainDir, filepath.FromSlash(manifest.Sources.Semantic.SnapshotPath))); err != nil {
		t.Fatalf("remove snapshot: %v", err)
	}
	output := filepath.Join(t.TempDir(), "brain.tar")
	if err := os.WriteFile(output, []byte("old archive"), 0o600); err != nil {
		t.Fatalf("write output: %v", err)
	}
	err = runSemanticBundleExport(cmd.Context(), cmd, opts, output)
	if err == nil || !strings.Contains(err.Error(), "active semantic snapshot missing") {
		t.Fatalf("export err = %v", err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if string(data) != "old archive" {
		t.Fatalf("output was truncated: %q", data)
	}
}

func TestBundleExportDoesNotTruncateExistingOutputWhenGenerationMissing(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(brainDir, filepath.FromSlash(manifest.Sources.Semantic.GenerationPath))); err != nil {
		t.Fatalf("remove generation: %v", err)
	}
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	output := filepath.Join(t.TempDir(), "brain.tar")
	if err := os.WriteFile(output, []byte("old archive"), 0o600); err != nil {
		t.Fatalf("write output: %v", err)
	}
	err = runSemanticBundleExport(cmd.Context(), cmd, opts, output)
	if err == nil {
		t.Fatalf("expected missing generation error")
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if string(data) != "old archive" {
		t.Fatalf("output was truncated: %q", data)
	}
}

func TestBundleExportRejectsUnsafeManifestSnapshotPath(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	manifest.Sources.Semantic.SnapshotPath = "semantic/audit.jsonl"
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	output := filepath.Join(t.TempDir(), "brain.tar")
	if err := os.WriteFile(output, []byte("old archive"), 0o600); err != nil {
		t.Fatalf("write output: %v", err)
	}
	err = runSemanticBundleExport(cmd.Context(), cmd, opts, output)
	if err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("export err = %v", err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if string(data) != "old archive" {
		t.Fatalf("output was truncated: %q", data)
	}
}

func TestBundleExportTightensExistingArchivePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose Unix group/other permission bits for this assertion")
	}
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	output := filepath.Join(t.TempDir(), "brain.tar")
	if err := os.WriteFile(output, []byte("old"), 0o644); err != nil {
		t.Fatalf("write old bundle: %v", err)
	}
	if err := runSemanticBundleExport(cmd.Context(), cmd, opts, output); err != nil {
		t.Fatalf("bundle export: %v", err)
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatalf("stat bundle: %v", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("bundle mode = %o, want no group/other permissions", info.Mode().Perm())
	}
}

func TestBundleExportExcludesLocalAuditLog(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	if err := appendSemanticAudit(brainDir, "bundle_export", "/local/path/brain.tar", "abc"); err != nil {
		t.Fatalf("append audit: %v", err)
	}
	output := filepath.Join(t.TempDir(), "brain.tar")
	if err := runSemanticBundleExport(cmd.Context(), cmd, opts, output); err != nil {
		t.Fatalf("bundle export: %v", err)
	}
	for _, entry := range readTestBundleEntries(t, output) {
		if entry == "semantic/audit.jsonl" {
			t.Fatalf("bundle included local audit log")
		}
	}
}

func TestBundleExportRejectsSymlinkedAuditLog(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	auditPath := filepath.Join(brainDir, semanticDirName, semanticAuditLogName)
	if err := os.Remove(auditPath); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove audit: %v", err)
	}
	target := filepath.Join(t.TempDir(), "audit.jsonl")
	if err := os.WriteFile(target, []byte("external\n"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Symlink(target, auditPath); err != nil {
		t.Fatalf("symlink audit: %v", err)
	}
	err := runSemanticBundleExport(cmd.Context(), cmd, opts, filepath.Join(t.TempDir(), "brain.tar"))
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("export err = %v", err)
	}
}

func TestBundleExportRejectsOutputInsideBrainDir(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	output := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo", semanticDirName, semanticBundleDir, "export.tar")
	err := runSemanticBundleExport(cmd.Context(), cmd, opts, output)
	if err == nil || !strings.Contains(err.Error(), "outside the active brain directory") {
		t.Fatalf("export err = %v", err)
	}
}

func TestBundleExportRejectsSymlinkAncestorIntoBrainDir(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	link := filepath.Join(t.TempDir(), "link-to-brain")
	if err := os.Symlink(brainDir, link); err != nil {
		t.Fatalf("symlink to brain: %v", err)
	}
	err := runSemanticBundleExport(cmd.Context(), cmd, opts, filepath.Join(link, "new", "bundle.tar"))
	if err == nil || !strings.Contains(err.Error(), "outside the active brain directory") {
		t.Fatalf("export err = %v", err)
	}
}

func TestBundleExportRejectsSymlinkOutputIntoBrainDir(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	target := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo", semanticDirName, semanticSnapshotsDir, "self.tar")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	output := filepath.Join(t.TempDir(), "link.tar")
	if err := os.Symlink(target, output); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	err := runSemanticBundleExport(cmd.Context(), cmd, opts, output)
	if err == nil || !strings.Contains(err.Error(), "outside the active brain directory") {
		t.Fatalf("export err = %v", err)
	}
}

func TestBundleExportRejectsHardLinkedOutputToBrainFile(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	snapshotPath := filepath.Join(brainDir, filepath.FromSlash(manifest.Sources.Semantic.SnapshotPath))
	before, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	output := filepath.Join(t.TempDir(), "brain.tar")
	if err := os.Link(snapshotPath, output); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	err = runSemanticBundleExport(cmd.Context(), cmd, opts, output)
	if err == nil || !strings.Contains(err.Error(), "hard link") {
		t.Fatalf("export err = %v", err)
	}
	after, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatalf("read snapshot after export: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("snapshot was modified through hard-linked output")
	}
}

func TestBundleExportRejectsSymlinkInsideSnapshotTree(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	snapshotPath := filepath.Join(brainDir, filepath.FromSlash(manifest.Sources.Semantic.SnapshotPath))
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	if err := os.Remove(snapshotPath); err != nil {
		t.Fatalf("remove snapshot: %v", err)
	}
	if err := os.Symlink(secret, snapshotPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	err = runSemanticBundleExport(cmd.Context(), cmd, opts, filepath.Join(t.TempDir(), "brain.tar"))
	if err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("export err = %v", err)
	}
}

func TestBundleExportAndQueryRejectSymlinkedSnapshotDirectory(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	snapshotPath := filepath.Join(brainDir, filepath.FromSlash(manifest.Sources.Semantic.SnapshotPath))
	snapshotDir := filepath.Dir(snapshotPath)
	externalDir := filepath.Join(t.TempDir(), "external-snapshot")
	if err := os.MkdirAll(externalDir, 0o700); err != nil {
		t.Fatalf("mkdir external: %v", err)
	}
	if err := os.WriteFile(filepath.Join(externalDir, semanticSnapshotName), []byte(semanticFixtureSnapshot("1.0")), 0o600); err != nil {
		t.Fatalf("write external snapshot: %v", err)
	}
	if err := os.RemoveAll(snapshotDir); err != nil {
		t.Fatalf("remove snapshot dir: %v", err)
	}
	if err := os.Symlink(externalDir, snapshotDir); err != nil {
		t.Fatalf("symlink snapshot dir: %v", err)
	}
	err = runSemanticBundleExport(cmd.Context(), cmd, opts, filepath.Join(t.TempDir(), "brain.tar"))
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("export err = %v", err)
	}
	err = runSemanticQuery(cmd.Context(), cmd, opts, semanticQueryOptions{limit: 10}, "ValidateToken")
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("query err = %v", err)
	}
}

func TestBundleImportRejectsRepoKeyMismatch(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	archive := filepath.Join(t.TempDir(), "bad.tar")
	writeTestBundle(t, archive, map[string]string{
		exportManifestFileName:                      `{"schema_version":3,"repo_key":"gh/other/repo","sources":{"semantic":{"snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson"}}}`,
		"semantic/snapshots/aaa111/snapshot.ndjson": semanticFixtureSnapshot("1.0"),
	})
	err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive))
	if err == nil || !strings.Contains(err.Error(), "does not match current repo") {
		t.Fatalf("import err = %v", err)
	}
}

func TestBundleImportRejectsManifestSnapshotMetadataMismatch(t *testing.T) {
	for name, manifestSource := range map[string]string{
		"commit":         `"schema_version":"1.0","commit":"bbb222","tree":"tree111","provider":"entire-sem","provider_version":"0.1.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson","symbols":1,"relations":1`,
		"tree":           `"schema_version":"1.0","commit":"aaa111","tree":"tree222","provider":"entire-sem","provider_version":"0.1.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson","symbols":1,"relations":1`,
		"provider":       `"schema_version":"1.0","commit":"aaa111","tree":"tree111","provider":"other-sem","provider_version":"0.1.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson","symbols":1,"relations":1`,
		"zero-symbols":   `"schema_version":"1.0","commit":"aaa111","tree":"tree111","provider":"entire-sem","provider_version":"0.1.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson","symbols":0,"relations":1`,
		"zero-relations": `"schema_version":"1.0","commit":"aaa111","tree":"tree111","provider":"entire-sem","provider_version":"0.1.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson","symbols":1,"relations":0`,
		"symbols":        `"schema_version":"1.0","commit":"aaa111","tree":"tree111","provider":"entire-sem","provider_version":"0.1.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson","symbols":2,"relations":1`,
		"relations":      `"schema_version":"1.0","commit":"aaa111","tree":"tree111","provider":"entire-sem","provider_version":"0.1.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson","symbols":1,"relations":2`,
	} {
		t.Run(name, func(t *testing.T) {
			repoDir := t.TempDir()
			env := semanticTestEnv(t, repoDir)
			runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
			archive := filepath.Join(t.TempDir(), "bad-metadata.tar")
			writeTestBundle(t, archive, map[string]string{
				exportManifestFileName:                      `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{` + manifestSource + `}}}`,
				"semantic/snapshots/aaa111/snapshot.ndjson": semanticFixtureSnapshot("1.0"),
			})
			err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive))
			if err == nil || !strings.Contains(err.Error(), "does not match semantic snapshot") {
				t.Fatalf("import err = %v", err)
			}
		})
	}
}

func TestBundleImportFillsOmittedCountsFromSnapshot(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	archive := filepath.Join(t.TempDir(), "omitted-counts.tar")
	writeTestBundle(t, archive, map[string]string{
		exportManifestFileName:                      `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"schema_version":"1.0","commit":"aaa111","tree":"tree111","provider":"entire-sem","provider_version":"0.1.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson"}}}`,
		"semantic/snapshots/aaa111/snapshot.ndjson": semanticFixtureSnapshot("1.0"),
	})
	if err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive)); err != nil {
		t.Fatalf("import: %v", err)
	}
	manifest, err := loadBrainManifest(filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo"))
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if manifest.Sources.Semantic.Symbols != 1 || manifest.Sources.Semantic.Relations != 1 {
		t.Fatalf("counts were not filled from snapshot: %+v", manifest.Sources.Semantic)
	}
}

func TestBundleImportFillsOmittedProvenanceFromSnapshot(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	archive := filepath.Join(t.TempDir(), "omitted-provenance.tar")
	writeTestBundle(t, archive, map[string]string{
		exportManifestFileName:                      `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"schema_version":"1.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson","symbols":1,"relations":1}}}`,
		"semantic/snapshots/aaa111/snapshot.ndjson": semanticFixtureSnapshot("1.0"),
	})
	if err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive)); err != nil {
		t.Fatalf("import: %v", err)
	}
	manifest, err := loadBrainManifest(filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo"))
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	semantic := manifest.Sources.Semantic
	if semantic.Commit != "aaa111" || semantic.Tree != "tree111" || semantic.Provider != "entire-sem" || semantic.ProviderVersion != "0.1.0" {
		t.Fatalf("provenance was not filled from snapshot: %+v", semantic)
	}
}

func TestBundleImportQuerySkipsBlankSnapshotLines(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	archive := filepath.Join(t.TempDir(), "blank-lines.tar")
	writeTestBundle(t, archive, map[string]string{
		exportManifestFileName:                      `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"schema_version":"1.0","commit":"aaa111","tree":"tree111","provider":"entire-sem","provider_version":"0.1.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson","symbols":1,"relations":1}}}`,
		"semantic/snapshots/aaa111/snapshot.ndjson": strings.Replace(semanticFixtureSnapshot("1.0"), "\n{\"record_type\":\"relation\"", "\n\n{\"record_type\":\"relation\"", 1),
	})
	if err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive)); err != nil {
		t.Fatalf("import: %v", err)
	}
	if err := runSemanticQuery((&cobra.Command{}).Context(), &cobra.Command{Use: "query"}, Options{Env: env, Runner: runner, Now: time.Now}, semanticQueryOptions{limit: 10}, "ValidateToken"); err != nil {
		t.Fatalf("query: %v", err)
	}
	if err := runSemanticContext((&cobra.Command{}).Context(), &cobra.Command{Use: "context"}, Options{Env: env, Runner: runner, Now: time.Now}, semanticContextOptions{limit: 10}, "ValidateToken"); err != nil {
		t.Fatalf("context: %v", err)
	}
}

func TestBundleImportRejectsAuditLogEntry(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	archive := filepath.Join(t.TempDir(), "audit.tar")
	writeTestBundle(t, archive, map[string]string{
		exportManifestFileName:                      `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"schema_version":"1.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson"}}}`,
		"semantic/audit.jsonl":                      `{"path":"/local/path"}` + "\n",
		"semantic/snapshots/aaa111/snapshot.ndjson": semanticFixtureSnapshot("1.0"),
	})
	err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive))
	if err == nil || !strings.Contains(err.Error(), "audit") {
		t.Fatalf("import err = %v", err)
	}
}

func TestBundleImportRejectsProviderPathEscape(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	archive := filepath.Join(t.TempDir(), "escape.tar")
	writeTestBundle(t, archive, map[string]string{
		exportManifestFileName: `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"schema_version":"1.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson"}}}`,
		"semantic/snapshots/aaa111/snapshot.ndjson": `{"schema_version":"1.0","provider":"entire-sem","provider_version":"0.1.0","repo_key":"gh/example/repo","commit":"aaa111","tree":"tree111","capabilities":["go"],"warnings":[],"partial_failures":[]}
{"record_type":"symbol","id":"escape","kind":"function","name":"Escape","qualified_name":"Escape","file_path":"../../secret.txt","start_line":1,"end_line":1,"language":"Go","stable_id_version":"1"}
`,
	})
	err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive))
	if err == nil || !strings.Contains(err.Error(), "semantic provider path") {
		t.Fatalf("import err = %v", err)
	}
}

func TestBundleImportRejectsUnreferencedGenerationEntry(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	archive := filepath.Join(t.TempDir(), "extra-generation.tar")
	writeTestBundle(t, archive, map[string]string{
		exportManifestFileName:                         `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"schema_version":"1.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson","generation_path":"semantic/generations/aaa111","store_path":"semantic/generations/aaa111/semantic.sqlite"}}}`,
		"semantic/snapshots/aaa111/snapshot.ndjson":    `{"schema_version":"1.0","provider":"entire-sem","provider_version":"0.1.0","repo_key":"gh/example/repo","commit":"aaa111","tree":"tree111","capabilities":["go"],"warnings":[],"partial_failures":[]}` + "\n",
		"semantic/generations/aaa111/semantic.sqlite":  "not checked before extra rejection",
		"semantic/generations/aaa111/extra-secret.txt": "secret",
	})
	err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive))
	if err == nil || !strings.Contains(err.Error(), "unreferenced semantic generation entry") {
		t.Fatalf("import err = %v", err)
	}
}

func TestBundleImportRejectsStorePathWithoutGenerationPath(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	archive := filepath.Join(t.TempDir(), "store-no-generation.tar")
	writeTestBundle(t, archive, map[string]string{
		exportManifestFileName:                      `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"schema_version":"1.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson","store_path":"semantic/generations/aaa111/semantic.sqlite"}}}`,
		"semantic/snapshots/aaa111/snapshot.ndjson": semanticFixtureSnapshot("1.0"),
	})
	err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive))
	if err == nil || !strings.Contains(err.Error(), "generation_path is required") {
		t.Fatalf("import err = %v", err)
	}
}

func TestBundleImportRejectsParseCachePathAtGenerationRoot(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	archive := filepath.Join(t.TempDir(), "bad-parse-cache.tar")
	writeTestBundle(t, archive, map[string]string{
		exportManifestFileName:                         `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"schema_version":"1.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson","generation_path":"semantic/generations/aaa111","parse_cache_path":"semantic/generations/aaa111"}}}`,
		"semantic/snapshots/aaa111/snapshot.ndjson":    semanticFixtureSnapshot("1.0"),
		"semantic/generations/aaa111/extra-secret.txt": "secret",
	})
	err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive))
	if err == nil || !strings.Contains(err.Error(), "parse_cache_path") {
		t.Fatalf("import err = %v", err)
	}
}

func TestBundleImportRejectsSymlinkedAuditLog(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	archive := filepath.Join(t.TempDir(), "brain.tar")
	writeTestBundle(t, archive, map[string]string{
		exportManifestFileName:                      `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"schema_version":"1.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson"}}}`,
		"semantic/snapshots/aaa111/snapshot.ndjson": semanticFixtureSnapshot("1.0"),
	})
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	auditPath := filepath.Join(brainDir, semanticDirName, semanticAuditLogName)
	if err := os.MkdirAll(filepath.Dir(auditPath), 0o700); err != nil {
		t.Fatalf("mkdir audit dir: %v", err)
	}
	target := filepath.Join(t.TempDir(), "audit.jsonl")
	if err := os.WriteFile(target, []byte("external\n"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Symlink(target, auditPath); err != nil {
		t.Fatalf("symlink audit: %v", err)
	}
	err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive))
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("import err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(brainDir, exportManifestFileName)); !os.IsNotExist(err) {
		t.Fatalf("manifest was written despite audit preflight failure: %v", err)
	}
}

func TestBundleImportPublishesNewGenerationWhenTargetExists(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	archive := filepath.Join(t.TempDir(), "generation.tar")
	if err := runSemanticBundleExport(cmd.Context(), cmd, opts, archive); err != nil {
		t.Fatalf("export: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	oldGeneration := manifest.Sources.Semantic.GenerationPath
	stale := filepath.Join(brainDir, filepath.FromSlash(oldGeneration), "parse-cache", "stale.json")
	if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
		t.Fatalf("mkdir stale generation: %v", err)
	}
	if err := os.WriteFile(stale, []byte("stale"), 0o600); err != nil {
		t.Fatalf("write stale generation: %v", err)
	}
	if err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, opts, archive, bundleSHA256(t, archive)); err != nil {
		t.Fatalf("import: %v", err)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("active generation was mutated in place: %v", err)
	}
	updated, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("load updated manifest: %v", err)
	}
	if updated.Sources.Semantic.GenerationPath == oldGeneration {
		t.Fatalf("import reused active generation path: %+v", updated.Sources.Semantic)
	}
	if _, err := os.Stat(filepath.Join(brainDir, filepath.FromSlash(updated.Sources.Semantic.StorePath))); err != nil {
		t.Fatalf("imported generation store missing: %v", err)
	}
}

func TestBundleImportRejectsInvalidSemanticSQLite(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	archive := filepath.Join(t.TempDir(), "invalid-sqlite.tar")
	writeTestBundle(t, archive, map[string]string{
		exportManifestFileName:                        `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"schema_version":"1.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson","generation_path":"semantic/generations/aaa111","store_path":"semantic/generations/aaa111/semantic.sqlite"}}}`,
		"semantic/snapshots/aaa111/snapshot.ndjson":   semanticFixtureSnapshot("1.0"),
		"semantic/generations/aaa111/semantic.sqlite": "not sqlite",
	})
	err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive))
	if err == nil || !strings.Contains(err.Error(), "semantic sqlite") {
		t.Fatalf("import err = %v", err)
	}
}

func TestBundleImportRejectsSQLiteCountMismatchWithOmittedCounts(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	dbPath := filepath.Join(t.TempDir(), semanticSQLiteName)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := initializeSemanticSQLite(db); err != nil {
		_ = db.Close()
		t.Fatalf("init sqlite: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close sqlite: %v", err)
	}
	dbData, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read sqlite: %v", err)
	}
	archive := filepath.Join(t.TempDir(), "mismatch-sqlite.tar")
	writeTestBundle(t, archive, map[string]string{
		exportManifestFileName:                        `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"schema_version":"1.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson","generation_path":"semantic/generations/aaa111","store_path":"semantic/generations/aaa111/semantic.sqlite"}}}`,
		"semantic/snapshots/aaa111/snapshot.ndjson":   semanticFixtureSnapshot("1.0"),
		"semantic/generations/aaa111/semantic.sqlite": string(dbData),
	})
	err = runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive))
	if err == nil || !strings.Contains(err.Error(), "symbol count") {
		t.Fatalf("import err = %v", err)
	}
}

func TestBundleImportRebuildsSQLiteStoreFromSnapshot(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	dbPath := filepath.Join(t.TempDir(), semanticSQLiteName)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := initializeSemanticSQLite(db); err != nil {
		_ = db.Close()
		t.Fatalf("init sqlite: %v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		_ = db.Close()
		t.Fatalf("begin sqlite: %v", err)
	}
	if err := insertSemanticSymbol(tx, semanticRecord{
		RecordType:      "symbol",
		ID:              "other",
		Kind:            "function",
		Name:            "OtherSymbol",
		QualifiedName:   "other.Symbol",
		FilePath:        "internal/other.go",
		StartLine:       1,
		EndLine:         2,
		Signature:       "func OtherSymbol()",
		Language:        "Go",
		StableIDVersion: "1",
	}); err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		t.Fatalf("insert symbol: %v", err)
	}
	if err := insertSemanticRelation(tx, semanticRecord{FromID: "other", ToID: "other", Type: "CALLS", Confidence: 1}); err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		t.Fatalf("insert relation: %v", err)
	}
	if err := tx.Commit(); err != nil {
		_ = db.Close()
		t.Fatalf("commit sqlite: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close sqlite: %v", err)
	}
	dbData, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read sqlite: %v", err)
	}
	archive := filepath.Join(t.TempDir(), "mismatched-store.tar")
	writeTestBundle(t, archive, map[string]string{
		exportManifestFileName:                        `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"schema_version":"1.0","commit":"aaa111","tree":"tree111","provider":"entire-sem","provider_version":"0.1.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson","generation_path":"semantic/generations/aaa111","store_path":"semantic/generations/aaa111/semantic.sqlite","symbols":1,"relations":1}}}`,
		"semantic/snapshots/aaa111/snapshot.ndjson":   semanticFixtureSnapshot("1.0"),
		"semantic/generations/aaa111/semantic.sqlite": string(dbData),
	})
	if err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive)); err != nil {
		t.Fatalf("import: %v", err)
	}
	manifest, err := loadBrainManifest(filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo"))
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	storePath := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo", filepath.FromSlash(manifest.Sources.Semantic.StorePath))
	results, err := findSemanticSymbolsInSQLite(storePath, "ValidateToken", 10, 0)
	if err != nil {
		t.Fatalf("query rebuilt store: %v", err)
	}
	if len(results) != 1 || results[0].Name != "ValidateToken" {
		t.Fatalf("rebuilt store missing snapshot symbol: %+v", results)
	}
	results, err = findSemanticSymbolsInSQLite(storePath, "OtherSymbol", 10, 0)
	if err != nil {
		t.Fatalf("query rebuilt store for stale symbol: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("rebuilt store retained mismatched bundled symbol: %+v", results)
	}
}

func TestBundleExportUsesSanitizedManifest(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	manifest.RepoRoot = "/private/local/repo"
	manifest.Sessions = []exportSession{{SessionID: "session", TranscriptPath: "sessions/main/session.jsonl"}}
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	output := filepath.Join(t.TempDir(), "brain.tar")
	if err := runSemanticBundleExport(cmd.Context(), cmd, opts, output); err != nil {
		t.Fatalf("export: %v", err)
	}
	manifestData := readTestBundleFile(t, output, exportManifestFileName)
	if strings.Contains(string(manifestData), "repo_root") || strings.Contains(string(manifestData), "sessions/main/session.jsonl") {
		t.Fatalf("bundle manifest was not sanitized:\n%s", manifestData)
	}
	if !strings.Contains(string(manifestData), `"semantic"`) {
		t.Fatalf("bundle manifest missing semantic source:\n%s", manifestData)
	}
}

func TestBundleExportRedactsSnapshotRepoRoot(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	snapshot := strings.Replace(semanticFixtureSnapshot("1.0"), `"repo_key":"gh/example/repo"`, `"repo_root":"/private/local/repo","repo_key":"gh/example/repo"`, 1)
	runner := semanticFixtureRunner(repoDir, snapshot)
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	output := filepath.Join(t.TempDir(), "brain.tar")
	if err := runSemanticBundleExport(cmd.Context(), cmd, opts, output); err != nil {
		t.Fatalf("export: %v", err)
	}
	snapshotData := readTestBundleFile(t, output, manifest.Sources.Semantic.SnapshotPath)
	if strings.Contains(string(snapshotData), "repo_root") || strings.Contains(string(snapshotData), "/private/local/repo") {
		t.Fatalf("bundle snapshot leaked repo root:\n%s", snapshotData)
	}
}

func TestBundleExportRedactsSnapshotRecordFreeText(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	recordPath := filepath.ToSlash(filepath.Join(repoDir, "secret", "symbol.go"))
	snapshot := strings.Replace(semanticFixtureSnapshot("1.0"), `"signature":"func ValidateToken(token string) error"`, `"signature":"func ValidateToken() // /opt/build/repo/secret.go"`, 1)
	snapshot = strings.Replace(snapshot, `"qualified_name":"auth.ValidateToken"`, `"qualified_name":"`+recordPath+`"`, 1)
	snapshot = strings.Replace(snapshot, `"confidence":1}`, `"confidence":1,"reason":"loaded D:/work/repo/secret.go"}`, 1)
	runner := semanticFixtureRunner(repoDir, snapshot)
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	output := filepath.Join(t.TempDir(), "brain.tar")
	if err := runSemanticBundleExport(cmd.Context(), cmd, opts, output); err != nil {
		t.Fatalf("export: %v", err)
	}
	snapshotData := readTestBundleFile(t, output, manifest.Sources.Semantic.SnapshotPath)
	if strings.Contains(string(snapshotData), "/opt/build") || strings.Contains(string(snapshotData), "D:/work") || strings.Contains(string(snapshotData), repoDir) || strings.Contains(string(snapshotData), recordPath) {
		t.Fatalf("bundle snapshot leaked record free text:\n%s", snapshotData)
	}
	if !strings.Contains(string(snapshotData), "redacted") {
		t.Fatalf("bundle snapshot did not redact record free text:\n%s", snapshotData)
	}
	storeData := readTestBundleFile(t, output, manifest.Sources.Semantic.StorePath)
	storePath := filepath.Join(t.TempDir(), semanticSQLiteName)
	if err := os.WriteFile(storePath, storeData, 0o600); err != nil {
		t.Fatalf("write bundled sqlite: %v", err)
	}
	db, err := sql.Open("sqlite", storePath)
	if err != nil {
		t.Fatalf("open bundled sqlite: %v", err)
	}
	defer db.Close()
	var qualifiedName, signature, reason string
	if err := db.QueryRow(`SELECT qualified_name, signature FROM symbols WHERE name = ?`, "ValidateToken").Scan(&qualifiedName, &signature); err != nil {
		t.Fatalf("read bundled symbol: %v", err)
	}
	if err := db.QueryRow(`SELECT reason FROM relations LIMIT 1`).Scan(&reason); err != nil {
		t.Fatalf("read bundled relation: %v", err)
	}
	for _, value := range []string{qualifiedName, signature, reason} {
		if strings.Contains(value, "/opt/build") || strings.Contains(value, "D:/work") || strings.Contains(value, repoDir) || strings.Contains(value, recordPath) {
			t.Fatalf("bundled sqlite leaked record free text: qualified_name=%q signature=%q reason=%q", qualifiedName, signature, reason)
		}
	}
}

func TestBundleExportSanitizesLegacySnapshotHeaderWarnings(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	snapshotPath := filepath.Join(brainDir, filepath.FromSlash(manifest.Sources.Semantic.SnapshotPath))
	snapshotData, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	lines := bytes.SplitN(snapshotData, []byte{'\n'}, 2)
	if len(lines) != 2 {
		t.Fatalf("unexpected snapshot data:\n%s", snapshotData)
	}
	absPath := filepath.ToSlash(filepath.Join(repoDir, "legacy", "header-warning.go"))
	var header semanticHeader
	if err := json.Unmarshal(lines[0], &header); err != nil {
		t.Fatalf("parse header: %v", err)
	}
	header.Warnings = []semanticWarning{{
		Code:     "legacy_header_absolute",
		Severity: "warning",
		Path:     absPath,
		Effect:   "read " + absPath,
		Detail:   "failed at " + absPath,
	}}
	headerData, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	rewrite := append(append(headerData, '\n'), lines[1]...)
	if err := os.WriteFile(snapshotPath, rewrite, 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	output := filepath.Join(t.TempDir(), "brain.tar")
	if err := runSemanticBundleExport(cmd.Context(), cmd, opts, output); err != nil {
		t.Fatalf("export: %v", err)
	}
	bundleSnapshot := readTestBundleFile(t, output, manifest.Sources.Semantic.SnapshotPath)
	if strings.Contains(string(bundleSnapshot), repoDir) || strings.Contains(string(bundleSnapshot), absPath) {
		t.Fatalf("bundle snapshot leaked legacy header warning:\n%s", bundleSnapshot)
	}
}

func TestBundleExportPreservesDistinctRedactedRecordIDs(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	snapshot := `{"schema_version":"1.0","provider":"entire-sem","provider_version":"0.1.0","repo_key":"gh/example/repo","commit":"aaa111","tree":"tree111","capabilities":["go"],"warnings":[],"partial_failures":[]}
{"record_type":"symbol","id":"/private/local/repo/a.go:function:A","kind":"function","name":"A","qualified_name":"pkg.A","file_path":"internal/a.go","start_line":1,"end_line":2,"signature":"func A()","language":"Go","stable_id_version":"1"}
{"record_type":"symbol","id":"/private/local/repo/b.go:function:B","kind":"function","name":"B","qualified_name":"pkg.B","file_path":"internal/b.go","start_line":1,"end_line":2,"signature":"func B()","language":"Go","stable_id_version":"1"}
{"record_type":"relation","from_id":"/private/local/repo/a.go:function:A","to_id":"/private/local/repo/b.go:function:B","type":"CALLS","confidence":1}
`
	runner := semanticFixtureRunner(repoDir, snapshot)
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	output := filepath.Join(t.TempDir(), "brain.tar")
	if err := runSemanticBundleExport(cmd.Context(), cmd, opts, output); err != nil {
		t.Fatalf("export: %v", err)
	}
	snapshotData := readTestBundleFile(t, output, manifest.Sources.Semantic.SnapshotPath)
	if strings.Contains(string(snapshotData), "/private/local/repo") {
		t.Fatalf("bundle snapshot leaked structural IDs:\n%s", snapshotData)
	}
	storeData := readTestBundleFile(t, output, manifest.Sources.Semantic.StorePath)
	storePath := filepath.Join(t.TempDir(), semanticSQLiteName)
	if err := os.WriteFile(storePath, storeData, 0o600); err != nil {
		t.Fatalf("write bundled sqlite: %v", err)
	}
	db, err := sql.Open("sqlite", storePath)
	if err != nil {
		t.Fatalf("open bundled sqlite: %v", err)
	}
	defer db.Close()
	if got := semanticTestSQLCount(t, storePath, "symbols"); got != 2 {
		t.Fatalf("bundled sqlite symbols = %d, want 2", got)
	}
	var fromID, toID string
	if err := db.QueryRow(`SELECT from_id, to_id FROM relations LIMIT 1`).Scan(&fromID, &toID); err != nil {
		t.Fatalf("read bundled relation: %v", err)
	}
	if fromID == toID || !strings.HasPrefix(fromID, "redacted:") || !strings.HasPrefix(toID, "redacted:") {
		t.Fatalf("relation endpoints were not distinctly redacted: from=%q to=%q", fromID, toID)
	}
}

func TestBundleExportRedactsSkipSemSnapshotRepoRoot(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{skipSem: true}, repoDir); err != nil {
		t.Fatalf("index --skip-sem: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	output := filepath.Join(t.TempDir(), "brain.tar")
	if err := runSemanticBundleExport(cmd.Context(), cmd, opts, output); err != nil {
		t.Fatalf("export: %v", err)
	}
	snapshotData := readTestBundleFile(t, output, manifest.Sources.Semantic.SnapshotPath)
	if strings.Contains(string(snapshotData), "repo_root") || strings.Contains(string(snapshotData), repoDir) {
		t.Fatalf("skip-sem bundle snapshot leaked repo root:\n%s", snapshotData)
	}
}

func TestBundleExportRedactsLegacySnapshotRepoRoot(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	snapshotPath := filepath.Join(brainDir, filepath.FromSlash(manifest.Sources.Semantic.SnapshotPath))
	data, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	legacy := strings.Replace(string(data), `"repo_key":"gh/example/repo"`, `"repo_root":"/private/local/repo","repo_key":"gh/example/repo"`, 1)
	if err := os.WriteFile(snapshotPath, []byte(legacy), 0o600); err != nil {
		t.Fatalf("write legacy snapshot: %v", err)
	}
	output := filepath.Join(t.TempDir(), "brain.tar")
	if err := runSemanticBundleExport(cmd.Context(), cmd, opts, output); err != nil {
		t.Fatalf("export: %v", err)
	}
	snapshotData := readTestBundleFile(t, output, manifest.Sources.Semantic.SnapshotPath)
	if strings.Contains(string(snapshotData), "repo_root") || strings.Contains(string(snapshotData), "/private/local/repo") {
		t.Fatalf("legacy bundle snapshot leaked repo root:\n%s", snapshotData)
	}
}

func TestBundleExportImportRoundTripPreservesQueryableSemanticStore(t *testing.T) {
	repoDir := t.TempDir()
	exportEnv := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	exportOpts := Options{Env: exportEnv, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, exportOpts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	output := filepath.Join(t.TempDir(), "brain.tar")
	if err := runSemanticBundleExport(cmd.Context(), cmd, exportOpts, output); err != nil {
		t.Fatalf("export: %v", err)
	}

	importEnv := semanticTestEnv(t, repoDir)
	importOpts := Options{Env: importEnv, Runner: runner, Now: time.Now}
	if err := runSemanticBundleImport(cmd.Context(), cmd, importOpts, output, bundleSHA256(t, output)); err != nil {
		t.Fatalf("import: %v", err)
	}
	report, err := semanticStaleReport(cmd.Context(), importOpts, repoDir)
	if err != nil {
		t.Fatalf("stale: %v", err)
	}
	if report.Severity != "ok" {
		t.Fatalf("round-trip stale report = %+v", report)
	}
	var queryOut bytes.Buffer
	queryCmd := &cobra.Command{Use: "query"}
	queryCmd.SetOut(&queryOut)
	if err := runSemanticQuery(queryCmd.Context(), queryCmd, importOpts, semanticQueryOptions{limit: 10, json: true}, "ValidateToken"); err != nil {
		t.Fatalf("query: %v", err)
	}
	if !strings.Contains(queryOut.String(), "ValidateToken") {
		t.Fatalf("round-trip query missing symbol:\n%s", queryOut.String())
	}
}

func TestBundleExportRejectsSymlinkOutput(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	target := filepath.Join(t.TempDir(), "target.tar")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	output := filepath.Join(t.TempDir(), "brain.tar")
	if err := os.Symlink(target, output); err != nil {
		t.Fatalf("symlink output: %v", err)
	}
	err := runSemanticBundleExport(cmd.Context(), cmd, opts, output)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("export err = %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(data) != "keep" {
		t.Fatalf("symlink target was modified: %q", data)
	}
}

func TestBundleExportRejectsWorktreeSemanticIndex(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{stdout: " M internal/cli/semantic.go\n"}
	runner.responses[fakeCommandKey("git", "diff", "--binary", "HEAD")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "diff", "--cached", "--binary", "HEAD")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("entire", "sem", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network", "--worktree")] = fakeCommandResponse{stdout: semanticFixtureSnapshot("1.0")}
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire", worktree: true}, repoDir); err != nil {
		t.Fatalf("index --worktree: %v", err)
	}
	err := runSemanticBundleExport(cmd.Context(), &cobra.Command{Use: "bundle export"}, opts, filepath.Join(t.TempDir(), "brain.tar"))
	if err == nil || !strings.Contains(err.Error(), "worktree-backed") {
		t.Fatalf("export err = %v", err)
	}
}

func TestBundleImportRejectsMissingRepoKey(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	archive := filepath.Join(t.TempDir(), "missing-repo-key.tar")
	writeTestBundle(t, archive, map[string]string{
		exportManifestFileName:                      `{"schema_version":3,"sources":{"semantic":{"schema_version":"1.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson"}}}`,
		"semantic/snapshots/aaa111/snapshot.ndjson": semanticFixtureSnapshot("1.0"),
	})
	err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive))
	if err == nil || !strings.Contains(err.Error(), "missing repo_key") {
		t.Fatalf("import err = %v", err)
	}
}

func TestBundleImportRejectsSnapshotRepoKeyMismatch(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	archive := filepath.Join(t.TempDir(), "bad-snapshot-repo.tar")
	writeTestBundle(t, archive, map[string]string{
		exportManifestFileName:                      `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"schema_version":"1.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson"}}}`,
		"semantic/snapshots/aaa111/snapshot.ndjson": strings.Replace(semanticFixtureSnapshot("1.0"), `"repo_key":"gh/example/repo"`, `"repo_key":"gh/other/repo"`, 1),
	})
	err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive))
	if err == nil || !strings.Contains(err.Error(), "snapshot repo_key") {
		t.Fatalf("import err = %v", err)
	}
}

func TestBundleImportRejectsMissingSnapshotRepoKey(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	archive := filepath.Join(t.TempDir(), "missing-snapshot-repo.tar")
	writeTestBundle(t, archive, map[string]string{
		exportManifestFileName:                      `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"schema_version":"1.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson"}}}`,
		"semantic/snapshots/aaa111/snapshot.ndjson": strings.Replace(semanticFixtureSnapshot("1.0"), `"repo_key":"gh/example/repo",`, "", 1),
	})
	err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive))
	if err == nil || !strings.Contains(err.Error(), "missing repo_key") {
		t.Fatalf("import err = %v", err)
	}
}

func TestBundleImportRejectsOperationalSemanticState(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	archive := filepath.Join(t.TempDir(), "lock.tar")
	writeTestBundle(t, archive, map[string]string{
		exportManifestFileName:                      `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"schema_version":"1.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson"}}}`,
		"semantic/locks/index.lock":                 "locked",
		"semantic/snapshots/aaa111/snapshot.ndjson": semanticFixtureSnapshot("1.0"),
	})
	err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive))
	if err == nil || !strings.Contains(err.Error(), "outside allowed paths") {
		t.Fatalf("import err = %v", err)
	}
}

func TestBundleImportRejectsUnsafeSnapshotPath(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	archive := filepath.Join(t.TempDir(), "bad.tar")
	writeTestBundle(t, archive, map[string]string{
		exportManifestFileName:                      `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"schema_version":"1.0","snapshot_path":"../outside.ndjson"}}}`,
		"semantic/snapshots/aaa111/snapshot.ndjson": semanticFixtureSnapshot("1.0"),
	})
	err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive))
	if err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("import err = %v", err)
	}
}

func TestBundleImportRejectsNonCanonicalSnapshotPath(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	archive := filepath.Join(t.TempDir(), "bad-canonical.tar")
	writeTestBundle(t, archive, map[string]string{
		exportManifestFileName:                 `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"schema_version":"1.0","snapshot_path":"semantic/snapshots/a/../b/snapshot.ndjson"}}}`,
		"semantic/snapshots/b/snapshot.ndjson": semanticFixtureSnapshot("1.0"),
	})
	err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive))
	if err == nil || !strings.Contains(err.Error(), "canonical") {
		t.Fatalf("import err = %v", err)
	}
}

func TestBundleImportRejectsUnsupportedSemanticSchema(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	archive := filepath.Join(t.TempDir(), "bad-schema.tar")
	writeTestBundle(t, archive, map[string]string{
		exportManifestFileName:                      `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"schema_version":"2.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson"}}}`,
		"semantic/snapshots/aaa111/snapshot.ndjson": semanticFixtureSnapshot("2.0"),
	})
	err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive))
	if err == nil || !strings.Contains(err.Error(), "schema unsupported") {
		t.Fatalf("import err = %v", err)
	}
}

func TestBundleImportRejectsWorktreeOverlayMetadata(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	archive := filepath.Join(t.TempDir(), "worktree.tar")
	writeTestBundle(t, archive, map[string]string{
		exportManifestFileName:                      `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"schema_version":"1.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson","worktree_mode":"worktree","dirty_worktree":true,"worktree_hash":"sha256:abc"}}}`,
		"semantic/snapshots/aaa111/snapshot.ndjson": semanticFixtureSnapshot("1.0"),
	})
	err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive))
	if err == nil || !strings.Contains(err.Error(), "worktree overlay") {
		t.Fatalf("import err = %v", err)
	}
}

func TestBundleImportRejectsChecksumMismatch(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	archive := filepath.Join(t.TempDir(), "bad-checksum.tar")
	writeTestBundle(t, archive, map[string]string{
		exportManifestFileName:                      `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"schema_version":"1.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson"}}}`,
		"semantic/snapshots/aaa111/snapshot.ndjson": semanticFixtureSnapshot("1.0"),
	})
	err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, strings.Repeat("0", sha256.Size*2))
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("import err = %v", err)
	}
}

func TestBundleImportChecksumCoversTrailingBytes(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	archive := filepath.Join(t.TempDir(), "trailing.tar")
	writeTestBundle(t, archive, map[string]string{
		exportManifestFileName:                      `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"schema_version":"1.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson"}}}`,
		"semantic/snapshots/aaa111/snapshot.ndjson": semanticFixtureSnapshot("1.0"),
	})
	originalChecksum := bundleSHA256(t, archive)
	f, err := os.OpenFile(archive, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("open bundle append: %v", err)
	}
	if _, err := f.WriteString("trailing garbage"); err != nil {
		_ = f.Close()
		t.Fatalf("append trailing: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close bundle: %v", err)
	}
	err = runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, originalChecksum)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("import err = %v", err)
	}
}

func TestBundleImportRejectsMalformedSnapshotRecord(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	archive := filepath.Join(t.TempDir(), "bad-record.tar")
	writeTestBundle(t, archive, map[string]string{
		exportManifestFileName:                      `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"schema_version":"1.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson"}}}`,
		"semantic/snapshots/aaa111/snapshot.ndjson": `{"schema_version":"1.0","repo_key":"gh/example/repo"}` + "\n" + `{not-json}` + "\n",
	})
	err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive))
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("import err = %v", err)
	}
}

func TestBundleImportIgnoresUnreferencedSnapshots(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	archive := filepath.Join(t.TempDir(), "extra-snapshot.tar")
	writeTestBundle(t, archive, map[string]string{
		exportManifestFileName:                      `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"schema_version":"1.0","commit":"aaa111","tree":"tree111","provider":"entire-sem","provider_version":"0.1.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson","symbols":1,"relations":1}}}`,
		"semantic/snapshots/aaa111/snapshot.ndjson": semanticFixtureSnapshot("1.0"),
		"semantic/snapshots/extra/snapshot.ndjson":  `{"schema_version":"1.0","repo_key":"gh/other/repo"}` + "\n",
	})
	if err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive)); err != nil {
		t.Fatalf("import: %v", err)
	}
	extraPath := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo", "semantic", "snapshots", "extra", "snapshot.ndjson")
	if _, err := os.Stat(extraPath); !os.IsNotExist(err) {
		t.Fatalf("unreferenced snapshot was persisted: %v", err)
	}
}

func TestBundleImportRejectsTotalSizeLimit(t *testing.T) {
	previous := semanticBundleMaxTotal
	semanticBundleMaxTotal = 10
	defer func() { semanticBundleMaxTotal = previous }()

	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	archive := filepath.Join(t.TempDir(), "large.tar")
	writeTestBundle(t, archive, map[string]string{
		exportManifestFileName:                      `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson"}}}`,
		"semantic/snapshots/aaa111/snapshot.ndjson": semanticFixtureSnapshot("1.0"),
	})
	err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive))
	if err == nil || !strings.Contains(err.Error(), "total size") {
		t.Fatalf("import err = %v", err)
	}
}

func TestBundleImportMergesSemanticSourceAndPreservesSeed(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	seed := &seedSourceManifest{
		GeneratedAt:     time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		WorktreeMode:    "tracked",
		FileFingerprint: "sha256:seed",
		SummaryPath:     "seed/repo-overview.md",
	}
	if err := writeBrainSeedSource(brainDir, "gh/example/repo", seed); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	archive := filepath.Join(t.TempDir(), "semantic.tar")
	writeTestBundle(t, archive, map[string]string{
		exportManifestFileName:                      `{"schema_version":3,"repo_key":"gh/example/repo","sources":{"semantic":{"generated_at":"2026-05-31T00:00:00Z","commit":"aaa111","tree":"tree111","provider":"entire-sem","provider_version":"0.1.0","schema_version":"1.0","snapshot_path":"semantic/snapshots/aaa111/snapshot.ndjson","symbols":1,"relations":1}}}`,
		"semantic/snapshots/aaa111/snapshot.ndjson": semanticFixtureSnapshot("1.0"),
	})
	if err := runSemanticBundleImport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle import"}, Options{Env: env, Runner: runner, Now: time.Now}, archive, bundleSHA256(t, archive)); err != nil {
		t.Fatalf("import: %v", err)
	}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if manifest.Sources.Seed == nil {
		t.Fatalf("seed source was not preserved: %+v", manifest.Sources)
	}
	if manifest.Sources.Semantic == nil || manifest.Sources.Semantic.Symbols != 1 {
		t.Fatalf("semantic source missing after import: %+v", manifest.Sources)
	}
}

func semanticTestEnv(t *testing.T, repoDir string) EntireEnv {
	t.Helper()
	return EntireEnv{
		RepoRoot:        repoDir,
		PluginConfigDir: t.TempDir(),
		PluginDataDir:   t.TempDir(),
		PluginStateDir:  t.TempDir(),
		PluginCacheDir:  t.TempDir(),
	}
}

func writeSemanticTestLock(t *testing.T, brainDir string) {
	t.Helper()
	unlock, err := acquireSemanticIndexLock(brainDir)
	if err != nil {
		t.Fatalf("acquire semantic test lock: %v", err)
	}
	t.Cleanup(unlock)
}

func semanticTestSQLCount(t *testing.T, path, table string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return count
}

func mustSemanticSource(t *testing.T, env EntireEnv) *semanticSourceManifest {
	t.Helper()
	manifest, err := loadBrainManifest(filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo"))
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	return manifest.Sources.Semantic
}

func semanticTestContainsEntrySuffix(entries []string, suffix string) bool {
	for _, entry := range entries {
		if strings.HasSuffix(entry, suffix) {
			return true
		}
	}
	return false
}

func semanticFixtureRunner(repoDir, snapshot string) *fakeCommandRunner {
	brainignorePath := filepath.Join(repoDir, ".brainignore")
	return &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "rev-parse", "--show-toplevel"):                                                                                                {stdout: repoDir + "\n"},
		fakeCommandKey("git", "remote", "get-url", "origin"):                                                                                                 {stdout: "git@github.com:example/repo.git\n"},
		fakeCommandKey("git", "rev-parse", "HEAD"):                                                                                                           {stdout: "aaa111\n"},
		fakeCommandKey("git", "rev-parse", "HEAD^{tree}"):                                                                                                    {stdout: "tree111\n"},
		fakeCommandKey("git", "branch", "--show-current"):                                                                                                    {stdout: "feature\n"},
		fakeCommandKey("git", "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"):                                                              {stdout: "origin/main\n"},
		fakeCommandKey("git", "status", "--porcelain"):                                                                                                       {stdout: ""},
		fakeCommandKey("entire", "sem", "doctor", "--json"):                                                                                                  {stdout: `{"no_egress":true}`},
		fakeCommandKey("entire", "sem", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network"):                                                 {stdout: snapshot},
		fakeCommandKey("entire", "sem", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network", "--ignore-file", brainignorePath):               {stdout: snapshot},
		fakeCommandKey("entire", "sem", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network", "--ignore-file", brainignorePath, "--worktree"): {stdout: snapshot},
	}}
}

func fakeRunnerCalled(runner *fakeCommandRunner, name string, args ...string) bool {
	for _, call := range runner.calls {
		if call.name != name || len(call.args) != len(args) {
			continue
		}
		match := true
		for i := range args {
			if call.args[i] != args[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func semanticArgsEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func semanticFixtureSnapshot(schema string) string {
	return `{"schema_version":"` + schema + `","provider":"entire-sem","provider_version":"0.1.0","repo_key":"gh/example/repo","commit":"aaa111","tree":"tree111","capabilities":["go"],"warnings":[],"partial_failures":[]}
{"record_type":"symbol","id":"gh/example/repo:go:internal/auth/token.go:function:auth.ValidateToken","kind":"function","name":"ValidateToken","qualified_name":"auth.ValidateToken","file_path":"internal/auth/token.go","start_line":10,"end_line":20,"signature":"func ValidateToken(token string) error","language":"Go","stable_id_version":"1"}
{"record_type":"relation","from_id":"caller","to_id":"gh/example/repo:go:internal/auth/token.go:function:auth.ValidateToken","type":"CALLS","confidence":1}
`
}

func semanticBoundaryFixtureSnapshot() string {
	return `{"schema_version":"1.0","provider":"entire-sem","provider_version":"0.1.0","repo_key":"gh/example/repo","commit":"aaa111","tree":"tree111","capabilities":["go","routes","tools","workflows"],"warnings":[],"partial_failures":[]}
{"record_type":"symbol","id":"gh/example/repo:go:internal/auth/token.go:function:auth.ValidateToken","kind":"function","name":"ValidateToken","qualified_name":"auth.ValidateToken","file_path":"internal/auth/token.go","start_line":10,"end_line":20,"signature":"func ValidateToken(token string) error","language":"Go","stable_id_version":"1"}
{"record_type":"symbol","id":"gh/example/repo:go:internal/http/routes.go:route:GET /tokens/{id}","kind":"route","name":"GET /tokens/{id}","qualified_name":"GET /tokens/{id}","file_path":"internal/http/routes.go","start_line":5,"end_line":5,"signature":"GET /tokens/{id}","language":"Go","stable_id_version":"1"}
{"record_type":"symbol","id":"gh/example/repo:go:internal/cli/root.go:cli_command:brain refresh","kind":"cli_command","name":"brain refresh","qualified_name":"brain refresh","file_path":"internal/cli/root.go","start_line":50,"end_line":60,"signature":"entire brain refresh","language":"Go","stable_id_version":"1"}
{"record_type":"symbol","id":"gh/example/repo:yaml:.github/workflows/test.yml:workflow:token validation","kind":"workflow","name":"token validation","qualified_name":"token validation","file_path":".github/workflows/test.yml","start_line":1,"end_line":20,"signature":"mise run check","language":"YAML","stable_id_version":"1"}
{"record_type":"symbol","id":"gh/example/repo:go:internal/auth/token_test.go:test:auth.TestValidateToken","kind":"test","name":"TestValidateToken","qualified_name":"auth.TestValidateToken","file_path":"internal/auth/token_test.go","start_line":8,"end_line":18,"signature":"func TestValidateToken(t *testing.T)","language":"Go","stable_id_version":"1"}
{"record_type":"relation","from_id":"gh/example/repo:go:internal/auth/token.go:function:auth.ValidateToken","to_id":"gh/example/repo:go:internal/http/routes.go:route:GET /tokens/{id}","type":"HANDLES_ROUTE","confidence":1}
{"record_type":"relation","from_id":"gh/example/repo:go:internal/auth/token.go:function:auth.ValidateToken","to_id":"gh/example/repo:go:internal/cli/root.go:cli_command:brain refresh","type":"HANDLES_TOOL","confidence":0.8}
{"record_type":"relation","from_id":"gh/example/repo:go:internal/auth/token.go:function:auth.ValidateToken","to_id":"gh/example/repo:yaml:.github/workflows/test.yml:workflow:token validation","type":"HANDLES_WORKFLOW","confidence":0.7}
`
}

func semanticBoundaryFixtureSnapshotWithExtraRoute() string {
	return strings.TrimSuffix(semanticBoundaryFixtureSnapshot(), "\n") + `
{"record_type":"symbol","id":"gh/example/repo:go:internal/http/routes.go:route:POST /sessions","kind":"route","name":"POST /sessions","qualified_name":"POST /sessions","file_path":"internal/http/routes.go","start_line":6,"end_line":6,"signature":"POST /sessions","language":"Go","stable_id_version":"1"}
{"record_type":"symbol","id":"gh/example/repo:go:internal/http/session.go:function:http.CreateSession","kind":"function","name":"CreateSession","qualified_name":"http.CreateSession","file_path":"internal/http/session.go","start_line":12,"end_line":24,"signature":"func CreateSession()","language":"Go","stable_id_version":"1"}
{"record_type":"relation","from_id":"gh/example/repo:go:internal/http/session.go:function:http.CreateSession","to_id":"gh/example/repo:go:internal/http/routes.go:route:POST /sessions","type":"HANDLES_ROUTE","confidence":1}
`
}

func semanticFixtureSnapshotWithDelayedRelevantRelation() string {
	var b strings.Builder
	b.WriteString(`{"schema_version":"1.0","provider":"entire-sem","provider_version":"0.1.0","repo_key":"gh/example/repo","commit":"aaa111","tree":"tree111","capabilities":["go"],"warnings":[],"partial_failures":[]}` + "\n")
	b.WriteString(`{"record_type":"symbol","id":"target","kind":"function","name":"ValidateToken","qualified_name":"auth.ValidateToken","file_path":"internal/auth/token.go","start_line":10,"end_line":20,"signature":"func ValidateToken(token string) error","language":"Go","stable_id_version":"1"}` + "\n")
	for i := 0; i < 8; i++ {
		fmt.Fprintf(&b, `{"record_type":"relation","from_id":"unrelated-%d","to_id":"other-%d","type":"CALLS","confidence":1}`+"\n", i, i)
	}
	b.WriteString(`{"record_type":"relation","from_id":"caller","to_id":"target","type":"CALLS","confidence":1}` + "\n")
	return b.String()
}

func semanticFixtureSnapshotWithIgnoredSecret() string {
	return `{"schema_version":"1.0","provider":"entire-sem","provider_version":"0.1.0","repo_key":"gh/example/repo","commit":"aaa111","tree":"tree111","capabilities":["go"],"warnings":[],"partial_failures":[]}
{"record_type":"symbol","id":"public","kind":"function","name":"ValidateToken","qualified_name":"auth.ValidateToken","file_path":"internal/auth/token.go","start_line":10,"end_line":20,"signature":"func ValidateToken(token string) error","language":"Go","stable_id_version":"1"}
{"record_type":"symbol","id":"secret","kind":"const","name":"SECRET_TOKEN","qualified_name":"config.SECRET_TOKEN","file_path":"secret/config.go","start_line":1,"end_line":1,"signature":"const SECRET_TOKEN","language":"Go","stable_id_version":"1"}
{"record_type":"relation","from_id":"public","to_id":"secret","type":"ACCESSES","confidence":1}
`
}

func semanticFixtureSnapshotWithIgnoredWarnings() string {
	return `{"schema_version":"1.0","provider":"entire-sem","provider_version":"0.1.0","repo_key":"gh/example/repo","commit":"aaa111","tree":"tree111","capabilities":["go"],"warnings":[{"code":"kept","severity":"warning","path":"internal/auth/token.go","detail":"visible"},{"code":"ignored","severity":"warning","path":"secret/config.go","detail":"SECRET_TOKEN in secret/config.go"}],"partial_failures":[{"code":"ignored_failure","severity":"error","path":"secret/config.go","detail":"parse failed for SECRET_TOKEN"}]}
{"record_type":"symbol","id":"public","kind":"function","name":"ValidateToken","qualified_name":"auth.ValidateToken","file_path":"internal/auth/token.go","start_line":10,"end_line":20,"signature":"func ValidateToken(token string) error","language":"Go","stable_id_version":"1"}
`
}

func semanticFixtureSnapshotWithGitHubWorkflow() string {
	return `{"schema_version":"1.0","provider":"entire-sem","provider_version":"0.1.0","repo_key":"gh/example/repo","commit":"aaa111","tree":"tree111","capabilities":["yaml"],"warnings":[],"partial_failures":[]}
{"record_type":"symbol","id":"workflow","kind":"workflow","name":"ci","qualified_name":"ci","file_path":".github/workflows/ci.yml","start_line":1,"end_line":20,"signature":"ci","language":"YAML","stable_id_version":"1"}
`
}

func semanticFixtureSnapshotWithIgnoredRelationID() string {
	return `{"schema_version":"1.0","provider":"entire-sem","provider_version":"0.1.0","repo_key":"gh/example/repo","commit":"aaa111","tree":"tree111","capabilities":["go"],"warnings":[],"partial_failures":[]}
{"record_type":"symbol","id":"public","kind":"function","name":"ValidateToken","qualified_name":"auth.ValidateToken","file_path":"internal/auth/token.go","start_line":10,"end_line":20,"signature":"func ValidateToken(token string) error","language":"Go","stable_id_version":"1"}
{"record_type":"relation","from_id":"public","to_id":"gh/example/repo:go:secret/config.go:function:Secret","type":"CALLS","confidence":1}
`
}

func writeTestBundle(t *testing.T, path string, entries map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		data := []byte(entries[name])
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data))}); err != nil {
			t.Fatalf("write tar header: %v", err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("write tar data: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
}

func readTestBundleEntries(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open bundle: %v", err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	var entries []string
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read bundle: %v", err)
		}
		entries = append(entries, header.Name)
	}
	return entries
}

func readTestBundleFile(t *testing.T, path, name string) []byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open bundle: %v", err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read bundle: %v", err)
		}
		if header.Name != name {
			continue
		}
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, tr); err != nil {
			t.Fatalf("read bundle file: %v", err)
		}
		return buf.Bytes()
	}
	t.Fatalf("bundle file %s not found", name)
	return nil
}

func bundleSHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestSemanticWarningUnmarshalProviderKeys(t *testing.T) {
	// The semantic provider emits file_path / effect_on_semantic_completeness;
	// entire-brain's canonical keys are path / effect. Both must round-trip.
	cases := []struct {
		name       string
		input      string
		wantPath   string
		wantEffect string
	}{
		{
			name:       "provider keys",
			input:      `{"code":"E_PARSE_ERROR","severity":"warning","file_path":"supabase/migrations/x.sql","effect_on_semantic_completeness":"file parsed with syntax errors","detail":"tree-sitter syntax error nodes present"}`,
			wantPath:   "supabase/migrations/x.sql",
			wantEffect: "file parsed with syntax errors",
		},
		{
			name:       "canonical keys",
			input:      `{"code":"default_branch_unknown","severity":"warning","path":"a/b.go","effect":"freshness","detail":"d"}`,
			wantPath:   "a/b.go",
			wantEffect: "freshness",
		},
		{
			name:       "canonical wins over provider",
			input:      `{"code":"c","path":"canonical","file_path":"provider","effect":"eff","effect_on_semantic_completeness":"provider-eff"}`,
			wantPath:   "canonical",
			wantEffect: "eff",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var w semanticWarning
			if err := json.Unmarshal([]byte(tc.input), &w); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if w.Path != tc.wantPath {
				t.Errorf("path = %q, want %q", w.Path, tc.wantPath)
			}
			if w.Effect != tc.wantEffect {
				t.Errorf("effect = %q, want %q", w.Effect, tc.wantEffect)
			}
		})
	}
}

func TestSemanticWarningProviderKeysRoundTripThroughHeader(t *testing.T) {
	// filterSemanticSnapshot parses the provider header then re-marshals it as the
	// stored snapshot header. The failing file path must survive that round-trip.
	header := `{"schema_version":"1.0","provider":"entire-sem","repo_key":"k","commit":"c","tree":"t","languages":["SQL"],"capabilities":[],"warnings":[],"partial_failures":[{"code":"E_PARSE_ERROR","severity":"warning","file_path":"db/x.sql","effect_on_semantic_completeness":"incomplete","detail":"tree-sitter syntax error nodes present"}]}`
	var h semanticHeader
	if err := json.Unmarshal([]byte(header), &h); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if len(h.PartialFailures) != 1 {
		t.Fatalf("partial failures = %d, want 1", len(h.PartialFailures))
	}
	if got := h.PartialFailures[0].Path; got != "db/x.sql" {
		t.Fatalf("partial failure path = %q, want db/x.sql", got)
	}
	out, err := json.Marshal(h)
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	var h2 semanticHeader
	if err := json.Unmarshal(out, &h2); err != nil {
		t.Fatalf("re-unmarshal header: %v", err)
	}
	if got := h2.PartialFailures[0].Path; got != "db/x.sql" {
		t.Fatalf("round-tripped partial failure path = %q, want db/x.sql", got)
	}
}
