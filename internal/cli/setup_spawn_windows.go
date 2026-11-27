//go:build windows

package cli

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// detachedSysProcAttr gives the backfill no console at all, which is what
// spawnDetached's "surviving the terminal that started it" actually requires
// here. Windows has no session to join, so the Unix Setsid has two separate
// analogues and only together do they match it:
//
//   - CREATE_NEW_PROCESS_GROUP stops a Ctrl-C aimed at setup reaching the child,
//     the direct counterpart of leaving the parent's process group.
//   - DETACHED_PROCESS is the part that was missing. A child keeps the parent's
//     console unless it is told otherwise, and every process attached to a
//     console is sent CTRL_CLOSE_EVENT and then killed when that console closes
//     — so closing the terminal took the backfill with it. Redirected stdio does
//     not change that: it moves where output goes, not which console the process
//     is attached to, which is what the previous comment here got wrong.
//
// Safe precisely because spawnDetached already points stdio at a log file and
// the null device, so a console-less child has nowhere it still needs to write.
//
// Deliberately NOT CREATE_NO_WINDOW: that asks for a console without a window,
// which contradicts DETACHED_PROCESS asking for no console. Microsoft documents
// the combination as undefined, and entireio/cli's execx_windows.go — the same
// detach, already shipping — uses this pair.
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS}
}

// noFollowOpenFlag has no Windows equivalent in the syscall package; symlink
// creation there requires a privilege the attack this guards against does not
// have by default.
const noFollowOpenFlag = 0
