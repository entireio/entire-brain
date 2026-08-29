package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
