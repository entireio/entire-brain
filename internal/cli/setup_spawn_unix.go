//go:build !windows

package cli

import (
	"os"
	"syscall"
)

// detachedSysProcAttr puts the backfill in its own session so it outlives the
// shell that started setup (the setsid/nohup pattern, without shelling out).
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// processAlive reports whether a recorded backfill pid is still running.
// Signal 0 performs the permission/existence check without delivering anything.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
