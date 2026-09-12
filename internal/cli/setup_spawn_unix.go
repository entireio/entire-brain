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

// noFollowOpenFlag makes an open REFUSE a symlink at the final path component.
// The backfill log lives in a predictable per-repo state directory and is opened
// O_CREATE|O_APPEND as the user; without this, anything that can plant a symlink
// there redirects the child's stdout+stderr into a file of its choosing and
// appends to it as the user, for hours. It is the one write in this package that
// does not go through writeFileAtomic (a log is appended to, not replaced), so
// it needed the guard spelled out.
const noFollowOpenFlag = syscall.O_NOFOLLOW

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
