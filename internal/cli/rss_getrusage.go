//go:build darwin || linux

package cli

import (
	"runtime"
	"syscall"
)

// processMaxRSSBytes reports peak resident memory across this process and the
// children it has waited for.
//
// RUSAGE_SELF alone was wrong for the thing this measures: indexing runs in an
// external provider spawned by streamSemanticSnapshot, so the work whose memory
// the benchmark publishes happens in a child. Self-only reported the harness.
//
// The children figure is the largest single waited-for child rather than their
// sum, and a child still running is not counted at all — so this is "the peak
// either this process or its biggest finished child reached", which is the
// honest reading of the number and not a total for the pipeline.
func processMaxRSSBytes() uint64 {
	self := rusageMaxRSSBytes(syscall.RUSAGE_SELF)
	children := rusageMaxRSSBytes(syscall.RUSAGE_CHILDREN)
	return max(self, children)
}

func rusageMaxRSSBytes(who int) uint64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(who, &ru); err != nil {
		return 0
	}
	if ru.Maxrss <= 0 {
		return 0
	}
	rss := uint64(ru.Maxrss)
	if runtime.GOOS == "linux" {
		// Linux reports kilobytes; darwin reports bytes.
		rss *= 1024
	}
	return rss
}
