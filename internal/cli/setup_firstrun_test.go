package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ashtom/entire-brain/internal/tui"
	"github.com/spf13/cobra"
)

// setup_firstrun_test.go pins the failures a NEW user hits on their first-ever
// `entire-brain setup`, each reproduced in a sandboxed trial before it was
// fixed. Every test here fails against the pre-fix code.

// --- 1. the 'U' bug ------------------------------------------------------

// hostUpdateBannerStdout is what a released `entire` CLI older than the fix
// that moved the notice to stderr actually writes: a complete JSON value on
// stdout, and THEN a human banner on the same stream, from a post-run hook.
const hostUpdateBannerStdout = "Update available! v0.9.9 -> v0.10.0\nRelease notes: https://example.invalid/releases/v0.10.0\nTo update, run:\n  brew upgrade entire\n"

// TestCheckpointListSurvivesTheHostUpdateBanner is the deterministic first-run
// failure that looked like flakiness:
//
//	session export failed (... parse checkpoint list json:
//	invalid character 'U' after top-level value)
//
// The 'U' is "Update available!". Because the host caches its version check for
// 24h, the SECOND run is clean — so setup failed once, on the first-ever run,
// and then silently healed, which is the worst possible shape for a bug.
func TestCheckpointListSurvivesTheHostUpdateBanner(t *testing.T) {
	t.Parallel()
	repoDir := t.TempDir()
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("entire", "checkpoint", "explain", "--json", "--search-all"): {
			stdout: "[\n  {\n    \"checkpoint_id\": \"abcdef012345\",\n    \"session_id\": \"s1\"\n  }\n]\n\n" + hostUpdateBannerStdout,
		},
	}}

	checkpoints, _, err := listAllRoutedCheckpoints(context.Background(), runner, repoDir, "entire", 0)
	if err != nil {
		t.Fatalf("a first-ever run must not fail on the host's own update banner: %v", err)
	}
	if len(checkpoints) != 1 || checkpoints[0].CheckpointID != "abcdef012345" {
		t.Fatalf("the JSON value before the banner must decode intact, got %+v", checkpoints)
	}
}

// TestCheckpointDetailSurvivesTheHostUpdateBanner covers the second place the
// same host stdout is parsed: the per-checkpoint fetch the export loop makes
// for every selected checkpoint.
func TestCheckpointDetailSurvivesTheHostUpdateBanner(t *testing.T) {
	t.Parallel()
	repoDir := t.TempDir()
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("entire", "checkpoint", "explain", "--json", "abcdef012345"): {
			stdout: "{\"checkpoint_id\":\"abcdef012345\"}\n" + hostUpdateBannerStdout,
		},
	}}

	detail, err := checkpointDetail(context.Background(), runner, repoDir, "entire", "abcdef012345")
	if err != nil {
		t.Fatalf("checkpoint detail must tolerate trailing host chatter: %v", err)
	}
	if detail.CheckpointID != "abcdef012345" {
		t.Fatalf("checkpoint id = %q", detail.CheckpointID)
	}
}

// TestHostCLIJSONStillRejectsALeadingNonJSONPrefix keeps the tolerance narrow.
// Trailing bytes are chatter this process does not own; a value that is not
// FIRST means no prefix of the stream can be trusted, and silently accepting
// that would turn a corrupt read into a confidently empty inventory.
func TestHostCLIJSONStillRejectsALeadingNonJSONPrefix(t *testing.T) {
	t.Parallel()
	var target []checkpointListEntry
	if err := decodeHostCLIJSON([]byte("Update available!\n[]\n"), &target); err == nil {
		t.Fatal("leading non-JSON output must still be an error, not an empty inventory")
	}
	if err := decodeHostCLIJSON(nil, &target); err == nil || !strings.Contains(err.Error(), "unexpected end of JSON input") {
		t.Fatalf("empty stdout must keep its old message, got %v", err)
	}
}

// --- 2. the opaque semantic-provider failure -----------------------------

// TestSemanticIndexNamesTheMissingGraphPlugin: the whole message a first-time
// user saw was
//
//	provider_doctor_failed: semantic provider no-egress status is not verified
//
// while the actual cause — the host CLI rejecting `graph` because the
// entire-graph plugin is not installed — was captured into the warning's Detail
// and then dropped. This is not a sandbox artifact: a dev-built `entire` with
// no `graph` command hits it on a real machine today.
func TestSemanticIndexNamesTheMissingGraphPlugin(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	runner.responses[fakeCommandKey("entire", "graph", "doctor", "--json")] = fakeCommandResponse{
		err: errors.New(`entire [graph doctor --json]: exit status 1: Invalid usage: unknown command "graph" for "entire"`),
	}

	err := runSemanticIndex(context.Background(), &cobra.Command{Use: "index"},
		Options{Env: env, Runner: runner, Now: time.Now},
		semanticIndexOptions{graphBinary: "entire"}, repoDir)
	if err == nil {
		t.Fatal("a provider that cannot be verified must still fail")
	}
	message := err.Error()
	if !strings.Contains(message, `unknown command "graph"`) {
		t.Fatalf("the real cause must reach the user, got: %s", message)
	}
	if !strings.Contains(message, "entire-graph") || !strings.Contains(message, "scripts/install.sh") {
		t.Fatalf("a missing plugin must name the install step, got: %s", message)
	}
}

// TestSemanticProviderErrorKeepsTheDetailForOtherFailures: the install hint is
// for the plugin-absent case only. A timeout or malformed diagnostics must
// still surface their own detail rather than be mislabelled as "not installed".
func TestSemanticProviderErrorKeepsTheDetailForOtherFailures(t *testing.T) {
	t.Parallel()
	err := semanticProviderUnverifiedError("entire", []semanticWarning{{
		Code:   "provider_doctor_timeout",
		Detail: "semantic provider doctor timed out after 10s",
	}})
	message := err.Error()
	if !strings.Contains(message, "provider_doctor_timeout") || !strings.Contains(message, "timed out after 10s") {
		t.Fatalf("the detail must survive for every doctor failure, got: %s", message)
	}
	if strings.Contains(message, "scripts/install.sh") {
		t.Fatalf("a timeout is not a missing plugin: %s", message)
	}
}

// TestSemanticProviderErrorStaysReadable: a CLI answers an unknown command by
// dumping its whole usage text to stderr, and that text arrives here as the
// error detail. Printed whole it buries the one sentence that matters inside
// setup's failure line, its summary block, `status` and `doctor` alike.
func TestSemanticProviderErrorStaysReadable(t *testing.T) {
	t.Parallel()
	usageDump := "entire [graph doctor --json]: exit status 1: Usage:\n" +
		"  entire [flags]\n  entire [command]\n\nEntire Setup:\n  agent   Manage agent integrations\n" +
		strings.Repeat("  more   filler line to stand in for sixty lines of usage\n", 40) +
		"\nError: Invalid usage: unknown command \"graph\" for \"entire\"\n\nDid you mean this?\n\tgrant\n"

	message := semanticProviderUnverifiedError("entire", []semanticWarning{{
		Code:   "provider_doctor_failed",
		Detail: usageDump,
	}}).Error()

	if strings.Contains(message, "filler line") {
		t.Fatalf("the provider's usage dump must not be reprinted verbatim:\n%s", message)
	}
	if !strings.Contains(message, `unknown command "graph"`) {
		t.Fatalf("the marked Error: line is the one sentence that must survive:\n%s", message)
	}
	if !strings.Contains(message, "entire-graph") {
		t.Fatalf("shortening must not lose the install step:\n%s", message)
	}
	if lines := strings.Count(message, "\n"); lines > 0 {
		t.Fatalf("the message must stay one line, got %d newlines:\n%s", lines, message)
	}
}

// --- 3. doctor was a dead end --------------------------------------------

// TestDoctorInfersTheRepoFromTheWorkingDirectory: setup prints "run
// 'entire-brain doctor' for detail" on a failed component, and doctor's
// repo-scoped section was gated on ENTIRE_REPO_ROOT — a variable the HOST sets
// when it dispatches the plugin and which is simply unset in a plain terminal.
// So the command setup points you at printed plugin-directory health and
// nothing whatsoever about the failure.
func TestDoctorInfersTheRepoFromTheWorkingDirectory(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	env.RepoRoot = "" // exactly what a standalone terminal run looks like
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "rev-parse", "--show-toplevel"): {stdout: repoDir + "\n"},
	}}
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return setupTestNow }}

	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatalf("repoStoragePaths: %v", err)
	}
	stateDir := filepath.Dir(storage.HeadPath)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The failure setup would have recorded and told the reader to come here for.
	if err := writeSetupInstantRecord(stateDir, setupTestNow, []setupComponent{
		{Name: brainComponentSemantic, State: "failed", Detail: `unknown command "graph" for "entire"`},
	}); err != nil {
		t.Fatalf("write instant record: %v", err)
	}

	t.Chdir(repoDir)
	out := &bytes.Buffer{}
	cmd := &cobra.Command{Use: "doctor"}
	cmd.SetContext(context.Background())
	cmd.SetOut(out)
	cmd.SetErr(out)
	if err := runDoctor(cmd, opts, false); err != nil {
		t.Fatalf("runDoctor: %v", err)
	}

	printed := out.String()
	if !strings.Contains(printed, `unknown command "graph"`) {
		t.Fatalf("doctor must print WHY the component setup named failed:\n%s", printed)
	}
	if !strings.Contains(printed, "repo root: "+repoDir) {
		t.Fatalf("doctor must say which repository it inferred:\n%s", printed)
	}
}

// TestDoctorSaysSoWhenNoRepositoryIsInScope: when the inference finds nothing
// either, doctor has to say that out loud. Silence is what made it look broken
// rather than out of scope.
func TestDoctorSaysSoWhenNoRepositoryIsInScope(t *testing.T) {
	dir := t.TempDir()
	env := semanticTestEnv(t, dir)
	env.RepoRoot = ""
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "rev-parse", "--show-toplevel"): {err: errors.New("fatal: not a git repository")},
	}}
	t.Chdir(dir)
	out := &bytes.Buffer{}
	cmd := &cobra.Command{Use: "doctor"}
	cmd.SetContext(context.Background())
	cmd.SetOut(out)
	cmd.SetErr(out)
	if err := runDoctor(cmd, Options{Version: "test", Env: env, Runner: runner, Now: time.Now}, false); err != nil {
		t.Fatalf("runDoctor: %v", err)
	}
	if !strings.Contains(out.String(), "no repository in scope") {
		t.Fatalf("doctor must name its own scope limit:\n%s", out.String())
	}
}

// --- 4. freshness degraded on a pristine clone ---------------------------

// TestLiveStateIgnoresTheHostsOwnLogFile: setup's own toolchain — the host
// `entire` CLI it shells out to — creates an empty .entire/logs/entire.log
// INSIDE the working tree. Every other reader in the brain already treats the
// .entire segment as ignored; this one computation read raw `git status`, so a
// pristine clone came out dirty, the seed axis came out "dirty-unindexed", and
// a successful onboarding ended on "freshness: degraded" that the user did not
// cause and could not act on.
func TestLiveStateIgnoresTheHostsOwnLogFile(t *testing.T) {
	t.Parallel()
	repoDir := t.TempDir()
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "rev-parse", "HEAD"):                              {stdout: "5cc739747766aaa4202a6f82641ba611a386e0a9\n"},
		fakeCommandKey("git", "branch", "--show-current"):                       {stdout: "main\n"},
		fakeCommandKey("git", "status", "--porcelain", "--untracked-files=all"): {stdout: "?? .entire/logs/entire.log\n"},
		fakeCommandKey("git", "diff", "--shortstat", "HEAD"):                    {stdout: "\n"},
	}}

	live, err := brainLiveStateReport(context.Background(), runner, repoDir, nil)
	if err != nil {
		t.Fatalf("brainLiveStateReport: %v", err)
	}
	if live.Dirty {
		t.Fatalf("the host's own log file must not make a pristine clone dirty; untracked=%v", live.Untracked)
	}
	if len(live.Untracked) != 0 {
		t.Fatalf("an ignored path must not be reported as untracked: %v", live.Untracked)
	}
}

// TestLiveStateStillReportsRealChanges keeps the filter honest: it removes what
// the brain already ignores and nothing else.
func TestLiveStateStillReportsRealChanges(t *testing.T) {
	t.Parallel()
	repoDir := t.TempDir()
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "rev-parse", "HEAD"):                              {stdout: "5cc739747766aaa4202a6f82641ba611a386e0a9\n"},
		fakeCommandKey("git", "branch", "--show-current"):                       {stdout: "main\n"},
		fakeCommandKey("git", "status", "--porcelain", "--untracked-files=all"): {stdout: "?? .entire/logs/entire.log\n M src/main.go\n"},
		fakeCommandKey("git", "diff", "--shortstat", "HEAD"):                    {stdout: " 1 file changed\n"},
	}}

	live, err := brainLiveStateReport(context.Background(), runner, repoDir, nil)
	if err != nil {
		t.Fatalf("brainLiveStateReport: %v", err)
	}
	if !live.Dirty || len(live.Unstaged) != 1 || live.Unstaged[0] != "src/main.go" {
		t.Fatalf("a real edit must still read as dirty: dirty=%v unstaged=%v", live.Dirty, live.Unstaged)
	}
}

// --- 5. a non-git directory exited 0 and installed a daemon --------------

// TestSetupRefusesANonGitDirectory: four of the instant phase's components
// failed with a raw `fatal: not a git repository`, and setup then registered a
// workspace, wrote per-repo state, installed the watcher and exited 0.
func TestSetupRefusesANonGitDirectory(t *testing.T) {
	f := newSetupTestFixture(t)
	f.runner.responses[fakeCommandKey("git", "rev-parse", "--show-toplevel")] = fakeCommandResponse{
		err: errors.New("exit status 128: fatal: not a git repository (or any of the parent directories): .git"),
	}
	rec := &recordedSetup{}
	opts := defaultSetupOptions()
	out := &bytes.Buffer{}

	err := runSetup(context.Background(), setupTestCommand(t, out, opts), f.opts, opts, f.repoDir, rec.steps(f))
	if err == nil {
		t.Fatal("setup in a plain directory must refuse, not exit 0 with four failed components")
	}
	if !strings.Contains(err.Error(), "not a git repository") || !strings.Contains(err.Error(), "git init") {
		t.Fatalf("the refusal must name the cause and the fix, got: %v", err)
	}
	if rec.instantCalls != 0 {
		t.Fatalf("nothing must be built before the repository check, instant ran %d times", rec.instantCalls)
	}
	if rec.installCalls != 0 {
		t.Fatalf("a non-repository must never get a background watcher, installs=%d", rec.installCalls)
	}
	if _, err := loadWorkspaceManifest(f.env, setupDefaultWorkspace); err == nil {
		t.Fatal("a non-repository must never be registered into a workspace")
	}
}

// TestSetupUninstallDaemonStillWorksOutsideARepository: --uninstall-daemon is
// machine-level maintenance and must keep working from anywhere — including the
// directory whose failed setup is the reason someone is uninstalling.
func TestSetupUninstallDaemonStillWorksOutsideARepository(t *testing.T) {
	f := newSetupTestFixture(t)
	f.runner.responses[fakeCommandKey("git", "rev-parse", "--show-toplevel")] = fakeCommandResponse{
		err: errors.New("exit status 128: fatal: not a git repository"),
	}
	rec := &recordedSetup{}
	opts := defaultSetupOptions()
	opts.uninstallDaemon = true
	out := &bytes.Buffer{}

	if err := runSetup(context.Background(), setupTestCommand(t, out, opts), f.opts, opts, f.repoDir, rec.steps(f)); err != nil {
		t.Fatalf("--uninstall-daemon must not require a repository: %v", err)
	}
	if rec.uninstallCalls != 1 {
		t.Fatalf("uninstall must still run, calls=%d", rec.uninstallCalls)
	}
}

// --- 6. the daemon nobody was told about ---------------------------------

// TestSetupSummaryNamesTheWatcherAndHowToRemoveIt: --uninstall-daemon existed
// ONLY in `setup --help`. Nothing in setup's output, its Next block or `status`
// said a persistent service had just been installed on the machine, let alone
// how to take it off.
func TestSetupSummaryNamesTheWatcherAndHowToRemoveIt(t *testing.T) {
	t.Parallel()
	out := &bytes.Buffer{}
	report := setupReport{
		Repo:      "/repo",
		RepoKey:   "local/repo-1",
		BrainPath: "/brain",
		Instant:   setupPhase{State: "ok"},
		Workspace: setupWorkspaceState{Name: "default", Registered: true},
		Daemon: daemonState{
			Manager:   daemonManagerLaunchd,
			Label:     "io.entire.brain-watch.1a2b3c4d",
			UnitPath:  "/home/u/Library/LaunchAgents/io.entire.brain-watch.1a2b3c4d.plist",
			Installed: true,
			Current:   true,
			Running:   true,
		},
	}
	renderSetupSummary(out, tui.NewRenderer(out), report, &setupTimings{}, setupWatchPlan{}, setupBrainBinaryName)

	printed := out.String()
	if !strings.Contains(printed, "--uninstall-daemon") {
		t.Fatalf("setup must name the command that removes what it just installed:\n%s", printed)
	}
	if !strings.Contains(printed, "persistent service") || !strings.Contains(printed, report.Daemon.UnitPath) {
		t.Fatalf("setup must say a persistent service was installed, and where:\n%s", printed)
	}
}

// TestStatusNamesTheUninstallCommand: same gap, from the other command a user
// is told to run.
func TestStatusNamesTheUninstallCommand(t *testing.T) {
	t.Parallel()
	out := &bytes.Buffer{}
	renderBrainOnboardingStatus(out, &brainStatusOnboarding{
		Daemon: daemonState{Manager: daemonManagerLaunchd, Label: "io.entire.brain-watch.1a2b3c4d", Installed: true, Running: true},
	}, setupTestNow, setupBrainBinaryName)
	if !strings.Contains(out.String(), "--uninstall-daemon") {
		t.Fatalf("status must name the removal command for a daemon it reports:\n%s", out.String())
	}
}

// --- 7. ENTIRE_BRAIN_DAEMON_DIR did not sandbox the registration ---------

// TestNoRegisterNeverTouchesTheServiceManager: ENTIRE_BRAIN_DAEMON_DIR's own
// comment claimed a sandboxed run "can never write into the developer's
// ~/Library/LaunchAgents". True of the plist FILE, false of the JOB:
// installDaemon still ran `launchctl load -w` (and `systemctl --user enable
// --now`) against the live session, so any CI job or smoke test that set only
// that variable registered a real KeepAlive agent.
func TestNoRegisterNeverTouchesTheServiceManager(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			unitDir := filepath.Join(t.TempDir(), "daemon")
			t.Setenv("HOME", t.TempDir())
			t.Setenv("USERPROFILE", t.TempDir())
			t.Setenv(envDaemonUnitDir, unitDir)
			t.Setenv(envDaemonNoRegister, "1")

			runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
			opts := Options{Version: "test", Env: semanticTestEnv(t, t.TempDir()), Runner: runner, Now: time.Now}
			plan, err := brainWatchDaemonPlanFor(goos, opts, defaultSetupOptions())
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			if !plan.NoRegister {
				t.Fatalf("%s must be honoured at plan time", envDaemonNoRegister)
			}
			if err := installDaemon(context.Background(), runner, plan); err != nil {
				t.Fatalf("installDaemon: %v", err)
			}
			if _, err := os.Stat(plan.UnitPath); err != nil {
				t.Fatalf("the unit must still be written so the artifact stays verifiable: %v", err)
			}
			if err := uninstallDaemon(context.Background(), runner, plan); err != nil {
				t.Fatalf("uninstallDaemon: %v", err)
			}
			for _, call := range runner.calls {
				if call.name == "launchctl" || call.name == "systemctl" {
					t.Fatalf("a no-register run reached the live service manager: %s %v", call.name, call.args)
				}
			}
			if state := inspectDaemon(context.Background(), runner, plan); state.Running {
				t.Fatal("a no-register run must never claim a running job in the caller's session")
			}
		})
	}
}

// TestRegistrationStillHappensWithoutTheNoRegisterKnob is the other half of the
// contract: the sandbox is opt-in, and a normal run still installs a real job.
func TestRegistrationStillHappensWithoutTheNoRegisterKnob(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv(envDaemonUnitDir, filepath.Join(t.TempDir(), "daemon"))
	t.Setenv(envDaemonNoRegister, "")

	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	opts := Options{Version: "test", Env: semanticTestEnv(t, t.TempDir()), Runner: runner, Now: time.Now}
	plan, err := brainWatchDaemonPlanFor("darwin", opts, defaultSetupOptions())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.NoRegister {
		t.Fatal("no-register must be opt-in, not the default")
	}
	_ = installDaemon(context.Background(), runner, plan)
	var sawLaunchctl bool
	for _, call := range runner.calls {
		if call.name == "launchctl" {
			sawLaunchctl = true
		}
	}
	if !sawLaunchctl {
		t.Fatal("a normal install must still register with the service manager")
	}
}

// --- the cheap ones ------------------------------------------------------

// TestWatchBannerNamesTheCapThatBinds: the workspace banner printed
// "budget=0", which is the value a setup-installed watcher ALWAYS has (setup
// deliberately never passes --budget), while --max-sessions — the cap that
// actually binds it, carried per workspace in the machine watch plan — was not
// printed at all.
func TestWatchBannerNamesTheCapThatBinds(t *testing.T) {
	env := semanticTestEnv(t, t.TempDir())
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{}}
	if err := writeWorkspaceManifest(env, workspaceManifest{
		SchemaVersion: workspaceSchemaVersion,
		Name:          "ws",
	}); err != nil {
		t.Fatalf("write workspace: %v", err)
	}
	w := defaultWatchOptions()
	w.once = true
	w.distillMaxSessions = setupDefaultBackfillBudget
	out := &bytes.Buffer{}
	if err := workspaceWatchLoop(context.Background(), out, Options{Env: env, Runner: runner, Now: time.Now}, w, "ws", func(string, *int) {}); err != nil {
		t.Fatalf("workspaceWatchLoop: %v", err)
	}
	printed := out.String()
	if !strings.Contains(printed, "max-sessions=25") {
		t.Fatalf("the daemon must log the cap that binds it:\n%s", printed)
	}
	if strings.Contains(printed, "budget=0") {
		t.Fatalf("a bare budget=0 reads as \"zero allowed\" and is never the real bound:\n%s", printed)
	}
}

// TestStatusReportsEveryComponentSetupReported: the component ids are declared
// (setup.go) to be "the same vocabulary `status` prints on its instant line",
// but status hard-coded five names. So setup could print "FAILED entities" and
// status — the command setup points at for exactly this — showed no entities
// line at all.
func TestStatusReportsEveryComponentSetupReported(t *testing.T) {
	t.Parallel()
	components := instantPhaseComponents(nil, setupInstantRecord{
		UpdatedAt: setupTestNow,
		Components: []setupComponent{
			{Name: brainComponentSessions, State: "ok"},
			{Name: brainComponentEntities, State: "failed", Detail: "entityindex: resolve HEAD"},
			{Name: brainComponentMemory, State: "ok"},
		},
	})
	states := map[string]string{}
	for _, component := range components {
		states[component.Name] = component.State
	}
	if states[brainComponentEntities] != "failed" {
		t.Fatalf("a component setup reported as FAILED must be findable in status: %v", states)
	}
	if states[brainComponentMemory] != "built" {
		t.Fatalf("status must speak setup's whole component vocabulary: %v", states)
	}
}

// --- 5. the enable -> setup seam ------------------------------------------
//
// These two pin the first run as a REAL user performs it, not as the synthetic
// demo staged it. scripts/demo-setup.sh writes checkpoint refs into the repo
// itself and never runs `entire enable`, so neither of these states could ever
// occur there; both occur every time in scripts/demo-agent-session.sh, which
// runs the host CLI for real.

// TestSetupNamesTheRemedyForADirtyWorktree: `entire enable` writes
// .entire/settings.json and .claude/settings.json and does not commit them, so
// the documented next step -- `entire-brain setup` -- finds a dirty worktree
// and refuses to seed or index it. The refusal names --worktree, a flag `setup`
// does not accept, so before this the reader was told to pass something they
// could not pass, about files they never edited.
func TestSetupNamesTheRemedyForADirtyWorktree(t *testing.T) {
	t.Parallel()
	for _, name := range []string{brainComponentSeed, brainComponentSemantic} {
		component := newSetupComponent(name, errors.New(dirtyWorktreeErrorCode+": refusing to index uncommitted content without --worktree"), setupBrainBinaryName)
		hint := component.Hint
		if hint == "" {
			t.Fatalf("%s: a dirty worktree must carry a remedy; the refusal names --worktree, which setup does not accept", name)
		}
		for _, want := range []string{"commit", "entire enable", ".entire/", ".claude/"} {
			if !strings.Contains(hint, want) {
				t.Fatalf("%s: the hint must say what to do and why the tree is dirty, missing %q: %s", name, want, hint)
			}
		}
	}
}

// TestSetupExplainsARepoWithNoSessionsYet: between `entire enable` and the
// first finished agent session there are no checkpoints, and when the
// checkpoint remote is unreachable too the brain cannot prove the inventory is
// empty rather than unreadable -- so it fails the component with "no readable
// checkpoint IDs from an incomplete persistent-store inventory". That sentence
// is true and tells a new user nothing.
func TestSetupExplainsARepoWithNoSessionsYet(t *testing.T) {
	t.Parallel()
	detail := checkpointScopeIncompleteCode + ": Entire returned no readable checkpoint IDs from an incomplete persistent-store inventory"
	component := newSetupComponent(brainComponentSessions, errors.New(detail), setupBrainBinaryName)
	if component.Hint == "" {
		t.Fatalf("a repo with no sessions yet must be explained, not just reported: %+v", component)
	}
	for _, want := range []string{"no captured sessions", "entire enable", "entire checkpoint list"} {
		if !strings.Contains(component.Hint, want) {
			t.Fatalf("the hint must name the expected state and how to check it, missing %q: %s", want, component.Hint)
		}
	}
	if !strings.Contains(component.Detail, "no readable checkpoint IDs") {
		t.Fatalf("the underlying detail must survive for anyone debugging a real fault: %s", component.Detail)
	}
}

// TestRoutedDiscoveryFailureKeepsItsOwnDetail is the other side of that line: a
// routed discovery that actually FAILED names a fault to fix, and must not be
// softened into "you probably have no sessions yet".
func TestRoutedDiscoveryFailureKeepsItsOwnDetail(t *testing.T) {
	t.Parallel()
	component := newSetupComponent(brainComponentSessions,
		errors.New(`complete routed checkpoint discovery failed: list checkpoints: entire [checkpoint explain --json --search-all]: exec: "entire": executable file not found in $PATH`), setupBrainBinaryName)
	if component.Hint == setupNoSessionsHint(setupBrainBinaryName) {
		t.Fatalf("a real discovery failure is not an empty repo: %+v", component)
	}
}

// TestSetupBuildsTheHistoryIndexTheFirstBriefNeeds. `setup` and a watch tick
// share one deterministic refresh path, and setup used to inherit the tick's
// decision to leave the durable history projection to the memory coordinator.
// The visible result was that the FIRST `entire-brain brief` after a first-ever
// `entire-brain setup` -- the command setup's own "Next" block recommends --
// answered without its transcript half and printed
//
//	warning: history index missing; run `entire brain refresh`
//
// Reproduced against a real captured session in scripts/demo-agent-session.sh:
// the brief returned the distilled fact but no `history` line until a bare
// `entire-brain refresh` was run by hand.
func TestSetupBuildsTheHistoryIndexTheFirstBriefNeeds(t *testing.T) {
	t.Parallel()
	if !watchDeterministicRefreshOptions(setupBuildsHistoryProjection).historyIndex {
		t.Fatal("setup must build the history projection: it runs once and then tells the user to run `brief`, which has nothing to read without it")
	}
	if watchDeterministicRefreshOptions(watchTickBuildsHistoryProjection).historyIndex {
		t.Fatal("a watch TICK must still leave the projection to the coordinator; re-projecting every few minutes bypasses the durable work ledger")
	}
}

// TestFirstRunNeverProjectsHistoryFromNothing is the other half of building the
// projection during setup. On a first run EVERY deterministic source can fail
// at once -- a repo enabled a minute ago has no sessions to export, and the
// worktree `entire enable` leaves dirty blocks the seed baseline too -- and then
// the brain directory the projector reads was never created. Running it anyway
// answered "you have no sessions yet" with a second, rawer line naming an
// internal path:
//
//	x history index: lstat <brainDir>: no such file or directory
//
// Both halves are asserted here, because the guard is only worth having if the
// thing it guards really does fail that way. An empty index over a brain
// directory that DOES exist stays built (TestRefreshSeedsWhenExportFindsNoSessions).
func TestFirstRunNeverProjectsHistoryFromNothing(t *testing.T) {
	t.Parallel()
	absent := filepath.Join(t.TempDir(), "brain-that-was-never-built")
	if historyProjectionTargetExists(absent) {
		t.Fatal("a brain directory that was never created is not a projection target")
	}
	if historyProjectionTargetExists("") {
		t.Fatal("an unresolved brain directory is not a projection target")
	}
	built := t.TempDir()
	if !historyProjectionTargetExists(built) {
		t.Fatal("a brain directory that exists must still be projected into, empty or not")
	}

	if _, err := writeBrainHistoryIndexAndSourceContext(context.Background(), absent, time.Now().UTC(), nil); err == nil {
		t.Fatal("the projector must fail on a brain directory that does not exist; if it no longer does, the guard above can go")
	}
}
