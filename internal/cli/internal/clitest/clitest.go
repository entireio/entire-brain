// Package clitest holds test support shared by the internal/cli package
// tree. It exists for the COR-2268 split: every package in the tree —
// including internal/cli itself — pins the store roots through PinXDGRoots, so
// no test can resolve a store under the developer's real home.
//
// Each package's TestMain delegates here. Native Go test runs use the same
// isolated storage on Linux, macOS, and Windows — and "and Windows" is load
// bearing, not a courtesy: see the pinnedRoots comment.
package clitest

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// hostXDGDataHome is the data root this machine would have used, captured by
// PinXDGRoots before the pin. Tests that shell out to the installed `entire`
// CLI need it back: the CLI finds its plugins under <data>/entire/plugins.
var hostXDGDataHome string

// pinnedRoots are the environment variables that decide where a default store
// lands. The four XDG ones are the Unix rule; LOCALAPPDATA is the Windows one.
//
// LOCALAPPDATA is here because leaving it out broke this sandbox once. The
// brain's data root deliberately ignores XDG on Windows and reads LOCALAPPDATA,
// to agree with the host CLI (base.resolvePluginDataRoot). This list pinned only
// the XDG four, so on Windows — and only on Windows — every test that resolved
// the default store walked straight out of the sandbox and into the developer's
// real %LOCALAPPDATA%\entire. It was not theoretical: a plain `go test ./...` on
// a Windows machine created and populated a real brain store.
//
// It is pinned on EVERY OS, not under a GOOS check. A pin that only exists on
// the platform that needs it cannot be checked by the platforms that run most
// of the CI, and this package's whole job is to be checkable from anywhere.
var pinnedRoots = []string{
	"XDG_CONFIG_HOME",
	"XDG_DATA_HOME",
	"XDG_STATE_HOME",
	"XDG_CACHE_HOME",
	"LOCALAPPDATA", // base.EnvLocalAppData; spelled out to avoid an import cycle
}

// PinnedRoots returns the variables PinXDGRoots pins, for tests that assert the
// sandbox holds. The slice is copied so a caller cannot shrink the sandbox.
func PinnedRoots() []string { return append([]string(nil), pinnedRoots...) }

// PinXDGRoots pins every store root at a per-run temporary directory before a
// single test runs, runs the suite, and removes the directory. Use it from a
// package's TestMain:
//
//	func TestMain(m *testing.M) { os.Exit(clitest.PinXDGRoots(m)) }
//
// The name is historical: it pins LOCALAPPDATA too. See pinnedRoots.
func PinXDGRoots(m *testing.M) int {
	hostXDGDataHome = hostDataHome()
	root, err := os.MkdirTemp("", "entire-brain-cli-test-xdg-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "pin test store roots: %v\n", err)
		return 1
	}
	for _, key := range pinnedRoots {
		if err := os.Setenv(key, filepath.Join(root, key)); err != nil {
			fmt.Fprintf(os.Stderr, "pin %s: %v\n", key, err)
			return 1
		}
	}
	code := m.Run()
	_ = os.RemoveAll(root)
	return code
}

func hostDataHome() string {
	if dir := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(dir) {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share")
}

// UseHostXDGDataHome restores the machine's own data root for one test, so a
// test that shells the installed provider can still find it. Scoped with
// t.Setenv, so it is undone at the end of the test and the test cannot be
// parallel.
func UseHostXDGDataHome(t *testing.T) {
	t.Helper()
	if hostXDGDataHome == "" {
		t.Skip("no host data root to restore")
	}
	t.Setenv("XDG_DATA_HOME", hostXDGDataHome)
}
