package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/entireio/entire-brain/internal/cli/command/daemonreregister"
)

// relaunchInstalledDaemon re-registers the launchd agent after the supervised
// daemon exits over a replaced binary.
//
// Exiting is not enough on darwin: launchd records the cdhash of an
// ad-hoc-signed service binary at registration, so after the binary is rebuilt
// every KeepAlive respawn fails with OS_REASON_CODESIGNING ("needs LWCR
// update") until the agent is re-registered (COR-2174). The re-registration
// has to run from a detached child because bootout kills this very process —
// and the child is the replaced binary itself running `daemon-reregister`, so
// the verbs, the registration lock, and the unit rendering all stay in
// installDaemon rather than being restated in a shell string. The child's
// output goes to the daemon log, so a failed bootstrap is diagnosable there.
//
// On linux a plain exit suffices — systemd's Restart=always relaunches the
// current binary with no signing pin — so everything but darwin is a no-op.
// Failures only cost the self-restart: the daemon still exits, launchd still
// attempts a respawn, and `setup` re-registers on its next run.
func relaunchInstalledDaemon(out io.Writer, env EntireEnv) {
	if runtime.GOOS != "darwin" || daemonRegistrationDisabled() {
		return
	}
	binary, err := os.Executable()
	if err != nil {
		fmt.Fprintf(out, "[watch] launchd re-registration skipped: %v\n", err)
		return
	}
	fmt.Fprintln(out, "[watch] re-registering with launchd so the restart lands on the current binary")
	logPath, _ := daemonLogPath(env)
	if _, err := spawnDetached(setupBackfillPlan{
		Binary:  binary,
		Args:    []string{"daemon-reregister"},
		LogPath: logPath,
	}); err != nil {
		fmt.Fprintf(out, "[watch] launchd re-registration failed (re-run `entire-brain setup` to restart the watcher): %v\n", err)
	}
}

// daemonReregisterConfig binds the re-registration engine at the assembly
// boundary: the command package never imports cli, and the unit plan, the
// registration lock, and the installer verbs stay here with `setup`.
func daemonReregisterConfig(opts Options) daemonreregister.Config {
	return daemonreregister.Config{Options: opts, Deps: daemonreregister.Deps{
		Reregister: runDaemonReregister,
	}}
}

// runDaemonReregister reinstalls the machine's one watcher under the same lock
// `setup` uses, after rechecking that the unit still exists. The recheck is
// what keeps this safe against a concurrent `setup --uninstall-daemon`:
// re-registering a unit the user just removed would resurrect a daemon they
// declined, so a missing unit file means there is nothing to restart.
func runDaemonReregister(ctx context.Context, out io.Writer, opts Options) error {
	plan, err := installedDaemonUnitPlan(opts)
	if err != nil {
		return err
	}
	if plan.NoRegister {
		return nil
	}
	return withDaemonRegistrationLock(opts.Env, func() error {
		if _, err := os.Stat(plan.UnitPath); err != nil {
			fmt.Fprintf(out, "daemon-reregister: nothing to restart: %v\n", err)
			return nil
		}
		if err := installDaemon(ctx, opts.Runner, plan); err != nil {
			return fmt.Errorf("daemon-reregister: %w", err)
		}
		fmt.Fprintf(out, "daemon-reregister: re-registered %s\n", plan.Label)
		return nil
	})
}

// installedDaemonUnitPlan resolves the plan for the daemon this machine has
// installed: the one plan builder, under the daemon name the machine watch
// plan records (empty falls back to the default inside the builder).
func installedDaemonUnitPlan(opts Options) (daemonPlan, error) {
	watchPlan, err := loadSetupWatchPlan(opts.Env)
	if err != nil {
		return daemonPlan{}, err
	}
	setupOpts := defaultSetupOptions()
	if watchPlan.DaemonName != "" {
		setupOpts.daemonName = watchPlan.DaemonName
	}
	return brainWatchDaemonPlan(opts, setupOpts)
}
