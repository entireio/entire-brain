package base

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestEnvFromOS(t *testing.T) {
	t.Setenv(EnvCLIVersion, "cli-test")
	t.Setenv(EnvRepoRoot, "/tmp/repo")
	t.Setenv(EnvPluginConfigDir, "/tmp/config")
	t.Setenv(EnvPluginDataDir, "/tmp/data")
	t.Setenv(EnvPluginStateDir, "/tmp/state")
	t.Setenv(EnvPluginCacheDir, "/tmp/cache")

	env := EnvFromOS()
	if env.CLIVersion != "cli-test" {
		t.Fatalf("CLIVersion = %q", env.CLIVersion)
	}
	if env.RepoRoot != "/tmp/repo" {
		t.Fatalf("RepoRoot = %q", env.RepoRoot)
	}
	if env.PluginConfigDir != "/tmp/config" {
		t.Fatalf("PluginConfigDir = %q", env.PluginConfigDir)
	}
	if env.PluginDataDir != "/tmp/data" {
		t.Fatalf("PluginDataDir = %q", env.PluginDataDir)
	}
	if env.PluginStateDir != "/tmp/state" {
		t.Fatalf("PluginStateDir = %q", env.PluginStateDir)
	}
	if env.PluginCacheDir != "/tmp/cache" {
		t.Fatalf("PluginCacheDir = %q", env.PluginCacheDir)
	}
}
func TestResolvePluginDirsUsesXDGDefaultsUnderEntireRoot(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv(XDGConfigHome, filepath.Join(xdg, "config"))
	t.Setenv(XDGDataHome, filepath.Join(xdg, "data"))
	t.Setenv(XDGStateHome, filepath.Join(xdg, "state"))
	t.Setenv(XDGCacheHome, filepath.Join(xdg, "cache"))

	dirs, err := ResolvePluginDirs(EntireEnv{})
	if err != nil {
		t.Fatalf("resolve plugin dirs: %v", err)
	}
	if dirs.Config != filepath.Join(xdg, "config", "entire") {
		t.Fatalf("Config = %q", dirs.Config)
	}
	// Data mirrors the host's plugin-data namespace so a standalone/dev run hits
	// the real brain store; config/state/cache stay at the bare XDG root (the host
	// leaves those env vars unset too).
	//
	// The data root is the one dir whose rule is OS-dependent, because the host
	// ignores XDG on Windows and reads %LOCALAPPDATA% instead. Config, state and
	// cache stay on the XDG rule everywhere, which is why only this assertion
	// branches.
	wantDataRoot := filepath.Join(xdg, "data", "entire")
	if runtime.GOOS == windowsGOOS {
		localAppData := filepath.Join(xdg, "localappdata")
		t.Setenv(EnvLocalAppData, localAppData)
		wantDataRoot = filepath.Join(localAppData, "entire")
		// Re-resolve: the env var above was set after the first call.
		dirs, err = ResolvePluginDirs(EntireEnv{})
		if err != nil {
			t.Fatalf("resolve plugin dirs: %v", err)
		}
	}
	if dirs.Data != filepath.Join(wantDataRoot, "plugins", "data", PluginDataName) {
		t.Fatalf("Data = %q, want under %q", dirs.Data, wantDataRoot)
	}
	if dirs.State != filepath.Join(xdg, "state", "entire") {
		t.Fatalf("State = %q", dirs.State)
	}
	if dirs.Cache != filepath.Join(xdg, "cache", "entire") {
		t.Fatalf("Cache = %q", dirs.Cache)
	}
}

// The brain and the host CLI must resolve the same plugin data root, or a
// standalone brain reads an empty store and reports "no brain built for this
// repository" — a confident wrong answer rather than an error. On Unix both
// sides already use XDG then ~/.local/share; Windows is where they diverged.
//
// These exercise the Windows rule from any machine by passing the target OS
// explicitly, the same way planBrainWatchDaemon is golden-tested for darwin and
// linux from one host. One caveat they cannot escape: filepath.IsAbs is
// host-dependent, so a literal "C:\\..." is not absolute on darwin. The inputs
// below are therefore host-absolute temp dirs, and the genuinely Windows-shaped
// paths are covered by this same code running on the Windows CI leg.

func TestWindowsPluginDataRootMirrorsTheHostLayout(t *testing.T) {
	localAppData := t.TempDir()
	home := t.TempDir()

	got, err := windowsPluginDataRoot(localAppData, home)
	if err != nil {
		t.Fatalf("with LOCALAPPDATA: %v", err)
	}
	if want := filepath.Join(localAppData, XDGRootDir); got != want {
		t.Fatalf("with LOCALAPPDATA: got %q, want %q", got, want)
	}

	got, err = windowsPluginDataRoot("", home)
	if err != nil {
		t.Fatalf("without LOCALAPPDATA: %v", err)
	}
	if want := filepath.Join(home, "AppData", "Local", XDGRootDir); got != want {
		t.Fatalf("without LOCALAPPDATA: got %q, want %q", got, want)
	}
}

// A relative %LOCALAPPDATA% is discarded rather than joined. The host requires
// an absolute one, so honouring a relative value here would invent a third
// location instead of agreeing with the host or with the home fallback.
func TestWindowsPluginDataRootDiscardsRelativeLocalAppData(t *testing.T) {
	home := t.TempDir()

	got, err := windowsPluginDataRoot(filepath.Join("relative", "appdata"), home)
	if err != nil {
		t.Fatalf("relative LOCALAPPDATA: %v", err)
	}
	if want := filepath.Join(home, "AppData", "Local", XDGRootDir); got != want {
		t.Fatalf("relative LOCALAPPDATA: got %q, want %q", got, want)
	}

	if _, err := windowsPluginDataRoot("", ""); err == nil {
		t.Fatal("no LOCALAPPDATA and no home: want an error, got nil")
	}
}

// The dispatched path must be unaffected on every OS: when the host sets
// ENTIRE_PLUGIN_DATA_DIR, that value wins outright and neither XDG nor
// LOCALAPPDATA is consulted.
func TestResolvePluginDataRootPrefersTheHostSuppliedDir(t *testing.T) {
	hostDir := t.TempDir()
	t.Setenv(EnvLocalAppData, t.TempDir())
	t.Setenv(XDGDataHome, t.TempDir())

	for _, goos := range []string{windowsGOOS, "linux", "darwin"} {
		got, err := resolvePluginDataRoot(hostDir, goos)
		if err != nil {
			t.Fatalf("goos %s: %v", goos, err)
		}
		if got != hostDir {
			t.Fatalf("goos %s: got %q, want the host-supplied %q", goos, got, hostDir)
		}
	}
}

// The divergence itself: with no host-supplied dir, Windows must follow
// LOCALAPPDATA and ignore XDG_DATA_HOME, while every other OS keeps the XDG
// rule untouched.
func TestResolvePluginDataRootIgnoresXDGOnWindowsOnly(t *testing.T) {
	localAppData := t.TempDir()
	xdgData := t.TempDir()
	t.Setenv(EnvLocalAppData, localAppData)
	t.Setenv(XDGDataHome, xdgData)

	got, err := resolvePluginDataRoot("", windowsGOOS)
	if err != nil {
		t.Fatalf("windows: %v", err)
	}
	if want := filepath.Join(localAppData, XDGRootDir); got != want {
		t.Fatalf("windows: got %q, want %q (XDG_DATA_HOME must be ignored)", got, want)
	}

	for _, goos := range []string{"linux", "darwin"} {
		got, err := resolvePluginDataRoot("", goos)
		if err != nil {
			t.Fatalf("goos %s: %v", goos, err)
		}
		if want := filepath.Join(xdgData, XDGRootDir); got != want {
			t.Fatalf("goos %s: got %q, want %q (XDG rule must be unchanged)", goos, got, want)
		}
	}
}

// The guard that was missing, and the reason the store-path change escaped its
// own test sandbox on Windows.
//
// Every other test here resolves the rule for whichever OS it happens to be
// compiled for, so a darwin or linux run never evaluates the Windows branch at
// all. A test that loops over the target OS does, and it fails on ANY machine
// the moment the Windows branch starts reading a variable the sandbox does not
// control. That is exactly what happened: LOCALAPPDATA was introduced here and
// not added to clitest.PinXDGRoots, so `go test ./...` on Windows resolved the
// developer's real %LOCALAPPDATA%\entire and created a store in it.
func TestResolvePluginDataRootHonoursASandboxedEnvironmentOnEveryOS(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)        // unix home
	t.Setenv("USERPROFILE", root) // windows home
	t.Setenv(EnvLocalAppData, filepath.Join(root, "AppData", "Local"))
	// Clear the XDG pin so each OS falls through to its own home-directory rule,
	// which is the path a real user with no XDG configuration takes.
	for _, key := range []string{XDGConfigHome, XDGDataHome, XDGStateHome, XDGCacheHome} {
		t.Setenv(key, "")
	}

	for _, goos := range []string{windowsGOOS, "linux", "darwin"} {
		got, err := resolvePluginDataRoot("", goos)
		if err != nil {
			t.Fatalf("goos %s: %v", goos, err)
		}
		if !PathInside(root, got) {
			t.Errorf("goos %s: data root resolved to %q, outside the sandboxed home %q; "+
				"every variable this branch reads must be redirectable", goos, got, root)
		}
	}
}
