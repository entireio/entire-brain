//go:build windows

package cli

import (
	"os"
	"syscall"
)

// detachedSysProcAttr has no session concept to use on Windows; the child is
// already detached from the console because its stdio is redirected and the
// parent releases the process handle.
func detachedSysProcAttr() *syscall.SysProcAttr {
	return nil
}

// processAlive reports whether a recorded backfill pid is still running.
// os.FindProcess opens a handle on Windows, so an error means the process is
// gone.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.FindProcess(pid)
	return err == nil
}
