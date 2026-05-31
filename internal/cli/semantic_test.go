package cli

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
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

	manifestPath := filepath.Join(env.PluginDataDir, brainDirName, "gh", "example", "repo", exportManifestFileName)
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
	if semantic.Symbols != 1 || semantic.Relations != 1 {
		t.Fatalf("counts = symbols %d relations %d", semantic.Symbols, semantic.Relations)
	}
	if !semantic.NoEgressVerified {
		t.Fatalf("no-egress was not verified")
	}
	if _, err := os.Stat(filepath.Join(env.PluginDataDir, brainDirName, "gh", "example", "repo", filepath.FromSlash(semantic.SnapshotPath))); err != nil {
		t.Fatalf("snapshot not written: %v", err)
	}
	if !strings.Contains(semantic.SnapshotPath, "aaa111-") {
		t.Fatalf("snapshot path is not generation-addressed: %s", semantic.SnapshotPath)
	}
	if !fakeRunnerCalled(runner, "entire", "sem", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network") {
		t.Fatalf("semantic snapshot was not invoked with --no-network: %+v", runner.calls)
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

func TestSemanticIndexRejectsWorktreeFlagUntilProviderSupportsIt(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{stdout: " M file.go\n"}
	err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire", worktree: true}, repoDir)
	if err == nil || !strings.Contains(err.Error(), "worktree indexing is not supported") {
		t.Fatalf("index err = %v", err)
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

func TestSemanticIndexRedactsBrainignoredRecordsFromSnapshotAndQuery(t *testing.T) {
	repoDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoDir, ".brainignore"), []byte("secret/\n"), 0o600); err != nil {
		t.Fatalf("write .brainignore: %v", err)
	}
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshotWithIgnoredSecret())
	cmd := &cobra.Command{Use: "index"}
	if err := runSemanticIndex(cmd.Context(), cmd, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, brainDirName, "gh", "example", "repo")
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
	if strings.Contains(string(data), "SECRET_TOKEN") || strings.Contains(string(data), "secret/config.go") {
		t.Fatalf("ignored semantic record persisted:\n%s", data)
	}
	results, err := findSemanticSymbols(snapshotPath, "SECRET_TOKEN", 10)
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
	brainDir := filepath.Join(env.PluginDataDir, brainDirName, "gh", "example", "repo")
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

func TestSemanticIndexDoesNotDefaultIgnoreGitHubPaths(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshotWithGitHubWorkflow())
	cmd := &cobra.Command{Use: "index"}
	if err := runSemanticIndex(cmd.Context(), cmd, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	manifest, err := loadBrainManifest(filepath.Join(env.PluginDataDir, brainDirName, "gh", "example", "repo"))
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
	brainDir := filepath.Join(env.PluginDataDir, brainDirName, "gh", "example", "repo")
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
			brainDir := filepath.Join(env.PluginDataDir, brainDirName, "gh", "example", "repo")
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

func TestSemanticQueryFindsExactSymbol(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	if err := runSemanticIndex(cmd.Context(), cmd, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}

	manifest, err := loadBrainManifest(filepath.Join(env.PluginDataDir, brainDirName, "gh", "example", "repo"))
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	results, err := findSemanticSymbols(filepath.Join(env.PluginDataDir, brainDirName, "gh", "example", "repo", filepath.FromSlash(manifest.Sources.Semantic.SnapshotPath)), "ValidateToken", 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(results) != 1 || results[0].QualifiedName != "auth.ValidateToken" {
		t.Fatalf("results = %+v", results)
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
	brainDir := filepath.Join(env.PluginDataDir, brainDirName, "gh", "example", "repo")
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
	brainDir := filepath.Join(env.PluginDataDir, brainDirName, "gh", "example", "repo")
	lockDir := filepath.Join(brainDir, semanticLockDir)
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		t.Fatalf("mkdir lock: %v", err)
	}
	if err := os.WriteFile(filepath.Join(lockDir, semanticIndexLockName), []byte("locked\n"), 0o600); err != nil {
		t.Fatalf("write lock: %v", err)
	}
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{semBinary: "entire"}, repoDir)
	if err == nil || !strings.Contains(err.Error(), "index_locked") {
		t.Fatalf("lock err = %v", err)
	}
}

func TestBundleExportLockFailsFast(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	brainDir := filepath.Join(env.PluginDataDir, brainDirName, "gh", "example", "repo")
	writeSemanticTestLock(t, brainDir)

	err := runSemanticBundleExport((&cobra.Command{}).Context(), &cobra.Command{Use: "bundle export"}, Options{Env: env, Runner: runner, Now: time.Now}, filepath.Join(t.TempDir(), "brain.tar"))
	if err == nil || !strings.Contains(err.Error(), "index_locked") {
		t.Fatalf("export lock err = %v", err)
	}
}

func TestSemanticGCLockFailsFast(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	brainDir := filepath.Join(env.PluginDataDir, brainDirName, "gh", "example", "repo")
	writeSemanticTestLock(t, brainDir)

	err := runSemanticGC((&cobra.Command{}).Context(), &cobra.Command{Use: "gc"}, Options{Env: env, Runner: runner, Now: time.Now}, repoDir, "24h")
	if err == nil || !strings.Contains(err.Error(), "index_locked") {
		t.Fatalf("gc lock err = %v", err)
	}
}

func TestBundleImportLockFailsFast(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	brainDir := filepath.Join(env.PluginDataDir, brainDirName, "gh", "example", "repo")
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

func TestSemanticGCFailsClosedOnCorruptManifest(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	brainDir := filepath.Join(env.PluginDataDir, brainDirName, "gh", "example", "repo")
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
	brainDir := filepath.Join(env.PluginDataDir, brainDirName, "gh", "example", "repo")
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

func TestBundleExportRejectsUnsafeManifestSnapshotPath(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, brainDirName, "gh", "example", "repo")
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
	brainDir := filepath.Join(env.PluginDataDir, brainDirName, "gh", "example", "repo")
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

func TestBundleExportRejectsOutputInsideBrainDir(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	output := filepath.Join(env.PluginDataDir, brainDirName, "gh", "example", "repo", semanticDirName, semanticBundleDir, "export.tar")
	err := runSemanticBundleExport(cmd.Context(), cmd, opts, output)
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
	target := filepath.Join(env.PluginDataDir, brainDirName, "gh", "example", "repo", semanticDirName, semanticSnapshotsDir, "self.tar")
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

func TestBundleExportRejectsSymlinkInsideSnapshotTree(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, brainDirName, "gh", "example", "repo")
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
	brainDir := filepath.Join(env.PluginDataDir, brainDirName, "gh", "example", "repo")
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

func TestBundleExportUsesSanitizedManifest(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, brainDirName, "gh", "example", "repo")
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
	extraPath := filepath.Join(env.PluginDataDir, brainDirName, "gh", "example", "repo", "semantic", "snapshots", "extra", "snapshot.ndjson")
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
	brainDir := filepath.Join(env.PluginDataDir, brainDirName, "gh", "example", "repo")
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
	lockDir := filepath.Join(brainDir, semanticLockDir)
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		t.Fatalf("mkdir lock: %v", err)
	}
	if err := os.WriteFile(filepath.Join(lockDir, semanticIndexLockName), []byte("locked\n"), 0o600); err != nil {
		t.Fatalf("write lock: %v", err)
	}
}

func semanticFixtureRunner(repoDir, snapshot string) *fakeCommandRunner {
	return &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "rev-parse", "--show-toplevel"):                                                {stdout: repoDir + "\n"},
		fakeCommandKey("git", "remote", "get-url", "origin"):                                                 {stdout: "git@github.com:example/repo.git\n"},
		fakeCommandKey("git", "rev-parse", "HEAD"):                                                           {stdout: "aaa111\n"},
		fakeCommandKey("git", "rev-parse", "HEAD^{tree}"):                                                    {stdout: "tree111\n"},
		fakeCommandKey("git", "branch", "--show-current"):                                                    {stdout: "feature\n"},
		fakeCommandKey("git", "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"):              {stdout: "origin/main\n"},
		fakeCommandKey("git", "status", "--porcelain"):                                                       {stdout: ""},
		fakeCommandKey("entire", "sem", "doctor", "--json"):                                                  {stdout: `{"no_egress":true}`},
		fakeCommandKey("entire", "sem", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network"): {stdout: snapshot},
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

func semanticFixtureSnapshot(schema string) string {
	return `{"schema_version":"` + schema + `","provider":"entire-sem","provider_version":"0.1.0","repo_key":"gh/example/repo","commit":"aaa111","tree":"tree111","capabilities":["go"],"warnings":[],"partial_failures":[]}
{"record_type":"symbol","id":"gh/example/repo:go:internal/auth/token.go:function:auth.ValidateToken","kind":"function","name":"ValidateToken","qualified_name":"auth.ValidateToken","file_path":"internal/auth/token.go","start_line":10,"end_line":20,"signature":"func ValidateToken(token string) error","language":"Go","stable_id_version":"1"}
{"record_type":"relation","from_id":"caller","to_id":"gh/example/repo:go:internal/auth/token.go:function:auth.ValidateToken","type":"CALLS","confidence":1}
`
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
