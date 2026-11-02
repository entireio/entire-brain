// Package parentwatch exits the process when the process that dispatched it
// dies. The Entire CLI forwards SIGTERM to this plugin, but a SIGKILL to the
// CLI cannot be forwarded: the plugin reparents to pid 1 and keeps running,
// holding the brain write lock until it finishes. The watchdog blocks on an
// OS parent-exit event (no polling) and aborts the moment the parent is gone.
package parentwatch

import (
	"fmt"
	"os"
)

// DetachedEnv marks a child that was spawned to outlive its parent
// (spawnDetached sets it). Such a child must never arm the watchdog.
const DetachedEnv = "ENTIRE_BRAIN_DETACHED"

// shouldArm decides whether this process dies with its parent. A ppid of 1
// means launchd or systemd started us as a daemon; the detached marker means
// our spawner deliberately handed us off to survive it.
func shouldArm(ppid int, getenv func(string) string) bool {
	return ppid != 1 && getenv(DetachedEnv) == ""
}

// Arm installs the watchdog: one goroutine blocking on the platform's
// parent-exit event, exiting the process when it fires. Registration is
// synchronous; only the wait runs in the goroutine. When the watcher cannot
// be registered and the parent is still alive, Arm degrades to no watchdog —
// never to killing a healthy process.
func Arm() {
	ppid := os.Getppid()
	if !shouldArm(ppid, os.Getenv) {
		return
	}
	wait, err := watchParent(ppid)
	// Close the registration race: if the parent died before (or while) the
	// watcher was registered, the event may never fire — and a registration
	// error (e.g. ESRCH) means the same thing. The ppid re-check decides.
	if os.Getppid() != ppid {
		exitWithParent()
	}
	if err != nil {
		return
	}
	go func() {
		wait()
		exitWithParent()
	}()
}

func exitWithParent() {
	fmt.Fprintln(os.Stderr, "parent process exited; aborting")
	os.Exit(1)
}
