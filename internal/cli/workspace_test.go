package cli

import (
	"bytes"
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

func TestWorkspaceWatchFansOverMembersWithSharedBudget(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	repoA, repoAKey := writeLocalWorkspaceBrainRepo(t, env, `{"text":"x"}`, "a.go", "package x\n")
	repoB, repoBKey := writeLocalWorkspaceBrainRepo(t, env, `{"text":"x"}`, "b.go", "package x\n")
	manifest := workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "ws",
		Repos: []workspaceRepo{
			{RepoKey: repoAKey, LocalPathHint: repoA},
			{RepoKey: repoBKey, LocalPathHint: repoB},
			{RepoKey: "gh/example/repoc", LocalPathHint: "/nonexistent/repoc-xyz"}, // unresolvable -> skipped
		},
	}
	if err := writeWorkspaceManifest(env, manifest); err != nil {
		t.Fatalf("write workspace: %v", err)
	}

	var ticked []string
	var seen []int
	repoTick := func(repoDir string, agentCalls *int) {
		ticked = append(ticked, repoDir)
		seen = append(seen, *agentCalls)
		*agentCalls++ // simulate a token-spending step to prove the counter is shared, not per-repo
	}
	w := defaultWatchOptions()
	w.once = true
	opts := Options{Version: "test", Env: env, Runner: runner, Now: time.Now}
	out := &bytes.Buffer{}
	if err := workspaceWatchLoop(context.Background(), out, opts, w, "ws", repoTick); err != nil {
		t.Fatalf("workspaceWatchLoop: %v", err)
	}
	// repoc is unresolvable, so only the two resolvable members tick.
	if len(ticked) != 2 {
		t.Fatalf("expected two members ticked (repoc skipped), got %v", ticked)
	}
	// The --budget counter is SHARED across members: the second member sees the first's increment.
	if len(seen) != 2 || seen[0] != 0 || seen[1] != 1 {
		t.Fatalf("agentCalls must be shared across members (not reset per repo), saw %v", seen)
	}
	o := out.String()
	if !strings.Contains(o, repoAKey) || !strings.Contains(o, repoBKey) {
		t.Fatalf("both resolvable repo keys should be reported:\n%s", o)
	}
	if !strings.Contains(o, "gh/example/repoc") || !strings.Contains(o, "skipped") {
		t.Fatalf("unresolvable member must be reported as skipped:\n%s", o)
	}
}

func TestWorkspaceWatchOnceSkipsStaleLocalPathHintBeforeTick(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	repoB := t.TempDir()
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "remote", "get-url", "origin"): {stdout: "git@github.com:example/repob.git\n"},
	}}
	manifest := workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "ws",
		Repos: []workspaceRepo{
			{RepoKey: "gh/example/repoa", LocalPathHint: repoB},
		},
	}
	if err := writeWorkspaceManifest(env, manifest); err != nil {
		t.Fatalf("write workspace: %v", err)
	}

	w := defaultWatchOptions()
	w.once = true
	opts := Options{Version: "test", Env: env, Runner: runner, Now: time.Now}
	out := &bytes.Buffer{}
	if err := workspaceWatchLoop(context.Background(), out, opts, w, "ws", func(repoDir string, agentCalls *int) {
		t.Fatalf("repoTick must not run for stale hint %s", repoDir)
	}); err != nil {
		t.Fatalf("workspaceWatchLoop: %v", err)
	}
	o := out.String()
	if !strings.Contains(o, "skipped (unsafe: local_path_hint repo_key mismatch: gh/example/repob)") {
		t.Fatalf("stale hint should be reported as an unsafe skip:\n%s", o)
	}
}

func TestWorkspaceWatchRejectsInvalidJobsBeforeWork(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	cmd := NewRootCommand(Options{Version: "test", Env: env, Runner: &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}, Now: time.Now})
	_, err := execute(t, cmd, "workspace", "watch", "ws", "--once", "--jobs", "0")
	if err == nil || !strings.Contains(err.Error(), "--jobs must be greater than 0") {
		t.Fatalf("expected invalid jobs error, got %v", err)
	}
}

func TestWorkspaceCreateAddRefreshAndContext(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})

	if _, err := execute(t, cmd, "refresh", "index", "--sem-binary", "entire"); err != nil {
		t.Fatalf("index: %v", err)
	}
	out, err := execute(t, cmd, "workspace", "create", "payments-platform")
	if err != nil {
		t.Fatalf("workspace create: %v", err)
	}
	if !strings.Contains(out, filepath.Join(workspaceDirName, "payments-platform")) {
		t.Fatalf("create output = %q", out)
	}
	if strings.Contains(out, filepath.Join(repoStoreDirName, workspaceDirName)) {
		t.Fatalf("workspace must live beside repos/, not inside it: %q", out)
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
	contextOut, err := execute(t, cmd, "workspace", "inspect", "context", "payments-platform", "ValidateToken", "--json")
	if err != nil {
		t.Fatalf("workspace context: %v", err)
	}
	if !strings.Contains(contextOut, `"repo_key": "gh/example/repo"`) || !strings.Contains(contextOut, `"ValidateToken"`) {
		t.Fatalf("context output missing workspace symbol:\n%s", contextOut)
	}
	if !strings.Contains(contextOut, `"contract_state": "ok"`) {
		t.Fatalf("context output missing contract freshness:\n%s", contextOut)
	}
	impactOut, err := execute(t, cmd, "workspace", "inspect", "impact", "payments-platform", "ValidateToken", "--json")
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
	if _, err := execute(t, cmd, "refresh", "index", "--sem-binary", "entire"); err != nil {
		t.Fatalf("index: %v", err)
	}
	manifest := mustSemanticSource(t, env)
	storePath := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo", filepath.FromSlash(manifest.StorePath))
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
	contextOut, err := execute(t, cmd, "workspace", "inspect", "context", "payments-platform", "ValidateToken", "--json")
	if err != nil {
		t.Fatalf("workspace context: %v", err)
	}
	if !strings.Contains(contextOut, `"state": "unsafe"`) {
		t.Fatalf("context output missing freshness:\n%s", contextOut)
	}
}

func TestWorkspaceContextAndImpactReportLockedSemanticIndex(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
	if _, err := execute(t, cmd, "refresh", "index", "--sem-binary", "entire"); err != nil {
		t.Fatalf("index: %v", err)
	}
	if _, err := execute(t, cmd, "workspace", "create", "payments-platform"); err != nil {
		t.Fatalf("workspace create: %v", err)
	}
	if _, err := execute(t, cmd, "workspace", "add", "payments-platform", repoDir, "--name", "api"); err != nil {
		t.Fatalf("workspace add: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	unlock, err := acquireSemanticIndexLock(brainDir)
	if err != nil {
		t.Fatalf("acquire lock: %v", err)
	}
	defer unlock()

	contextOut, err := execute(t, cmd, "workspace", "inspect", "context", "payments-platform", "ValidateToken", "--json")
	if err != nil {
		t.Fatalf("workspace context: %v", err)
	}
	if !strings.Contains(contextOut, `"error": "index_locked:`) {
		t.Fatalf("context output missing lock error:\n%s", contextOut)
	}
	impactOut, err := execute(t, cmd, "workspace", "inspect", "impact", "payments-platform", "ValidateToken", "--json")
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
	if _, err := execute(t, cmd, "refresh", "index", "--sem-binary", "entire"); err != nil {
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
	data, err := os.ReadFile(filepath.Join(env.PluginDataDir, workspaceDirName, "payments-platform", workspaceManifestName))
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
	if _, err := execute(t, cmd, "refresh", "index", "--sem-binary", "entire"); err != nil {
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
	if _, err := os.Stat(filepath.Join(env.PluginDataDir, repoStoreDirName, exportManifestFileName)); !os.IsNotExist(err) {
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
	workspacesRoot := filepath.Join(env.PluginDataDir, workspaceDirName)
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

func TestWorkspaceCreateIgnoresSymlinkedLegacyTree(t *testing.T) {
	// Workspaces live beside repos/, so a symlinked repos root no longer
	// blocks workspace create — but the legacy-migration path must refuse to
	// read through it, and nothing may be written behind the symlink.
	env := semanticTestEnv(t, t.TempDir())
	brainRoot := filepath.Join(env.PluginDataDir, repoStoreDirName)
	external := t.TempDir()
	if err := os.MkdirAll(filepath.Join(external, workspaceDirName, "payments-platform"), 0o700); err != nil {
		t.Fatalf("seed external legacy tree: %v", err)
	}
	if err := os.Symlink(external, brainRoot); err != nil {
		t.Fatalf("symlink brain root: %v", err)
	}
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}, Now: time.Now})
	if _, err := execute(t, cmd, "workspace", "create", "payments-platform"); err != nil {
		t.Fatalf("workspace create: %v", err)
	}
	if _, err := os.Stat(filepath.Join(env.PluginDataDir, workspaceDirName, "payments-platform", workspaceManifestName)); err != nil {
		t.Fatalf("workspace not created at its new home: %v", err)
	}
	// The symlinked legacy entry was neither migrated nor written through.
	if _, err := os.Stat(filepath.Join(external, workspaceDirName, "payments-platform", workspaceManifestName)); !os.IsNotExist(err) {
		t.Fatalf("write leaked through symlinked legacy tree: %v", err)
	}
}

func TestWorkspaceLegacyDirMigratesOnTouch(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	// A pre-relocation workspace at repos/workspaces/<name> with a valid manifest.
	legacy := filepath.Join(env.PluginDataDir, repoStoreDirName, workspaceDirName, "payments-platform")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := workspaceManifest{SchemaVersion: workspaceSchemaVersion, Name: "payments-platform", Repos: []workspaceRepo{{RepoKey: "gh/acme/api"}}}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, workspaceManifestName), data, 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadWorkspaceManifest(env, "payments-platform")
	if err != nil {
		t.Fatalf("legacy workspace must load (and migrate): %v", err)
	}
	if len(loaded.Repos) != 1 || loaded.Repos[0].RepoKey != "gh/acme/api" {
		t.Fatalf("migrated manifest content wrong: %+v", loaded)
	}
	if _, err := os.Stat(filepath.Join(env.PluginDataDir, workspaceDirName, "payments-platform", workspaceManifestName)); err != nil {
		t.Fatalf("manifest not at the new home after migration: %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy dir should be gone after migration: %v", err)
	}
}

func TestBrainDirForKeyReservesWorkspacesSegment(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	if _, err := brainDirForKey(env, "workspaces/acme/api"); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("workspaces/ must be reserved in the repo keyspace, got %v", err)
	}
	if _, err := brainDirForKey(env, "gh/acme/api"); err != nil {
		t.Fatalf("ordinary key must resolve: %v", err)
	}
}

func TestWorkspaceGraphReportsSharedExternalContracts(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC) }}
	cmd := &cobra.Command{Use: "index"}

	repoA := t.TempDir()
	keyA := filepath.ToSlash(filepath.Join("local", localRepoKey(repoA)))
	indexWorkspaceGraphRepo(t, cmd, opts, runner, repoA, keyA, "HandleSharedA")
	repoB := t.TempDir()
	keyB := filepath.ToSlash(filepath.Join("local", localRepoKey(repoB)))
	indexWorkspaceGraphRepo(t, cmd, opts, runner, repoB, keyB, "HandleSharedB")

	manifest := workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "graph",
		Repos: []workspaceRepo{
			{RepoKey: keyA, LocalPathHint: repoA},
			{RepoKey: keyB, LocalPathHint: repoB},
		},
	}
	if err := writeWorkspaceManifest(env, manifest); err != nil {
		t.Fatalf("write workspace: %v", err)
	}

	var out bytes.Buffer
	graphCmd := &cobra.Command{Use: "graph"}
	graphCmd.SetOut(&out)
	if err := runWorkspaceGraph(graphCmd, opts, workspaceGraphOptions{limit: 10, json: true}, "graph"); err != nil {
		t.Fatalf("workspace graph: %v", err)
	}
	if !strings.Contains(out.String(), `"contracts"`) || !strings.Contains(out.String(), `external:route:/shared`) {
		t.Fatalf("workspace graph missing shared external contract:\n%s", out.String())
	}
	if !strings.Contains(out.String(), `"cross_edges"`) ||
		!strings.Contains(out.String(), `"relation_kind": "shared_external_contract"`) ||
		!strings.Contains(out.String(), `external:config:kubernetes/image/shared:latest`) {
		t.Fatalf("workspace graph missing cross-repo symbol/resource edges:\n%s", out.String())
	}
	if !strings.Contains(out.String(), `"metrics"`) || !strings.Contains(out.String(), `"relation_types"`) {
		t.Fatalf("workspace graph missing per-repo graph metadata:\n%s", out.String())
	}
	artifactPath := filepath.Join(env.PluginDataDir, workspaceDirName, "graph", workspaceGraphName)
	artifact, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatalf("read workspace graph artifact: %v", err)
	}
	if !strings.Contains(string(artifact), `"contracts"`) || !strings.Contains(string(artifact), `external:route:/shared`) {
		t.Fatalf("workspace graph artifact missing contract:\n%s", artifact)
	}
	if !strings.Contains(string(artifact), `"cross_edges"`) || !strings.Contains(string(artifact), `external:config:kubernetes/image/shared:latest`) {
		t.Fatalf("workspace graph artifact missing cross-repo edges:\n%s", artifact)
	}
}

func TestWorkspaceGraphReportsCrossRepoImportCandidates(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return time.Date(2026, 6, 20, 13, 0, 0, 0, time.UTC) }}
	cmd := &cobra.Command{Use: "index"}

	repoA := t.TempDir()
	keyA := filepath.ToSlash(filepath.Join("local", localRepoKey(repoA)))
	repoB := t.TempDir()
	keyB := filepath.ToSlash(filepath.Join("local", localRepoKey(repoB)))
	indexWorkspaceGraphRepoWithSnapshot(t, cmd, opts, runner, repoA, keyA, workspaceGraphImportingSnapshot(keyA, "HandleAPI", keyB+"/pkg"))
	indexWorkspaceGraphRepoWithSnapshot(t, cmd, opts, runner, repoB, keyB, workspaceGraphLibrarySnapshot(keyB, "pkg/service.go", "Service"))

	manifest := workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "imports",
		Repos: []workspaceRepo{
			{RepoKey: keyA, LocalPathHint: repoA},
			{RepoKey: keyB, LocalPathHint: repoB},
		},
	}
	if err := writeWorkspaceManifest(env, manifest); err != nil {
		t.Fatalf("write workspace: %v", err)
	}

	var out bytes.Buffer
	graphCmd := &cobra.Command{Use: "graph"}
	graphCmd.SetOut(&out)
	if err := runWorkspaceGraph(graphCmd, opts, workspaceGraphOptions{limit: 10, json: true}, "imports"); err != nil {
		t.Fatalf("workspace graph: %v", err)
	}
	for _, want := range []string{
		`"relation_kind": "cross_repo_import_candidate"`,
		`external:import:` + keyB + `/pkg`,
		`"from_repo": "` + keyA + `"`,
		`"to_repo": "` + keyB + `"`,
		`pkg/service.go`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("workspace graph import edge missing %q:\n%s", want, out.String())
		}
	}
}

func TestWorkspaceGraphMatchesScopedPackageImportCandidates(t *testing.T) {
	edges := workspaceGraphImportCrossEdges([]workspaceRepoGraphIndex{
		{
			RepoKey: "local/app",
			Imports: []workspaceGraphImportRef{{
				Spec: "@acme/lib/pkg",
				Source: workspaceGraphSymbolRef{
					RepoKey:       "local/app",
					ID:            "app:sym",
					Kind:          "function",
					Name:          "HandleAPI",
					QualifiedName: "service.HandleAPI",
					FilePath:      "service.go",
				},
				Count: 1,
			}},
		},
		{
			RepoKey: "gh/acme/lib",
			Candidates: []workspaceGraphSymbolRef{{
				RepoKey:       "gh/acme/lib",
				ID:            "lib:sym",
				Kind:          "function",
				Name:          "Service",
				QualifiedName: "pkg.Service",
				FilePath:      "pkg/service.go",
			}},
		},
	}, 10)
	if len(edges) != 1 {
		t.Fatalf("scoped package import edges = %#v", edges)
	}
	edge := edges[0]
	if edge.RelationKind != "cross_repo_import_candidate" || edge.Endpoint != "external:import:@acme/lib/pkg" || edge.FromRepo != "local/app" || edge.ToRepo != "gh/acme/lib" || edge.ToSymbol.FilePath != "pkg/service.go" {
		t.Fatalf("unexpected scoped package import edge: %#v", edge)
	}
}

func TestWorkspaceGraphMatchesPackageRepoImportCandidates(t *testing.T) {
	edges := workspaceGraphImportCrossEdges([]workspaceRepoGraphIndex{
		{
			RepoKey: "local/app",
			Imports: []workspaceGraphImportRef{{
				Spec: "@acme/lib/pkg",
				Source: workspaceGraphSymbolRef{
					RepoKey:       "local/app",
					ID:            "app:sym",
					Kind:          "function",
					Name:          "HandleAPI",
					QualifiedName: "service.HandleAPI",
					FilePath:      "service.go",
				},
				Count: 1,
			}},
		},
		{
			RepoKey: "npm/@acme/lib",
			Candidates: []workspaceGraphSymbolRef{{
				RepoKey:       "npm/@acme/lib",
				ID:            "lib:sym",
				Kind:          "function",
				Name:          "Service",
				QualifiedName: "pkg.Service",
				FilePath:      "pkg/service.ts",
			}},
		},
	}, 10)
	if len(edges) != 1 {
		t.Fatalf("package repo import edges = %#v", edges)
	}
	edge := edges[0]
	if edge.RelationKind != "cross_repo_import_candidate" || edge.Endpoint != "external:import:@acme/lib/pkg" || edge.FromRepo != "local/app" || edge.ToRepo != "npm/@acme/lib" || edge.ToSymbol.FilePath != "pkg/service.ts" {
		t.Fatalf("unexpected package repo import edge: %#v", edge)
	}
}

func TestWorkspaceGraphMatchesGoModuleImportCandidates(t *testing.T) {
	edges := workspaceGraphImportCrossEdges([]workspaceRepoGraphIndex{
		{
			RepoKey: "local/app",
			Imports: []workspaceGraphImportRef{{
				Spec: "golang.org/x/sync/errgroup",
				Source: workspaceGraphSymbolRef{
					RepoKey:       "local/app",
					ID:            "app:sym",
					Kind:          "function",
					Name:          "Run",
					QualifiedName: "service.Run",
					FilePath:      "service.go",
				},
				Count: 1,
			}},
		},
		{
			RepoKey: "gomod/golang.org/x/sync",
			Candidates: []workspaceGraphSymbolRef{{
				RepoKey:       "gomod/golang.org/x/sync",
				ID:            "lib:sym",
				Kind:          "function",
				Name:          "WithContext",
				QualifiedName: "errgroup.WithContext",
				FilePath:      "errgroup/errgroup.go",
			}},
		},
	}, 10)
	if len(edges) != 1 {
		t.Fatalf("go module import edges = %#v", edges)
	}
	edge := edges[0]
	if edge.RelationKind != "cross_repo_import_candidate" || edge.Endpoint != "external:import:golang.org/x/sync/errgroup" || edge.FromRepo != "local/app" || edge.ToRepo != "gomod/golang.org/x/sync" || edge.ToSymbol.FilePath != "errgroup/errgroup.go" {
		t.Fatalf("unexpected go module import edge: %#v", edge)
	}
}

func TestWorkspaceGraphMatchesCargoImportCandidates(t *testing.T) {
	edges := workspaceGraphImportCrossEdges([]workspaceRepoGraphIndex{
		{
			RepoKey: "local/app",
			Imports: []workspaceGraphImportRef{{
				Spec: "tokio::sync",
				Source: workspaceGraphSymbolRef{
					RepoKey:       "local/app",
					ID:            "app:sym",
					Kind:          "function",
					Name:          "run",
					QualifiedName: "app.run",
					FilePath:      "src/main.rs",
				},
				Count: 1,
			}},
		},
		{
			RepoKey: "cargo/tokio",
			Candidates: []workspaceGraphSymbolRef{{
				RepoKey:       "cargo/tokio",
				ID:            "lib:sym",
				Kind:          "function",
				Name:          "channel",
				QualifiedName: "sync.channel",
				FilePath:      "sync/channel.rs",
			}},
		},
	}, 10)
	if len(edges) != 1 {
		t.Fatalf("cargo import edges = %#v", edges)
	}
	edge := edges[0]
	if edge.RelationKind != "cross_repo_import_candidate" || edge.Endpoint != "external:import:tokio::sync" || edge.FromRepo != "local/app" || edge.ToRepo != "cargo/tokio" || edge.ToSymbol.FilePath != "sync/channel.rs" {
		t.Fatalf("unexpected cargo import edge: %#v", edge)
	}
}

func TestWorkspaceGraphMatchesMavenImportCandidates(t *testing.T) {
	edges := workspaceGraphImportCrossEdges([]workspaceRepoGraphIndex{
		{
			RepoKey: "local/app",
			Imports: []workspaceGraphImportRef{{
				Spec: "com.acme.lib.Service",
				Source: workspaceGraphSymbolRef{
					RepoKey:       "local/app",
					ID:            "app:sym",
					Kind:          "function",
					Name:          "run",
					QualifiedName: "service.run",
					FilePath:      "src/main/java/com/acme/app/App.java",
				},
				Count: 1,
			}},
		},
		{
			RepoKey: "maven/com.acme/lib",
			Candidates: []workspaceGraphSymbolRef{{
				RepoKey:       "maven/com.acme/lib",
				ID:            "lib:sym",
				Kind:          "class",
				Name:          "Service",
				QualifiedName: "Service",
				FilePath:      "Service.java",
			}},
		},
	}, 10)
	if len(edges) != 1 {
		t.Fatalf("maven import edges = %#v", edges)
	}
	edge := edges[0]
	if edge.RelationKind != "cross_repo_import_candidate" || edge.Endpoint != "external:import:com.acme.lib.Service" || edge.FromRepo != "local/app" || edge.ToRepo != "maven/com.acme/lib" || edge.ToSymbol.FilePath != "Service.java" {
		t.Fatalf("unexpected maven import edge: %#v", edge)
	}
}

func TestWorkspaceGraphMatchesAdditionalPackageImportCandidates(t *testing.T) {
	for _, tc := range []struct {
		name          string
		spec          string
		repoKey       string
		targetName    string
		qualifiedName string
		filePath      string
	}{
		{
			name:          "nuget",
			spec:          "Newtonsoft.Json.Linq",
			repoKey:       "nuget/Newtonsoft.Json",
			targetName:    "Linq",
			qualifiedName: "Linq",
			filePath:      "Linq/JToken.cs",
		},
		{
			name:          "gem",
			spec:          "active_support/core_ext",
			repoKey:       "gem/active_support",
			targetName:    "core_ext",
			qualifiedName: "core_ext",
			filePath:      "lib/active_support/core_ext.rb",
		},
		{
			name:          "composer",
			spec:          "monolog/monolog/src/Logger",
			repoKey:       "composer/monolog/monolog",
			targetName:    "Logger",
			qualifiedName: "src/Logger",
			filePath:      "src/Logger.php",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			edges := workspaceGraphImportCrossEdges([]workspaceRepoGraphIndex{
				{
					RepoKey: "local/app",
					Imports: []workspaceGraphImportRef{{
						Spec: tc.spec,
						Source: workspaceGraphSymbolRef{
							RepoKey:       "local/app",
							ID:            "app:sym",
							Kind:          "function",
							Name:          "run",
							QualifiedName: "service.run",
							FilePath:      "service.go",
						},
						Count: 1,
					}},
				},
				{
					RepoKey: tc.repoKey,
					Candidates: []workspaceGraphSymbolRef{{
						RepoKey:       tc.repoKey,
						ID:            "pkg:sym",
						Kind:          "class",
						Name:          tc.targetName,
						QualifiedName: tc.qualifiedName,
						FilePath:      tc.filePath,
					}},
				},
			}, 10)
			if len(edges) != 1 {
				t.Fatalf("%s package import edges = %#v", tc.name, edges)
			}
			edge := edges[0]
			if edge.RelationKind != "cross_repo_import_candidate" ||
				edge.Endpoint != "external:import:"+tc.spec ||
				edge.FromRepo != "local/app" ||
				edge.ToRepo != tc.repoKey ||
				edge.ToSymbol.FilePath != tc.filePath ||
				edge.ToSymbol.Direction != "import_symbol_target" {
				t.Fatalf("unexpected %s package import edge: %#v", tc.name, edge)
			}
		})
	}
}

func TestWorkspaceGraphMatchesKubernetesResourceCandidates(t *testing.T) {
	tests := []struct {
		name       string
		endpoint   string
		targetID   string
		targetName string
		filePath   string
	}{
		{
			name:       "service",
			endpoint:   "external:config:kubernetes/service/api",
			targetID:   "platform:resource:Service.api",
			targetName: "Service.api",
			filePath:   "k8s/service.yaml",
		},
		{
			name:       "configmap",
			endpoint:   "external:config:kubernetes/configmap/podinfo-values",
			targetID:   "platform:resource:ConfigMap.podinfo-values",
			targetName: "ConfigMap.podinfo-values",
			filePath:   "k8s/podinfo-values.yaml",
		},
		{
			name:       "secret",
			endpoint:   "external:config:kubernetes/secret/podinfo-secret-values",
			targetID:   "platform:resource:Secret.podinfo-secret-values",
			targetName: "Secret.podinfo-secret-values",
			filePath:   "k8s/podinfo-secret-values.yaml",
		},
		{
			name:       "custom-resource",
			endpoint:   "external:config:kubernetes/broker/default",
			targetID:   "platform:resource:Broker.default",
			targetName: "Broker.default",
			filePath:   "k8s/knative-broker.yaml",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			edges := workspaceGraphResourceCrossEdges(map[string]*workspaceExternalContractAggregate{
				"RESOURCE_DEPENDS_ON\x00" + tc.endpoint: {
					Endpoint:   tc.endpoint,
					Type:       "RESOURCE_DEPENDS_ON",
					RepoCounts: map[string]int{"local/app": 1},
					Participants: []workspaceGraphSymbolRef{{
						RepoKey:       "local/app",
						ID:            "app:resource:Deployment.api",
						Kind:          "resource",
						Name:          "Deployment.api",
						QualifiedName: "Deployment.api",
						FilePath:      "k8s/deployment.yaml",
						Direction:     "to_endpoint",
						Count:         1,
					}},
				},
			}, []workspaceRepoGraphIndex{
				{
					RepoKey: "local/app",
				},
				{
					RepoKey: "local/platform",
					Candidates: []workspaceGraphSymbolRef{{
						RepoKey:       "local/platform",
						ID:            tc.targetID,
						Kind:          "resource",
						Name:          tc.targetName,
						QualifiedName: tc.targetName,
						FilePath:      tc.filePath,
					}},
				},
			}, 10)
			if len(edges) != 1 {
				t.Fatalf("kubernetes resource candidate edges = %#v", edges)
			}
			edge := edges[0]
			if edge.RelationKind != "cross_repo_resource_candidate" ||
				edge.Endpoint != tc.endpoint ||
				edge.Type != "RESOURCE_DEPENDS_ON" ||
				edge.FromRepo != "local/app" ||
				edge.ToRepo != "local/platform" ||
				edge.ToSymbol.ID != tc.targetID ||
				edge.ToSymbol.Direction != "external_resource_target" {
				t.Fatalf("unexpected kubernetes resource candidate edge: %#v", edge)
			}
		})
	}
}

func TestWorkspaceGraphMatchesComposeServiceResourceCandidates(t *testing.T) {
	edges := workspaceGraphResourceCrossEdges(map[string]*workspaceExternalContractAggregate{
		"RESOURCE_DEPENDS_ON\x00external:config:compose/service/db": {
			Endpoint:   "external:config:compose/service/db",
			Type:       "RESOURCE_DEPENDS_ON",
			RepoCounts: map[string]int{"local/app": 1},
			Participants: []workspaceGraphSymbolRef{{
				RepoKey:       "local/app",
				ID:            "app:resource:compose.service.api",
				Kind:          "resource",
				Name:          "compose.service.api",
				QualifiedName: "compose.service.api",
				FilePath:      "compose.yaml",
				Direction:     "to_endpoint",
				Count:         1,
			}},
		},
	}, []workspaceRepoGraphIndex{
		{
			RepoKey: "local/app",
		},
		{
			RepoKey: "local/platform",
			Candidates: []workspaceGraphSymbolRef{{
				RepoKey:       "local/platform",
				ID:            "platform:resource:compose.service.db",
				Kind:          "resource",
				Name:          "compose.service.db",
				QualifiedName: "compose.service.db",
				FilePath:      "docker-compose.yml",
			}},
		},
	}, 10)
	if len(edges) != 1 {
		t.Fatalf("compose resource candidate edges = %#v", edges)
	}
	edge := edges[0]
	if edge.RelationKind != "cross_repo_resource_candidate" ||
		edge.Endpoint != "external:config:compose/service/db" ||
		edge.Type != "RESOURCE_DEPENDS_ON" ||
		edge.FromRepo != "local/app" ||
		edge.ToRepo != "local/platform" ||
		edge.ToSymbol.ID != "platform:resource:compose.service.db" ||
		edge.ToSymbol.Direction != "external_resource_target" {
		t.Fatalf("unexpected compose resource candidate edge: %#v", edge)
	}
}

func TestWorkspaceGraphReportsCrossRepoGraphQLCalls(t *testing.T) {
	edges := workspaceGraphGraphQLCrossEdges(map[string]*workspaceExternalContractAggregate{
		"HANDLES_GRAPHQL\x00external:graphql:query user": {
			Endpoint:   "external:graphql:query user",
			Type:       "HANDLES_GRAPHQL",
			RepoCounts: map[string]int{"local/web": 1, "local/api": 1},
			Participants: []workspaceGraphSymbolRef{
				{
					RepoKey:       "local/web",
					ID:            "web:sym:fetchUser",
					Kind:          "function",
					Name:          "fetchUser",
					QualifiedName: "client.fetchUser",
					FilePath:      "src/client.ts",
					Direction:     "to_endpoint",
					Count:         1,
				},
				{
					RepoKey:       "local/schema",
					ID:            "schema:sym:Query.user",
					Kind:          "graphql_schema_field",
					Name:          "Query.user",
					QualifiedName: "Query.user",
					FilePath:      "schema.graphql",
					Direction:     "to_endpoint",
					Count:         1,
				},
				{
					RepoKey:       "local/api",
					ID:            "api:sym:Query.user",
					Kind:          "graphql_resolver",
					Name:          "Query.user",
					QualifiedName: "Query.user",
					FilePath:      "src/resolvers.ts",
					Direction:     "to_endpoint",
					Count:         1,
				},
			},
		},
		"HANDLES_GRAPHQL\x00external:graphql:query viewer": {
			Endpoint:   "external:graphql:query viewer",
			Type:       "HANDLES_GRAPHQL",
			RepoCounts: map[string]int{"local/web": 1, "local/other": 1},
			Participants: []workspaceGraphSymbolRef{
				{RepoKey: "local/web", ID: "web:sym:fetchViewer", Kind: "function", Name: "fetchViewer", Count: 1},
				{RepoKey: "local/other", ID: "other:sym:fetchViewer", Kind: "function", Name: "fetchViewer", Count: 1},
			},
		},
	}, 10)
	if len(edges) != 2 {
		t.Fatalf("graphql cross edges = %#v", edges)
	}
	seen := map[string]workspaceGraphCrossEdge{}
	for _, edge := range edges {
		seen[edge.RelationKind+"->"+edge.ToSymbol.Kind] = edge
		if edge.FromSymbol.Kind == "graphql_schema_field" {
			t.Fatalf("schema field was used as GraphQL operation source: %#v", edge)
		}
	}
	resolverEdge := seen["cross_repo_graphql_call->graphql_resolver"]
	if resolverEdge.Endpoint != "external:graphql:query user" ||
		resolverEdge.Type != "CALLS" ||
		resolverEdge.FromRepo != "local/web" ||
		resolverEdge.ToRepo != "local/api" {
		t.Fatalf("unexpected GraphQL resolver cross edge: %#v", resolverEdge)
	}
	schemaEdge := seen["cross_repo_graphql_schema->graphql_schema_field"]
	if schemaEdge.Endpoint != "external:graphql:query user" ||
		schemaEdge.Type != "CALLS" ||
		schemaEdge.FromRepo != "local/web" ||
		schemaEdge.ToRepo != "local/schema" {
		t.Fatalf("unexpected GraphQL schema cross edge: %#v", schemaEdge)
	}
}

func TestWorkspaceGraphImportCandidatesPreferImportedSymbols(t *testing.T) {
	edges := workspaceGraphImportCrossEdges([]workspaceRepoGraphIndex{
		{
			RepoKey: "local/app",
			Imports: []workspaceGraphImportRef{{
				Spec: "requests.auth.HTTPBasicAuth",
				Source: workspaceGraphSymbolRef{
					RepoKey:       "local/app",
					ID:            "app:sym",
					Kind:          "function",
					Name:          "run",
					QualifiedName: "service.run",
					FilePath:      "service.py",
				},
				Count: 1,
			}},
		},
		{
			RepoKey: "pypi/requests",
			Candidates: []workspaceGraphSymbolRef{
				{
					RepoKey:       "pypi/requests",
					ID:            "module:sym",
					Kind:          "module",
					Name:          "auth",
					QualifiedName: "auth",
					FilePath:      "requests/auth.py",
				},
				{
					RepoKey:       "pypi/requests",
					ID:            "class:sym",
					Kind:          "class",
					Name:          "HTTPBasicAuth",
					QualifiedName: "auth.HTTPBasicAuth",
					FilePath:      "requests/auth.py",
				},
			},
		},
	}, 10)
	if len(edges) != 1 {
		t.Fatalf("python import symbol edges = %#v", edges)
	}
	edge := edges[0]
	if edge.RelationKind != "cross_repo_import_candidate" ||
		edge.Endpoint != "external:import:requests.auth.HTTPBasicAuth" ||
		edge.ToRepo != "pypi/requests" ||
		edge.ToSymbol.ID != "class:sym" ||
		edge.ToSymbol.Direction != "import_symbol_target" {
		t.Fatalf("unexpected python import symbol edge: %#v", edge)
	}
}

func TestWorkspaceGraphReportsCrossRepoRouteCalls(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return time.Date(2026, 6, 20, 14, 30, 0, 0, time.UTC) }}
	cmd := &cobra.Command{Use: "index"}

	repoA := t.TempDir()
	keyA := filepath.ToSlash(filepath.Join("local", localRepoKey(repoA)))
	repoB := t.TempDir()
	keyB := filepath.ToSlash(filepath.Join("local", localRepoKey(repoB)))
	indexWorkspaceGraphRepoWithSnapshot(t, cmd, opts, runner, repoA, keyA, workspaceGraphRouteCallerSnapshot(keyA, "CallShared", "/shared"))
	indexWorkspaceGraphRepoWithSnapshot(t, cmd, opts, runner, repoB, keyB, workspaceGraphRouteHandlerSnapshot(keyB, "HandleShared", "/shared"))

	manifest := workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "routes",
		Repos: []workspaceRepo{
			{RepoKey: keyA, LocalPathHint: repoA},
			{RepoKey: keyB, LocalPathHint: repoB},
		},
	}
	if err := writeWorkspaceManifest(env, manifest); err != nil {
		t.Fatalf("write workspace: %v", err)
	}

	var out bytes.Buffer
	graphCmd := &cobra.Command{Use: "graph"}
	graphCmd.SetOut(&out)
	if err := runWorkspaceGraph(graphCmd, opts, workspaceGraphOptions{limit: 10, json: true}, "routes"); err != nil {
		t.Fatalf("workspace graph: %v", err)
	}
	for _, want := range []string{
		`"relation_kind": "cross_repo_route_call"`,
		`external:route:/shared`,
		`"type": "CALLS"`,
		`"from_repo": "` + keyA + `"`,
		`"to_repo": "` + keyB + `"`,
		`"qualified_name": "service.CallShared"`,
		`"qualified_name": "service.HandleShared"`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("workspace graph route call edge missing %q:\n%s", want, out.String())
		}
	}
}

func TestWorkspaceGraphMatchesCanonicalRouteTemplateCalls(t *testing.T) {
	edges := workspaceGraphRouteCallCrossEdges(map[string]*workspaceExternalContractAggregate{
		"HTTP_CALLS\x00external:route:/api/users/{id}": {
			Endpoint: "external:route:/api/users/{id}",
			Type:     "HTTP_CALLS",
			Participants: []workspaceGraphSymbolRef{{
				RepoKey:       "local/client",
				ID:            "local/client:go:client.go:function:CallUser",
				Kind:          "function",
				Name:          "CallUser",
				QualifiedName: "client.CallUser",
				Count:         1,
			}},
		},
		"HANDLES_ROUTE\x00external:route:/api/users/:userID": {
			Endpoint: "external:route:/api/users/:userID",
			Type:     "HANDLES_ROUTE",
			Participants: []workspaceGraphSymbolRef{{
				RepoKey:       "local/service",
				ID:            "local/service:ts:routes.ts:function:showUser",
				Kind:          "function",
				Name:          "showUser",
				QualifiedName: "routes.showUser",
				Count:         1,
			}},
		},
		"HANDLES_ROUTE\x00external:route:/api/projects/<project_id>": {
			Endpoint: "external:route:/api/projects/<project_id>",
			Type:     "HANDLES_ROUTE",
			Participants: []workspaceGraphSymbolRef{{
				RepoKey:       "local/other",
				ID:            "local/other:py:routes.py:function:show_project",
				Kind:          "function",
				Name:          "show_project",
				QualifiedName: "routes.show_project",
				Count:         1,
			}},
		},
	}, 10)
	if len(edges) != 1 {
		t.Fatalf("expected exactly one canonical route edge, got %#v", edges)
	}
	edge := edges[0]
	if edge.Endpoint != "external:route:/api/users/{param}" ||
		edge.RelationKind != "cross_repo_route_call" ||
		edge.FromRepo != "local/client" ||
		edge.ToRepo != "local/service" ||
		edge.FromSymbol.QualifiedName != "client.CallUser" ||
		edge.ToSymbol.QualifiedName != "routes.showUser" {
		t.Fatalf("unexpected canonical route edge: %#v", edge)
	}
}

func TestWorkspaceGraphReportsExactExternalSymbolEdges(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return time.Date(2026, 6, 20, 14, 0, 0, 0, time.UTC) }}
	cmd := &cobra.Command{Use: "index"}

	repoA := t.TempDir()
	keyA := filepath.ToSlash(filepath.Join("local", localRepoKey(repoA)))
	repoB := t.TempDir()
	keyB := filepath.ToSlash(filepath.Join("local", localRepoKey(repoB)))
	indexWorkspaceGraphRepoWithSnapshot(t, cmd, opts, runner, repoA, keyA, workspaceGraphExternalSymbolSnapshot(keyA, "HandleAPI", "lib.Service"))
	indexWorkspaceGraphRepoWithSnapshot(t, cmd, opts, runner, repoB, keyB, workspaceGraphQualifiedSymbolSnapshot(keyB, "lib/service.go", "Service", "lib.Service"))

	manifest := workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "symbols",
		Repos: []workspaceRepo{
			{RepoKey: keyA, LocalPathHint: repoA},
			{RepoKey: keyB, LocalPathHint: repoB},
		},
	}
	if err := writeWorkspaceManifest(env, manifest); err != nil {
		t.Fatalf("write workspace: %v", err)
	}

	var out bytes.Buffer
	graphCmd := &cobra.Command{Use: "graph"}
	graphCmd.SetOut(&out)
	if err := runWorkspaceGraph(graphCmd, opts, workspaceGraphOptions{limit: 10, json: true}, "symbols"); err != nil {
		t.Fatalf("workspace graph: %v", err)
	}
	for _, want := range []string{
		`"relation_kind": "cross_repo_external_symbol"`,
		`external:symbol:lib.Service`,
		`"from_repo": "` + keyA + `"`,
		`"to_repo": "` + keyB + `"`,
		`"qualified_name": "lib.Service"`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("workspace graph external symbol edge missing %q:\n%s", want, out.String())
		}
	}
}

func TestWorkspaceGraphMatchesRepoPrefixedExternalSymbols(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return time.Date(2026, 6, 20, 14, 0, 0, 0, time.UTC) }}
	cmd := &cobra.Command{Use: "index"}

	repoA := t.TempDir()
	keyA := filepath.ToSlash(filepath.Join("local", localRepoKey(repoA)))
	repoB := t.TempDir()
	keyB := filepath.ToSlash(filepath.Join("local", localRepoKey(repoB)))
	externalSpec := keyB + "/pkg.Service"
	indexWorkspaceGraphRepoWithSnapshot(t, cmd, opts, runner, repoA, keyA, workspaceGraphExternalSymbolSnapshot(keyA, "HandleAPI", externalSpec))
	indexWorkspaceGraphRepoWithSnapshot(t, cmd, opts, runner, repoB, keyB, workspaceGraphQualifiedSymbolSnapshot(keyB, "pkg/service.go", "Service", "pkg.Service"))

	manifest := workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "prefixed-symbols",
		Repos: []workspaceRepo{
			{RepoKey: keyA, LocalPathHint: repoA},
			{RepoKey: keyB, LocalPathHint: repoB},
		},
	}
	if err := writeWorkspaceManifest(env, manifest); err != nil {
		t.Fatalf("write workspace: %v", err)
	}

	var out bytes.Buffer
	graphCmd := &cobra.Command{Use: "graph"}
	graphCmd.SetOut(&out)
	if err := runWorkspaceGraph(graphCmd, opts, workspaceGraphOptions{limit: 10, json: true}, "prefixed-symbols"); err != nil {
		t.Fatalf("workspace graph: %v", err)
	}
	for _, want := range []string{
		`"relation_kind": "cross_repo_external_symbol"`,
		`external:symbol:` + externalSpec,
		`"to_repo": "` + keyB + `"`,
		`"qualified_name": "pkg.Service"`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("workspace graph repo-prefixed external symbol edge missing %q:\n%s", want, out.String())
		}
	}
}

func TestWorkspaceGraphMatchesPackageRepoExternalSymbols(t *testing.T) {
	edges := workspaceGraphExternalSymbolCrossEdges([]workspaceRepoGraphIndex{
		{
			RepoKey: "local/app",
			ExternalSymbols: []workspaceGraphExternalSymbolRef{{
				Spec: "@acme/lib/pkg.Service",
				Type: "CALLS",
				Source: workspaceGraphSymbolRef{
					RepoKey:       "local/app",
					ID:            "app:sym",
					Kind:          "function",
					Name:          "HandleAPI",
					QualifiedName: "service.HandleAPI",
					FilePath:      "service.go",
				},
				Count: 1,
			}},
		},
		{
			RepoKey: "npm/@acme/lib",
			Candidates: []workspaceGraphSymbolRef{{
				RepoKey:       "npm/@acme/lib",
				ID:            "lib:sym",
				Kind:          "function",
				Name:          "Service",
				QualifiedName: "pkg.Service",
				FilePath:      "pkg/service.ts",
			}},
		},
	}, 10)
	if len(edges) != 1 {
		t.Fatalf("package repo external symbol edges = %#v", edges)
	}
	edge := edges[0]
	if edge.RelationKind != "cross_repo_external_symbol" || edge.Endpoint != "external:symbol:@acme/lib/pkg.Service" || edge.FromRepo != "local/app" || edge.ToRepo != "npm/@acme/lib" || edge.ToSymbol.QualifiedName != "pkg.Service" {
		t.Fatalf("unexpected package repo external symbol edge: %#v", edge)
	}
}

func TestWorkspaceGraphMatchesMavenExternalSymbols(t *testing.T) {
	edges := workspaceGraphExternalSymbolCrossEdges([]workspaceRepoGraphIndex{
		{
			RepoKey: "local/app",
			ExternalSymbols: []workspaceGraphExternalSymbolRef{{
				Spec: "com.acme.lib.Service",
				Type: "CALLS",
				Source: workspaceGraphSymbolRef{
					RepoKey:       "local/app",
					ID:            "app:sym",
					Kind:          "function",
					Name:          "Run",
					QualifiedName: "service.Run",
					FilePath:      "App.java",
				},
				Count: 1,
			}},
		},
		{
			RepoKey: "maven/com.acme/lib",
			Candidates: []workspaceGraphSymbolRef{{
				RepoKey:       "maven/com.acme/lib",
				ID:            "lib:sym",
				Kind:          "class",
				Name:          "Service",
				QualifiedName: "Service",
				FilePath:      "Service.java",
			}},
		},
	}, 10)
	if len(edges) != 1 {
		t.Fatalf("maven external symbol edges = %#v", edges)
	}
	edge := edges[0]
	if edge.RelationKind != "cross_repo_external_symbol" || edge.Endpoint != "external:symbol:com.acme.lib.Service" || edge.FromRepo != "local/app" || edge.ToRepo != "maven/com.acme/lib" || edge.ToSymbol.QualifiedName != "Service" {
		t.Fatalf("unexpected maven external symbol edge: %#v", edge)
	}
}

func TestWorkspaceGraphMatchesAdditionalPackageExternalSymbols(t *testing.T) {
	edges := workspaceGraphExternalSymbolCrossEdges([]workspaceRepoGraphIndex{
		{
			RepoKey: "local/app",
			ExternalSymbols: []workspaceGraphExternalSymbolRef{{
				Spec: "Newtonsoft.Json.Linq.JToken",
				Type: "CALLS",
				Source: workspaceGraphSymbolRef{
					RepoKey:       "local/app",
					ID:            "app:sym",
					Kind:          "function",
					Name:          "Run",
					QualifiedName: "service.Run",
					FilePath:      "Program.cs",
				},
				Count: 1,
			}},
		},
		{
			RepoKey: "nuget/Newtonsoft.Json",
			Candidates: []workspaceGraphSymbolRef{{
				RepoKey:       "nuget/Newtonsoft.Json",
				ID:            "pkg:sym",
				Kind:          "class",
				Name:          "JToken",
				QualifiedName: "Linq.JToken",
				FilePath:      "Src/Newtonsoft.Json/Linq/JToken.cs",
			}},
		},
	}, 10)
	if len(edges) != 1 {
		t.Fatalf("nuget external symbol edges = %#v", edges)
	}
	edge := edges[0]
	if edge.RelationKind != "cross_repo_external_symbol" ||
		edge.Endpoint != "external:symbol:Newtonsoft.Json.Linq.JToken" ||
		edge.FromRepo != "local/app" ||
		edge.ToRepo != "nuget/Newtonsoft.Json" ||
		edge.ToSymbol.QualifiedName != "Linq.JToken" ||
		edge.ToSymbol.Direction != "external_symbol_target" {
		t.Fatalf("unexpected nuget external symbol edge: %#v", edge)
	}
}

func TestWorkspaceExternalSymbolTargetMatchesGitHubRepoPrefixes(t *testing.T) {
	target, ok := workspaceExternalSymbolTarget([]workspaceGraphSymbolRef{{
		RepoKey:       "gh/acme/lib",
		ID:            "sym",
		Kind:          "function",
		Name:          "Service",
		QualifiedName: "pkg.Service",
		FilePath:      "pkg/service.go",
	}}, "github.com/acme/lib/pkg.Service")
	if !ok || target.ID != "sym" || target.Direction != "external_symbol_target" {
		t.Fatalf("github-prefixed external symbol target = %#v, %v", target, ok)
	}
}

func TestWorkspaceExternalSymbolTargetMatchesPackageRepoPrefixes(t *testing.T) {
	target, ok := workspaceExternalSymbolTarget([]workspaceGraphSymbolRef{{
		RepoKey:       "npm/@acme/lib",
		ID:            "sym",
		Kind:          "function",
		Name:          "Service",
		QualifiedName: "pkg.Service",
		FilePath:      "pkg/service.ts",
	}}, "@acme/lib/pkg.Service")
	if !ok || target.ID != "sym" || target.Direction != "external_symbol_target" {
		t.Fatalf("package-prefixed external symbol target = %#v, %v", target, ok)
	}
}

func TestWorkspaceImportMatchesGitHubRepoKeys(t *testing.T) {
	subpath, ok := workspaceImportMatchesRepo("github.com/acme/lib/pkg/sub", "gh/acme/lib")
	if !ok || subpath != "pkg/sub" {
		t.Fatalf("github import match = %q, %v", subpath, ok)
	}
	subpath, ok = workspaceImportMatchesRepo("acme/lib/pkg", "gh/acme/lib")
	if !ok || subpath != "pkg" {
		t.Fatalf("owner/repo import match = %q, %v", subpath, ok)
	}
	subpath, ok = workspaceImportMatchesRepo("@acme/lib/pkg", "gh/acme/lib")
	if !ok || subpath != "pkg" {
		t.Fatalf("scoped package import match = %q, %v", subpath, ok)
	}
	if _, ok := workspaceImportMatchesRepo("github.com/acme/other/pkg", "gh/acme/lib"); ok {
		t.Fatalf("unrelated repo import should not match")
	}
}

func TestWorkspaceImportMatchesPackageRepoKeys(t *testing.T) {
	subpath, ok := workspaceImportMatchesRepo("@acme/lib/pkg", "npm/@acme/lib")
	if !ok || subpath != "pkg" {
		t.Fatalf("npm scoped import match = %q, %v", subpath, ok)
	}
	subpath, ok = workspaceImportMatchesRepo("requests/auth", "pypi/requests")
	if !ok || subpath != "auth" {
		t.Fatalf("pypi import match = %q, %v", subpath, ok)
	}
	subpath, ok = workspaceImportMatchesRepo("golang.org/x/sync/errgroup", "gomod/golang.org/x/sync")
	if !ok || subpath != "errgroup" {
		t.Fatalf("go module import match = %q, %v", subpath, ok)
	}
	subpath, ok = workspaceImportMatchesRepo("tokio::sync", "cargo/tokio")
	if !ok || subpath != "sync" {
		t.Fatalf("cargo import match = %q, %v", subpath, ok)
	}
	subpath, ok = workspaceImportMatchesRepo("com.acme.lib.Service", "maven/com.acme/lib")
	if !ok || subpath != "Service" {
		t.Fatalf("maven import match = %q, %v", subpath, ok)
	}
	subpath, ok = workspaceImportMatchesRepo("Newtonsoft.Json.Linq", "nuget/Newtonsoft.Json")
	if !ok || subpath != "Linq" {
		t.Fatalf("nuget import match = %q, %v", subpath, ok)
	}
	subpath, ok = workspaceImportMatchesRepo("active_support/core_ext", "gem/active_support")
	if !ok || subpath != "core_ext" {
		t.Fatalf("gem import match = %q, %v", subpath, ok)
	}
	subpath, ok = workspaceImportMatchesRepo("monolog/monolog/src/Logger", "composer/monolog/monolog")
	if !ok || subpath != "src/Logger" {
		t.Fatalf("composer import match = %q, %v", subpath, ok)
	}
	if _, ok := workspaceImportMatchesRepo("@acme/other/pkg", "npm/@acme/lib"); ok {
		t.Fatalf("unrelated package import should not match")
	}
}

func indexWorkspaceGraphRepo(t *testing.T, cmd *cobra.Command, opts Options, runner *fakeCommandRunner, repoDir, repoKey, symbolName string) {
	t.Helper()
	indexWorkspaceGraphRepoWithSnapshot(t, cmd, opts, runner, repoDir, repoKey, workspaceGraphSnapshot(repoKey, symbolName))
}

func indexWorkspaceGraphRepoWithSnapshot(t *testing.T, cmd *cobra.Command, opts Options, runner *fakeCommandRunner, repoDir, repoKey, snapshot string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repoDir, "service.go"), []byte("package service\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner.responses = map[string]fakeCommandResponse{
		fakeCommandKey("git", "rev-parse", "--show-toplevel"):                                                {stdout: repoDir + "\n"},
		fakeCommandKey("git", "rev-parse", "HEAD"):                                                           {stdout: "aaa111\n"},
		fakeCommandKey("git", "rev-parse", "HEAD^{tree}"):                                                    {stdout: "tree111\n"},
		fakeCommandKey("git", "branch", "--show-current"):                                                    {stdout: "main\n"},
		fakeCommandKey("git", "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"):              {stdout: "origin/main\n"},
		fakeCommandKey("git", "status", "--porcelain"):                                                       {stdout: ""},
		fakeCommandKey("entire", "sem", "doctor", "--json"):                                                  {stdout: `{"no_egress":true}`},
		fakeCommandKey("entire", "sem", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network"): {stdout: snapshot},
	}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index %s: %v", repoKey, err)
	}
}

func workspaceGraphSnapshot(repoKey, symbolName string) string {
	symbolID := repoKey + ":go:service.go:function:" + symbolName
	return `{"schema_version":"1.1","provider":"entire-sem","provider_version":"0.1.0","repo_key":"` + repoKey + `","commit":"aaa111","tree":"tree111","languages":["Go"],"capabilities":["ndjson"],"profile":"full","relation_set":["DEFINES","CONFIGURES","HANDLES_ROUTE"],"warnings":[],"partial_failures":[]}
{"record_type":"file","id":"` + repoKey + `:file:service.go","path":"service.go","blob":"abc","language":"Go","bytes":16}
{"record_type":"symbol","id":"` + symbolID + `","kind":"function","name":"` + symbolName + `","qualified_name":"service.` + symbolName + `","file_path":"service.go","start_line":1,"end_line":1,"signature":"func ` + symbolName + `()","language":"Go","stable_id_version":"1"}
{"record_type":"relation","from_id":"` + symbolID + `","to_id":"external:config:kubernetes/image/shared:latest","type":"CONFIGURES","confidence":0.82}
{"record_type":"relation","from_id":"` + symbolID + `","to_id":"external:route:/shared","type":"HANDLES_ROUTE","confidence":0.95}
`
}

func workspaceGraphImportingSnapshot(repoKey, symbolName, importSpec string) string {
	symbolID := repoKey + ":go:service.go:function:" + symbolName
	return `{"schema_version":"1.1","provider":"entire-sem","provider_version":"0.1.0","repo_key":"` + repoKey + `","commit":"aaa111","tree":"tree111","languages":["Go"],"capabilities":["ndjson"],"profile":"full","relation_set":["DEFINES","IMPORTS"],"warnings":[],"partial_failures":[]}
{"record_type":"file","id":"` + repoKey + `:file:service.go","path":"service.go","blob":"abc","language":"Go","bytes":16}
{"record_type":"symbol","id":"` + symbolID + `","kind":"function","name":"` + symbolName + `","qualified_name":"service.` + symbolName + `","file_path":"service.go","start_line":1,"end_line":1,"signature":"func ` + symbolName + `()","language":"Go","stable_id_version":"1"}
{"record_type":"relation","from_id":"` + symbolID + `","to_id":"external:import:` + importSpec + `","type":"IMPORTS","confidence":0.8}
`
}

func workspaceGraphRouteCallerSnapshot(repoKey, symbolName, route string) string {
	symbolID := repoKey + ":go:client.go:function:" + symbolName
	return `{"schema_version":"1.1","provider":"entire-sem","provider_version":"0.1.0","repo_key":"` + repoKey + `","commit":"aaa111","tree":"tree111","languages":["Go"],"capabilities":["ndjson"],"profile":"full","relation_set":["DEFINES","HTTP_CALLS"],"warnings":[],"partial_failures":[]}
{"record_type":"file","id":"` + repoKey + `:file:client.go","path":"client.go","blob":"abc","language":"Go","bytes":16}
{"record_type":"symbol","id":"` + symbolID + `","kind":"function","name":"` + symbolName + `","qualified_name":"service.` + symbolName + `","file_path":"client.go","start_line":1,"end_line":1,"signature":"func ` + symbolName + `()","language":"Go","stable_id_version":"1"}
{"record_type":"relation","from_id":"` + symbolID + `","to_id":"external:route:` + route + `","type":"HTTP_CALLS","confidence":0.82}
`
}

func workspaceGraphRouteHandlerSnapshot(repoKey, symbolName, route string) string {
	symbolID := repoKey + ":go:server.go:function:" + symbolName
	return `{"schema_version":"1.1","provider":"entire-sem","provider_version":"0.1.0","repo_key":"` + repoKey + `","commit":"aaa111","tree":"tree111","languages":["Go"],"capabilities":["ndjson"],"profile":"full","relation_set":["DEFINES","HANDLES_ROUTE"],"warnings":[],"partial_failures":[]}
{"record_type":"file","id":"` + repoKey + `:file:server.go","path":"server.go","blob":"abc","language":"Go","bytes":16}
{"record_type":"symbol","id":"` + symbolID + `","kind":"function","name":"` + symbolName + `","qualified_name":"service.` + symbolName + `","file_path":"server.go","start_line":1,"end_line":1,"signature":"func ` + symbolName + `()","language":"Go","stable_id_version":"1"}
{"record_type":"relation","from_id":"` + symbolID + `","to_id":"external:route:` + route + `","type":"HANDLES_ROUTE","confidence":0.95}
`
}

func workspaceGraphLibrarySnapshot(repoKey, path, symbolName string) string {
	symbolID := repoKey + ":go:" + path + ":function:" + symbolName
	return `{"schema_version":"1.1","provider":"entire-sem","provider_version":"0.1.0","repo_key":"` + repoKey + `","commit":"aaa111","tree":"tree111","languages":["Go"],"capabilities":["ndjson"],"profile":"full","relation_set":["DEFINES"],"warnings":[],"partial_failures":[]}
{"record_type":"file","id":"` + repoKey + `:file:` + path + `","path":"` + path + `","blob":"abc","language":"Go","bytes":16}
{"record_type":"symbol","id":"` + symbolID + `","kind":"function","name":"` + symbolName + `","qualified_name":"pkg.` + symbolName + `","file_path":"` + path + `","start_line":1,"end_line":1,"signature":"func ` + symbolName + `()","language":"Go","stable_id_version":"1"}
`
}

func workspaceGraphExternalSymbolSnapshot(repoKey, symbolName, externalQualifiedName string) string {
	symbolID := repoKey + ":go:service.go:function:" + symbolName
	return `{"schema_version":"1.1","provider":"entire-sem","provider_version":"0.1.0","repo_key":"` + repoKey + `","commit":"aaa111","tree":"tree111","languages":["Go"],"capabilities":["ndjson"],"profile":"full","relation_set":["CALLS"],"warnings":[],"partial_failures":[]}
{"record_type":"file","id":"` + repoKey + `:file:service.go","path":"service.go","blob":"abc","language":"Go","bytes":16}
{"record_type":"symbol","id":"` + symbolID + `","kind":"function","name":"` + symbolName + `","qualified_name":"service.` + symbolName + `","file_path":"service.go","start_line":1,"end_line":1,"signature":"func ` + symbolName + `()","language":"Go","stable_id_version":"1"}
{"record_type":"relation","from_id":"` + symbolID + `","to_id":"external:symbol:` + externalQualifiedName + `","type":"CALLS","confidence":0.82}
`
}

func workspaceGraphQualifiedSymbolSnapshot(repoKey, path, symbolName, qualifiedName string) string {
	symbolID := repoKey + ":go:" + path + ":function:" + symbolName
	return `{"schema_version":"1.1","provider":"entire-sem","provider_version":"0.1.0","repo_key":"` + repoKey + `","commit":"aaa111","tree":"tree111","languages":["Go"],"capabilities":["ndjson"],"profile":"full","relation_set":["DEFINES"],"warnings":[],"partial_failures":[]}
{"record_type":"file","id":"` + repoKey + `:file:` + path + `","path":"` + path + `","blob":"abc","language":"Go","bytes":16}
{"record_type":"symbol","id":"` + symbolID + `","kind":"function","name":"` + symbolName + `","qualified_name":"` + qualifiedName + `","file_path":"` + path + `","start_line":1,"end_line":1,"signature":"func ` + symbolName + `()","language":"Go","stable_id_version":"1"}
`
}

// writeWorkspaceBrainRepo builds a sessions-only brain at brainDirForKey(env, key) plus a working
// tree, so the cross-repo regression/review fan-out can be exercised without a semantic index.
func writeWorkspaceBrainRepo(t *testing.T, env EntireEnv, key, sessionText, repoRel, fileBody string) string {
	t.Helper()
	repoDir := t.TempDir()
	writeWorkspaceBrainRepoAt(t, env, key, repoDir, sessionText, repoRel, fileBody)
	return repoDir
}

func writeLocalWorkspaceBrainRepo(t *testing.T, env EntireEnv, sessionText, repoRel, fileBody string) (string, string) {
	t.Helper()
	repoDir := t.TempDir()
	key := filepath.ToSlash(filepath.Join("local", localRepoKey(repoDir)))
	writeWorkspaceBrainRepoAt(t, env, key, repoDir, sessionText, repoRel, fileBody)
	return repoDir, key
}

func writeWorkspaceBrainRepoAt(t *testing.T, env EntireEnv, key, repoDir, sessionText, repoRel, fileBody string) {
	t.Helper()
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, filepath.FromSlash(key))
	sessDir := filepath.Join(brainDir, "sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, "s.jsonl"), []byte(sessionText+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(repoDir, filepath.FromSlash(repoRel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(fileBody), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceReviewFlagsRegressedRepoOnly(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})

	session := `{"text":"in pkg/review_context.go the scope diff uses scopeBaseRef+\"..HEAD\" for the range"}`
	regressed := "package x\nfunc f() string {\n\treturn \"master..HEAD\"\n}\n"
	clean := "package x\nfunc f() string {\n\treturn scopeBaseRef + \"..HEAD\"\n}\n"
	repoA, keyA := writeLocalWorkspaceBrainRepo(t, env, session, "pkg/review_context.go", regressed)
	repoB, keyB := writeLocalWorkspaceBrainRepo(t, env, session, "pkg/review_context.go", clean)

	manifest := workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "related",
		Repos: []workspaceRepo{
			{RepoKey: keyA, Name: "a", LocalPathHint: repoA},
			{RepoKey: keyB, Name: "b", LocalPathHint: repoB},
		},
	}
	if err := writeWorkspaceManifest(env, manifest); err != nil {
		t.Fatalf("write workspace: %v", err)
	}

	out, err := execute(t, cmd, "workspace", "review", "related", "fix scopeBaseRef base scope", "--json")
	if err != nil {
		t.Fatalf("workspace review: %v", err)
	}
	var payload struct {
		SchemaVersion int                     `json:"schema_version"`
		Mode          string                  `json:"mode"`
		Summary       string                  `json:"summary"`
		Results       []workspaceReviewResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("parse review json: %v\n%s", err, out)
	}
	if payload.SchemaVersion != reviewReportSchemaVersion || payload.Mode != "diff-less (brain memory vs current tree)" {
		t.Fatalf("review envelope schema_version/mode wrong: v=%d mode=%q", payload.SchemaVersion, payload.Mode)
	}
	if !strings.Contains(payload.Summary, "1 suspected regression(s) across 1/2 repo(s)") {
		t.Fatalf("summary = %q", payload.Summary)
	}
	byKey := map[string]workspaceReviewResult{}
	for _, r := range payload.Results {
		byKey[r.RepoKey] = r
	}
	a, b := byKey[keyA], byKey[keyB]
	// changed-findings are "medium" (hedged "could be a rename"), not "high" — see regressionSeverity.
	if len(a.Findings) != 1 || a.Findings[0].Severity != "medium" || !strings.Contains(a.Findings[0].File, "review_context.go") {
		t.Fatalf("repoa findings wrong: %+v", a)
	}
	if len(b.Findings) != 0 {
		t.Fatalf("repob (clean) should have no findings: %+v", b)
	}
	// The clean repo must NOT be falsely summarized; and Findings serializes as [] not null.
	if !strings.Contains(b.Summary, "no suspected regressions") {
		t.Fatalf("clean repo summary wrong: %q", b.Summary)
	}
	if !strings.Contains(out, `"findings": []`) {
		t.Fatalf("empty findings must serialize as [] not null:\n%s", out)
	}
}

func TestWorkspaceReviewTextOutputRendersFindingsAndFreshness(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})

	session := `{"text":"in pkg/review_context.go the scope diff uses scopeBaseRef+\"..HEAD\" for the range"}`
	regressed := "package x\nfunc f() string {\n\treturn \"master..HEAD\"\n}\n"
	repoA, keyA := writeLocalWorkspaceBrainRepo(t, env, session, "pkg/review_context.go", regressed)
	manifest := workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "related",
		Repos:         []workspaceRepo{{RepoKey: keyA, Name: "a", LocalPathHint: repoA}},
	}
	if err := writeWorkspaceManifest(env, manifest); err != nil {
		t.Fatalf("write workspace: %v", err)
	}

	// Text mode (no --json): must render the summary, the per-repo freshness label, and the finding.
	out, err := execute(t, cmd, "workspace", "review", "related", "fix scopeBaseRef base scope")
	if err != nil {
		t.Fatalf("workspace review (text): %v", err)
	}
	for _, want := range []string{"Cross-repo diff-less review:", keyA, "[", "Suspected regression", "review_context.go"} {
		if !strings.Contains(out, want) {
			t.Fatalf("text review output missing %q:\n%s", want, out)
		}
	}
	// Sessions-only brain → "missing-brain" freshness, but a finding still renders; the warning must
	// be the accurate "scanned raw sessions only" form, not a misleading "nothing to compare".
	if strings.Contains(out, "nothing to compare") {
		t.Fatalf("misleading missing-brain warning surfaced despite a real finding:\n%s", out)
	}
}

func TestWorkspaceRegressionsAggregatesAndToleratesMissingBrain(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})

	session := `{"text":"in pkg/review_context.go the scope diff uses scopeBaseRef+\"..HEAD\" for the range"}`
	regressed := "package x\nfunc f() string {\n\treturn \"master..HEAD\"\n}\n"
	repoA, keyA := writeLocalWorkspaceBrainRepo(t, env, session, "pkg/review_context.go", regressed)
	// A repo registered with a real working tree but NO brain prepped (missing-brain).
	missingTree := t.TempDir()
	missingKey := filepath.ToSlash(filepath.Join("local", localRepoKey(missingTree)))

	manifest := workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "related",
		Repos: []workspaceRepo{
			{RepoKey: keyA, Name: "a", LocalPathHint: repoA},
			{RepoKey: missingKey, Name: "missing", LocalPathHint: missingTree},
		},
	}
	if err := writeWorkspaceManifest(env, manifest); err != nil {
		t.Fatalf("write workspace: %v", err)
	}

	out, err := execute(t, cmd, "workspace", "inspect", "regressions", "related", "fix scopeBaseRef base scope", "--json", "--location-only")
	if err != nil {
		t.Fatalf("workspace regressions: %v", err)
	}
	var payload struct {
		Results []workspaceRegressionResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("parse regressions json: %v\n%s", err, out)
	}
	byKey := map[string]workspaceRegressionResult{}
	for _, r := range payload.Results {
		byKey[r.RepoKey] = r
	}
	a := byKey[keyA]
	if len(a.Anomalies) != 1 || a.Anomalies[0].Kind != "changed" {
		t.Fatalf("repoa anomalies wrong: %+v", a)
	}
	if a.Anomalies[0].Expected != "" || a.Anomalies[0].Current != "" {
		t.Fatalf("--location-only must blank expected/current: %+v", a.Anomalies[0])
	}
	// The missing-brain repo must not abort the run; it just yields no anomalies.
	m := byKey[missingKey]
	if len(m.Anomalies) != 0 {
		t.Fatalf("missing-brain repo should yield no anomalies: %+v", m)
	}
}

func TestWorkspaceRegressionsDeletionLocationOnlyReportsRelatedLoci(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})

	session := `{"text":"pkg/resolve.go must set state.TranscriptPath = resolved so later reads work"}`
	body := "package x\nfunc missOne(state *State) string {\n\tresolved := compute()\n\t_ = state.TranscriptPath\n\treturn resolved\n}\nfunc missTwo(state *State) string {\n\tresolved := compute()\n\t_ = state.TranscriptPath\n\treturn resolved\n}\n"
	repoA, keyA := writeLocalWorkspaceBrainRepo(t, env, session, "pkg/resolve.go", body)
	if err := writeWorkspaceManifest(env, workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "related",
		Repos:         []workspaceRepo{{RepoKey: keyA, Name: "a", LocalPathHint: repoA}},
	}); err != nil {
		t.Fatalf("write workspace: %v", err)
	}

	out, err := execute(t, cmd, "workspace", "inspect", "regressions", "related", "fix TranscriptPath resolved", "--json", "--include-deletions", "--location-only")
	if err != nil {
		t.Fatalf("workspace regressions: %v", err)
	}
	var payload struct {
		Results []workspaceRegressionResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("parse regressions json: %v\n%s", err, out)
	}
	if len(payload.Results) != 1 {
		t.Fatalf("results = %d, want 1: %s", len(payload.Results), out)
	}
	result := payload.Results[0]
	if result.RepoKey != keyA {
		t.Fatalf("repo key = %q, want %q", result.RepoKey, keyA)
	}
	if len(result.Anomalies) != 2 {
		t.Fatalf("anomalies = %d, want 2: %+v", len(result.Anomalies), result.Anomalies)
	}
	bySymbol := map[string]regressionAnomaly{}
	for _, a := range result.Anomalies {
		if a.Expected != "" || a.Current != "" {
			t.Fatalf("--location-only must blank expected/current: %+v", a)
		}
		bySymbol[a.Symbol] = a
	}
	for _, sym := range []string{"missOne", "missTwo"} {
		a, ok := bySymbol[sym]
		if !ok {
			t.Fatalf("missing %s anomaly: %+v", sym, result.Anomalies)
		}
		if len(a.RelatedLocations) == 0 {
			t.Fatalf("%s missing related location peer: %+v", sym, a)
		}
	}
	if !strings.Contains(strings.Join(bySymbol["missOne"].RelatedLocations, "\n"), "missTwo") ||
		!strings.Contains(strings.Join(bySymbol["missTwo"].RelatedLocations, "\n"), "missOne") {
		t.Fatalf("related locations did not preserve both loci: %+v", result.Anomalies)
	}
}

func TestWorkspaceFreshnessBlocksScan(t *testing.T) {
	// Only an untrusted brain<->tree pairing (PairingUnsafe) blocks the scan. State alone does NOT —
	// a stale/broken semantic index (even State "unsafe") over a valid pairing is still scannable
	// from raw sessions, and blocking it would drop real findings.
	for _, st := range []string{"ok", "degraded", "missing-brain", "missing-semantic", "unknown", "unsafe", ""} {
		if workspaceFreshnessBlocksScan(workspaceRepoFreshness{State: st}) {
			t.Errorf("state %q without PairingUnsafe should NOT block the scan", st)
		}
	}
	if !workspaceFreshnessBlocksScan(workspaceRepoFreshness{State: "unsafe", PairingUnsafe: true}) {
		t.Error("PairingUnsafe must block the scan")
	}
}

func TestWorkspaceRegressionsSkipsUnsafeRepo(t *testing.T) {
	// A stale local_path_hint whose repo_key no longer matches the registered key means the brain
	// (by key) and the tree (by hint) can't be trusted to match — scanning would compare one repo's
	// memory against an unrelated tree and manufacture bogus regressions. The scan must be skipped.
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
	if _, err := execute(t, cmd, "refresh", "index", "--sem-binary", "entire"); err != nil {
		t.Fatalf("index: %v", err)
	}
	// Seed a session asserting the invariant + a regressed working tree, so a scan WOULD fire.
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	if err := os.MkdirAll(filepath.Join(brainDir, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brainDir, "sessions", "s.jsonl"),
		[]byte(`{"text":"in pkg/review_context.go the scope diff uses scopeBaseRef+\"..HEAD\""}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "pkg", "review_context.go"),
		[]byte("package x\nfunc f() string {\n\treturn \"master..HEAD\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "related",
		Repos:         []workspaceRepo{{RepoKey: "gh/example/repo", Name: "api", LocalPathHint: repoDir}},
	}
	if err := writeWorkspaceManifest(env, manifest); err != nil {
		t.Fatalf("write workspace: %v", err)
	}
	// Flip the remote so the hint's repo_key no longer matches the registered key -> unsafe.
	runner.responses[fakeCommandKey("git", "remote", "get-url", "origin")] = fakeCommandResponse{stdout: "git@github.com:other/repo.git\n"}

	out, err := execute(t, cmd, "workspace", "inspect", "regressions", "related", "fix scopeBaseRef base scope", "--json")
	if err != nil {
		t.Fatalf("workspace regressions: %v", err)
	}
	var payload struct {
		Results []workspaceRegressionResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("parse json: %v\n%s", err, out)
	}
	if len(payload.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(payload.Results))
	}
	r := payload.Results[0]
	if len(r.Anomalies) != 0 {
		t.Fatalf("unsafe repo must be skipped (no anomalies despite a real regression), got %+v", r.Anomalies)
	}
	if !strings.Contains(r.Error, "unsafe") {
		t.Fatalf("expected an unsafe-skip error, got %q", r.Error)
	}

	// Text output must also surface the unsafe state + skip, and must NOT print a bogus finding.
	// Use a fresh command: cobra flag state (--json) persists on a reused root command.
	textCmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
	textOut, err := execute(t, textCmd, "workspace", "inspect", "regressions", "related", "fix scopeBaseRef base scope")
	if err != nil {
		t.Fatalf("workspace regressions (text): %v", err)
	}
	if !strings.Contains(textOut, "[unsafe]") || !strings.Contains(textOut, "skipped (unsafe)") {
		t.Fatalf("text output must surface unsafe freshness + skip:\n%s", textOut)
	}
	if strings.Contains(textOut, "review_context.go") {
		t.Fatalf("unsafe repo must not emit a finding in text:\n%s", textOut)
	}
}

func TestWorkspaceRegressionsSkipsUnsafeSessionsOnlyRepo(t *testing.T) {
	// Sessions-only brains have no export manifest, but they still must validate
	// the workspace brain<->tree pairing before scanning raw session history.
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})

	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	if err := os.MkdirAll(filepath.Join(brainDir, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brainDir, "sessions", "s.jsonl"),
		[]byte(`{"text":"in pkg/review_context.go the scope diff uses scopeBaseRef+\"..HEAD\""}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "pkg", "review_context.go"),
		[]byte("package x\nfunc f() string {\n\treturn \"master..HEAD\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "related",
		Repos:         []workspaceRepo{{RepoKey: "gh/example/repo", Name: "api", LocalPathHint: repoDir}},
	}
	if err := writeWorkspaceManifest(env, manifest); err != nil {
		t.Fatalf("write workspace: %v", err)
	}
	runner.responses[fakeCommandKey("git", "remote", "get-url", "origin")] = fakeCommandResponse{stdout: "git@github.com:other/repo.git\n"}

	out, err := execute(t, cmd, "workspace", "inspect", "regressions", "related", "fix scopeBaseRef base scope", "--json")
	if err != nil {
		t.Fatalf("workspace regressions: %v", err)
	}
	var payload struct {
		Results []workspaceRegressionResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("parse json: %v\n%s", err, out)
	}
	if len(payload.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(payload.Results))
	}
	r := payload.Results[0]
	if len(r.Anomalies) != 0 {
		t.Fatalf("unsafe sessions-only repo must be skipped, got %+v", r.Anomalies)
	}
	if !strings.Contains(r.Error, "unsafe") || !strings.Contains(r.Error, "local_path_hint repo_key mismatch") {
		t.Fatalf("expected unsafe repo-key mismatch error, got %q", r.Error)
	}
}

func TestWorkspaceRegressionsSkipsUnverifiableRemoteKeyHint(t *testing.T) {
	// A registered remote-key brain cannot be safely paired with a local_path_hint whose repo key
	// falls back to local/<hash>. That is unverifiable, not "probably fine"; scanning it could
	// compare one repo's memory against an unrelated local tree.
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "remote", "get-url", "origin"): {stdout: "not a parseable remote\n"},
	}}
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})

	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, "gh", "example", "repo")
	if err := os.MkdirAll(filepath.Join(brainDir, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brainDir, "sessions", "s.jsonl"),
		[]byte(`{"text":"in pkg/review_context.go the scope diff uses scopeBaseRef+\"..HEAD\""}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "pkg", "review_context.go"),
		[]byte("package x\nfunc f() string {\n\treturn \"master..HEAD\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeWorkspaceManifest(env, workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "related",
		Repos:         []workspaceRepo{{RepoKey: "gh/example/repo", Name: "api", LocalPathHint: repoDir}},
	}); err != nil {
		t.Fatalf("write workspace: %v", err)
	}

	out, err := execute(t, cmd, "workspace", "inspect", "regressions", "related", "fix scopeBaseRef base scope", "--json")
	if err != nil {
		t.Fatalf("workspace regressions: %v", err)
	}
	var payload struct {
		Results []workspaceRegressionResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("parse json: %v\n%s", err, out)
	}
	if len(payload.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(payload.Results))
	}
	r := payload.Results[0]
	if len(r.Anomalies) != 0 {
		t.Fatalf("unverifiable remote-key hint must be skipped, got %+v", r.Anomalies)
	}
	if !strings.Contains(r.Error, "unsafe") || !strings.Contains(r.Error, "local_path_hint repo_key mismatch: local/") {
		t.Fatalf("expected unsafe local fallback mismatch error, got %q", r.Error)
	}
}

func TestWorkspaceFreshnessWarning(t *testing.T) {
	if workspaceFreshnessWarning(workspaceRepoFreshness{State: "ok"}) != "" {
		t.Error("ok freshness should not warn")
	}
	// A scanned repo with a non-ideal state gets a loud warning. "unsafe" here means a stale/broken
	// index over a still-valid pairing (a genuine pairing block sets PairingUnsafe and never reaches
	// this warning path), so it warns "scanned raw sessions only" rather than being silent.
	for _, st := range []string{"degraded", "missing-brain", "missing-semantic", "unknown", "unsafe"} {
		if workspaceFreshnessWarning(workspaceRepoFreshness{State: st}) == "" {
			t.Errorf("state %q should produce a loud warning", st)
		}
	}
}

func TestWorkspaceListAndRemove(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})

	repoA, keyA := writeLocalWorkspaceBrainRepo(t, env, `{"text":"x"}`, "a.go", "package x\n")
	keyB := "gh/example/repob"
	manifest := workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "related",
		Repos: []workspaceRepo{
			{RepoKey: keyA, Name: "a", LocalPathHint: repoA},
			{RepoKey: keyB, Name: "b", LocalPathHint: repoA},
		},
	}
	if err := writeWorkspaceManifest(env, manifest); err != nil {
		t.Fatalf("write workspace: %v", err)
	}

	listOut, err := execute(t, cmd, "workspace", "list", "--json")
	if err != nil {
		t.Fatalf("workspace list: %v", err)
	}
	if !strings.Contains(listOut, `"name": "related"`) || !strings.Contains(listOut, `"repos": 2`) {
		t.Fatalf("list output = %s", listOut)
	}

	if _, err := execute(t, cmd, "workspace", "remove", "related", keyB); err != nil {
		t.Fatalf("workspace remove repo: %v", err)
	}
	reloaded, err := loadWorkspaceManifest(env, "related")
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}
	if len(reloaded.Repos) != 1 || reloaded.Repos[0].RepoKey != keyA {
		t.Fatalf("repo not removed: %+v", reloaded.Repos)
	}

	// Removing a repo that is not present is an error.
	if _, err := execute(t, cmd, "workspace", "remove", "related", "gh/example/nope"); err == nil {
		t.Fatalf("remove of absent repo should error")
	}

	// Removing the whole workspace deletes it.
	if _, err := execute(t, cmd, "workspace", "remove", "related"); err != nil {
		t.Fatalf("workspace remove all: %v", err)
	}
	if _, err := loadWorkspaceManifest(env, "related"); err == nil {
		t.Fatalf("workspace should be gone")
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

func writeWorkspaceDocIndex(t *testing.T, env EntireEnv, key string, records ...docRecord) {
	t.Helper()
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Join(brainDir, docDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(docIndex{Records: records})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brainDir, filepath.FromSlash(docIndexPath)), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceSearchAndGetFanOutWithQualifiedIDs(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})

	_, keyA := writeLocalWorkspaceBrainRepo(t, env, `{"text":"x"}`, "a.go", "package x\n")
	_, keyB := writeLocalWorkspaceBrainRepo(t, env, `{"text":"x"}`, "b.go", "package x\n")
	writeWorkspaceDocIndex(t, env, keyA, docRecord{ID: "guide", Path: "docs/guide.md", Line: 1, Text: "checkout flow retries twice before failing"})
	writeWorkspaceDocIndex(t, env, keyB, docRecord{ID: "other", Path: "docs/other.md", Line: 1, Text: "billing ledger reconciliation"})

	manifest := workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "related",
		Repos:         []workspaceRepo{{RepoKey: keyA, Name: "a"}, {RepoKey: keyB, Name: "b"}},
	}
	if err := writeWorkspaceManifest(env, manifest); err != nil {
		t.Fatalf("write workspace: %v", err)
	}

	searchOut, err := execute(t, cmd, "workspace", "search", "related", "checkout", "--json")
	if err != nil {
		t.Fatalf("workspace search: %v", err)
	}
	if !strings.Contains(searchOut, `"id": "doc:guide"`) {
		t.Fatalf("search output missing doc hit from repo A:\n%s", searchOut)
	}
	// Results stay grouped per repo: repo B has no "checkout" hit, but its group
	// (with an empty results array) is still present.
	if !strings.Contains(searchOut, `"repo_key": "`+keyB+`"`) {
		t.Fatalf("search output missing repo B group:\n%s", searchOut)
	}
	if strings.Contains(searchOut, `"id": "doc:other"`) {
		t.Fatalf("search output leaked repo B's unrelated doc:\n%s", searchOut)
	}

	// Text mode prints repo-qualified ids so they can be pasted into `workspace get`.
	// Fresh root command: cobra flag values (--json) stick across executions.
	textCmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
	textOut, err := execute(t, textCmd, "workspace", "search", "related", "checkout")
	if err != nil {
		t.Fatalf("workspace search (text): %v", err)
	}
	if !strings.Contains(textOut, keyA+"/doc:guide") {
		t.Fatalf("text output missing qualified id %s/doc:guide:\n%s", keyA, textOut)
	}

	// The hybrid verb shares the same fan-out; without an embedder it degrades to
	// the lexical arms and must still return the doc hit.
	queryOut, err := execute(t, cmd, "workspace", "query", "related", "checkout", "--json")
	if err != nil {
		t.Fatalf("workspace query: %v", err)
	}
	if !strings.Contains(queryOut, `"id": "doc:guide"`) {
		t.Fatalf("query output missing doc hit:\n%s", queryOut)
	}

	getOut, err := execute(t, cmd, "workspace", "get", "related", keyA+"/doc:guide", keyB+"/doc:nope", "--json")
	if err != nil {
		t.Fatalf("workspace get: %v", err)
	}
	if !strings.Contains(getOut, "checkout flow retries twice before failing") {
		t.Fatalf("get output missing full doc text:\n%s", getOut)
	}
	if !strings.Contains(getOut, `"`+keyB+`/doc:nope"`) {
		t.Fatalf("get output missing re-qualified missing id:\n%s", getOut)
	}

	if _, err := execute(t, cmd, "workspace", "get", "related", "local/notamember/doc:x"); err == nil || !strings.Contains(err.Error(), "not a member") {
		t.Fatalf("expected non-member error, got %v", err)
	}
	if _, err := execute(t, cmd, "workspace", "get", "related", "doc:guide"); err == nil || !strings.Contains(err.Error(), "missing its repo key") {
		t.Fatalf("expected unqualified-id error, got %v", err)
	}
}

func TestSplitWorkspaceID(t *testing.T) {
	cases := []struct {
		in      string
		repoKey string
		id      string
		wantErr bool
	}{
		{in: "gh/owner/repo/fact:abc", repoKey: "gh/owner/repo", id: "fact:abc"},
		{in: "local/abc123/history:h1", repoKey: "local/abc123", id: "history:h1"},
		{in: "gh/owner/repo/doc:guide", repoKey: "gh/owner/repo", id: "doc:guide"},
		{in: "fact:abc", wantErr: true},      // unqualified
		{in: "gh/owner/repo", wantErr: true}, // no id
		{in: "gh/owner/repo/note:x", wantErr: true},
	}
	for _, tc := range cases {
		repoKey, id, err := splitWorkspaceID(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("splitWorkspaceID(%q): expected error, got %q %q", tc.in, repoKey, id)
			}
			continue
		}
		if err != nil {
			t.Errorf("splitWorkspaceID(%q): %v", tc.in, err)
			continue
		}
		if repoKey != tc.repoKey || id != tc.id {
			t.Errorf("splitWorkspaceID(%q) = %q, %q; want %q, %q", tc.in, repoKey, id, tc.repoKey, tc.id)
		}
	}
}

func TestWorkspaceFullRefreshGatesAndCarriesPerMemberFailures(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	opts := Options{Version: "test", Env: env, Runner: runner, Now: time.Now}
	repoGood, keyGood := writeLocalWorkspaceBrainRepo(t, env, `{"text":"x"}`, "a.go", "package x\n")
	repoBad, keyBad := writeLocalWorkspaceBrainRepo(t, env, `{"text":"x"}`, "b.go", "package x\n")
	manifest := workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "related",
		Repos: []workspaceRepo{
			{RepoKey: keyGood, LocalPathHint: repoGood},
			{RepoKey: keyBad, LocalPathHint: repoBad},
			{RepoKey: "gh/example/nohint"}, // no local_path_hint: must be gated out, not refreshed
		},
	}

	var refreshed []string
	out := &bytes.Buffer{}
	err := workspaceFullRefresh(context.Background(), out, opts, manifest, func(repoDir string) error {
		refreshed = append(refreshed, repoDir)
		if repoDir == repoBad {
			return errors.New("boom")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("workspaceFullRefresh: %v", err)
	}
	if len(refreshed) != 2 || refreshed[0] != repoGood || refreshed[1] != repoBad {
		t.Fatalf("expected refresh attempts on both local members, got %v", refreshed)
	}
	got := out.String()
	if !strings.Contains(got, keyGood+" refreshed") {
		t.Fatalf("missing success line for %s:\n%s", keyGood, got)
	}
	// A failing member is reported and must not abort the fan-out.
	if !strings.Contains(got, keyBad+" refresh failed: boom") {
		t.Fatalf("missing failure line for %s:\n%s", keyBad, got)
	}
	if !strings.Contains(got, "gh/example/nohint skipped (no resolvable local path)") {
		t.Fatalf("missing identity-gate skip line:\n%s", got)
	}
}
