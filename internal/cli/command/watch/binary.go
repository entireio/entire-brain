package watch

import (
	"errors"
	"os"
)

// ErrBinaryReplaced ends the supervised loop when the executable this
// daemon was started from has been replaced on disk. A long-running daemon
// otherwise outlives its binary: after an upgrade it keeps running the old
// build, whose writer-path manifest load correctly refuses the new build's
// fields, and every tick fails forever while `status` still says running
// (COR-2174). Exiting hands the restart to the service manager, which spawns
// the current binary.
var ErrBinaryReplaced = errors.New("binary replaced on disk")

// BinaryChanged returns a probe reporting whether this process's
// executable has been replaced since the probe was created, or nil when the
// executable cannot be resolved (then the staleness check is simply off).
func BinaryChanged() func() bool {
	path, err := os.Executable()
	if err != nil {
		return nil
	}
	return watchBinaryChangeProbe(path)
}

func watchBinaryChangeProbe(path string) func() bool {
	started, err := os.Stat(path)
	if err != nil {
		return nil
	}
	return func() bool {
		current, err := os.Stat(path)
		if err != nil {
			// Mid-replace or unreadable: decide on a later pass rather than
			// restarting onto a binary that may be half-written.
			return false
		}
		// All three signals carry weight: SameFile catches install-by-rename,
		// ModTime catches the in-place rebuild, Size catches an in-place copy
		// that preserved mtime (cp -p, rsync -t).
		return !os.SameFile(started, current) ||
			!current.ModTime().Equal(started.ModTime()) ||
			current.Size() != started.Size()
	}
}
