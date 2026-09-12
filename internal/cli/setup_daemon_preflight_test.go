package cli

import (
	"strings"
	"testing"
)

// TestSetupAnnouncesTheWatcherBeforeInstallingIt: one `entire brain setup` on a
// brand-new repository registered a persistent launchd agent in the user's real
// ~/Library/LaunchAgents and started it, and the only mention of that arrived
// mid-summary, AFTER it had happened. The default is deliberately unchanged —
// whether the watcher installs unasked is a product decision — but the
// consequence is now stated before the work starts, with the flag that declines
// it and the command that removes it on the same screen.
//
// Deleting the pre-flight call in runSetup still compiles, and fails here.
func TestSetupAnnouncesTheWatcherBeforeInstallingIt(t *testing.T) {
	f := newSetupTestFixture(t)
	rec := &recordedSetup{}
	printed := runSetupForTest(t, f, defaultSetupOptions(), rec)

	announce := strings.Index(printed, "this run will install")
	if announce < 0 {
		t.Fatalf("setup must say what it is about to register with the machine before it does it:\n%s", printed)
	}
	work := strings.Index(printed, "instant core")
	if work < 0 || announce > work {
		t.Fatalf("the announcement must come BEFORE the work, not in the closing summary:\n%s", printed)
	}
	for _, want := range []string{
		"starts again at every login",
		"--no-daemon",
		"--uninstall-daemon",
		rec.state.UnitPath,
	} {
		if want == "" {
			t.Fatal("fixture produced no unit path to name")
		}
		if !strings.Contains(printed[:announce+len("this run will install")+400], want) {
			t.Fatalf("the pre-flight line must name %q:\n%s", want, printed)
		}
	}
}

// TestSetupDoesNotClaimToInstallAWatcherItAlreadyHas: a line that says "will
// install" on every re-run is a line readers stop reading.
func TestSetupDoesNotClaimToInstallAWatcherItAlreadyHas(t *testing.T) {
	f := newSetupTestFixture(t)
	rec := &recordedSetup{}
	_ = runSetupForTest(t, f, defaultSetupOptions(), rec)
	second := runSetupForTest(t, f, defaultSetupOptions(), rec)

	if strings.Contains(second, "this run will install") {
		t.Fatalf("a re-run finds the watcher already installed and must not claim otherwise:\n%s", second)
	}
	if !strings.Contains(second, "this run keeps") || !strings.Contains(second, "--uninstall-daemon") {
		t.Fatalf("a re-run must still say the service is there and how to remove it:\n%s", second)
	}
}

// TestSetupSaysNothingAboutAWatcherItWillNotInstall: --no-daemon installs
// nothing, so a warning about a persistent service would be noise.
func TestSetupSaysNothingAboutAWatcherItWillNotInstall(t *testing.T) {
	f := newSetupTestFixture(t)
	opts := defaultSetupOptions()
	opts.noDaemon = true
	printed := runSetupForTest(t, f, opts, &recordedSetup{})

	if strings.Contains(printed, "this run will install") {
		t.Fatalf("--no-daemon installs nothing and must not warn about a service:\n%s", printed)
	}
}

// TestDaemonPreflightIsSilentWhereThereIsNoWatcher: on a platform with no
// service manager this file plans for, there is nothing to announce.
func TestDaemonPreflightIsSilentWhereThereIsNoWatcher(t *testing.T) {
	t.Parallel()
	unsupported := daemonPlan{OS: "plan9", Manager: daemonManagerUnsupported}
	if line := setupDaemonPreflightLine(unsupported, daemonState{}, setupBrainBinaryName); line != "" {
		t.Fatalf("nothing is installed on an unsupported platform, so nothing is announced: %q", line)
	}
}

// TestDaemonPreflightFollowsTheReadersSpelling ties the announcement to the
// vocabulary fix: the removal command it names must be one the reader can run.
func TestDaemonPreflightFollowsTheReadersSpelling(t *testing.T) {
	t.Parallel()
	plan := daemonPlan{
		OS:       "darwin",
		Manager:  daemonManagerLaunchd,
		Label:    "io.entire.brain-watch.1a2b3c4d",
		UnitPath: "/Users/u/Library/LaunchAgents/io.entire.brain-watch.1a2b3c4d.plist",
	}
	line := setupDaemonPreflightLine(plan, daemonState{}, setupPluginCommand)
	if !strings.Contains(line, setupPluginCommand+" setup --uninstall-daemon") {
		t.Fatalf("the removal command must be spelled the way the reader reached this binary: %s", line)
	}
	if !strings.Contains(line, plan.UnitPath) || !strings.Contains(line, "--no-daemon") {
		t.Fatalf("the pre-flight must name the artifact and the opt-out: %s", line)
	}
}
