package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// workspaceCommandFixture deliberately uses the same on-disk member corpus and
// workspace manifest paths as the root command.  The test providers are native
// local binaries (fakeCodexProvider), and HOME/XDG are isolated so no command
// can discover or write a user's provider or skill configuration.
func workspaceCommandFixture(t *testing.T) (Options, EntireEnv, workspaceManifest) {
	t.Helper()
	isolateSkillFormHome(t)
	env, manifest := twoRepoWorkspace(t,
		"gh/acme/a", "release:build", []string{"mise build", "mise deploy"},
		"gh/acme/b", "release:ship", []string{"make build", "make release"})
	if err := writeWorkspaceManifest(env, manifest); err != nil {
		t.Fatal(err)
	}
	return Options{
		Version: "test", Env: env,
		Runner: &fakeCommandRunner{responses: map[string]fakeCommandResponse{}},
		Now:    func() time.Time { return time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC) },
	}, env, manifest
}

func workspaceSkillInstallPath(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(home, ".agents", "skills", "deploy-release", "SKILL.md")
}

func assertWorkspaceSkillNotWritten(t *testing.T, wsDir string) {
	t.Helper()
	for _, path := range []string{workspaceSkillInstallPath(t), filepath.Join(wsDir, filepath.FromSlash(patternsSkillMemoryPath))} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("workspace form wrote %s: %v", path, err)
		}
	}
}

func TestWorkspaceCommandLifecycleThroughRootRoutes(t *testing.T) {
	opts, _, manifest := workspaceCommandFixture(t)

	// Refresh is the live legacy-view bridge consumed by the subsequent list and
	// status routes.  Exercise text output first, then JSON filtering and limits.
	out, err := execute(t, NewRootCommand(opts), "workspace", "patterns", "refresh", manifest.Name)
	if err != nil || !strings.Contains(out, "cross-repo V2 pattern") {
		t.Fatalf("refresh: %v\n%s", err, out)
	}
	out, err = execute(t, NewRootCommand(opts), "workspace", "patterns", manifest.Name, "--scope", "workspace", "--type", "task", "--limit", "1", "--json")
	if err != nil {
		t.Fatalf("filtered list: %v\n%s", err, out)
	}
	var views []patternView
	if err := json.Unmarshal([]byte(out), &views); err != nil || len(views) != 1 || views[0].Scope != "workspace" || views[0].Type != "task" {
		t.Fatalf("filtered list = %#v, decode=%v\n%s", views, err, out)
	}
	out, err = execute(t, NewRootCommand(opts), "workspace", "patterns", "status", manifest.Name)
	if err != nil || !strings.Contains(out, "member repos: 2 (0 with patterns)") || !strings.Contains(out, "cross-repo V2 patterns: 2") {
		t.Fatalf("status: %v\n%s", err, out)
	}
	// Until an explicit verifier run, the skills list must not expose raw task
	// candidates as formable workspace skills.
	out, err = execute(t, NewRootCommand(opts), "workspace", "patterns", "skills", manifest.Name)
	if err != nil || !strings.Contains(out, "no accepted cross-repo families") {
		t.Fatalf("unverified skills list: %v\n%s", err, out)
	}

	// Verification is egress-gated in production.  A portable, native fake
	// provider gives the root command a deterministic accepted family instead.
	family := `{"families":[{"title":"release the build","trigger":"shipping a release","purpose":"build then publish","common_workflow":["build","publish"],"per_repo":[{"repo_key":"gh/acme/a","commands":["mise build","mise deploy"]},{"repo_key":"gh/acme/b","commands":["make build","make release"]}],"verdict":"accepted"}]}`
	fakeCodexProvider(t, family)
	out, err = execute(t, NewRootCommand(opts), "workspace", "patterns", "verify", manifest.Name, "--agent", "codex", "--json")
	if err != nil || !strings.Contains(out, `"Verified": 1`) {
		t.Fatalf("verify: %v\n%s", err, out)
	}
	// A second root-route verification has unchanged evidence and must take the
	// cached path. Replacing the provider with invalid output proves the command
	// did not execute it while evidence is unchanged.
	fakeCodexProvider(t, "not JSON")
	out, err = execute(t, NewRootCommand(opts), "workspace", "patterns", "verify", manifest.Name, "--agent", "codex", "--json")
	if err != nil || !strings.Contains(out, `"Cached": 1`) {
		t.Fatalf("cached verify: %v\n%s", err, out)
	}
	out, err = execute(t, NewRootCommand(opts), "workspace", "patterns", "skills", manifest.Name, "--limit", "1")
	if err != nil || !strings.Contains(out, "[family] release the build") || !strings.Contains(out, "gh/acme/a, gh/acme/b") {
		t.Fatalf("skills text list: %v\n%s", err, out)
	}

	out, err = execute(t, NewRootCommand(opts), "workspace", "patterns", "skills", manifest.Name, "--limit", "1", "--json")
	if err != nil {
		t.Fatalf("skills list: %v\n%s", err, out)
	}
	var families []workspaceFamily
	if err := json.Unmarshal([]byte(out), &families); err != nil || len(families) != 1 || families[0].repoCount() != 2 {
		t.Fatalf("skills list = %#v, decode=%v\n%s", families, err, out)
	}

	fakeCodexProvider(t, regressionSkill)
	out, err = execute(t, NewRootCommand(opts), "workspace", "patterns", "skills", "form", manifest.Name, families[0].ID, "--agent", "codex", "--json")
	if err != nil || !strings.Contains(out, `"would_write"`) || !strings.Contains(out, `"name": "deploy-release"`) {
		t.Fatalf("preview form: %v\n%s", err, out)
	}
	wsDir, err := workspaceDir(opts.Env, manifest.Name)
	if err != nil {
		t.Fatal(err)
	}
	assertWorkspaceSkillNotWritten(t, wsDir)
	// draft-only is also read-only, while --yes reaches the actual global
	// workspace destination and records the workspace decision.
	out, err = execute(t, NewRootCommand(opts), "workspace", "patterns", "skills", "form", manifest.Name, families[0].ID, "--agent", "codex", "--draft-only")
	if err != nil || !strings.Contains(out, "# Deploy release") {
		t.Fatalf("draft-only form: %v\n%s", err, out)
	}
	assertWorkspaceSkillNotWritten(t, wsDir)
	out, err = execute(t, NewRootCommand(opts), "workspace", "patterns", "skills", "form", manifest.Name, families[0].ID, "--agent", "codex", "--yes")
	if err != nil || !strings.Contains(out, "formed skill") || !strings.Contains(out, "wrote ~/.agents/skills/deploy-release/SKILL.md") {
		t.Fatalf("install form: %v\n%s", err, out)
	}
	installed, err := os.ReadFile(workspaceSkillInstallPath(t))
	if err != nil || !strings.Contains(string(installed), "# Deploy release") {
		t.Fatalf("workspace install content missing: %q, err=%v", installed, err)
	}
	if _, err := os.Stat(filepath.Join(wsDir, filepath.FromSlash(patternsSkillMemoryPath))); err != nil {
		t.Fatalf("workspace skill memory missing: %v", err)
	}
}

func TestWorkspaceCommandEmptyAndNoEgressRoutes(t *testing.T) {
	opts, _, manifest := workspaceCommandFixture(t)
	wsDir, err := workspaceDir(opts.Env, manifest.Name)
	if err != nil {
		t.Fatal(err)
	}
	// No-egress refusal happens before any provider lookup or execution.
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
	fakeCodexProvider(t, "not JSON")
	out, err := execute(t, NewRootCommand(opts), "workspace", "patterns", "verify", manifest.Name, "--agent", "codex")
	if err == nil || !strings.Contains(err.Error(), "egress") {
		t.Fatalf("no-egress refusal: %v\n%s", err, out)
	}
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "")
	if err := os.Remove(filepath.Join(wsDir, filepath.FromSlash(patternCorpusPath))); err != nil {
		t.Fatal(err)
	}
	// These root routes are read-only before a corpus has been built.
	out, err = execute(t, NewRootCommand(opts), "workspace", "patterns", manifest.Name)
	if err != nil || !strings.Contains(out, "no cross-repo corpus") {
		t.Fatalf("empty list text: %v\n%s", err, out)
	}
	out, err = execute(t, NewRootCommand(opts), "workspace", "patterns", manifest.Name, "--json")
	if err != nil || !strings.Contains(out, "no cross-repo corpus") {
		t.Fatalf("empty list json route: %v\n%s", err, out)
	}
	out, err = execute(t, NewRootCommand(opts), "workspace", "patterns", "status", manifest.Name)
	if err != nil || strings.Contains(out, "cross-repo V2 patterns:") {
		t.Fatalf("empty status: %v\n%s", err, out)
	}
	out, err = execute(t, NewRootCommand(opts), "workspace", "patterns", "skills", manifest.Name, "--json")
	if err != nil || strings.TrimSpace(out) != "null" {
		t.Fatalf("empty skills json: %v\n%s", err, out)
	}
	out, err = execute(t, NewRootCommand(opts), "workspace", "patterns", "skills", "unknown", "--json")
	if err != nil || strings.TrimSpace(out) != "null" {
		t.Fatalf("missing workspace skills: %v\n%s", err, out)
	}
}

func TestWorkspaceCommandListEmptyFilterAndMalformedSkillStore(t *testing.T) {
	opts, _, manifest := workspaceCommandFixture(t)
	if out, err := execute(t, NewRootCommand(opts), "workspace", "patterns", "refresh", manifest.Name); err != nil {
		t.Fatalf("refresh: %v\n%s", err, out)
	}
	out, err := execute(t, NewRootCommand(opts), "workspace", "patterns", manifest.Name, "--type", "risk")
	if err != nil || !strings.Contains(out, "no cross-repo patterns") {
		t.Fatalf("empty filter: %v\n%s", err, out)
	}
	wsDir, err := workspaceDir(opts.Env, manifest.Name)
	if err != nil {
		t.Fatal(err)
	}
	corpus := filepath.Join(wsDir, filepath.FromSlash(patternCorpusPath))
	if err := os.WriteFile(corpus, []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = execute(t, NewRootCommand(opts), "workspace", "patterns", "skills", manifest.Name)
	if err == nil || !strings.Contains(err.Error(), memoryErrStateCorrupt) {
		t.Fatalf("malformed skills store: %v\n%s", err, out)
	}
	out, err = execute(t, NewRootCommand(opts), "workspace", "patterns", "status", manifest.Name)
	if err == nil || !strings.Contains(err.Error(), memoryErrStateCorrupt) {
		t.Fatalf("malformed status store: %v\n%s", err, out)
	}
}

func TestWorkspaceCommandRejectedFamilyStaysUnavailable(t *testing.T) {
	opts, _, manifest := workspaceCommandFixture(t)
	fakeCodexProvider(t, `{"families":[{"title":"generic git","trigger":"commit","purpose":"push","common_workflow":["git push"],"per_repo":[{"repo_key":"gh/acme/a","commands":["git push"]},{"repo_key":"gh/acme/b","commands":["git push"]}],"verdict":"rejected"}]}`)
	out, err := execute(t, NewRootCommand(opts), "workspace", "patterns", "verify", manifest.Name, "--agent", "codex")
	if err != nil || !strings.Contains(out, "0 accepted") || !strings.Contains(out, "0 cached") {
		t.Fatalf("rejected verify: %v\n%s", err, out)
	}
	out, err = execute(t, NewRootCommand(opts), "workspace", "patterns", "skills", manifest.Name, "--limit", "0", "--json")
	if err != nil || strings.TrimSpace(out) != "null" {
		t.Fatalf("rejected skills list: %v\n%s", err, out)
	}
}

func TestWorkspaceCommandVerifyWithOneContributingMemberNeedsNoProvider(t *testing.T) {
	opts, env, manifest := workspaceCommandFixture(t)
	manifest.Repos = manifest.Repos[:1]
	if err := writeWorkspaceManifest(env, manifest); err != nil {
		t.Fatal(err)
	}
	fakeCodexProvider(t, "not JSON")
	out, err := execute(t, NewRootCommand(opts), "workspace", "patterns", "verify", manifest.Name, "--agent", "codex", "--json")
	if err != nil || !strings.Contains(out, `"Considered": 0`) || !strings.Contains(out, `"Verified": 0`) {
		t.Fatalf("one-member verify: %v\n%s", err, out)
	}
}

func TestWorkspaceCommandVerifierAndFormAgentAbsence(t *testing.T) {
	opts, _, manifest := workspaceCommandFixture(t)
	out, err := execute(t, NewRootCommand(opts), "workspace", "patterns", "verify", manifest.Name, "--agent", "none")
	if err == nil || !strings.Contains(err.Error(), "workspace family judgment requires an agent") {
		t.Fatalf("verify agent none: %v\n%s", err, out)
	}
	// The fake command runner has no discoverable provider, so auto resolves to
	// none through the same root path instead of reaching the user's PATH.
	out, err = execute(t, NewRootCommand(opts), "workspace", "patterns", "skills", "form", manifest.Name, "family:missing", "--agent", "auto")
	if err == nil || !strings.Contains(err.Error(), "skill synthesis requires an agent") {
		t.Fatalf("form agent auto: %v\n%s", err, out)
	}
}

func TestWorkspaceCommandVerifierProviderFailureAndAutoResolution(t *testing.T) {
	t.Run("invalid provider response reaches the root error", func(t *testing.T) {
		opts, _, manifest := workspaceCommandFixture(t)
		fakeCodexProvider(t, "not JSON")
		out, err := execute(t, NewRootCommand(opts), "workspace", "patterns", "verify", manifest.Name, "--agent", "codex")
		if err == nil || !strings.Contains(err.Error(), "parse workspace families") {
			t.Fatalf("invalid verifier response: %v\n%s", err, out)
		}
	})
	t.Run("auto selects the isolated native provider", func(t *testing.T) {
		opts, _, manifest := workspaceCommandFixture(t)
		runner := opts.Runner.(*fakeCommandRunner)
		runner.responses[fakeCommandKey("codex", "--version")] = fakeCommandResponse{stdout: "codex test\n"}
		family := `{"families":[{"title":"release","trigger":"ship","purpose":"publish","common_workflow":["build"],"per_repo":[{"repo_key":"gh/acme/a","commands":["mise deploy"]},{"repo_key":"gh/acme/b","commands":["make release"]}],"verdict":"accepted"}]}`
		fakeCodexProvider(t, family)
		out, err := execute(t, NewRootCommand(opts), "workspace", "patterns", "verify", manifest.Name, "--agent", "auto")
		if err != nil || !strings.Contains(out, "1 accepted") {
			t.Fatalf("auto verifier: %v\n%s", err, out)
		}
	})
}

// Every public workspace-pattern route must propagate a malformed manifest
// instead of treating it as an empty workspace. This table intentionally drives
// Cobra/root routing; it does not call the handlers directly.
func TestWorkspaceCommandMalformedManifestErrorsAtEveryRoute(t *testing.T) {
	opts, _, _ := workspaceCommandFixture(t)
	const name = "broken"
	dir, err := workspaceDir(opts.Env, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, workspaceManifestName), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"workspace", "patterns", "refresh", name},
		{"workspace", "patterns", name},
		{"workspace", "patterns", "status", name},
		{"workspace", "patterns", "verify", name, "--agent", "none"},
		{"workspace", "patterns", "skills", name},
		{"workspace", "patterns", "skills", "form", name, "family:any", "--agent", "none"},
	} {
		out, err := execute(t, NewRootCommand(opts), args...)
		if err == nil || !strings.Contains(err.Error(), "invalid character") {
			t.Fatalf("%s: err=%v\n%s", strings.Join(args, " "), err, out)
		}
	}
}

func TestWorkspaceCommandMalformedMemberPrivacyFailsClosed(t *testing.T) {
	opts, env, manifest := workspaceCommandFixture(t)
	memberDir, err := brainDirForKey(env, manifest.Repos[0].RepoKey)
	if err != nil {
		t.Fatal(err)
	}
	tombstones := filepath.Join(memberDir, filepath.FromSlash(sessionTombstonesPath))
	if err := os.MkdirAll(filepath.Dir(tombstones), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tombstones, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"workspace", "patterns", "refresh", manifest.Name},
		{"workspace", "patterns", manifest.Name},
		{"workspace", "patterns", "status", manifest.Name},
		{"workspace", "patterns", "verify", manifest.Name, "--agent", "none"},
		{"workspace", "patterns", "skills", manifest.Name},
		{"workspace", "patterns", "skills", "form", manifest.Name, "family:any", "--agent", "none"},
	} {
		out, err := execute(t, NewRootCommand(opts), args...)
		if err == nil || (!strings.Contains(err.Error(), memoryErrStateCorrupt) && !strings.Contains(err.Error(), memoryErrStateUnsafe)) {
			t.Fatalf("%s: err=%v\n%s", strings.Join(args, " "), err, out)
		}
	}
}

func TestWorkspaceCommandOutputFailuresPropagate(t *testing.T) {
	run := func(t *testing.T, opts Options, args ...string) error {
		t.Helper()
		cmd := NewRootCommand(opts)
		cmd.SetOut(failingVitalityWriter{})
		cmd.SetErr(failingVitalityWriter{})
		cmd.SetArgs(args)
		return cmd.Execute()
	}
	t.Run("refresh list and status", func(t *testing.T) {
		opts, _, manifest := workspaceCommandFixture(t)
		for _, args := range [][]string{
			{"workspace", "patterns", "refresh", manifest.Name},
			{"workspace", "patterns", manifest.Name, "--json"},
			{"workspace", "patterns", "status", manifest.Name},
		} {
			if err := run(t, opts, args...); err == nil || !strings.Contains(err.Error(), errVitalityTestOutput.Error()) {
				t.Fatalf("%s error = %v", strings.Join(args, " "), err)
			}
		}
	})
	t.Run("verify and skills list", func(t *testing.T) {
		opts, _, manifest := workspaceCommandFixture(t)
		family := `{"families":[{"title":"release","trigger":"ship","purpose":"publish","common_workflow":["build"],"per_repo":[{"repo_key":"gh/acme/a","commands":["mise deploy"]},{"repo_key":"gh/acme/b","commands":["make release"]}],"verdict":"accepted"}]}`
		fakeCodexProvider(t, family)
		if err := run(t, opts, "workspace", "patterns", "verify", manifest.Name, "--agent", "codex", "--json"); err == nil || !strings.Contains(err.Error(), errVitalityTestOutput.Error()) {
			t.Fatalf("verify error = %v", err)
		}
		if err := run(t, opts, "workspace", "patterns", "skills", manifest.Name, "--json"); err == nil || !strings.Contains(err.Error(), errVitalityTestOutput.Error()) {
			t.Fatalf("skills error = %v", err)
		}
	})
}

func TestWorkspaceCommandUnsafePublicationArtifactFailsClosed(t *testing.T) {
	opts, _, manifest := workspaceCommandFixture(t)
	wsDir, err := workspaceDir(opts.Env, manifest.Name)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(wsDir, filepath.FromSlash(patternCorpusPublicationMarkerPath))
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"workspace", "patterns", "refresh", manifest.Name},
		{"workspace", "patterns", manifest.Name},
		{"workspace", "patterns", "status", manifest.Name},
		{"workspace", "patterns", "verify", manifest.Name, "--agent", "none"},
		{"workspace", "patterns", "skills", manifest.Name},
		{"workspace", "patterns", "skills", "form", manifest.Name, "family:any", "--agent", "none"},
	} {
		out, err := execute(t, NewRootCommand(opts), args...)
		if err == nil || (!strings.Contains(err.Error(), memoryErrStateCorrupt) && !strings.Contains(err.Error(), memoryErrStateUnsafe)) {
			t.Fatalf("%s: err=%v\n%s", strings.Join(args, " "), err, out)
		}
	}
}

func TestWorkspaceCommandListScopeAndTextLimitContracts(t *testing.T) {
	opts, _, manifest := workspaceCommandFixture(t)
	out, err := execute(t, NewRootCommand(opts), "workspace", "patterns", manifest.Name, "--scope", "repo")
	if err != nil || !strings.Contains(out, "no cross-repo patterns") {
		t.Fatalf("scope mismatch: %v\n%s", err, out)
	}
	out, err = execute(t, NewRootCommand(opts), "workspace", "patterns", manifest.Name, "--limit", "1")
	if err != nil || strings.Count(out, "[HIGH]") != 1 {
		t.Fatalf("text limit: %v\n%s", err, out)
	}
}

func TestWorkspaceSkillsListLimitTruncatesAcceptedFamilies(t *testing.T) {
	opts, _, manifest := workspaceCommandFixture(t)
	fakeCodexProvider(t, `{"families":[{"title":"release build","trigger":"ship","purpose":"publish","common_workflow":["build"],"per_repo":[{"repo_key":"gh/acme/a","commands":["mise deploy"]},{"repo_key":"gh/acme/b","commands":["make release"]}],"verdict":"accepted"},{"title":"release notes","trigger":"ship","purpose":"document","common_workflow":["notes"],"per_repo":[{"repo_key":"gh/acme/a","commands":["mise notes"]},{"repo_key":"gh/acme/b","commands":["make notes"]}],"verdict":"accepted"}]}`)
	if out, err := execute(t, NewRootCommand(opts), "workspace", "patterns", "verify", manifest.Name, "--agent", "codex"); err != nil {
		t.Fatalf("verify families: %v\n%s", err, out)
	}
	out, err := execute(t, NewRootCommand(opts), "workspace", "patterns", "skills", manifest.Name, "--limit", "1", "--json")
	if err != nil {
		t.Fatalf("limited skills: %v\n%s", err, out)
	}
	var families []workspaceFamily
	if err := json.Unmarshal([]byte(out), &families); err != nil || len(families) != 1 {
		t.Fatalf("limited families=%v decode=%v\n%s", families, err, out)
	}
}

func TestWorkspaceCommandHandlesMissingAndMalformedMembers(t *testing.T) {
	t.Run("missing member is reported while healthy members refresh", func(t *testing.T) {
		opts, env, manifest := workspaceCommandFixture(t)
		manifest.Repos = append(manifest.Repos, workspaceRepo{RepoKey: "gh/acme/missing"})
		if err := writeWorkspaceManifest(env, manifest); err != nil {
			t.Fatal(err)
		}
		out, stderr, err := executeSplit(t, NewRootCommand(opts), "workspace", "patterns", "refresh", manifest.Name)
		if err != nil || !strings.Contains(out, "from 2/3 member corpora") || !strings.Contains(stderr, "gh/acme/missing: no corpus") {
			t.Fatalf("partial refresh: %v\nstdout=%s\nstderr=%s", err, out, stderr)
		}
		out, stderr, err = executeSplit(t, NewRootCommand(opts), "workspace", "patterns", "status", manifest.Name)
		if err != nil || !strings.Contains(out, "member repos: 3 (0 with patterns)") || !strings.Contains(stderr, "gh/acme/missing") {
			t.Fatalf("partial status: %v\nstdout=%s\nstderr=%s", err, out, stderr)
		}
	})

	t.Run("malformed member corpus fails closed", func(t *testing.T) {
		opts, env, manifest := workspaceCommandFixture(t)
		badKey := "gh/acme/bad"
		badDir, err := brainDirForKey(env, badKey)
		if err != nil {
			t.Fatal(err)
		}
		badCorpus := filepath.Join(badDir, filepath.FromSlash(patternCorpusPath))
		if err := os.MkdirAll(filepath.Dir(badCorpus), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(badCorpus, []byte("not sqlite"), 0o600); err != nil {
			t.Fatal(err)
		}
		manifest.Repos = append(manifest.Repos, workspaceRepo{RepoKey: badKey})
		if err := writeWorkspaceManifest(env, manifest); err != nil {
			t.Fatal(err)
		}
		out, err := execute(t, NewRootCommand(opts), "workspace", "patterns", "refresh", manifest.Name)
		if err == nil || !strings.Contains(err.Error(), memoryErrStateCorrupt) {
			t.Fatalf("corrupt refresh err=%v\n%s", err, out)
		}
	})
}

func TestWorkspaceSkillsFormRejectsWithoutWrites(t *testing.T) {
	opts, _, manifest := workspaceCommandFixture(t)
	wsDir, err := workspaceDir(opts.Env, manifest.Name)
	if err != nil {
		t.Fatal(err)
	}
	fakeCodexProvider(t, regressionSkill)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"workspace", "patterns", "skills", "form", manifest.Name, "family:missing", "--agent", "none", "--yes"}, "skill synthesis requires an agent"},
		{[]string{"workspace", "patterns", "skills", "form", manifest.Name, "family:missing", "--agent", "codex", "--yes"}, "no accepted workspace family"},
	} {
		out, err := execute(t, NewRootCommand(opts), tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err=%v\n%s", strings.Join(tc.args, " "), err, out)
		}
		assertWorkspaceSkillNotWritten(t, wsDir)
	}
}
