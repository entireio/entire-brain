package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain pins the four XDG roots at a per-run temporary directory before a
// single test runs, so no test in this package can resolve a store under the
// developer's real home.
//
// Isolation here is by convention: a test threads an EntireEnv whose four
// Plugin*Dir fields are t.TempDir()s, and resolveXDGDir returns those without
// consulting the environment. Nothing enforces the convention. Any path that
// reaches resolveXDGDir with an empty pluginDir and no XDG variable set falls
// through to os.UserHomeDir() and resolves under the REAL home — with no error,
// because the callers treat store creation as best-effort.
//
// That is not a hypothetical gap. Instrumenting the fallthrough to record a
// stack and running the package once produced 1,193 escapes from 77 test
// functions across 21 files, and left a real directory behind:
//
//	~/.local/share/entire/plugins/data/brain/repos/local/cli-<hash>/sessions
//
// i.e. `go test ./internal/cli` writes a junk repository into the developer's
// actual brain store. The suite passes either way, so the convention can rot
// without anything reporting it.
//
// Pinning here rather than fixing 77 call sites is deliberate: a call-site fix
// holds only until the next test forgets, while this holds for every test in the
// package including ones not yet written.
//
// XDG rather than HOME, deliberately. resolveXDGDir consults the XDG variable
// BEFORE falling back to the home directory, so pinning these four closes the
// same escape — and leaves HOME alone. Repointing HOME also works, but it
// silently disables entities_live_e2e_test.go: that test feature-detects the
// real provider by running `entire graph capabilities`, which needs the user's
// own config, so a sandboxed HOME turns the package's one live end-to-end proof
// into a permanent skip. Closing a hole by disabling a test is not closing it.
func TestMain(m *testing.M) {
	hostXDGDataHome = hostDataHome()
	root, err := os.MkdirTemp("", "entire-brain-cli-test-xdg-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "pin test XDG roots: %v\n", err)
		os.Exit(1)
	}
	for _, key := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		if err := os.Setenv(key, filepath.Join(root, key)); err != nil {
			fmt.Fprintf(os.Stderr, "pin %s: %v\n", key, err)
			os.Exit(1)
		}
	}
	code := m.Run()
	_ = os.RemoveAll(root)
	os.Exit(code)
}

// hostXDGDataHome is the data root this machine would have used, captured before
// the pin. Only the live end-to-end test needs it: the installed `entire` CLI
// finds its plugins — entire-graph among them — under <data>/entire/plugins, so a
// test that drives the REAL provider has to hand the subprocess the real data
// root back. useHostXDGDataHome does that for the duration of one test.
var hostXDGDataHome string

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

// useHostXDGDataHome restores the machine's own data root for one test, so a test
// that shells the installed provider can still find it. Scoped with t.Setenv, so
// it is undone at the end of the test and the test cannot be parallel.
func useHostXDGDataHome(t *testing.T) {
	t.Helper()
	if hostXDGDataHome == "" {
		t.Skip("no host data root to restore")
	}
	t.Setenv("XDG_DATA_HOME", hostXDGDataHome)
}

func TestTestMainPinsXDGRoots(t *testing.T) {
	var root string
	for _, key := range []string{xdgConfigHome, xdgDataHome, xdgStateHome, xdgCacheHome} {
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
	dirs, err := resolvePluginDirs(EntireEnv{})
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

func TestHostDataHomeSelection(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no host home directory")
	}
	abs := t.TempDir()
	for _, tt := range []struct{ name, value, want string }{
		{"absolute", abs, abs},
		{"unset", "", filepath.Join(home, ".local", "share")},
		{"relative", "relative-data", filepath.Join(home, ".local", "share")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(xdgDataHome, tt.value)
			if got := hostDataHome(); got != tt.want {
				t.Fatalf("hostDataHome() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHostXDGDataHomeOverrideIsScoped(t *testing.T) {
	original := os.Getenv(xdgDataHome)
	t.Run("live provider", func(t *testing.T) {
		useHostXDGDataHome(t)
		if got := os.Getenv(xdgDataHome); got != hostXDGDataHome {
			t.Fatalf("live provider data root = %q, want %q", got, hostXDGDataHome)
		}
	})
	if got := os.Getenv(xdgDataHome); got != original {
		t.Fatalf("live provider override leaked: got %q, want %q", got, original)
	}
}
