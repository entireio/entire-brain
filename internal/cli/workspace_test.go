package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/entireio/entire-brain/internal/cli/command/brainreview"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/command/watch"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/docstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	workspacelib "github.com/entireio/entire-brain/internal/cli/workspace"
	"github.com/spf13/cobra"
)

func TestWorkspaceWatchFansOverMembersWithSharedBudget(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	repoA, repoAKey := writeLocalWorkspaceBrainRepo(t, env, `{"text":"x"}`, "a.go", "package x\n")
	repoB, repoBKey := writeLocalWorkspaceBrainRepo(t, env, `{"text":"x"}`, "b.go", "package x\n")
	manifest := workspacelib.Manifest{
		SchemaVersion: workspacelib.SchemaVersion,
		Name:          "ws",
		Repos: []workspacelib.Repo{
			{RepoKey: repoAKey, LocalPathHint: repoA},
			{RepoKey: repoBKey, LocalPathHint: repoB},
			{RepoKey: "gh/example/repoc", LocalPathHint: "/nonexistent/repoc-xyz"}, // unresolvable -> skipped
		},
	}
	if err := workspacelib.DefaultStore(env).WriteManifest(manifest); err != nil {
		t.Fatalf("write workspace: %v", err)
	}

	var ticked []string
	var seen []int
	repoTick := func(_ io.Writer, repoDir string, agentCalls *int) {
		ticked = append(ticked, repoDir)
		seen = append(seen, *agentCalls)
		*agentCalls++ // simulate a token-spending step to prove the counter is shared, not per-repo
	}
	w := watch.DefaultFlags()
	w.Once = true
	opts := Options{Version: "test", Env: env, Runner: runner, Now: time.Now}
	out := &bytes.Buffer{}
	agentCalls := 0
	if err := workspaceWatchLoop(context.Background(), out, opts, w, "ws", &agentCalls, time.Now, repoTick); err != nil {
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

func TestWorkspaceWatchMemoryQueuesStayIsolatedAcrossRepoCopies(t *testing.T) {
	parent := t.TempDir()
	repoA := filepath.Join(parent, "repo-a")
	repoB := filepath.Join(parent, "repo-b")
	if err := os.MkdirAll(repoA, 0o700); err != nil {
		t.Fatalf("mkdir repo A: %v", err)
	}
	if err := os.MkdirAll(repoB, 0o700); err != nil {
		t.Fatalf("mkdir repo B: %v", err)
	}
	aliasA := filepath.Join(parent, "repo-a-alias")
	if err := os.Symlink(repoA, aliasA); err != nil {
		t.Skipf("directory symlinks are unavailable on this platform: %v", err)
	}
	env := EntireEnv{
		RepoRoot:        aliasA,
		PluginConfigDir: t.TempDir(),
		PluginDataDir:   t.TempDir(),
		PluginStateDir:  t.TempDir(),
		PluginCacheDir:  t.TempDir(),
	}
	opts := Options{Version: "test", Env: env, Now: func() time.Time {
		return time.Date(2026, 8, 9, 15, 0, 0, 0, time.UTC)
	}}

	storageA, err := repoStoragePaths(context.Background(), nil, env, aliasA)
	if err != nil {
		t.Fatalf("repo A storage: %v", err)
	}
	storageB, err := repoStoragePaths(context.Background(), nil, env, repoB)
	if err != nil {
		t.Fatalf("repo B storage: %v", err)
	}
	if storageA.Key == storageB.Key || storageA.BrainDir == storageB.BrainDir || storageA.HeadPath == storageB.HeadPath {
		t.Fatalf("workspace repos share storage: A=%+v B=%+v", storageA, storageB)
	}
	writeQueueManifest := func(storage repoStorage, sessionID string) {
		t.Helper()
		manifest := exportManifest{
			SchemaVersion: bmanifest.SchemaVersion,
			RepoKey:       storage.Key,
			Sources: &brainSources{Sessions: &sessionSourceManifest{
				DefaultBranch: "main",
				Sessions: []exportSession{{
					SessionID:      sessionID,
					Branch:         "main",
					TranscriptPath: filepath.ToSlash(filepath.Join("sessions", "main", sessionID+".jsonl")),
				}},
			}},
		}
		if err := writeBrainManifestAndReadme(storage.BrainDir, manifest); err != nil {
			t.Fatalf("write %s manifest: %v", sessionID, err)
		}
	}
	writeQueueManifest(storageA, "session-a")
	writeQueueManifest(storageB, "session-b")

	oldLaunch := memoryWorkerLaunch
	defer func() { memoryWorkerLaunch = oldLaunch }()
	var launched []string
	memoryWorkerLaunch = func(repoDir string) error {
		launched = append(launched, repoDir)
		return nil
	}
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	w := watch.DefaultFlags()
	for _, repoDir := range []string{aliasA, repoB} {
		steps := watchStepsForRepo(cmd, opts, w, repoDir, opts.Now)
		if err := steps.Reconcile(context.Background()); err != nil {
			t.Fatalf("reconcile %s: %v", repoDir, err)
		}
	}
	if len(launched) != 2 || launched[0] != aliasA || launched[1] != repoB {
		t.Fatalf("worker launches = %v, want [%s %s]", launched, aliasA, repoB)
	}

	for _, check := range []struct {
		storage   repoStorage
		sessionID string
	}{
		{storage: storageA, sessionID: "session-a"},
		{storage: storageB, sessionID: "session-b"},
	} {
		jobs, issues := loadMemoryJobsChecked(check.storage.BrainDir)
		if len(issues) != 0 || len(jobs) == 0 {
			t.Fatalf("queue %s jobs=%+v issues=%+v", check.storage.Key, jobs, issues)
		}
		for _, job := range jobs {
			if job.RepoKey != check.storage.Key || job.SessionID != check.sessionID {
				t.Fatalf("cross-repo queue contamination in %s: %+v", check.storage.BrainDir, job)
			}
		}
	}
}

func TestWorkspaceWatchOnceSkipsStaleLocalPathHintBeforeTick(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	repoB := t.TempDir()
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "remote", "get-url", "origin"): {stdout: "git@github.com:example/repob.git\n"},
	}}
	manifest := workspacelib.Manifest{
		SchemaVersion: workspacelib.SchemaVersion,
		Name:          "ws",
		Repos: []workspacelib.Repo{
			{RepoKey: "gh/example/repoa", LocalPathHint: repoB},
		},
	}
	if err := workspacelib.DefaultStore(env).WriteManifest(manifest); err != nil {
		t.Fatalf("write workspace: %v", err)
	}

	w := watch.DefaultFlags()
	w.Once = true
	opts := Options{Version: "test", Env: env, Runner: runner, Now: time.Now}
	out := &bytes.Buffer{}
	agentCalls := 0
	if err := workspaceWatchLoop(context.Background(), out, opts, w, "ws", &agentCalls, time.Now, func(_ io.Writer, repoDir string, agentCalls *int) {
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

	if _, err := execute(t, cmd, "refresh", "index", "--graph-binary", "entire"); err != nil {
		t.Fatalf("index: %v", err)
	}
	out, err := execute(t, cmd, "workspace", "create", "payments-platform")
	if err != nil {
		t.Fatalf("workspace create: %v", err)
	}
	if !strings.Contains(out, filepath.Join(base.WorkspacesDirName, "payments-platform")) {
		t.Fatalf("create output = %q", out)
	}
	if strings.Contains(out, filepath.Join(base.RepoStoreDirName, base.WorkspacesDirName)) {
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
	if _, err := execute(t, cmd, "refresh", "index", "--graph-binary", "entire"); err != nil {
		t.Fatalf("index: %v", err)
	}
	delete(runner.graphStoreFixtures, repoDir+"\x00full")
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
	if !strings.Contains(out, "gh/example/repo degraded") {
		t.Fatalf("refresh output = %q", out)
	}
	contextOut, err := execute(t, cmd, "workspace", "inspect", "context", "payments-platform", "ValidateToken", "--json")
	if err != nil {
		t.Fatalf("workspace context: %v", err)
	}
	if !strings.Contains(contextOut, `"state": "degraded"`) {
		t.Fatalf("context output missing freshness:\n%s", contextOut)
	}
}

func TestWorkspaceContextAndImpactReportLockedSemanticIndex(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
	if _, err := execute(t, cmd, "refresh", "index", "--graph-binary", "entire"); err != nil {
		t.Fatalf("index: %v", err)
	}
	if _, err := execute(t, cmd, "workspace", "create", "payments-platform"); err != nil {
		t.Fatalf("workspace create: %v", err)
	}
	if _, err := execute(t, cmd, "workspace", "add", "payments-platform", repoDir, "--name", "api"); err != nil {
		t.Fatalf("workspace add: %v", err)
	}
	brainDir := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo")
	unlock, err := acquireSemanticIndexLock(brainDir)
	if err != nil {
		t.Fatalf("acquire lock: %v", err)
	}
	defer unlock()
	shortSemanticLockTimeout(t)

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
	if _, err := execute(t, cmd, "refresh", "index", "--graph-binary", "entire"); err != nil {
		t.Fatalf("index: %v", err)
	}
	manifest := workspacelib.Manifest{
		SchemaVersion: workspacelib.SchemaVersion,
		Name:          "payments-platform",
		Repos:         []workspacelib.Repo{{RepoKey: "gh/example/repo", Name: "api", LocalPathHint: repoDir}},
	}
	if err := workspacelib.DefaultStore(env).WriteManifest(manifest); err != nil {
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
	data, err := os.ReadFile(filepath.Join(env.PluginDataDir, base.WorkspacesDirName, "payments-platform", workspacelib.ManifestName))
	if err != nil {
		t.Fatalf("read workspace: %v", err)
	}
	var manifest workspacelib.Manifest
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
	if _, err := execute(t, cmd, "refresh", "index", "--graph-binary", "entire"); err != nil {
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
	manifest, err := workspacelib.DefaultStore(env).LoadManifest("payments-platform")
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
	manifest, err := workspacelib.DefaultStore(env).LoadManifest("payments-platform")
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}
	if len(manifest.Repos) != 1 || manifest.Repos[0].Name != "api" {
		t.Fatalf("workspace repos were not preserved: %+v", manifest.Repos)
	}
}

func TestWorkspacePropagatesPluginDirErrors(t *testing.T) {
	env := EntireEnv{PluginDataDir: "relative-data"}
	if _, err := workspacelib.DefaultStore(env).LoadManifest("payments-platform"); err == nil || !strings.Contains(err.Error(), base.EnvPluginDataDir) {
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
	if _, err := os.Stat(filepath.Join(env.PluginDataDir, base.RepoStoreDirName, bmanifest.FileName)); !os.IsNotExist(err) {
		t.Fatalf("dot workspace wrote outside workspaces: %v", err)
	}
}

func TestWorkspaceRejectsUnsafeRepoKey(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	dir, err := workspacelib.DefaultStore(env).Dir("payments-platform")
	if err != nil {
		t.Fatalf("workspace dir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	data := []byte(`{"schema_version":1,"name":"payments-platform","repos":[{"repo_key":"../../outside","name":"bad"}]}` + "\n")
	if err := os.WriteFile(filepath.Join(dir, workspacelib.ManifestName), data, 0o600); err != nil {
		t.Fatalf("write workspace: %v", err)
	}
	if _, err := workspacelib.DefaultStore(env).LoadManifest("payments-platform"); err == nil || !strings.Contains(err.Error(), "repo_key") {
		t.Fatalf("load workspace err = %v", err)
	}
}

func TestWorkspaceRejectsSymlinkedWorkspacePath(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	workspacesRoot := filepath.Join(env.PluginDataDir, base.WorkspacesDirName)
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
	if _, err := os.Stat(filepath.Join(target, workspacelib.ManifestName)); !os.IsNotExist(err) {
		t.Fatalf("workspace manifest written through symlink: %v", err)
	}
}

func TestWorkspaceCreateIgnoresSymlinkedLegacyTree(t *testing.T) {
	// Workspaces live beside repos/, so a symlinked repos root no longer
	// blocks workspace create — but the legacy-migration path must refuse to
	// read through it, and nothing may be written behind the symlink.
	env := semanticTestEnv(t, t.TempDir())
	brainRoot := filepath.Join(env.PluginDataDir, base.RepoStoreDirName)
	external := t.TempDir()
	if err := os.MkdirAll(filepath.Join(external, base.WorkspacesDirName, "payments-platform"), 0o700); err != nil {
		t.Fatalf("seed external legacy tree: %v", err)
	}
	if err := os.Symlink(external, brainRoot); err != nil {
		t.Fatalf("symlink brain root: %v", err)
	}
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}, Now: time.Now})
	if _, err := execute(t, cmd, "workspace", "create", "payments-platform"); err != nil {
		t.Fatalf("workspace create: %v", err)
	}
	if _, err := os.Stat(filepath.Join(env.PluginDataDir, base.WorkspacesDirName, "payments-platform", workspacelib.ManifestName)); err != nil {
		t.Fatalf("workspace not created at its new home: %v", err)
	}
	// The symlinked legacy entry was neither migrated nor written through.
	if _, err := os.Stat(filepath.Join(external, base.WorkspacesDirName, "payments-platform", workspacelib.ManifestName)); !os.IsNotExist(err) {
		t.Fatalf("write leaked through symlinked legacy tree: %v", err)
	}
}

func TestWorkspaceLegacyDirMigratesOnTouch(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	// A pre-relocation workspace at repos/workspaces/<name> with a valid manifest.
	legacy := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, base.WorkspacesDirName, "payments-platform")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := workspacelib.Manifest{SchemaVersion: workspacelib.SchemaVersion, Name: "payments-platform", Repos: []workspacelib.Repo{{RepoKey: "gh/acme/api"}}}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, workspacelib.ManifestName), data, 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := workspacelib.DefaultStore(env).LoadManifest("payments-platform")
	if err != nil {
		t.Fatalf("legacy workspace must load (and migrate): %v", err)
	}
	if len(loaded.Repos) != 1 || loaded.Repos[0].RepoKey != "gh/acme/api" {
		t.Fatalf("migrated manifest content wrong: %+v", loaded)
	}
	if _, err := os.Stat(filepath.Join(env.PluginDataDir, base.WorkspacesDirName, "payments-platform", workspacelib.ManifestName)); err != nil {
		t.Fatalf("manifest not at the new home after migration: %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy dir should be gone after migration: %v", err)
	}
}

func TestBrainDirForKeyReservesWorkspacesSegment(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	if _, err := base.BrainDirForKey(env, "workspaces/acme/api"); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("workspaces/ must be reserved in the repo keyspace, got %v", err)
	}
	if _, err := base.BrainDirForKey(env, "gh/acme/api"); err != nil {
		t.Fatalf("ordinary key must resolve: %v", err)
	}
}

func TestWorkspaceGraphReportsPerRepoTrust(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC) }}
	cmd := &cobra.Command{Use: "index"}

	repoA := t.TempDir()
	keyA := testLocalRepoStorageKey(t, repoA)
	indexWorkspaceGraphRepo(t, cmd, opts, runner, repoA, keyA, "HandleA")

	d := runner.graphStoreFixtures[repoA+"\x00full"]
	d.Header.Stats = json.RawMessage(`{"completeness_level":"degraded"}`)
	d.Header.LanguageTiers = map[string]string{"Go": "semantic"}
	runner.graphStoreFixtures[repoA+"\x00full"] = d

	manifest := workspacelib.Manifest{
		SchemaVersion: workspacelib.SchemaVersion,
		Name:          "graph",
		Repos:         []workspacelib.Repo{{RepoKey: keyA, LocalPathHint: repoA}},
	}
	if err := workspacelib.DefaultStore(env).WriteManifest(manifest); err != nil {
		t.Fatalf("write workspace: %v", err)
	}

	var out bytes.Buffer
	graphCmd := &cobra.Command{Use: "graph"}
	graphCmd.SetOut(&out)
	if err := runWorkspaceGraph(graphCmd, opts, workspaceGraphOptions{limit: 10, json: true}, "graph"); err != nil {
		t.Fatalf("workspace graph: %v", err)
	}
	if !strings.Contains(out.String(), `"completeness_level": "degraded"`) || !strings.Contains(out.String(), `"trust": "partial"`) {
		t.Fatalf("workspace graph missing per-repo trust diagnostics:\n%s", out.String())
	}
	if !strings.Contains(out.String(), `"language_tiers"`) || !strings.Contains(out.String(), `"Go": "semantic"`) {
		t.Fatalf("workspace graph missing per-repo language_tiers:\n%s", out.String())
	}
}

func TestWorkspaceGraphReportsSharedExternalContracts(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC) }}
	cmd := &cobra.Command{Use: "index"}

	repoA := t.TempDir()
	keyA := testLocalRepoStorageKey(t, repoA)
	indexWorkspaceGraphRepo(t, cmd, opts, runner, repoA, keyA, "HandleSharedA")
	repoB := t.TempDir()
	keyB := testLocalRepoStorageKey(t, repoB)
	indexWorkspaceGraphRepo(t, cmd, opts, runner, repoB, keyB, "HandleSharedB")

	manifest := workspacelib.Manifest{
		SchemaVersion: workspacelib.SchemaVersion,
		Name:          "graph",
		Repos: []workspacelib.Repo{
			{RepoKey: keyA, LocalPathHint: repoA},
			{RepoKey: keyB, LocalPathHint: repoB},
		},
	}
	if err := workspacelib.DefaultStore(env).WriteManifest(manifest); err != nil {
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
	artifactPath := filepath.Join(env.PluginDataDir, base.WorkspacesDirName, "graph", workspaceGraphName)
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
	keyA := testLocalRepoStorageKey(t, repoA)
	repoB := t.TempDir()
	keyB := testLocalRepoStorageKey(t, repoB)
	indexWorkspaceGraphRepoWithSnapshot(t, cmd, opts, runner, repoA, keyA, workspaceGraphImportingSnapshot(keyA, "HandleAPI", keyB+"/pkg"))
	indexWorkspaceGraphRepoWithSnapshot(t, cmd, opts, runner, repoB, keyB, workspaceGraphLibrarySnapshot(keyB, "pkg/service.go", "Service"))

	manifest := workspacelib.Manifest{
		SchemaVersion: workspacelib.SchemaVersion,
		Name:          "imports",
		Repos: []workspacelib.Repo{
			{RepoKey: keyA, LocalPathHint: repoA},
			{RepoKey: keyB, LocalPathHint: repoB},
		},
	}
	if err := workspacelib.DefaultStore(env).WriteManifest(manifest); err != nil {
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

func TestWorkspaceGraphReportsCrossRepoRouteCalls(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return time.Date(2026, 6, 20, 14, 30, 0, 0, time.UTC) }}
	cmd := &cobra.Command{Use: "index"}

	repoA := t.TempDir()
	keyA := testLocalRepoStorageKey(t, repoA)
	repoB := t.TempDir()
	keyB := testLocalRepoStorageKey(t, repoB)
	indexWorkspaceGraphRepoWithSnapshot(t, cmd, opts, runner, repoA, keyA, workspaceGraphRouteCallerSnapshot(keyA, "CallShared", "/shared"))
	indexWorkspaceGraphRepoWithSnapshot(t, cmd, opts, runner, repoB, keyB, workspaceGraphRouteHandlerSnapshot(keyB, "HandleShared", "/shared"))

	manifest := workspacelib.Manifest{
		SchemaVersion: workspacelib.SchemaVersion,
		Name:          "routes",
		Repos: []workspacelib.Repo{
			{RepoKey: keyA, LocalPathHint: repoA},
			{RepoKey: keyB, LocalPathHint: repoB},
		},
	}
	if err := workspacelib.DefaultStore(env).WriteManifest(manifest); err != nil {
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

func TestWorkspaceGraphReportsExactExternalSymbolEdges(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return time.Date(2026, 6, 20, 14, 0, 0, 0, time.UTC) }}
	cmd := &cobra.Command{Use: "index"}

	repoA := t.TempDir()
	keyA := testLocalRepoStorageKey(t, repoA)
	repoB := t.TempDir()
	keyB := testLocalRepoStorageKey(t, repoB)
	indexWorkspaceGraphRepoWithSnapshot(t, cmd, opts, runner, repoA, keyA, workspaceGraphExternalSymbolSnapshot(keyA, "HandleAPI", "lib.Service"))
	indexWorkspaceGraphRepoWithSnapshot(t, cmd, opts, runner, repoB, keyB, workspaceGraphQualifiedSymbolSnapshot(keyB, "lib/service.go", "Service", "lib.Service"))

	manifest := workspacelib.Manifest{
		SchemaVersion: workspacelib.SchemaVersion,
		Name:          "symbols",
		Repos: []workspacelib.Repo{
			{RepoKey: keyA, LocalPathHint: repoA},
			{RepoKey: keyB, LocalPathHint: repoB},
		},
	}
	if err := workspacelib.DefaultStore(env).WriteManifest(manifest); err != nil {
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
	keyA := testLocalRepoStorageKey(t, repoA)
	repoB := t.TempDir()
	keyB := testLocalRepoStorageKey(t, repoB)
	externalSpec := keyB + "/pkg.Service"
	indexWorkspaceGraphRepoWithSnapshot(t, cmd, opts, runner, repoA, keyA, workspaceGraphExternalSymbolSnapshot(keyA, "HandleAPI", externalSpec))
	indexWorkspaceGraphRepoWithSnapshot(t, cmd, opts, runner, repoB, keyB, workspaceGraphQualifiedSymbolSnapshot(keyB, "pkg/service.go", "Service", "pkg.Service"))

	manifest := workspacelib.Manifest{
		SchemaVersion: workspacelib.SchemaVersion,
		Name:          "prefixed-symbols",
		Repos: []workspacelib.Repo{
			{RepoKey: keyA, LocalPathHint: repoA},
			{RepoKey: keyB, LocalPathHint: repoB},
		},
	}
	if err := workspacelib.DefaultStore(env).WriteManifest(manifest); err != nil {
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
		fakeCommandKey("git", "rev-parse", "--show-toplevel"):                                                  {stdout: repoDir + "\n"},
		fakeCommandKey("git", "rev-parse", "HEAD"):                                                             {stdout: "aaa111\n"},
		fakeCommandKey("git", "rev-parse", "HEAD^{tree}"):                                                      {stdout: "tree111\n"},
		fakeCommandKey("git", "branch", "--show-current"):                                                      {stdout: "main\n"},
		fakeCommandKey("git", "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"):                {stdout: "origin/main\n"},
		fakeCommandKey("git", "status", "--porcelain"):                                                         {stdout: ""},
		fakeCommandKey("entire", "graph", "doctor", "--json"):                                                  {stdout: `{"no_egress":true}`},
		fakeCommandKey("entire", "graph", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network"): {stdout: snapshot},
	}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index %s: %v", repoKey, err)
	}
}

func workspaceGraphSnapshot(repoKey, symbolName string) string {
	symbolID := repoKey + ":go:service.go:function:" + symbolName
	return `{"schema_version":"1.1","provider":"entire-graph","provider_version":"0.1.0","repo_key":"` + repoKey + `","commit":"aaa111","tree":"tree111","languages":["Go"],"capabilities":["ndjson"],"profile":"full","relation_set":["DEFINES","CONFIGURES","HANDLES_ROUTE"],"warnings":[],"partial_failures":[]}
{"record_type":"file","id":"` + repoKey + `:file:service.go","path":"service.go","blob":"abc","language":"Go","bytes":16}
{"record_type":"symbol","id":"` + symbolID + `","kind":"function","name":"` + symbolName + `","qualified_name":"service.` + symbolName + `","file_path":"service.go","start_line":1,"end_line":1,"signature":"func ` + symbolName + `()","language":"Go","stable_id_version":"1"}
{"record_type":"relation","from_id":"` + symbolID + `","to_id":"external:config:kubernetes/image/shared:latest","type":"CONFIGURES","confidence":0.82}
{"record_type":"relation","from_id":"` + symbolID + `","to_id":"external:route:/shared","type":"HANDLES_ROUTE","confidence":0.95}
{"record_type":"summary"}
`
}

func workspaceGraphImportingSnapshot(repoKey, symbolName, importSpec string) string {
	symbolID := repoKey + ":go:service.go:function:" + symbolName
	return `{"schema_version":"1.1","provider":"entire-graph","provider_version":"0.1.0","repo_key":"` + repoKey + `","commit":"aaa111","tree":"tree111","languages":["Go"],"capabilities":["ndjson"],"profile":"full","relation_set":["DEFINES","IMPORTS"],"warnings":[],"partial_failures":[]}
{"record_type":"file","id":"` + repoKey + `:file:service.go","path":"service.go","blob":"abc","language":"Go","bytes":16}
{"record_type":"symbol","id":"` + symbolID + `","kind":"function","name":"` + symbolName + `","qualified_name":"service.` + symbolName + `","file_path":"service.go","start_line":1,"end_line":1,"signature":"func ` + symbolName + `()","language":"Go","stable_id_version":"1"}
{"record_type":"relation","from_id":"` + symbolID + `","to_id":"external:import:` + importSpec + `","type":"IMPORTS","confidence":0.8}
{"record_type":"summary"}
`
}

func workspaceGraphRouteCallerSnapshot(repoKey, symbolName, route string) string {
	symbolID := repoKey + ":go:client.go:function:" + symbolName
	return `{"schema_version":"1.1","provider":"entire-graph","provider_version":"0.1.0","repo_key":"` + repoKey + `","commit":"aaa111","tree":"tree111","languages":["Go"],"capabilities":["ndjson"],"profile":"full","relation_set":["DEFINES","HTTP_CALLS"],"warnings":[],"partial_failures":[]}
{"record_type":"file","id":"` + repoKey + `:file:client.go","path":"client.go","blob":"abc","language":"Go","bytes":16}
{"record_type":"symbol","id":"` + symbolID + `","kind":"function","name":"` + symbolName + `","qualified_name":"service.` + symbolName + `","file_path":"client.go","start_line":1,"end_line":1,"signature":"func ` + symbolName + `()","language":"Go","stable_id_version":"1"}
{"record_type":"relation","from_id":"` + symbolID + `","to_id":"external:route:` + route + `","type":"HTTP_CALLS","confidence":0.82}
{"record_type":"summary"}
`
}

func workspaceGraphRouteHandlerSnapshot(repoKey, symbolName, route string) string {
	symbolID := repoKey + ":go:server.go:function:" + symbolName
	return `{"schema_version":"1.1","provider":"entire-graph","provider_version":"0.1.0","repo_key":"` + repoKey + `","commit":"aaa111","tree":"tree111","languages":["Go"],"capabilities":["ndjson"],"profile":"full","relation_set":["DEFINES","HANDLES_ROUTE"],"warnings":[],"partial_failures":[]}
{"record_type":"file","id":"` + repoKey + `:file:server.go","path":"server.go","blob":"abc","language":"Go","bytes":16}
{"record_type":"symbol","id":"` + symbolID + `","kind":"function","name":"` + symbolName + `","qualified_name":"service.` + symbolName + `","file_path":"server.go","start_line":1,"end_line":1,"signature":"func ` + symbolName + `()","language":"Go","stable_id_version":"1"}
{"record_type":"relation","from_id":"` + symbolID + `","to_id":"external:route:` + route + `","type":"HANDLES_ROUTE","confidence":0.95}
{"record_type":"summary"}
`
}

func workspaceGraphLibrarySnapshot(repoKey, path, symbolName string) string {
	symbolID := repoKey + ":go:" + path + ":function:" + symbolName
	return `{"schema_version":"1.1","provider":"entire-graph","provider_version":"0.1.0","repo_key":"` + repoKey + `","commit":"aaa111","tree":"tree111","languages":["Go"],"capabilities":["ndjson"],"profile":"full","relation_set":["DEFINES"],"warnings":[],"partial_failures":[]}
{"record_type":"file","id":"` + repoKey + `:file:` + path + `","path":"` + path + `","blob":"abc","language":"Go","bytes":16}
{"record_type":"symbol","id":"` + symbolID + `","kind":"function","name":"` + symbolName + `","qualified_name":"pkg.` + symbolName + `","file_path":"` + path + `","start_line":1,"end_line":1,"signature":"func ` + symbolName + `()","language":"Go","stable_id_version":"1"}
{"record_type":"summary"}
`
}

func workspaceGraphExternalSymbolSnapshot(repoKey, symbolName, externalQualifiedName string) string {
	symbolID := repoKey + ":go:service.go:function:" + symbolName
	return `{"schema_version":"1.1","provider":"entire-graph","provider_version":"0.1.0","repo_key":"` + repoKey + `","commit":"aaa111","tree":"tree111","languages":["Go"],"capabilities":["ndjson"],"profile":"full","relation_set":["CALLS"],"warnings":[],"partial_failures":[]}
{"record_type":"file","id":"` + repoKey + `:file:service.go","path":"service.go","blob":"abc","language":"Go","bytes":16}
{"record_type":"symbol","id":"` + symbolID + `","kind":"function","name":"` + symbolName + `","qualified_name":"service.` + symbolName + `","file_path":"service.go","start_line":1,"end_line":1,"signature":"func ` + symbolName + `()","language":"Go","stable_id_version":"1"}
{"record_type":"relation","from_id":"` + symbolID + `","to_id":"external:symbol:` + externalQualifiedName + `","type":"CALLS","confidence":0.82}
{"record_type":"summary"}
`
}

func workspaceGraphQualifiedSymbolSnapshot(repoKey, path, symbolName, qualifiedName string) string {
	symbolID := repoKey + ":go:" + path + ":function:" + symbolName
	return `{"schema_version":"1.1","provider":"entire-graph","provider_version":"0.1.0","repo_key":"` + repoKey + `","commit":"aaa111","tree":"tree111","languages":["Go"],"capabilities":["ndjson"],"profile":"full","relation_set":["DEFINES"],"warnings":[],"partial_failures":[]}
{"record_type":"file","id":"` + repoKey + `:file:` + path + `","path":"` + path + `","blob":"abc","language":"Go","bytes":16}
{"record_type":"symbol","id":"` + symbolID + `","kind":"function","name":"` + symbolName + `","qualified_name":"` + qualifiedName + `","file_path":"` + path + `","start_line":1,"end_line":1,"signature":"func ` + symbolName + `()","language":"Go","stable_id_version":"1"}
{"record_type":"summary"}
`
}

// writeWorkspaceBrainRepo builds a sessions-only brain at base.BrainDirForKey(env, key) plus a working
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
	key := testLocalRepoStorageKey(t, repoDir)
	writeWorkspaceBrainRepoAt(t, env, key, repoDir, sessionText, repoRel, fileBody)
	return repoDir, key
}

func writeWorkspaceBrainRepoAt(t *testing.T, env EntireEnv, key, repoDir, sessionText, repoRel, fileBody string) {
	t.Helper()
	brainDir := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, filepath.FromSlash(key))
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

	manifest := workspacelib.Manifest{
		SchemaVersion: workspacelib.SchemaVersion,
		Name:          "related",
		Repos: []workspacelib.Repo{
			{RepoKey: keyA, Name: "a", LocalPathHint: repoA},
			{RepoKey: keyB, Name: "b", LocalPathHint: repoB},
		},
	}
	if err := workspacelib.DefaultStore(env).WriteManifest(manifest); err != nil {
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
	if payload.SchemaVersion != brainreview.ReportSchemaVersion || payload.Mode != "diff-less (brain memory vs current tree)" {
		t.Fatalf("review envelope schema_version/mode wrong: v=%d mode=%q", payload.SchemaVersion, payload.Mode)
	}
	if !strings.Contains(payload.Summary, "1 suspected regression(s) across 1/2 repo(s)") {
		t.Fatalf("summary = %q", payload.Summary)
	}
	byKey := map[string]workspaceReviewResult{}
	for _, r := range payload.Results {
		byKey[r.RepoKey] = r
	}
	a, okA := byKey[keyA]
	b, okB := byKey[keyB]
	if !okA || !okB {
		t.Fatalf("missing workspace results: %v", byKey)
	}
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
	manifest := workspacelib.Manifest{
		SchemaVersion: workspacelib.SchemaVersion,
		Name:          "related",
		Repos:         []workspacelib.Repo{{RepoKey: keyA, Name: "a", LocalPathHint: repoA}},
	}
	if err := workspacelib.DefaultStore(env).WriteManifest(manifest); err != nil {
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
	missingKey := testLocalRepoStorageKey(t, missingTree)

	manifest := workspacelib.Manifest{
		SchemaVersion: workspacelib.SchemaVersion,
		Name:          "related",
		Repos: []workspacelib.Repo{
			{RepoKey: keyA, Name: "a", LocalPathHint: repoA},
			{RepoKey: missingKey, Name: "missing", LocalPathHint: missingTree},
		},
	}
	if err := workspacelib.DefaultStore(env).WriteManifest(manifest); err != nil {
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
	m, ok := byKey[missingKey]
	if !ok {
		t.Fatalf("missing-brain repo omitted from results: %v", byKey)
	}
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
	if err := workspacelib.DefaultStore(env).WriteManifest(workspacelib.Manifest{
		SchemaVersion: workspacelib.SchemaVersion,
		Name:          "related",
		Repos:         []workspacelib.Repo{{RepoKey: keyA, Name: "a", LocalPathHint: repoA}},
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

func TestWorkspaceRegressionsSkipsUnsafeRepo(t *testing.T) {
	// A stale local_path_hint whose repo_key no longer matches the registered key means the brain
	// (by key) and the tree (by hint) can't be trusted to match — scanning would compare one repo's
	// memory against an unrelated tree and manufacture bogus regressions. The scan must be skipped.
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
	if _, err := execute(t, cmd, "refresh", "index", "--graph-binary", "entire"); err != nil {
		t.Fatalf("index: %v", err)
	}
	// Seed a session asserting the invariant + a regressed working tree, so a scan WOULD fire.
	brainDir := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo")
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
	manifest := workspacelib.Manifest{
		SchemaVersion: workspacelib.SchemaVersion,
		Name:          "related",
		Repos:         []workspacelib.Repo{{RepoKey: "gh/example/repo", Name: "api", LocalPathHint: repoDir}},
	}
	if err := workspacelib.DefaultStore(env).WriteManifest(manifest); err != nil {
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

	brainDir := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo")
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
	manifest := workspacelib.Manifest{
		SchemaVersion: workspacelib.SchemaVersion,
		Name:          "related",
		Repos:         []workspacelib.Repo{{RepoKey: "gh/example/repo", Name: "api", LocalPathHint: repoDir}},
	}
	if err := workspacelib.DefaultStore(env).WriteManifest(manifest); err != nil {
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

	brainDir := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, "gh", "example", "repo")
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
	if err := workspacelib.DefaultStore(env).WriteManifest(workspacelib.Manifest{
		SchemaVersion: workspacelib.SchemaVersion,
		Name:          "related",
		Repos:         []workspacelib.Repo{{RepoKey: "gh/example/repo", Name: "api", LocalPathHint: repoDir}},
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
	if workspaceFreshnessWarning(workspacelib.RepoFreshness{State: "ok"}) != "" {
		t.Error("ok freshness should not warn")
	}
	// A scanned repo with a non-ideal state gets a loud warning. "unsafe" here means a stale/broken
	// index over a still-valid pairing (a genuine pairing block sets PairingUnsafe and never reaches
	// this warning path), so it warns "scanned raw sessions only" rather than being silent.
	for _, st := range []string{"degraded", "missing-brain", "missing-semantic", "unknown", "unsafe"} {
		if workspaceFreshnessWarning(workspacelib.RepoFreshness{State: st}) == "" {
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
	manifest := workspacelib.Manifest{
		SchemaVersion: workspacelib.SchemaVersion,
		Name:          "related",
		Repos: []workspacelib.Repo{
			{RepoKey: keyA, Name: "a", LocalPathHint: repoA},
			{RepoKey: keyB, Name: "b", LocalPathHint: repoA},
		},
	}
	if err := workspacelib.DefaultStore(env).WriteManifest(manifest); err != nil {
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
	reloaded, err := workspacelib.DefaultStore(env).LoadManifest("related")
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
	if _, err := workspacelib.DefaultStore(env).LoadManifest("related"); err == nil {
		t.Fatalf("workspace should be gone")
	}
}

func TestWorkspaceRefreshReportsMissingBrains(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	manifest := workspacelib.Manifest{
		SchemaVersion: workspacelib.SchemaVersion,
		Name:          "payments-platform",
		Repos:         []workspacelib.Repo{{RepoKey: "gh/example/missing", Name: "missing"}},
	}
	if err := workspacelib.DefaultStore(env).WriteManifest(manifest); err != nil {
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
	brainDir := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Join(brainDir, docstore.DirName), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(docIndex{Records: records})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brainDir, filepath.FromSlash(docstore.IndexPath)), data, 0o600); err != nil {
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

	manifest := workspacelib.Manifest{
		SchemaVersion: workspacelib.SchemaVersion,
		Name:          "related",
		Repos:         []workspacelib.Repo{{RepoKey: keyA, Name: "a"}, {RepoKey: keyB, Name: "b"}},
	}
	if err := workspacelib.DefaultStore(env).WriteManifest(manifest); err != nil {
		t.Fatalf("write workspace: %v", err)
	}

	searchOut, err := execute(t, cmd, "workspace", "search", "related", "checkout", "--json")
	if err != nil {
		t.Fatalf("workspace search: %v", err)
	}
	if !strings.Contains(searchOut, `"id": "`+keyA+`/doc:guide"`) {
		t.Fatalf("search output missing doc hit from repo A:\n%s", searchOut)
	}
	// Results stay grouped per repo: repo B has no "checkout" hit, but its group
	// (with an empty results array) is still present.
	if !strings.Contains(searchOut, `"repo_key": "`+keyB+`"`) {
		t.Fatalf("search output missing repo B group:\n%s", searchOut)
	}
	if strings.Contains(searchOut, `"id": "`+keyB+`/doc:other"`) {
		t.Fatalf("search output leaked repo B's unrelated doc:\n%s", searchOut)
	}

	for _, args := range [][]string{
		{"workspace", "query", "related", "--keyword", "--query", "checkout", "--json"},
		{"workspace", "query", "--query", "checkout", "related", "--keyword", "--json"},
		{"workspace", "query", "--keyword", "related", "checkout", "--json"},
	} {
		root := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
		out, err := execute(t, root, args...)
		if err != nil {
			t.Fatal(err)
		}
		if out != searchOut {
			t.Fatalf("workspace query keyword differs from search: %s / %s", out, searchOut)
		}
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
	if !strings.Contains(queryOut, `"id": "`+keyA+`/doc:guide"`) {
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

func TestWorkspaceSearchAndGetIncludesPersistedGraphEdges(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	opts := Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now}
	cmd := NewRootCommand(opts)

	_, keyA := writeLocalWorkspaceBrainRepo(t, env, `{"text":"x"}`, "client.ts", "export function fetchViewer() {}\n")
	_, keyB := writeLocalWorkspaceBrainRepo(t, env, `{"text":"x"}`, "schema.graphql", "type Query { viewer: User }\n")
	manifest := workspacelib.Manifest{
		SchemaVersion: workspacelib.SchemaVersion,
		Name:          "related",
		Repos:         []workspacelib.Repo{{RepoKey: keyA, Name: "client"}, {RepoKey: keyB, Name: "api"}},
	}
	if err := workspacelib.DefaultStore(env).WriteManifest(manifest); err != nil {
		t.Fatalf("write workspace: %v", err)
	}
	edge := workspacelib.CrossEdge{
		Endpoint:     "external:graphql:query viewer",
		Type:         "graphql",
		FromRepo:     keyA,
		ToRepo:       keyB,
		FromSymbol:   workspacelib.SymbolRef{RepoKey: keyA, ID: "symbol:client.fetchViewer", Kind: "function", Name: "fetchViewer", QualifiedName: "client.fetchViewer", FilePath: "client.ts"},
		ToSymbol:     workspacelib.SymbolRef{RepoKey: keyB, ID: "symbol:Query.viewer", Kind: "field", Name: "viewer", QualifiedName: "Query.viewer", FilePath: "schema.graphql"},
		SharedCount:  1,
		RelationKind: "cross_repo_graphql_call",
	}
	if _, err := writeWorkspaceGraphPayload(env, workspaceGraphPayload{
		Generation:  workspaceGraphTestGeneration(t, env, manifest),
		Workspace:   manifest.Name,
		GeneratedAt: time.Now(),
		CrossEdges:  []workspacelib.CrossEdge{edge},
	}); err != nil {
		t.Fatalf("write workspace graph: %v", err)
	}

	searchOut, err := execute(t, cmd, "workspace", "search", "related", "viewer graphql", "--json")
	if err != nil {
		t.Fatalf("workspace search: %v", err)
	}
	graphID := workspacelib.GraphCrossEdgeID(edge)
	for _, want := range []string{`"source": "workspace_graph"`, `"id": "` + graphID + `"`, "cross_repo_graphql_call", "external:graphql:query viewer"} {
		if !strings.Contains(searchOut, want) {
			t.Fatalf("search output missing %q:\n%s", want, searchOut)
		}
	}

	queryOut, err := execute(t, cmd, "workspace", "query", "related", "viewer graphql", "--json")
	if err != nil {
		t.Fatalf("workspace query: %v", err)
	}
	if !strings.Contains(queryOut, `"id": "`+graphID+`"`) {
		t.Fatalf("query output missing workspace graph hit:\n%s", queryOut)
	}

	textCmd := NewRootCommand(opts)
	textOut, err := execute(t, textCmd, "workspace", "search", "related", "viewer graphql")
	if err != nil {
		t.Fatalf("workspace search text: %v", err)
	}
	if !strings.Contains(textOut, "[workspace_graph] "+graphID) {
		t.Fatalf("text output missing pasteable graph id %s:\n%s", graphID, textOut)
	}

	getOut, err := execute(t, cmd, "workspace", "get", "related", graphID, "--json")
	if err != nil {
		t.Fatalf("workspace get: %v", err)
	}
	for _, want := range []string{`"source": "workspace_graph"`, "client.fetchViewer", "Query.viewer", "schema.graphql"} {
		if !strings.Contains(getOut, want) {
			t.Fatalf("get output missing %q:\n%s", want, getOut)
		}
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
		{in: "gh/owner/repo/review:abc", repoKey: "gh/owner/repo", id: "review:abc"},
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
	manifest := workspacelib.Manifest{
		SchemaVersion: workspacelib.SchemaVersion,
		Name:          "related",
		Repos: []workspacelib.Repo{
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

// TestWorkspaceConversationRecallFanOut is Phase 5's conversation increment:
// the conversation source fans out across member brains with per-repo
// grouping, the historical-evidence contract, filter support, namespace
// isolation, and repo-qualified get expansion (the explicit cross-repo step).
func TestWorkspaceConversationRecallFanOut(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: func() time.Time { return now }})

	_, keyA := writeLocalWorkspaceBrainRepo(t, env, `{"text":"x"}`, "a.go", "package x\n")
	_, keyB := writeLocalWorkspaceBrainRepo(t, env, `{"text":"x"}`, "b.go", "package x\n")

	// Member A gets a captured conversation about a deploy rollback; member B
	// one about billing. Each member brain indexes its own sessions.
	writeMemberConversation := func(key, sessionID, request, response string) {
		t.Helper()
		brainDir := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, filepath.FromSlash(key))
		rel := "sessions/main/20260808T100000Z_" + sessionID + ".jsonl"
		body := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"` + request + `"}]}}` + "\n" +
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"` + response + `"}]}}` + "\n"
		full := filepath.Join(brainDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		manifest, err := bmanifest.Load(brainDir)
		if err != nil {
			t.Fatal(err)
		}
		if manifest.Sources == nil {
			manifest.Sources = &brainSources{}
		}
		if manifest.Sources.Sessions == nil {
			manifest.Sources.Sessions = &sessionSourceManifest{GeneratedAt: now, DefaultBranch: "main"}
		}
		manifest.RepoKey = key
		manifest.Sources.Sessions.Sessions = append(manifest.Sources.Sessions.Sessions, exportSession{
			SessionID: sessionID, Branch: "main", Agent: "Claude Code", LatestCheckpoint: "cp",
			TranscriptPath: rel, CreatedAt: now.Add(-time.Hour),
		})
		if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
			t.Fatal(err)
		}
		if _, err := writeBrainHistoryIndexAndSource(brainDir, now, nil); err != nil {
			t.Fatal(err)
		}
	}
	writeMemberConversation(keyA, "sess-a", "why did the deploy rollback loop", "Decision: the rollback loop came from a stale health probe; pinned the probe version.")
	writeMemberConversation(keyB, "sess-b", "reconcile the billing ledger", "Decision: ledger drift came from double-posted credits.")

	manifest := workspacelib.Manifest{
		SchemaVersion: workspacelib.SchemaVersion,
		Name:          "convspace",
		Repos:         []workspacelib.Repo{{RepoKey: keyA, Name: "a"}, {RepoKey: keyB, Name: "b"}},
	}
	if err := workspacelib.DefaultStore(env).WriteManifest(manifest); err != nil {
		t.Fatal(err)
	}

	searchOut, err := execute(t, cmd, "workspace", "search", "convspace", "deploy rollback loop", "--source", "conversation", "--json")
	if err != nil {
		t.Fatalf("workspace conversation search: %v", err)
	}
	if !strings.Contains(searchOut, "stale health probe") {
		t.Fatalf("missing member A conversation hit:\n%s", searchOut)
	}
	if strings.Contains(searchOut, "billing ledger") {
		t.Fatalf("member B's unrelated conversation leaked:\n%s", searchOut)
	}
	if !strings.Contains(searchOut, `"repo_key": "`+keyA+`"`) || !strings.Contains(searchOut, `"repo_key": "`+keyB+`"`) {
		t.Fatalf("results not grouped by repo identity:\n%s", searchOut)
	}
	if !strings.Contains(searchOut, `"historical_conversation"`) || !strings.Contains(searchOut, `"verification_required": true`) {
		t.Fatalf("historical-evidence contract missing in workspace fan-out:\n%s", searchOut)
	}
	// Source isolation per member: no fact/doc/history record in a
	// conversation-scoped fan-out.
	if strings.Contains(searchOut, `"source": "doc"`) || strings.Contains(searchOut, `"source": "history"`) {
		t.Fatalf("non-conversation source leaked:\n%s", searchOut)
	}

	// Filters flow through the shared contract; a wrong-source filter is one
	// structured error before the fan-out. Fresh root command: cobra flag
	// values stick across executions.
	badCmd := NewRootCommand(Options{Version: "t", Env: env, Runner: runner, Now: func() time.Time { return now }})
	if _, err := execute(t, badCmd, "workspace", "search", "convspace", "deploy", "--agent", "Claude Code"); err == nil {
		t.Fatal("agent filter without conversation source must error")
	}
	filtered, err := execute(t, NewRootCommand(Options{Version: "t", Env: env, Runner: runner, Now: func() time.Time { return now }}),
		"workspace", "search", "convspace", "deploy rollback loop", "--source", "conversation", "--agent", "Codex", "--json")
	if err != nil {
		t.Fatalf("filtered search: %v", err)
	}
	if strings.Contains(filtered, "stale health probe") {
		t.Fatalf("agent filter did not exclude Claude Code sessions:\n%s", filtered)
	}

	// Explicit cross-repo expansion: extract the conversation id and get it
	// repo-qualified; an unqualified id stays rejected (tested in the base
	// workspace test).
	var payload struct {
		Results []struct {
			RepoKey string `json:"repo_key"`
			Results []struct {
				ID string `json:"id"`
			} `json:"results"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(searchOut), &payload); err != nil {
		t.Fatal(err)
	}
	var convID string
	for _, group := range payload.Results {
		if group.RepoKey == keyA && len(group.Results) > 0 {
			convID = group.Results[0].ID
		}
	}
	if !strings.HasPrefix(convID, keyA+"/conversation:") {
		t.Fatalf("no conversation id in repo A group: %+v", payload.Results)
	}
	getOut, err := execute(t, cmd, "workspace", "get", "convspace", convID, "--json")
	if err != nil {
		t.Fatalf("workspace get conversation: %v", err)
	}
	if !strings.Contains(getOut, "pinned the probe version") {
		t.Fatalf("qualified conversation get missing expansion:\n%s", getOut)
	}
	if !strings.Contains(getOut, `"historical_conversation"`) {
		t.Fatalf("expansion missing caveat:\n%s", getOut)
	}
}

// Workspace source-isolation fixture: a workspace holding both graph matches and
// conversation matches for the same query must keep source=conversation
// results conversation-only, and the branch filter must reach every member
// brain in the fan-out.
func TestWorkspaceConversationSourceIsolationAndBranchPropagation(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	opts := Options{Version: "test-version", Env: env, Runner: runner, Now: func() time.Time { return now }}

	_, keyA := writeLocalWorkspaceBrainRepo(t, env, `{"text":"x"}`, "a.go", "package x\n")

	writeMemberConversation := func(key, sessionID, request, response string) {
		t.Helper()
		brainDir := filepath.Join(env.PluginDataDir, base.RepoStoreDirName, filepath.FromSlash(key))
		rel := "sessions/main/20260808T100000Z_" + sessionID + ".jsonl"
		body := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"` + request + `"}]}}` + "\n" +
			`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"` + response + `"}]}}` + "\n"
		full := filepath.Join(brainDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		manifest, err := bmanifest.Load(brainDir)
		if err != nil {
			t.Fatal(err)
		}
		if manifest.Sources == nil {
			manifest.Sources = &brainSources{}
		}
		if manifest.Sources.Sessions == nil {
			manifest.Sources.Sessions = &sessionSourceManifest{GeneratedAt: now, DefaultBranch: "main"}
		}
		manifest.RepoKey = key
		manifest.Sources.Sessions.Sessions = append(manifest.Sources.Sessions.Sessions, exportSession{
			SessionID: sessionID, Branch: "main", Agent: "Claude Code", LatestCheckpoint: "cp",
			TranscriptPath: rel, CreatedAt: now.Add(-time.Hour),
		})
		if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
			t.Fatal(err)
		}
		if _, err := writeBrainHistoryIndexAndSource(brainDir, now, nil); err != nil {
			t.Fatal(err)
		}
	}
	writeMemberConversation(keyA, "sess-a", "why did the deploy rollback loop", "Decision: the rollback loop came from a stale health probe; pinned the probe version.")

	manifest := workspacelib.Manifest{
		SchemaVersion: workspacelib.SchemaVersion,
		Name:          "isospace",
		Repos:         []workspacelib.Repo{{RepoKey: keyA, Name: "a"}},
	}
	if err := workspacelib.DefaultStore(env).WriteManifest(manifest); err != nil {
		t.Fatal(err)
	}
	// A persisted graph edge whose text matches the same query terms.
	edge := workspacelib.CrossEdge{
		Endpoint:     "external:deploy:rollback",
		Type:         "http",
		FromRepo:     keyA,
		ToRepo:       keyA,
		FromSymbol:   workspacelib.SymbolRef{RepoKey: keyA, ID: "symbol:deployRollback", Kind: "function", Name: "deployRollback", QualifiedName: "ops.deployRollback", FilePath: "a.go"},
		ToSymbol:     workspacelib.SymbolRef{RepoKey: keyA, ID: "symbol:rollbackLoop", Kind: "function", Name: "rollbackLoop", QualifiedName: "ops.rollbackLoop", FilePath: "a.go"},
		SharedCount:  1,
		RelationKind: "cross_repo_http_call",
	}
	if _, err := writeWorkspaceGraphPayload(env, workspaceGraphPayload{
		Generation:  workspaceGraphTestGeneration(t, env, manifest),
		Workspace:   manifest.Name,
		GeneratedAt: now,
		CrossEdges:  []workspacelib.CrossEdge{edge},
	}); err != nil {
		t.Fatal(err)
	}

	// Source isolation: conversation-scoped search must not append the
	// workspace-graph group even though the edge matches the query.
	convOut, err := execute(t, NewRootCommand(opts), "workspace", "search", "isospace", "deploy rollback loop", "--source", "conversation", "--json")
	if err != nil {
		t.Fatalf("conversation search: %v", err)
	}
	if !strings.Contains(convOut, "stale health probe") {
		t.Fatalf("missing conversation hit:\n%s", convOut)
	}
	if strings.Contains(convOut, `"source": "workspace_graph"`) {
		t.Fatalf("workspace graph leaked into source=conversation results:\n%s", convOut)
	}

	// Any explicit single source keeps the same isolation.
	histOut, err := execute(t, NewRootCommand(opts), "workspace", "search", "isospace", "deploy rollback loop", "--source", "history", "--json")
	if err != nil {
		t.Fatalf("history search: %v", err)
	}
	if strings.Contains(histOut, `"source": "workspace_graph"`) {
		t.Fatalf("workspace graph leaked into source=history results:\n%s", histOut)
	}

	// Default source keeps the graph group (existing behavior).
	allOut, err := execute(t, NewRootCommand(opts), "workspace", "search", "isospace", "deploy rollback loop", "--json")
	if err != nil {
		t.Fatalf("default search: %v", err)
	}
	if !strings.Contains(allOut, `"source": "workspace_graph"`) {
		t.Fatalf("default-source search lost the workspace graph group:\n%s", allOut)
	}

	// Branch propagation: the captured branch is main, so a release-branch
	// filter must exclude the exchange in every member, and a main filter must
	// keep it.
	missOut, err := execute(t, NewRootCommand(opts), "workspace", "search", "isospace", "deploy rollback loop", "--source", "conversation", "--branch", "release", "--json")
	if err != nil {
		t.Fatalf("branch-filtered search: %v", err)
	}
	if strings.Contains(missOut, "stale health probe") {
		t.Fatalf("branch filter did not reach the member fan-out:\n%s", missOut)
	}
	hitOut, err := execute(t, NewRootCommand(opts), "workspace", "search", "isospace", "deploy rollback loop", "--source", "conversation", "--branch", "main", "--json")
	if err != nil {
		t.Fatalf("branch-matched search: %v", err)
	}
	if !strings.Contains(hitOut, "stale health probe") {
		t.Fatalf("matching branch filter dropped the exchange:\n%s", hitOut)
	}
}

// workspaceManifestRepos reads back what `workspace add` actually registered,
// which is the half of the bug the exit code hid: the refusal has to happen
// before the member row exists, not after.
func workspaceManifestRepos(t *testing.T, env EntireEnv, workspace string) []workspacelib.Repo {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(env.PluginDataDir, base.WorkspacesDirName, workspace, workspacelib.ManifestName))
	if err != nil {
		t.Fatalf("read workspace: %v", err)
	}
	var manifest workspacelib.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse workspace: %v", err)
	}
	return manifest.Repos
}

// TestWorkspaceAddRefusesAPathThatIsNotARepository pins the check `workspace
// add` never made. It tested for EXISTENCE, and a regular file exists, so
// `workspace add ws /etc/passwd` rewrote the file to its parent directory,
// hashed /etc into a local/ key, printed "added" and exited 0 -- registering a
// member that can never grow a brain.
func TestWorkspaceAddRefusesAPathThatIsNotARepository(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	// An empty response map makes every git call fail, which is what
	// `rev-parse --show-toplevel` does outside a repository.
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
	if _, err := execute(t, cmd, "workspace", "create", "payments-platform"); err != nil {
		t.Fatalf("workspace create: %v", err)
	}

	plainDir := t.TempDir()
	plainFile := filepath.Join(plainDir, "passwd")
	if err := os.WriteFile(plainFile, []byte("root:x:0:0:\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	for _, target := range []string{plainDir, plainFile} {
		_, err := execute(t, cmd, "workspace", "add", "payments-platform", target)
		if err == nil {
			t.Fatalf("workspace add accepted a non-repository: %s", target)
		}
		if !strings.Contains(err.Error(), "not a git repository") {
			t.Fatalf("the refusal does not name the condition for %s: %v", target, err)
		}
		// A file names the directory that was actually checked, so the parent
		// rewrite is visible rather than silent.
		if !strings.Contains(err.Error(), plainDir) {
			t.Fatalf("the refusal does not name the path it checked for %s: %v", target, err)
		}
		if !strings.Contains(err.Error(), "git init") {
			t.Fatalf("the refusal does not name the fix for %s: %v", target, err)
		}
	}
	if repos := workspaceManifestRepos(t, env, "payments-platform"); len(repos) != 0 {
		t.Fatalf("a refused path was registered anyway: %+v", repos)
	}
}

// TestWorkspaceAddStillResolvesAFileInsideARepository keeps the half of the
// parent-directory rewrite that was always legitimate: naming a file in a repo
// is a reasonable way to name the repo, and git resolves it to the work-tree
// root exactly as it resolves a subdirectory. Only the case where the rewrite
// landed OUTSIDE a repository was ever wrong.
func TestWorkspaceAddStillResolvesAFileInsideARepository(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
	if _, err := execute(t, cmd, "workspace", "create", "payments-platform"); err != nil {
		t.Fatalf("workspace create: %v", err)
	}
	readme := filepath.Join(repoDir, "README.md")
	if err := os.WriteFile(readme, []byte("# api\n"), 0o600); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	if _, err := execute(t, cmd, "workspace", "add", "payments-platform", readme); err != nil {
		t.Fatalf("workspace add: %v", err)
	}
	repos := workspaceManifestRepos(t, env, "payments-platform")
	if len(repos) != 1 || repos[0].RepoKey != "gh/example/repo" || repos[0].LocalPathHint != repoDir {
		t.Fatalf("a file inside a repo did not register the repo: %+v", repos)
	}
}

// TestWorkspaceAddValidatesTheMemberDisplayName closes the asymmetry inside one
// command: the workspace name positional was allowlisted, while the --name flag
// on the same invocation was stored raw, so a 5000-character value and a
// `../`-laden one both round-tripped into workspace.json and from there,
// unescaped, into the workspace README.
func TestWorkspaceAddValidatesTheMemberDisplayName(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})
	if _, err := execute(t, cmd, "workspace", "create", "payments-platform"); err != nil {
		t.Fatalf("workspace create: %v", err)
	}
	for _, name := range []string{
		strings.Repeat("A", 5000),
		"../../../etc/passwd",
		"..",
		".",
		"api server",
		"api/server",
	} {
		_, err := execute(t, cmd, "workspace", "add", "payments-platform", repoDir, "--name", name)
		if err == nil {
			t.Fatalf("workspace add accepted --name %q", base.TruncateString(name, 40))
		}
		if !strings.Contains(err.Error(), "workspace repo name must") {
			t.Fatalf("the refusal for --name %q does not say what a name may be: %v", base.TruncateString(name, 40), err)
		}
		// The refusal must not echo a 5000-character value back at the reader.
		if len(err.Error()) > 200 {
			t.Fatalf("the refusal echoed the whole oversized name back: %d bytes", len(err.Error()))
		}
	}
	if repos := workspaceManifestRepos(t, env, "payments-platform"); len(repos) != 0 {
		t.Fatalf("a refused --name was registered anyway: %+v", repos)
	}
	// The names people actually use keep working, including the dots, dashes
	// and underscores the workspace name allows.
	for _, name := range []string{"api", "api-gateway", "api_v2.1"} {
		if _, err := execute(t, cmd, "workspace", "add", "payments-platform", repoDir, "--name", name); err != nil {
			t.Fatalf("workspace add rejected --name %q: %v", name, err)
		}
	}
}

// TestWorkspaceWatchStampsEveryLineWithClockAndRepo is the fan-out half of
// COR-2176. The single-repo daemon could be read by assuming every line was
// about the one repo; a workspace log cannot. The loop, not the tick, decides
// attribution, so this drives the real loop and asserts what a reader of
// watch.log would see.
func TestWorkspaceWatchStampsEveryLineWithClockAndRepo(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	repoA, repoAKey := writeLocalWorkspaceBrainRepo(t, env, `{"text":"x"}`, "a.go", "package x\n")
	repoB, repoBKey := writeLocalWorkspaceBrainRepo(t, env, `{"text":"x"}`, "b.go", "package x\n")
	manifest := workspacelib.Manifest{
		SchemaVersion: workspacelib.SchemaVersion,
		Name:          "ws",
		Repos: []workspacelib.Repo{
			{RepoKey: repoAKey, LocalPathHint: repoA},
			{RepoKey: repoBKey, LocalPathHint: repoB},
		},
	}
	if err := workspacelib.DefaultStore(env).WriteManifest(manifest); err != nil {
		t.Fatalf("write workspace: %v", err)
	}

	at := time.Date(2026, 10, 24, 9, 30, 15, 0, time.UTC)
	// The member's writer is the loop's decision; the tick only writes to it.
	repoTick := func(out io.Writer, repoDir string, agentCalls *int) {
		fmt.Fprintf(out, "[watch] refresh failed: %v\n", errors.New("brain_locked"))
	}
	w := watch.DefaultFlags()
	w.Once = true
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return at }}
	out := &bytes.Buffer{}
	agentCalls := 0
	if err := workspaceWatchLoop(context.Background(), out, opts, w, "ws", &agentCalls, func() time.Time { return at }, repoTick); err != nil {
		t.Fatalf("workspaceWatchLoop: %v", err)
	}

	stamp := "2026-10-24T09:30:15Z pid="
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, stamp) {
			t.Fatalf("line %d carries no clock or process: %q\nfull log:\n%s", i, line, out.String())
		}
		if !strings.Contains(line, " tick=1 ") {
			t.Fatalf("line %d does not say which sweep it belongs to: %q", i, line)
		}
	}
	// Each member's tick line must name THAT member, not the one above it.
	for _, key := range []string{repoAKey, repoBKey} {
		want := " tick=1 repo=" + key + " [watch] refresh failed: brain_locked"
		if !slicesContainsSuffix(lines, want) {
			t.Fatalf("no tick line attributed to %s; want a line ending %q\nfull log:\n%s", key, want, out.String())
		}
	}
}

func slicesContainsSuffix(lines []string, want string) bool {
	for _, l := range lines {
		if strings.HasSuffix(l, want) {
			return true
		}
	}
	return false
}
