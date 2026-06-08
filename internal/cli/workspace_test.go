package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorkspaceWatchFansOverMembersWithSharedBudget(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	repoA := writeWorkspaceBrainRepo(t, env, "gh/example/repoa", `{"text":"x"}`, "a.go", "package x\n")
	repoB := writeWorkspaceBrainRepo(t, env, "gh/example/repob", `{"text":"x"}`, "b.go", "package x\n")
	manifest := workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "ws",
		Repos: []workspaceRepo{
			{RepoKey: "gh/example/repoa", LocalPathHint: repoA},
			{RepoKey: "gh/example/repob", LocalPathHint: repoB},
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
	if !strings.Contains(o, "gh/example/repoa") || !strings.Contains(o, "gh/example/repob") {
		t.Fatalf("both resolvable repo keys should be reported:\n%s", o)
	}
	if !strings.Contains(o, "gh/example/repoc") || !strings.Contains(o, "skipped") {
		t.Fatalf("unresolvable member must be reported as skipped:\n%s", o)
	}
}

func TestWorkspaceCreateAddRefreshAndQuery(t *testing.T) {
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
	if !strings.Contains(out, filepath.Join(repoStoreDirName, workspaceDirName, "payments-platform")) {
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
	if !strings.Contains(queryOut, `"contract_state": "ok"`) {
		t.Fatalf("query output missing contract freshness:\n%s", queryOut)
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
	data, err := os.ReadFile(filepath.Join(env.PluginDataDir, repoStoreDirName, workspaceDirName, "payments-platform", workspaceManifestName))
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
	workspacesRoot := filepath.Join(env.PluginDataDir, repoStoreDirName, workspaceDirName)
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
	brainRoot := filepath.Join(env.PluginDataDir, repoStoreDirName)
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

// writeWorkspaceBrainRepo builds a sessions-only brain at brainDirForKey(env, key) plus a working
// tree, so the cross-repo regression/review fan-out can be exercised without a semantic index.
func writeWorkspaceBrainRepo(t *testing.T, env EntireEnv, key, sessionText, repoRel, fileBody string) string {
	t.Helper()
	brainDir := filepath.Join(env.PluginDataDir, repoStoreDirName, filepath.FromSlash(key))
	sessDir := filepath.Join(brainDir, "sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessDir, "s.jsonl"), []byte(sessionText+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repoDir := t.TempDir()
	full := filepath.Join(repoDir, filepath.FromSlash(repoRel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(fileBody), 0o644); err != nil {
		t.Fatal(err)
	}
	return repoDir
}

func TestWorkspaceReviewFlagsRegressedRepoOnly(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	cmd := NewRootCommand(Options{Version: "test-version", Env: env, Runner: runner, Now: time.Now})

	session := `{"text":"in pkg/review_context.go the scope diff uses scopeBaseRef+\"..HEAD\" for the range"}`
	regressed := "package x\nfunc f() string {\n\treturn \"master..HEAD\"\n}\n"
	clean := "package x\nfunc f() string {\n\treturn scopeBaseRef + \"..HEAD\"\n}\n"
	repoA := writeWorkspaceBrainRepo(t, env, "gh/example/repoa", session, "pkg/review_context.go", regressed)
	repoB := writeWorkspaceBrainRepo(t, env, "gh/example/repob", session, "pkg/review_context.go", clean)

	manifest := workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "related",
		Repos: []workspaceRepo{
			{RepoKey: "gh/example/repoa", Name: "a", LocalPathHint: repoA},
			{RepoKey: "gh/example/repob", Name: "b", LocalPathHint: repoB},
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
	a, b := byKey["gh/example/repoa"], byKey["gh/example/repob"]
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
	repoA := writeWorkspaceBrainRepo(t, env, "gh/example/repoa", session, "pkg/review_context.go", regressed)
	manifest := workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "related",
		Repos:         []workspaceRepo{{RepoKey: "gh/example/repoa", Name: "a", LocalPathHint: repoA}},
	}
	if err := writeWorkspaceManifest(env, manifest); err != nil {
		t.Fatalf("write workspace: %v", err)
	}

	// Text mode (no --json): must render the summary, the per-repo freshness label, and the finding.
	out, err := execute(t, cmd, "workspace", "review", "related", "fix scopeBaseRef base scope")
	if err != nil {
		t.Fatalf("workspace review (text): %v", err)
	}
	for _, want := range []string{"Cross-repo diff-less review:", "gh/example/repoa", "[", "Suspected regression", "review_context.go"} {
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
	repoA := writeWorkspaceBrainRepo(t, env, "gh/example/repoa", session, "pkg/review_context.go", regressed)
	// A repo registered with a real working tree but NO brain prepped (missing-brain).
	missingTree := t.TempDir()

	manifest := workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "related",
		Repos: []workspaceRepo{
			{RepoKey: "gh/example/repoa", Name: "a", LocalPathHint: repoA},
			{RepoKey: "gh/example/missing", Name: "missing", LocalPathHint: missingTree},
		},
	}
	if err := writeWorkspaceManifest(env, manifest); err != nil {
		t.Fatalf("write workspace: %v", err)
	}

	out, err := execute(t, cmd, "workspace", "regressions", "related", "fix scopeBaseRef base scope", "--json", "--location-only")
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
	a := byKey["gh/example/repoa"]
	if len(a.Anomalies) != 1 || a.Anomalies[0].Kind != "changed" {
		t.Fatalf("repoa anomalies wrong: %+v", a)
	}
	if a.Anomalies[0].Expected != "" || a.Anomalies[0].Current != "" {
		t.Fatalf("--location-only must blank expected/current: %+v", a.Anomalies[0])
	}
	// The missing-brain repo must not abort the run; it just yields no anomalies.
	m := byKey["gh/example/missing"]
	if len(m.Anomalies) != 0 {
		t.Fatalf("missing-brain repo should yield no anomalies: %+v", m)
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

	out, err := execute(t, cmd, "workspace", "regressions", "related", "fix scopeBaseRef base scope", "--json")
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
	textOut, err := execute(t, textCmd, "workspace", "regressions", "related", "fix scopeBaseRef base scope")
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

	repoA := writeWorkspaceBrainRepo(t, env, "gh/example/repoa", `{"text":"x"}`, "a.go", "package x\n")
	manifest := workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "related",
		Repos: []workspaceRepo{
			{RepoKey: "gh/example/repoa", Name: "a", LocalPathHint: repoA},
			{RepoKey: "gh/example/repob", Name: "b", LocalPathHint: repoA},
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

	if _, err := execute(t, cmd, "workspace", "remove", "related", "gh/example/repob"); err != nil {
		t.Fatalf("workspace remove repo: %v", err)
	}
	reloaded, err := loadWorkspaceManifest(env, "related")
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}
	if len(reloaded.Repos) != 1 || reloaded.Repos[0].RepoKey != "gh/example/repoa" {
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
