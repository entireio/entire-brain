//go:build !darwin && !linux && !windows

package parentwatch

import "errors"

// No parent-exit primitive is wired up for this platform; Arm degrades to no
// watchdog, which is the pre-watchdog behavior.
func watchParent(int) (func(), error) {
	return nil, errors.New("parentwatch: unsupported platform")
}
