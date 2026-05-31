package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorkspaceCreateAddRefreshAndQuery(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})

	if _, err := execute(t, cmd, "index", "--sem-binary", "entire"); err != nil {
		t.Fatalf("index: %v", err)
	}
	out, err := execute(t, cmd, "workspace", "create", "payments-platform")
	if err != nil {
		t.Fatalf("workspace create: %v", err)
	}
	if !strings.Contains(out, filepath.Join(brainDirName, workspaceDirName, "payments-platform")) {
		t.Fatalf("create output = %q", out)
	}
	if _, err := execute(t, cmd, "workspace", "add", "payments-platform", repoDir, "--name", "api"); err != nil {
		t.Fatalf("workspace add: %v", err)
	}
	refreshOut, err := execute(t, cmd, "workspace", "refresh", "payments-platform")
	if err != nil {
		t.Fatalf("workspace refresh: %v", err)
	}
	if !strings.Contains(refreshOut, "gh/example/repo ok") {
		t.Fatalf("refresh output = %q", refreshOut)
	}
	queryOut, err := execute(t, cmd, "workspace", "query", "payments-platform", "ValidateToken", "--json")
	if err != nil {
		t.Fatalf("workspace query: %v", err)
	}
	if !strings.Contains(queryOut, `"repo_key": "gh/example/repo"`) || !strings.Contains(queryOut, `"ValidateToken"`) {
		t.Fatalf("query output missing workspace symbol:\n%s", queryOut)
	}
	impactOut, err := execute(t, cmd, "workspace", "impact", "payments-platform", "ValidateToken", "--json")
	if err != nil {
		t.Fatalf("workspace impact: %v", err)
	}
	if !strings.Contains(impactOut, `"relations"`) || !strings.Contains(impactOut, `"ValidateToken"`) {
		t.Fatalf("impact output missing workspace facts:\n%s", impactOut)
	}
}

func TestWorkspaceRefreshReportsRepoSemanticFreshness(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
	if _, err := execute(t, cmd, "index", "--sem-binary", "entire"); err != nil {
		t.Fatalf("index: %v", err)
	}
	manifest := mustSemanticSource(t, env)
	storePath := filepath.Join(env.PluginDataDir, brainDirName, "gh", "example", "repo", filepath.FromSlash(manifest.StorePath))
	if err := os.Remove(storePath); err != nil {
		t.Fatalf("remove store: %v", err)
	}
	if _, err := execute(t, cmd, "workspace", "create", "payments-platform"); err != nil {
		t.Fatalf("workspace create: %v", err)
	}
	if _, err := execute(t, cmd, "workspace", "add", "payments-platform", repoDir, "--name", "api"); err != nil {
		t.Fatalf("workspace add: %v", err)
	}
	out, err := execute(t, cmd, "workspace", "refresh", "payments-platform")
	if err != nil {
		t.Fatalf("workspace refresh: %v", err)
	}
	if !strings.Contains(out, "gh/example/repo unsafe") {
		t.Fatalf("refresh output = %q", out)
	}
	queryOut, err := execute(t, cmd, "workspace", "query", "payments-platform", "ValidateToken", "--json")
	if err != nil {
		t.Fatalf("workspace query: %v", err)
	}
	if !strings.Contains(queryOut, `"state": "unsafe"`) {
		t.Fatalf("query output missing freshness:\n%s", queryOut)
	}
}

func TestWorkspaceQueryAndImpactReportLockedSemanticIndex(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
	if _, err := execute(t, cmd, "index", "--sem-binary", "entire"); err != nil {
		t.Fatalf("index: %v", err)
	}
	if _, err := execute(t, cmd, "workspace", "create", "payments-platform"); err != nil {
		t.Fatalf("workspace create: %v", err)
	}
	if _, err := execute(t, cmd, "workspace", "add", "payments-platform", repoDir, "--name", "api"); err != nil {
		t.Fatalf("workspace add: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, brainDirName, "gh", "example", "repo")
	unlock, err := acquireSemanticIndexLock(brainDir)
	if err != nil {
		t.Fatalf("acquire lock: %v", err)
	}
	defer unlock()

	queryOut, err := execute(t, cmd, "workspace", "query", "payments-platform", "ValidateToken", "--json")
	if err != nil {
		t.Fatalf("workspace query: %v", err)
	}
	if !strings.Contains(queryOut, `"error": "index_locked:`) {
		t.Fatalf("query output missing lock error:\n%s", queryOut)
	}
	impactOut, err := execute(t, cmd, "workspace", "impact", "payments-platform", "ValidateToken", "--json")
	if err != nil {
		t.Fatalf("workspace impact: %v", err)
	}
	if !strings.Contains(impactOut, `"error": "index_locked:`) {
		t.Fatalf("impact output missing lock error:\n%s", impactOut)
	}
}

func TestWorkspaceRefreshRejectsMismatchedLocalPathHintRepoKey(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
	if _, err := execute(t, cmd, "index", "--sem-binary", "entire"); err != nil {
		t.Fatalf("index: %v", err)
	}
	manifest := workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "payments-platform",
		Repos:         []workspaceRepo{{RepoKey: "gh/example/repo", Name: "api", LocalPathHint: repoDir}},
	}
	if err := writeWorkspaceManifest(env, manifest); err != nil {
		t.Fatalf("write workspace: %v", err)
	}
	runner.responses[fakeCommandKey("git", "remote", "get-url", "origin")] = fakeCommandResponse{stdout: "git@github.com:other/repo.git\n"}
	out, err := execute(t, cmd, "workspace", "refresh", "payments-platform")
	if err != nil {
		t.Fatalf("workspace refresh: %v", err)
	}
	if !strings.Contains(out, "gh/example/repo unsafe") {
		t.Fatalf("refresh output = %q", out)
	}
}

func TestWorkspaceStoresRepoKeyAndLocalPathHint(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
	if _, err := execute(t, cmd, "workspace", "create", "payments-platform"); err != nil {
		t.Fatalf("workspace create: %v", err)
	}
	if _, err := execute(t, cmd, "workspace", "add", "payments-platform", repoDir, "--name", "api"); err != nil {
		t.Fatalf("workspace add: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(env.PluginDataDir, brainDirName, workspaceDirName, "payments-platform", workspaceManifestName))
	if err != nil {
		t.Fatalf("read workspace: %v", err)
	}
	var manifest workspaceManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse workspace: %v", err)
	}
	if len(manifest.Repos) != 1 || manifest.Repos[0].RepoKey != "gh/example/repo" || manifest.Repos[0].LocalPathHint != repoDir {
		t.Fatalf("workspace repos = %+v", manifest.Repos)
	}
}

func TestWorkspaceAddStoresResolvedRepoRootForRelativeHint(t *testing.T) {
	parent := t.TempDir()
	repoDir := filepath.Join(parent, "api")
	if err := os.MkdirAll(repoDir, 0o700); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	otherDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
	if _, err := execute(t, cmd, "index", "--sem-binary", "entire"); err != nil {
		t.Fatalf("index: %v", err)
	}
	if _, err := execute(t, cmd, "workspace", "create", "payments-platform"); err != nil {
		t.Fatalf("workspace create: %v", err)
	}
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(parent); err != nil {
		t.Fatalf("chdir parent: %v", err)
	}
	if _, err := execute(t, cmd, "workspace", "add", "payments-platform", "api", "--name", "api"); err != nil {
		_ = os.Chdir(oldWD)
		t.Fatalf("workspace add: %v", err)
	}
	if err := os.Chdir(otherDir); err != nil {
		_ = os.Chdir(oldWD)
		t.Fatalf("chdir other: %v", err)
	}
	defer func() { _ = os.Chdir(oldWD) }()
	out, err := execute(t, cmd, "workspace", "refresh", "payments-platform")
	if err != nil {
		t.Fatalf("workspace refresh: %v", err)
	}
	if !strings.Contains(out, "gh/example/repo ok") {
		t.Fatalf("refresh output = %q", out)
	}
	manifest, err := loadWorkspaceManifest(env, "payments-platform")
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}
	if manifest.Repos[0].LocalPathHint != repoDir {
		t.Fatalf("local path hint = %q, want %q", manifest.Repos[0].LocalPathHint, repoDir)
	}
}

func TestWorkspaceCreateDoesNotOverwriteExistingWorkspace(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
	if _, err := execute(t, cmd, "workspace", "create", "payments-platform"); err != nil {
		t.Fatalf("workspace create: %v", err)
	}
	if _, err := execute(t, cmd, "workspace", "add", "payments-platform", repoDir, "--name", "api"); err != nil {
		t.Fatalf("workspace add: %v", err)
	}
	if _, err := execute(t, cmd, "workspace", "create", "payments-platform"); err == nil {
		t.Fatalf("workspace create overwrote existing workspace")
	}
	manifest, err := loadWorkspaceManifest(env, "payments-platform")
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}
	if len(manifest.Repos) != 1 || manifest.Repos[0].Name != "api" {
		t.Fatalf("workspace repos were not preserved: %+v", manifest.Repos)
	}
}

func TestWorkspacePropagatesPluginDirErrors(t *testing.T) {
	env := EntireEnv{PluginDataDir: "relative-data"}
	if _, err := loadWorkspaceManifest(env, "payments-platform"); err == nil || !strings.Contains(err.Error(), envPluginDataDir) {
		t.Fatalf("load workspace err = %v", err)
	}
}

func TestWorkspaceRejectsDotNamesBeforePathResolution(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}, Now: time.Now})
	for _, name := range []string{".", "..", "..."} {
		if _, err := execute(t, cmd, "workspace", "create", name); err == nil {
			t.Fatalf("workspace create accepted %q", name)
		}
	}
	if _, err := os.Stat(filepath.Join(env.PluginDataDir, brainDirName, exportManifestFileName)); !os.IsNotExist(err) {
		t.Fatalf("dot workspace wrote outside workspaces: %v", err)
	}
}

func TestWorkspaceRejectsUnsafeRepoKey(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	dir, err := workspaceDir(env, "payments-platform")
	if err != nil {
		t.Fatalf("workspace dir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	data := []byte(`{"schema_version":1,"name":"payments-platform","repos":[{"repo_key":"../../outside","name":"bad"}]}` + "\n")
	if err := os.WriteFile(filepath.Join(dir, workspaceManifestName), data, 0o600); err != nil {
		t.Fatalf("write workspace: %v", err)
	}
	if _, err := loadWorkspaceManifest(env, "payments-platform"); err == nil || !strings.Contains(err.Error(), "repo_key") {
		t.Fatalf("load workspace err = %v", err)
	}
}

func TestWorkspaceRejectsDotRepoKeys(t *testing.T) {
	for _, key := range []string{".", "..", "gh/../repo", "gh//repo"} {
		if err := validateWorkspaceRepoKey(key); err == nil {
			t.Fatalf("repo key accepted: %q", key)
		}
	}
}

func TestWorkspaceRejectsSymlinkedWorkspacePath(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	workspacesRoot := filepath.Join(env.PluginDataDir, brainDirName, workspaceDirName)
	if err := os.MkdirAll(workspacesRoot, 0o700); err != nil {
		t.Fatalf("mkdir workspaces: %v", err)
	}
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(workspacesRoot, "payments-platform")); err != nil {
		t.Fatalf("symlink workspace: %v", err)
	}
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}, Now: time.Now})
	if _, err := execute(t, cmd, "workspace", "create", "payments-platform"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("workspace create err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, workspaceManifestName)); !os.IsNotExist(err) {
		t.Fatalf("workspace manifest written through symlink: %v", err)
	}
}

func TestWorkspaceRejectsSymlinkedBrainRoot(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	brainRoot := filepath.Join(env.PluginDataDir, brainDirName)
	external := t.TempDir()
	if err := os.Symlink(external, brainRoot); err != nil {
		t.Fatalf("symlink brain root: %v", err)
	}
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}, Now: time.Now})
	if _, err := execute(t, cmd, "workspace", "create", "payments-platform"); err == nil || !strings.Contains(err.Error(), "brain directory must not be a symlink") {
		t.Fatalf("workspace create err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(external, workspaceDirName)); !os.IsNotExist(err) {
		t.Fatalf("workspace directory was created through symlinked brain root: %v", err)
	}
}

func TestWorkspaceRefreshReportsMissingBrains(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	manifest := workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "payments-platform",
		Repos:         []workspaceRepo{{RepoKey: "gh/example/missing", Name: "missing"}},
	}
	if err := writeWorkspaceManifest(env, manifest); err != nil {
		t.Fatalf("write workspace: %v", err)
	}
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}, Now: time.Now})
	out, err := execute(t, cmd, "workspace", "refresh", "payments-platform")
	if err != nil {
		t.Fatalf("workspace refresh: %v", err)
	}
	if !strings.Contains(out, "gh/example/missing missing-brain") {
		t.Fatalf("refresh output = %q", out)
	}
}
