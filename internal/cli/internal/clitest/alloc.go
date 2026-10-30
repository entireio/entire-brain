package clitest

import (
	"runtime"
	"testing"
)

// AllocatedBy measures cumulative bytes allocated during fn, not peak live memory.
func AllocatedBy(t *testing.T, fn func()) uint64 {
	t.Helper()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}
