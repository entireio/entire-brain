package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/entireio/entire-brain/internal/cli/command/watch"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
)

// install_ux_test.go covers two defects a first-time user hits, both reported
// from the field and both reproduced in source.
//
// A. Entire's own installer dirties the tree that Entire then refuses:
//
//	$ entire graph setup          # writes AGENTS.md, CLAUDE.md, .claude/settings.json
//	$ entire brain setup
//	seed:     dirty_worktree: refusing to seed uncommitted content without --worktree
//	semantic: dirty_worktree: refusing to index uncommitted content without --worktree
//
// The reporter's words: "the brain install is really picky about there not
// being ANY changes in any files in git. even the previous config file and
// agent file updates from the graph extension install. it's not the easiest to
// figure that out, could be me". It was not them.
//
// And when it did refuse, the message named a flag and nothing else — not which
// files were in the way, and not that committing would also work.
//
// B. Fact distillation silently routed repository content to Codex, even for a
// user sitting inside Claude Code, and never said which agent it had chosen:
// "the brain facts import defaults to Codex, idk what model."

// --- Fix A -------------------------------------------------------------------

// TestWorktreeGateIgnoresEntireAuthoredConfig is the reported bug at the gate
// itself: a tree dirty with NOTHING but files Entire's own installers wrote is
// not a tree with uncommitted source in it.
func TestWorktreeGateIgnoresEntireAuthoredConfig(t *testing.T) {
	t.Parallel()
	repoDir := t.TempDir()
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "status", "--porcelain", "--untracked-files=all"): {stdout: strings.Join([]string{
			"?? AGENTS.md",
			" M CLAUDE.md",
			"?? .claude/settings.json",
			"?? .brainignore",
			"?? .entire/settings.json",
		}, "\n") + "\n"},
	}}
	dirty, err := worktreeDirty((&cobra.Command{}).Context(), runner, repoDir)
	if err != nil {
		t.Fatalf("worktreeDirty: %v", err)
	}
	if dirty {
		t.Fatalf("a tree dirty only with Entire-authored config was gated as dirty; this is the reported install failure")
	}
}

// TestWorktreeGateIgnoresCodexInstallFiles is the same bug for a Codex user.
// `entire enable --agent codex` writes .codex/hooks.json (the cli's
// cmd/entire/cli/agent/codex/hook_root.go), so a Codex install dirties the tree
// exactly as a Claude Code one does. Shipping the fix with only the Claude Code
// paths exempt would have left precisely those users broken — in a change whose
// other half exists to stop disadvantaging them.
func TestWorktreeGateIgnoresCodexInstallFiles(t *testing.T) {
	t.Parallel()
	repoDir := t.TempDir()
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "status", "--porcelain", "--untracked-files=all"): {stdout: strings.Join([]string{
			"?? AGENTS.md",
			" M CLAUDE.md",
			"?? .codex/hooks.json",
			"?? .codex/config.toml",
			"?? .codex/skills/review.md",
			"?? .claude/skills/review.md",
		}, "\n") + "\n"},
	}}
	dirty, err := worktreeDirty((&cobra.Command{}).Context(), runner, repoDir)
	if err != nil {
		t.Fatalf("worktreeDirty: %v", err)
	}
	if dirty {
		t.Fatalf("a Codex-shaped install was gated as dirty; the same bug, for the other agent")
	}
}

// TestWorktreeGateStillRefusesOneDirtySourceFile is the guard that stops the
// fix from degenerating into "ignore everything". One uncommitted source file
// alongside the config still has to stop a HEAD-attributed index, and the
// refusal has to name THAT file and not the config.
func TestWorktreeGateStillRefusesOneDirtySourceFile(t *testing.T) {
	t.Parallel()
	repoDir := t.TempDir()
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "status", "--porcelain", "--untracked-files=all"): {stdout: strings.Join([]string{
			"?? AGENTS.md",
			" M CLAUDE.md",
			" M internal/auth/token.go",
		}, "\n") + "\n"},
	}}
	ignore, err := worktreeGateIgnore(repoDir)
	if err != nil {
		t.Fatalf("worktreeGateIgnore: %v", err)
	}
	dirty, paths, err := worktreeDirtyDetail((&cobra.Command{}).Context(), runner, repoDir, ignore)
	if err != nil {
		t.Fatalf("worktreeDirtyDetail: %v", err)
	}
	if !dirty {
		t.Fatalf("one uncommitted source file was gated as clean; the fix became 'ignore everything'")
	}
	if len(paths) != 1 || paths[0] != "internal/auth/token.go" {
		t.Fatalf("dirty paths = %q, want exactly [internal/auth/token.go]", paths)
	}
}

// TestWorktreeGateKeepsUserBrainIgnore pins that the built-ins are ADDED to a
// user's .brainignore, never substituted for it. A gate that replaced the
// user's patterns would quietly start refusing paths they had excluded.
func TestWorktreeGateKeepsUserBrainIgnore(t *testing.T) {
	t.Parallel()
	repoDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoDir, ".brainignore"), []byte("scratch/\n"), 0o600); err != nil {
		t.Fatalf("write .brainignore: %v", err)
	}
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "status", "--porcelain", "--untracked-files=all"): {stdout: "?? scratch/notes.md\n?? AGENTS.md\n"},
	}}
	ctx := (&cobra.Command{}).Context()
	dirty, err := worktreeDirty(ctx, runner, repoDir)
	if err != nil {
		t.Fatalf("worktreeDirty: %v", err)
	}
	if dirty {
		t.Fatalf("a user .brainignore pattern stopped applying once the built-ins were added")
	}
	// ...and the user's file does not widen into a blanket exemption either.
	runner.responses[fakeCommandKey("git", "status", "--porcelain", "--untracked-files=all")] = fakeCommandResponse{stdout: "?? src/main.go\n"}
	dirty, err = worktreeDirty(ctx, runner, repoDir)
	if err != nil {
		t.Fatalf("worktreeDirty: %v", err)
	}
	if !dirty {
		t.Fatalf("uncommitted source was gated as clean in a repo that has a .brainignore")
	}
}

// TestWorktreeGateIgnoreKeepsIndexingIgnoreIntact is the decision this change
// turns on. AGENTS.md and CLAUDE.md must stay in the INDEXED corpus — this
// repo's own .brainignore deliberately lists .claude/settings.json and
// deliberately does not list those two — so the gate exemption must live on a
// separate brainIgnore instance and never leak into the shared one.
func TestWorktreeGateIgnoreKeepsIndexingIgnoreIntact(t *testing.T) {
	t.Parallel()
	repoDir := t.TempDir()
	indexing, err := loadBrainIgnore(repoDir)
	if err != nil {
		t.Fatalf("loadBrainIgnore: %v", err)
	}
	if _, err := worktreeGateIgnore(repoDir); err != nil {
		t.Fatalf("worktreeGateIgnore: %v", err)
	}
	for _, path := range []string{"AGENTS.md", "CLAUDE.md", ".claude/settings.json", ".brainignore"} {
		if indexing.Ignored(path) {
			t.Fatalf("%s became ignored by the INDEXING filter; that deletes it from the indexed doc corpus, which is a regression disguised as a fix", path)
		}
	}
	gate, err := worktreeGateIgnore(repoDir)
	if err != nil {
		t.Fatalf("worktreeGateIgnore: %v", err)
	}
	for _, path := range entireAuthoredGatePaths {
		if !gate.Ignored(path) {
			t.Fatalf("%s is not exempt from the GATE", path)
		}
	}
	if gate.Ignored("internal/auth/token.go") {
		t.Fatalf("the gate ignore swallowed ordinary source")
	}
}

// TestFilterWorktreeStatusKeepsPartiallyIgnoredLines gives filterWorktreeStatus
// the direct unit test it never had. It was only ever covered transitively, and
// the gate-only ignore runs straight through it: a line is dropped only when
// EVERY path on it is ignored, so a rename out of exempt config into source
// survives.
func TestFilterWorktreeStatusKeepsPartiallyIgnoredLines(t *testing.T) {
	t.Parallel()
	ignore := brainIgnore{patterns: entireAuthoredGatePaths}
	for name, testCase := range map[string]struct {
		status string
		want   string
	}{
		"every path ignored": {
			status: "?? AGENTS.md\n M CLAUDE.md\n",
			want:   "",
		},
		"one source path keeps the line": {
			status: "?? AGENTS.md\n M src/main.go\n",
			want:   " M src/main.go\n",
		},
		"rename out of exempt config into source is kept": {
			status: "R  AGENTS.md -> docs/AGENTS.md\n",
			want:   "R  AGENTS.md -> docs/AGENTS.md\n",
		},
		"rename within exempt config is dropped": {
			status: "R  AGENTS.md -> CLAUDE.md\n",
			want:   "",
		},
		"blank lines are not paths": {
			status: "\n\n",
			want:   "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := string(filterWorktreeStatus([]byte(testCase.status), ignore)); got != testCase.want {
				t.Fatalf("filterWorktreeStatus(%q) = %q, want %q", testCase.status, got, testCase.want)
			}
		})
	}
}

// TestDirtyWorktreeRefusalKeepsMachinePrefix pins the one part of the refusal
// that is a contract rather than prose. setupIsDirtyWorktree matches on the
// `dirty_worktree:` PREFIX to attach its hint, and watch keys on it to continue
// rather than die, so the message may be reworded and that prefix may not move.
func TestDirtyWorktreeRefusalKeepsMachinePrefix(t *testing.T) {
	t.Parallel()
	err := dirtyWorktreeRefusal("seed", []string{"src/main.go"})
	if err == nil {
		t.Fatal("dirtyWorktreeRefusal returned no error")
	}
	if !strings.HasPrefix(err.Error(), brainstore.DirtyWorktreeCode+":") {
		t.Fatalf("refusal lost its machine-readable prefix: %q", err.Error())
	}
	if !setupIsDirtyWorktree(err.Error()) {
		t.Fatalf("setup no longer recognizes the refusal, so the reader loses the hint: %q", err.Error())
	}
}

// TestDirtyWorktreeRefusalNamesOffendingPathsAndRemedy covers what the old
// message left out. It named `--worktree` and stopped: never which files, and
// never that committing them is the other way out.
func TestDirtyWorktreeRefusalNamesOffendingPathsAndRemedy(t *testing.T) {
	t.Parallel()
	err := dirtyWorktreeRefusal("index", []string{"src/a.go", "src/b.go"})
	message := err.Error()
	for _, want := range []string{"2 uncommitted paths", "src/a.go", "src/b.go", "--worktree", "commit or stash"} {
		if !strings.Contains(message, want) {
			t.Fatalf("refusal does not mention %q: %q", want, message)
		}
	}
	// A tree with hundreds of dirty files needs the shape, not the list.
	many := make([]string, 0, dirtyWorktreeRefusalPathLimit+3)
	for i := 0; i < cap(many); i++ {
		many = append(many, "src/file"+string(rune('a'+i))+".go")
	}
	message = dirtyWorktreeRefusal("index", many).Error()
	if !strings.Contains(message, "and 3 more") {
		t.Fatalf("long path list was not summarized: %q", message)
	}
	if strings.Contains(message, many[len(many)-1]) {
		t.Fatalf("refusal listed every path instead of a sample: %q", message)
	}
	// Singular, because "1 uncommitted paths" is how a reader learns the tool
	// is not paying attention.
	if message := dirtyWorktreeRefusal("seed", []string{"src/a.go"}).Error(); !strings.Contains(message, "1 uncommitted path:") {
		t.Fatalf("single-path refusal reads wrong: %q", message)
	}
}

// TestSeedRefusalNamesOffendingPaths is the command-level half: the text a user
// actually sees when `refresh seed` stops.
func TestSeedRefusalNamesOffendingPaths(t *testing.T) {
	t.Parallel()
	repoDir := seedFixtureRepo(t)
	runner := seedFixtureRunner(repoDir)
	runner.responses[fakeCommandKey("git", "status", "--porcelain", "--untracked-files=all")] = fakeCommandResponse{stdout: " M README.md\n M src/index.ts\n"}
	cmd := NewRootCommand(Options{Version: "test-version", Runner: runner})
	_, err := execute(t, cmd, "refresh", "seed", "--output", filepath.Join(t.TempDir(), "seed"), repoDir)
	if err == nil {
		t.Fatal("dirty seed refresh succeeded")
	}
	for _, want := range []string{brainstore.DirtyWorktreeCode + ":", "README.md", "src/index.ts", "--worktree", "commit or stash"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("seed refusal does not mention %q: %v", want, err)
		}
	}
}

// TestSeedProceedsWhenOnlyEntireAuthoredConfigIsDirty is the reported scenario
// end to end on the seed component: `entire graph setup` left three files
// uncommitted and the next command must not treat that as uncommitted source.
func TestSeedProceedsWhenOnlyEntireAuthoredConfigIsDirty(t *testing.T) {
	t.Parallel()
	repoDir := seedFixtureRepo(t)
	runner := seedFixtureRunner(repoDir)
	runner.responses[fakeCommandKey("git", "status", "--porcelain", "--untracked-files=all")] = fakeCommandResponse{stdout: "?? AGENTS.md\n M CLAUDE.md\n?? .claude/settings.json\n"}
	cmd := NewRootCommand(Options{
		Version: "test-version",
		Env: EntireEnv{
			PluginConfigDir: filepath.Join(t.TempDir(), "config"),
			PluginDataDir:   filepath.Join(t.TempDir(), "data"),
			PluginStateDir:  filepath.Join(t.TempDir(), "state"),
			PluginCacheDir:  filepath.Join(t.TempDir(), "cache"),
		},
		Runner: runner,
		Now:    func() time.Time { return time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC) },
	})
	out, err := execute(t, cmd, "refresh", "seed", repoDir)
	if err != nil {
		t.Fatalf("seed refused a tree dirty only with Entire-authored config: %v\n%s", err, out)
	}
}

// TestSemanticIndexProceedsWhenOnlyEntireAuthoredConfigIsDirty is the same
// scenario on the other component. A user who clears one gate hits the other,
// so both have to move together.
func TestSemanticIndexProceedsWhenOnlyEntireAuthoredConfigIsDirty(t *testing.T) {
	t.Parallel()
	repoDir := t.TempDir()
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{stdout: "?? AGENTS.md\n M CLAUDE.md\n?? .claude/settings.json\n"}
	cmd := &cobra.Command{Use: "index"}
	opts := Options{Env: semanticTestEnv(t, repoDir), Runner: runner, Now: time.Now}
	if err := runSemanticIndex(cmd.Context(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("semantic index refused a tree dirty only with Entire-authored config: %v", err)
	}
}

// Graph's persistent index always describes HEAD, including in a dirty checkout.
func TestSemanticIndexUsesHEADWhenSourceIsDirty(t *testing.T) {
	t.Parallel()
	repoDir := t.TempDir()
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{stdout: " M internal/auth/token.go\n"}
	opts := Options{Env: semanticTestEnv(t, repoDir), Runner: runner, Now: time.Now}
	if err := runSemanticIndex(context.Background(), &cobra.Command{}, opts, semanticIndexOptions{}, repoDir); err != nil {
		t.Fatal(err)
	}
	if !fakeRunnerCalled(runner, "entire", "graph", "index", "--repo", repoDir, "--head", "--profile", "full", "--format", "json") {
		t.Fatal("persistent graph did not use committed HEAD")
	}
}

// TestRefreshBuildsBothComponentsAfterInitAgents is the reported bug end to
// end, on the surface that actually fails for it. `setup` is NOT that surface:
// watchDeterministicRefreshComponents passes bestEffort=true, so a dirty tree
// leaves setup reporting failed components and still exiting 0 — a test that
// asserted setup's exit code would pass before and after this change and prove
// nothing. `refresh` is not best-effort, and its phase lines name the component
// outcomes, which is the signal.
func TestRefreshBuildsBothComponentsAfterInitAgents(t *testing.T) {
	t.Parallel()
	repoDir := seedFixtureRepo(t)
	runner := seedFixtureRunner(repoDir)
	addRefreshSemanticFixture(runner, repoDir)
	runner.responses[fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef)] = fakeCommandResponse{err: os.ErrNotExist}
	runner.responses[fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1OriginRef)] = fakeCommandResponse{err: os.ErrNotExist}
	// Exactly what `entire graph setup` leaves behind.
	initAgents := "?? AGENTS.md\n M CLAUDE.md\n?? .claude/settings.json\n"
	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{stdout: initAgents}
	runner.responses[fakeCommandKey("git", "status", "--porcelain", "--untracked-files=all")] = fakeCommandResponse{stdout: initAgents}

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
		Now:    func() time.Time { return time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC) },
	})
	out, err := execute(t, cmd, "refresh", "--entire-binary", "entire-test")
	if err != nil {
		t.Fatalf("refresh after agent wiring: %v\n%s", err, out)
	}
	// Component outcomes, not the exit code: both stages have to BUILD.
	for _, want := range []string{
		"refresh: repository context: 3 documents, 2 entrypoints, 4 commands done",
		"refresh: code index: 1 symbol, 1 relation, 1 file done",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("component did not build after agent wiring (%q missing):\n%s", want, out)
		}
	}
	if strings.Contains(out, brainstore.DirtyWorktreeCode) {
		t.Fatalf("Entire's own install files still read as uncommitted content:\n%s", out)
	}
}

// --- Fix B -------------------------------------------------------------------

// agentAvailability builds a runner that answers `<name> --version` only for
// the agents named, so a test states which CLIs exist without touching PATH.
func agentAvailability(available ...string) *fakeCommandRunner {
	responses := map[string]fakeCommandResponse{}
	for _, name := range available {
		responses[fakeCommandKey(name, "--version")] = fakeCommandResponse{stdout: name + " 1.0\n"}
	}
	return &fakeCommandRunner{responses: responses}
}

// clearAgentHarnessEnv removes the harness signals this process may itself be
// running under. Without it, every one of these tests would read the env of the
// Claude Code session that happens to be running them.
//
// t.Setenv forbids t.Parallel, so nothing in this group is parallel.
func clearAgentHarnessEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDECODE", "")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "")
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "")
	t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "")
}

// isolateRealAgentBinaries guards full-command tests that resolve an agent.
// Agent RESOLUTION goes through the fake CommandRunner, but the seed synthesis
// phases execute the resolved agent as a real subprocess (deliberately: agents
// are real programs, not runner calls). On a developer machine with codex
// installed, a fixture refresh therefore silently ran the REAL agent — about a
// minute per test, with real agent-side spend — while CI never saw it because
// no agent is installed there. Shimming the agent binaries to fail immediately
// pins every machine to the CI behavior: the synthesis phase fails fast and
// refresh continues without it.
func isolateRealAgentBinaries(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		// The Windows runners install no agent, and a POSIX shell shim would
		// not execute there; absent binaries already fail the phase instantly.
		return
	}
	dir := t.TempDir()
	for _, name := range []string{"codex", "claude"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestDefaultRefreshAgentPrefersDetectedHarness is the reported defect: a user
// inside Claude Code who also had codex installed had their repository's
// content sent to codex, because the resolver asked PATH before it asked where
// it was running.
func TestDefaultRefreshAgentPrefersDetectedHarness(t *testing.T) {
	clearAgentHarnessEnv(t)
	t.Setenv("CLAUDECODE", "1")
	runner := agentAvailability("codex", "claude")
	if got := defaultRefreshAgent(context.Background(), runner, t.TempDir()); got != "claude-code" {
		t.Fatalf("defaultRefreshAgent inside Claude Code = %q, want claude-code", got)
	}
	if fakeRunnerCalled(runner, "codex", "--version") {
		t.Fatalf("the detected harness was confirmed and codex was still probed: %+v", runner.calls)
	}
}

// TestDefaultRefreshAgentDetectsEntrypointAlone covers the second signal:
// CLAUDE_CODE_ENTRYPOINT carries a mode ("cli", "vscode"), not a boolean, so it
// counts when present rather than when truthy.
func TestDefaultRefreshAgentDetectsEntrypointAlone(t *testing.T) {
	clearAgentHarnessEnv(t)
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "cli")
	runner := agentAvailability("codex", "claude")
	if got := defaultRefreshAgent(context.Background(), runner, t.TempDir()); got != "claude-code" {
		t.Fatalf("defaultRefreshAgent with CLAUDE_CODE_ENTRYPOINT=cli = %q, want claude-code", got)
	}
}

// TestDefaultRefreshAgentFallsBackWhenHarnessBinaryMissing is the regression
// this preference could otherwise ship. distillAgentCommandArgs turns
// "claude-code" into an argv starting with `claude` and there is no fallback at
// exec time, so preferring a detected-but-absent harness would turn a WORKING
// codex run into a hard exec failure.
func TestDefaultRefreshAgentFallsBackWhenHarnessBinaryMissing(t *testing.T) {
	clearAgentHarnessEnv(t)
	t.Setenv("CLAUDECODE", "1")
	runner := agentAvailability("codex")
	if got := defaultRefreshAgent(context.Background(), runner, t.TempDir()); got != "codex" {
		t.Fatalf("defaultRefreshAgent with CLAUDECODE set but no claude binary = %q, want codex", got)
	}
}

// TestDefaultRefreshAgentWithoutHarnessIsDeterministic pins the no-signal case:
// the PATH order is unchanged, "none" is reached when nothing is installed, and
// nothing anywhere prompts — a resolver that asked the user would hang every
// piped install forever.
func TestDefaultRefreshAgentWithoutHarnessIsDeterministic(t *testing.T) {
	clearAgentHarnessEnv(t)
	for name, testCase := range map[string]struct {
		available []string
		want      string
	}{
		"codex only":   {available: []string{"codex"}, want: "codex"},
		"claude only":  {available: []string{"claude"}, want: "claude-code"},
		"both on PATH": {available: []string{"codex", "claude"}, want: "codex"},
		"neither":      {available: nil, want: "none"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		got := defaultRefreshAgent(ctx, agentAvailability(testCase.available...), t.TempDir())
		cancel()
		if got != testCase.want {
			t.Fatalf("%s: defaultRefreshAgent = %q, want %q", name, got, testCase.want)
		}
	}
}

// TestResolveRefreshAgentAnnouncesChoice covers the second half of the report:
// "idk what model". Auto-selection was silent on every path but setup's
// backfill spend label, so a user was never told which vendor's agent was about
// to read their repository.
func TestResolveRefreshAgentAnnouncesChoice(t *testing.T) {
	clearAgentHarnessEnv(t)
	t.Setenv("CLAUDECODE", "1")
	var out bytes.Buffer
	agent := resolveRefreshAgentAnnounced(context.Background(), agentAvailability("codex", "claude"), t.TempDir(), "", &out)
	if agent != "claude-code" {
		t.Fatalf("resolved agent = %q, want claude-code", agent)
	}
	for _, want := range []string{"auto-selected", "claude-code", "--agent"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("announcement does not mention %q: %q", want, out.String())
		}
	}
	// "none" is announced too: a user whose distillation silently did nothing
	// deserves to know no agent was found.
	out.Reset()
	if agent := resolveRefreshAgentAnnounced(context.Background(), agentAvailability(), t.TempDir(), "", &out); agent != "none" {
		t.Fatalf("resolved agent with nothing installed = %q, want none", agent)
	}
	if !strings.Contains(out.String(), "none") {
		t.Fatalf("falling through to no agent was not announced: %q", out.String())
	}
	// A nil writer is how every --json caller stays silent.
	out.Reset()
	if agent := resolveRefreshAgentAnnounced(context.Background(), agentAvailability("codex"), t.TempDir(), "sonnet", nil); agent != "codex" {
		t.Fatalf("resolved agent with a nil writer = %q, want codex", agent)
	}
	if out.Len() != 0 {
		t.Fatalf("a nil writer still produced output: %q", out.String())
	}
}

// TestRefreshAnnouncesAutoSelectedAgentOnStderr is the call-site half, and the
// --json case beside it. The announcement has to reach stderr before anything
// is spent, and has to vanish for a machine reader.
func TestRefreshAnnouncesAutoSelectedAgentOnStderr(t *testing.T) {
	clearAgentHarnessEnv(t)
	isolateRealAgentBinaries(t)
	installFakeSeedAgentBinaries(t)
	logDir := t.TempDir()
	t.Setenv("FAKE_SEED_AGENT_LOG_DIR", logDir)
	repoDir := seedFixtureRepo(t)
	runner := seedFixtureRunner(repoDir)
	addRefreshSemanticFixture(runner, repoDir)
	runner.responses[fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef)] = fakeCommandResponse{err: os.ErrNotExist}
	runner.responses[fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1OriginRef)] = fakeCommandResponse{err: os.ErrNotExist}
	runner.responses[fakeCommandKey("codex", "--version")] = fakeCommandResponse{stdout: "codex 1.0\n"}
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
		Now:    func() time.Time { return time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC) },
	})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"refresh", "--agent", "auto", "--entire-binary", "entire-test"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("refresh: %v\n%s", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "auto-selected codex") {
		t.Fatalf("refresh did not announce the agent it chose on stderr: %q", stderr.String())
	}
	if strings.Contains(stdout.String(), "auto-selected") {
		t.Fatalf("the announcement landed on stdout, where a piped reader parses results: %q", stdout.String())
	}
	for _, phase := range []string{"quick", "deep"} {
		if _, err := os.Stat(filepath.Join(logDir, "codex-"+phase+".json")); err != nil {
			t.Fatalf("explicit --agent auto did not run the fake %s phase: %v", phase, err)
		}
	}
}

// TestDistillJSONStaysSilentAboutTheAgent is the suppression half of the pair
// above: the same resolution, under --json, must not put a human line where a
// machine reader is parsing.
func TestDistillJSONStaysSilentAboutTheAgent(t *testing.T) {
	clearAgentHarnessEnv(t)
	for _, jsonMode := range []bool{false, true} {
		repoDir := seedFixtureRepo(t)
		runner := seedFixtureRunner(repoDir)
		runner.responses[fakeCommandKey("codex", "--version")] = fakeCommandResponse{stdout: "codex 1.0\n"}
		cmd := NewRootCommand(Options{
			Version: "test-version",
			Env: EntireEnv{
				PluginConfigDir: filepath.Join(t.TempDir(), "config"),
				PluginDataDir:   filepath.Join(t.TempDir(), "data"),
				PluginStateDir:  filepath.Join(t.TempDir(), "state"),
				PluginCacheDir:  filepath.Join(t.TempDir(), "cache"),
			},
			Runner: runner,
			Now:    func() time.Time { return time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC) },
		})
		args := []string{"distill", "--dry-run", repoDir}
		if jsonMode {
			args = append(args, "--json")
		}
		// The fixture repo has no exported sessions, so the dry run ends in an
		// error. That is fine and deliberate: the agent is resolved BEFORE the
		// dry-run branch (so a dry run salts its cache with the same agent a
		// real run would), which is the only thing under test here.
		out, _ := execute(t, cmd, args...)
		announced := strings.Contains(out, "auto-selected")
		if jsonMode && announced {
			t.Fatalf("json: the agent announcement leaked into machine output: %q", out)
		}
		if !jsonMode && !announced {
			t.Fatalf("human: distill never announced the agent, so the json case above proves nothing: %q", out)
		}
	}
}

// TestSetupDetectAgentAnnouncesExceptUnderJSON covers the third call site, and
// the one that matters most: setup resolves the agent ONCE and embeds it in the
// daemon plan the watcher then runs forever, so this is the moment a user is
// which vendor's agent will read their repository — but at the moment of
// consent, not as a preamble. Setup's detectAgent must stay SILENT in every
// mode: the consent block names agent/model/effort before a human answers,
// and verbose mode prints the spend label for explicit opt-in. A line
// printed at detect time described a spend that had not been agreed to yet.
func TestSetupDetectAgentStaysSilent(t *testing.T) {
	clearAgentHarnessEnv(t)
	for _, jsonMode := range []bool{false, true} {
		repoDir := t.TempDir()
		runner := agentAvailability("codex")
		var stderr bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&stderr)
		steps := resolveSetupSteps(cmd, Options{Runner: runner, Now: time.Now}, setupCommandOptions{json: jsonMode, model: "opus"}, repoDir, "entire-brain", setupSteps{})
		if agent := steps.detectAgent(context.Background()); agent != "codex" {
			t.Fatalf("json=%v: setup detected agent = %q, want codex", jsonMode, agent)
		}
		if stderr.Len() != 0 {
			t.Fatalf("json=%v: detectAgent wrote a preamble; disclosure belongs to the consent gate: %q", jsonMode, stderr.String())
		}
	}
}

// TestAnnounceResolvedAgentNamesTheModel answers the report's SECOND sentence,
// "idk what model". The fix is visibility, not a default: setup.go's
// setupBackfillDefaultEffort block already decided not to hardcode a model id
// ("model ids age badly"), and pinning Sonnet or Opus here would be the
// codex-first mistake pointing the other way. So an unpinned model is STATED,
// not omitted — a line that named only the agent would leave the reader
// guessing about the model for exactly the reason they complained.
func TestAnnounceResolvedAgentNamesTheModel(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer

	announceResolvedAgent(&out, "claude-code", "opus")
	for _, want := range []string{"claude-code", "model opus", "--model"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("pinned-model announcement does not mention %q: %q", want, out.String())
		}
	}

	out.Reset()
	announceResolvedAgent(&out, "claude-code", "")
	if !strings.Contains(out.String(), "the agent's own default") {
		t.Fatalf("an unpinned model was not stated outright, which is the whole complaint: %q", out.String())
	}
	if !strings.Contains(out.String(), "--model") {
		t.Fatalf("the unpinned announcement does not say how to pin one: %q", out.String())
	}

	// Whitespace is not a model.
	out.Reset()
	announceResolvedAgent(&out, "codex", "   ")
	if !strings.Contains(out.String(), "the agent's own default") {
		t.Fatalf("a blank --model was treated as a pinned model: %q", out.String())
	}

	// No agent means no model to name, and the line must not invent one.
	out.Reset()
	announceResolvedAgent(&out, "none", "opus")
	if strings.Contains(out.String(), "opus") {
		t.Fatalf("the no-agent line named a model nothing will use: %q", out.String())
	}
}

// TestRefreshAnnouncesPinnedModel is the call-site half: --seed-model has to
// reach the line a user actually reads.
func TestRefreshAnnouncesPinnedModel(t *testing.T) {
	clearAgentHarnessEnv(t)
	isolateRealAgentBinaries(t)
	installFakeSeedAgentBinaries(t)
	t.Setenv("FAKE_SEED_AGENT_LOG_DIR", t.TempDir())
	repoDir := seedFixtureRepo(t)
	runner := seedFixtureRunner(repoDir)
	addRefreshSemanticFixture(runner, repoDir)
	runner.responses[fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1MainRef)] = fakeCommandResponse{err: os.ErrNotExist}
	runner.responses[fakeCommandKey("git", "ls-tree", "-r", "--name-only", v1OriginRef)] = fakeCommandResponse{err: os.ErrNotExist}
	runner.responses[fakeCommandKey("codex", "--version")] = fakeCommandResponse{stdout: "codex 1.0\n"}
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
		Now:    func() time.Time { return time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC) },
	})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"refresh", "--agent", "auto", "--entire-binary", "entire-test", "--seed-model", "haiku"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("refresh: %v\n%s", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "model haiku") {
		t.Fatalf("refresh did not name the pinned model: %q", stderr.String())
	}
}

// TestSetupWatchPlanKeepsAResolvedAgent pins the daemon gap. daemonEnv builds an
// EXPLICIT env map (plugin dirs + PATH) and carries no CLAUDECODE, so a watch
// plan row with a blank agent hands the daemon back its own codex-first
// resolution — a silent vendor switch on the one unattended path that spends
// tokens by itself. The upsert used to replace the whole row, so any run that
// produced a blank agent erased what an earlier one recorded.
func TestSetupWatchPlanKeepsAResolvedAgent(t *testing.T) {
	t.Parallel()
	existing := setupWatchPlanEntry{Workspace: "default", Agent: "claude-code", Model: "opus", Effort: "low"}

	// A run with nothing to say leaves the recorded choice alone.
	blank := setupWatchPlanEntry{Workspace: "default", Effort: "high"}
	merged := mergeSetupWatchPlanEntry(existing, blank)
	if merged.Agent != "claude-code" {
		t.Fatalf("a blank incoming agent erased the recorded one: %+v", merged)
	}
	if merged.Model != "opus" {
		t.Fatalf("a blank incoming model erased the recorded one: %+v", merged)
	}
	if merged.Effort != "high" {
		t.Fatalf("the merge stopped applying this run's own values: %+v", merged)
	}

	// ...and a run that DID resolve one writes it, so a stale agent can never
	// latch after the CLI it named is uninstalled.
	merged = mergeSetupWatchPlanEntry(existing, setupWatchPlanEntry{Workspace: "default", Agent: "codex", Model: "sonnet"})
	if merged.Agent != "codex" || merged.Model != "sonnet" {
		t.Fatalf("a resolved agent/model did not win over the recorded one: %+v", merged)
	}
}

// TestSetupWatchPlanEntryCarriesTheResolvedAgent covers the producer side: the
// row setup writes names a concrete agent, never the literal "auto" the daemon
// would have to resolve for itself.
func TestSetupWatchPlanEntryCarriesTheResolvedAgent(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
	entry := setupWatchPlanEntryFor(setupCommandOptions{workspace: "default", agent: "claude-code", model: "opus"}, now)
	if entry.Agent != "claude-code" {
		t.Fatalf("the resolved agent did not reach the watch plan: %+v", entry)
	}
	if entry.Model != "opus" {
		t.Fatalf("the pinned model did not reach the watch plan: %+v", entry)
	}
	// "auto" is refused rather than written: writing it hands the daemon its own
	// codex-first resolution under an env with no CLAUDECODE in it.
	unresolved := setupWatchPlanEntryFor(setupCommandOptions{workspace: "default", agent: "auto"}, now)
	if unresolved.Agent != "" {
		t.Fatalf("the literal \"auto\" was written into the plan: %+v", unresolved)
	}
}

// TestWatchDistillAgentDefaultsToAuto covers the third hardcoded codex default,
// the one that never reached defaultRefreshAgent at all. The watcher is the one
// unattended path that spends tokens on its own, so a vendor pinned here
// survives any reorder of the resolver.
func TestWatchDistillAgentDefaultsToAuto(t *testing.T) {
	t.Parallel()
	if got := watch.DefaultFlags().DistillAgent; got != "auto" {
		t.Fatalf("watch distill agent default = %q, want auto (a hardcoded vendor here bypasses defaultRefreshAgent entirely)", got)
	}
}
