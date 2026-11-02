//go:build darwin || linux

package parentwatch

import "golang.org/x/sys/unix"

// waitReady adapts a blocking readiness syscall to the watcher contract:
// true only when the syscall reports an event, retry on EINTR, and false on
// anything unexpected so Arm degrades instead of killing a healthy process.
func waitReady(block func() (int, error)) bool {
	for {
		n, err := block()
		if n > 0 {
			return true
		}
		if err != unix.EINTR {
			return false
		}
	}
}
