package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// spawnDetached starts the background backfill and returns immediately with the
// child's pid. Detachment is the whole point: the prompt must come back the
// instant the deterministic core is built, while an O(sessions) distill pass
// keeps running — surviving the terminal that started it.
//
// Three properties make that true:
//   - a new session/process group (see detachedSysProcAttr), so the child is not
//     killed by the shell's SIGHUP or a Ctrl-C aimed at setup;
//   - stdio pointed at a log file and /dev/null, never at the parent's terminal,
//     so a background run can never scribble over the user's prompt;
//   - Release(), so no goroutine is left waiting on the child.
func spawnDetached(plan setupBackfillPlan) (int, error) {
	if plan.Binary == "" {
		return 0, fmt.Errorf("no executable to run")
	}
	if plan.LogPath != "" {
		if err := os.MkdirAll(filepath.Dir(plan.LogPath), 0o700); err != nil {
			return 0, fmt.Errorf("create log directory: %w", err)
		}
	}
	cmd := exec.Command(plan.Binary, plan.Args...)
	cmd.Dir = plan.Dir
	cmd.Env = plan.Env
	cmd.SysProcAttr = detachedSysProcAttr()

	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", os.DevNull, err)
	}
	defer func() { _ = devNull.Close() }()
	cmd.Stdin = devNull

	var log *os.File
	if plan.LogPath != "" {
		log, err = os.OpenFile(plan.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND|noFollowOpenFlag, 0o600)
		if err != nil {
			return 0, fmt.Errorf("open backfill log: %w", err)
		}
		defer func() { _ = log.Close() }()
		cmd.Stdout = log
		cmd.Stderr = log
	} else {
		cmd.Stdout = devNull
		cmd.Stderr = devNull
	}

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start background backfill: %w", err)
	}
	pid := cmd.Process.Pid
	// Release the handle: setup exits in seconds and must not hold a child
	// reference for the hours the backfill may run.
	if err := cmd.Process.Release(); err != nil {
		return pid, nil
	}
	return pid, nil
}

// backfillRunning answers "is the detached backfill this repo recorded STILL
// running", which is the question both `setup`'s re-entrancy guard and `status`
// actually ask. processAlive alone cannot answer it.
//
// processAlive is a bare kill(pid, 0): it asks whether SOMETHING owns that pid,
// not whether it is our child. Pids are reused, and a reboot reuses low ones
// almost immediately — so a setup.json written before a reboot names a pid that
// after the reboot belongs to a system daemon. processAlive says true, `setup`
// skips the backfill as "already running (pid 412, started <yesterday>)" for as
// long as that process lives, and `status` reports a backfill that does not
// exist. The repo's facts are then never backfilled again and nothing says why.
//
// The cross-check is the distill PASS LOCK. A live backfill is a distill pass,
// and a distill pass holds that lock for its whole duration. So: pid gone means
// gone; pid present AND the lock held means running; pid present and the lock
// FREE means the record is stale — some other process inherited the number.
//
// Known race, deliberately accepted: between the child's Start() and its
// acquisition of the pass lock, this reports "not running" and a second `setup`
// spawns a rival child. That is benign — the pass lock makes whichever child
// arrives second skip its pass — and it is a far smaller cost than the failure
// it replaces, which is permanent and silent.
func backfillRunning(brainDir string, pid int) bool {
	if !processAlive(pid) {
		return false
	}
	if strings.TrimSpace(brainDir) == "" {
		// No brain to check the lock in: fall back to the pid answer rather than
		// claiming a backfill is dead on no evidence.
		return true
	}
	lock, err := acquireFileLock(filepath.Join(brainDir, brainLockDirName, brainDistillLockName), "distill_pass_locked", 0)
	if err != nil {
		if errors.Is(err, errFileLockTimeout) {
			// Contended: a distill pass IS running. That it might be the watch
			// daemon's pass rather than our child does not change the answer a
			// caller needs — spawning another backfill now would only produce a
			// child that skips on this same lock.
			return true
		}
		// The lock is unreadable (permissions, a path that is not a directory).
		// Do not turn an unrelated failure into "your backfill is dead".
		return true
	}
	_ = lock.Close()
	return false
}
