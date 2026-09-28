//go:build darwin || linux

package cli

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
)

const rssChildHelperEnv = "ENTIRE_BRAIN_RSS_CHILD_HELPER"

// The benchmark publishes peak RSS for work that happens in an external
// provider, not in this process. RUSAGE_SELF never includes a child, so the
// figure described the harness rather than the indexer.
//
// Asserted against the children figure directly rather than against this
// process before and after: the parent's own RSS grows while it runs and
// buffers a child, so a before/after comparison rises either way and would
// pass with the bug in place.
func TestPeakRSSIncludesAChildProcess(t *testing.T) {
	if os.Getenv(rssChildHelperEnv) != "" {
		size, _ := strconv.Atoi(os.Getenv(rssChildHelperEnv))
		block := make([]byte, size)
		for i := 0; i < len(block); i += 4096 {
			block[i] = byte(i)
		}
		rssKeepAlive = block[len(block)-1]
		return
	}

	if rusageMaxRSSBytes(syscall.RUSAGE_SELF) == 0 {
		t.Skip("getrusage unavailable on this platform")
	}

	const childBytes = 512 << 20
	cmd := exec.Command(os.Args[0], "-test.run=^TestPeakRSSIncludesAChildProcess$")
	cmd.Env = append(os.Environ(), rssChildHelperEnv+"="+strconv.Itoa(childBytes))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("child helper could not run: %v\n%s", err, out)
	}

	children := rusageMaxRSSBytes(syscall.RUSAGE_CHILDREN)
	if children == 0 {
		t.Skip("this platform reports no child rusage")
	}
	self := rusageMaxRSSBytes(syscall.RUSAGE_SELF)
	if children <= self {
		t.Skipf("the child (%d) did not outgrow the harness (%d); the comparison proves nothing", children, self)
	}

	if got := processMaxRSSBytes(); got < children {
		t.Fatalf("peak RSS reported %d, below the %d a child reached: children are invisible to this measurement",
			got, children)
	}
}

// Package-level so the compiler cannot discard the child's allocation.
var rssKeepAlive byte
