// Package clitest holds test support shared by the internal/cli package
// tree. It exists for the COR-2268 split: every package in the tree —
// including internal/cli itself — pins the XDG roots through PinXDGRoots, so
// no test can resolve a store under the developer's real home.
//
// Each package's TestMain delegates here. Native Go test runs use the same
// isolated storage on Linux, macOS, and Windows.
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

// PinXDGRoots pins the four XDG roots at a per-run temporary directory before
// a single test runs, runs the suite, and removes the directory. Use it from
// a package's TestMain:
//
//	func TestMain(m *testing.M) { os.Exit(clitest.PinXDGRoots(m)) }
func PinXDGRoots(m *testing.M) int {
	hostXDGDataHome = hostDataHome()
	root, err := os.MkdirTemp("", "entire-brain-cli-test-xdg-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "pin test XDG roots: %v\n", err)
		return 1
	}
	for _, key := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
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
