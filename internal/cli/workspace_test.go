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
