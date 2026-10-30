package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/command/watch"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
)

// A stale binary must end the supervised loop — the one loop the installed
// service runs — so the service manager restarts it on the current build,
// instead of the old build failing every tick forever.
func TestSupervisedWatchExitsWhenBinaryReplaced(t *testing.T) {
	f := newSetupTestFixture(t)
	if _, err := recordSetupWatchPlan(f.env, daemonDefaultName, setupWatchPlanEntry{Workspace: "alpha", Interval: "5m"}); err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	passes := 0
	err := supervisedWatchLoop(context.Background(), out, f.env, watch.DefaultFlags(),
		func(string, watch.Flags, *int) error { passes++; return nil },
		nil, func() bool { return true })
	if !errors.Is(err, watch.ErrBinaryReplaced) {
		t.Fatalf("a replaced binary must end the loop with its sentinel, got %v", err)
	}
	if passes != 0 {
		t.Fatalf("an outdated build ran %d workspace passes before exiting", passes)
	}
	if !strings.Contains(out.String(), "binary replaced") {
		t.Fatalf("the log must say why the daemon exited: %q", out.String())
	}
}

// Registration is not liveness: a job whose spawn failed (e.g. launchd's cdhash
// pin after an ad-hoc-signed binary was rebuilt) still lists successfully, and
// reporting it as running is what hid the wedge behind "daemon ✓ running".
func TestDaemonRunningRequiresALivePID(t *testing.T) {
	home := t.TempDir()
	spec := fixedDaemonSpec()
	spec.LogPath = filepath.Join(home, "logs", "watch.log")
	plan, err := planBrainWatchDaemon("darwin", home, "", "", spec)
	if err != nil {
		t.Fatal(err)
	}
	dead := &scriptedListRunner{listOutput: "{\n\t\"LastExitStatus\" = 0;\n}\n"}
	running, detail := daemonRunning(context.Background(), dead, plan)
	if running {
		t.Fatal("a registered-but-dead job must not report as running")
	}
	if detail == "" {
		t.Fatal("the registered-but-dead state must be named, not silent")
	}
	live := &scriptedListRunner{listOutput: "{\n\t\"PID\" = 123;\n}\n"}
	if running, _ := daemonRunning(context.Background(), live, plan); !running {
		t.Fatal("a job with a live PID must report as running")
	}
}

// The re-register child must repair the registration under the shared lock —
// but only while the unit still exists: a concurrent `setup --uninstall-daemon`
// removing the unit means the user declined the watcher, and re-registering it
// would resurrect what they just removed.
func TestDaemonReregisterRespectsUninstall(t *testing.T) {
	f := newSetupTestFixture(t)
	t.Setenv(envDaemonNoRegister, "")
	runner := &recordingDaemonRunner{}
	opts := f.opts
	opts.Runner = runner

	plan, err := installedDaemonUnitPlan(opts)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if err := runDaemonReregister(context.Background(), io.Discard, opts); err != nil {
		t.Fatalf("reregister with no unit installed: %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("no unit on disk, but the service manager was touched: %v", runner.calls)
	}

	if !plan.supported() {
		// windows: no service manager, so the calm no-op above is the whole
		// contract — there is no unit to write and nothing to re-register.
		return
	}
	if err := os.MkdirAll(filepath.Dir(plan.UnitPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := brainstore.WriteFileAtomic(plan.UnitPath, []byte(plan.Contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runDaemonReregister(context.Background(), io.Discard, opts); err != nil {
		t.Fatalf("reregister: %v", err)
	}
	// The plan resolves for the HOST service manager, so assert the verbs that
	// manager uses: the CI matrix covers darwin and linux with one test.
	verbs := []string{"launchctl bootout", "launchctl enable", "launchctl bootstrap"}
	if plan.Manager == daemonManagerSystemd {
		verbs = []string{"systemctl --user daemon-reload", "systemctl --user enable --now"}
	}
	got := strings.Join(runner.calls, "\n")
	for _, verb := range verbs {
		if !strings.Contains(got, verb) {
			t.Fatalf("re-registration must run %q:\n%s", verb, got)
		}
	}
}

// Uninstall must treat an already-unregistered job as success: bootout fails
// for absence too, and absence is exactly what uninstall wants. Only a job the
// service manager still lists turns a failed bootout into an error.
func TestUninstallDaemonToleratesAnUnregisteredJob(t *testing.T) {
	plan, err := planBrainWatchDaemon("darwin", t.TempDir(), "", "", fixedDaemonSpec())
	if err != nil {
		t.Fatal(err)
	}
	if err := brainstore.WriteFileAtomic(plan.UnitPath, []byte(plan.Contents), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &recordingDaemonRunner{
		stopErr: errors.New("Boot-out failed: 36: No such process"),
		listErr: errors.New("Could not find service"),
	}
	if err := uninstallDaemon(context.Background(), runner, plan); err != nil {
		t.Fatalf("uninstall of an unregistered job must succeed: %v", err)
	}
	if _, err := os.Stat(plan.UnitPath); !os.IsNotExist(err) {
		t.Fatalf("unit must be removed: %v", err)
	}
}

type scriptedListRunner struct {
	listOutput string
}

func (r *scriptedListRunner) Run(_ context.Context, _, _ string, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && args[0] == "list" {
		return []byte(r.listOutput), nil, nil
	}
	return nil, nil, nil
}
