//go:build windows

package config

import (
	"errors"
	"time"

	"golang.org/x/sys/windows"
)

// A rename over an existing file fails on Windows while anything else holds the
// target open, and on a developer or CI machine something routinely does:
// antivirus, the search indexer, or a backup agent opening the file moments
// after it is written. Unlike Unix, where rename(2) does not care, that surfaces
// as a hard write failure on a path that is merely unlucky, so the replace is
// retried rather than reported.
//
// These are deliberately the same numbers as the brainstore twin
// (internal/cli/internal/brainstore/atomic_replace_windows.go), which already
// had them: ~500ms of total patience, short enough that a genuinely stuck file
// still fails promptly. The duplication is forced rather than careless —
// brainstore sits under internal/cli/internal, so Go's internal-package rule
// makes it unimportable from here, which is why filelock_windows.go is likewise
// spelled out in three packages. Extracting one shared helper is possible (a new
// top-level internal package both can import) but that is a refactor across
// three packages, not this fix.
const (
	windowsAtomicReplaceAttempts = 100
	windowsAtomicReplaceDelay    = 5 * time.Millisecond
)

func replaceConfigFileAtomic(tmpName, path string) error {
	tmpNamePtr := windows.StringToUTF16Ptr(tmpName)
	pathPtr := windows.StringToUTF16Ptr(path)
	var err error
	for attempt := 0; attempt < windowsAtomicReplaceAttempts; attempt++ {
		err = windows.MoveFileEx(
			tmpNamePtr,
			pathPtr,
			windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH,
		)
		if err == nil || !windowsAtomicReplaceRetryable(err) {
			return err
		}
		time.Sleep(windowsAtomicReplaceDelay)
	}
	return err
}

// The three errors a transient holder produces. Anything else — a missing
// directory, a bad path, a permissions problem that will not clear — is
// returned immediately rather than retried for half a second first.
func windowsAtomicReplaceRetryable(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
