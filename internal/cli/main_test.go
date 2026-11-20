package cli

import (
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/internal/clitest"
)

// TestMain pins every store root at a per-run temporary directory before a
// single test runs, so no test in this package can resolve a store under the
// developer's real home. The pin lives in clitest and is shared by every
// extracted package in the internal/cli tree; see clitest.PinXDGRoots for the
// escape analysis that motivated pinning here rather than fixing call sites,
// and clitest.UseHostXDGDataHome for the one live end-to-end exception.
func TestMain(m *testing.M) {
	os.Exit(clitest.PinXDGRoots(m))
}

// TestTestMainPinsXDGRoots walks clitest.PinnedRoots rather than a list of its
// own, so a variable added to the sandbox is checked here automatically and a
// variable REMOVED from it cannot quietly stop being checked.
//
// The second half is the part that matters and the part that was missing:
// asserting that the variables are set says nothing about whether the resolver
// actually honours them. LOCALAPPDATA went unpinned while the Windows data-root
// branch read it, and this test passed throughout, because it only ever
// resolved the rule for the OS it happened to be running on.
func TestTestMainPinsXDGRoots(t *testing.T) {
	var root string
	for _, key := range clitest.PinnedRoots() {
		dir := os.Getenv(key)
		if !filepath.IsAbs(dir) || filepath.Base(dir) != key {
			t.Fatalf("%s = %q, want an absolute per-run root", key, dir)
		}
		if root == "" {
			root = filepath.Dir(dir)
		}
		if filepath.Dir(dir) != root || !strings.HasPrefix(filepath.Base(root), "entire-brain-cli-test-xdg-") {
			t.Fatalf("%s = %q, want a child of the shared temporary root %q", key, dir, root)
		}
	}
	if root == "" {
		t.Fatal("clitest.PinnedRoots is empty: the sandbox pins nothing")
	}
	// The sandbox must cover the variable the Windows data-root rule reads, or
	// a Windows run resolves the developer's real store while every other
	// platform stays green.
	if os.Getenv(base.EnvLocalAppData) == "" {
		t.Fatalf("%s is not pinned; the Windows data-root branch would escape the sandbox", base.EnvLocalAppData)
	}
	dirs, err := base.ResolvePluginDirs(EntireEnv{})
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{dirs.Config, dirs.Data, dirs.State, dirs.Cache} {
		rel, err := filepath.Rel(root, dir)
		if err != nil || !filepath.IsLocal(rel) {
			t.Fatalf("default plugin directory %q escaped temporary root %q: %v", dir, root, err)
		}
	}
}
