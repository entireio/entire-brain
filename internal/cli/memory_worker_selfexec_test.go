package cli

import (
	"strings"
	"testing"
)

// The regression this guards is not a wrong value, it is a process chain.
//
// launchDetachedSelf re-execs os.Executable(). Under `go test` that is the
// package's own test binary, which does not parse "memory worker --once" and
// re-runs the whole package instead — reaching launchDetachedSelf again. A
// Windows run produced 34 chained test binaries that outlived the suite, timed
// the package out, and wrote into the user's home directory on the way.
//
// This test calls the real, uninjected path, so it fails if the refusal is ever
// removed — and it is the only test here that can, because every other test in
// the package stubs memoryWorkerLaunch.
func TestLaunchDetachedSelfRefusesUnderGoTest(t *testing.T) {
	t.Parallel()

	if !underGoTest() {
		t.Fatal("underGoTest() is false inside a go test binary; the detection is broken " +
			"and the self-relaunch guard cannot fire")
	}

	err := launchDetachedSelf(t.TempDir(), "memory", "worker", "--once")
	if err == nil {
		t.Fatal("launchDetachedSelf must refuse under go test; it just spawned a copy of this test binary")
	}
	if !strings.Contains(err.Error(), "refusing to re-exec") {
		t.Fatalf("refusal should say why, got: %v", err)
	}
}

// Both detached launch sites share launchDetachedSelf, so neither can spawn a
// chain. memoryWorkerLaunchAfter is the one reached from notify, which is what
// ran on the Windows machine.
func TestMemoryWorkerLaunchAfterIsCoveredByTheSameRefusal(t *testing.T) {
	t.Parallel()

	if err := memoryWorkerLaunchAfter(t.TempDir(), 0, 0); err == nil {
		t.Fatal("memoryWorkerLaunchAfter must inherit the refusal; it just spawned a test binary")
	}
}

func TestIsGoTestBinaryName(t *testing.T) {
	t.Parallel()

	cases := map[string]bool{
		`C:\Users\sam\AppData\Local\Temp\go-build123\b001\cli.test.exe`: true,
		"/tmp/go-build123/b001/cli.test":                                true,
		`C:\Users\sam\AppData\Local\Temp\go-build123\b001\CLI.TEST.EXE`: true,
		"/usr/local/bin/entire-brain":                                   false,
		`C:\Program Files\entire\entire-brain.exe`:                      false,
		// A product binary whose name merely contains "test" is not a test binary.
		"/usr/local/bin/entire-brain-testdata": false,
	}
	for exe, want := range cases {
		if got := isGoTestBinaryName(exe); got != want {
			t.Errorf("isGoTestBinaryName(%q) = %v, want %v", exe, got, want)
		}
	}
}
