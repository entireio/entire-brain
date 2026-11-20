package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
)

// redirectHomeDir points os.UserHomeDir at a fresh temp directory on EVERY
// platform, and returns that directory.
//
// t.Setenv("HOME", dir) is a POSIX habit and a SILENT NO-OP on Windows:
// os.UserHomeDir switches on runtime.GOOS and reads USERPROFILE there. A test
// that redirects only HOME therefore still resolves the real home of whoever is
// running it. Nothing errors -- the test simply leaves its sandbox.
//
// That is how it hid. Every POSIX shard agreed with the fixture, and the
// mismatch surfaced only on Windows, and only in the one test that ASSERTS on a
// resolved store path:
//
//	printed ENTIRE_PLUGIN_DATA_DIR
//	  = "C:\Users\runneradmin\.local\share\entire\plugins\data\brain"
//	want "C:\Users\RUNNER~1\AppData\Local\Temp\Test...\001\.local\share\..."
//
// A test that merely READS the wrong home says nothing at all, which is the
// more dangerous half. Three sites in this package already set USERPROFILE by
// hand and two did not, with no helper or check making the convention hold.
//
// The redirect is VERIFIED, not assumed. If os.UserHomeDir ever consults a
// variable this helper does not set, the failure lands HERE, naming the cause,
// instead of downstream as an unexplained path mismatch.
func redirectHomeDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// All three variables, unconditionally: the redirect must not depend on
	// which platform the test was compiled for.
	t.Setenv("HOME", dir)        // unix, darwin, and the default everywhere else
	t.Setenv("USERPROFILE", dir) // windows
	// LOCALAPPDATA is not read by os.UserHomeDir, so the check below cannot
	// catch it -- but the Windows data-root rule reads it BEFORE falling back to
	// %USERPROFILE%\AppData\Local, so a test that redirects the home and leaves
	// this pointing at the real profile still resolves the real store. Mirroring
	// the real layout under the redirected home keeps the fallback and the
	// variable in agreement, so both paths land in the sandbox.
	// TestDefaultStoreFallbackStaysInsideTheSandbox is what actually asserts it.
	t.Setenv(base.EnvLocalAppData, filepath.Join(dir, "AppData", "Local"))
	got, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("redirect home dir: os.UserHomeDir: %v", err)
	}
	if got != dir {
		t.Fatalf("the home redirect did not take on %s: os.UserHomeDir() = %q, want %q\n"+
			"os.UserHomeDir reads a variable this helper does not set on this platform; "+
			"add it here rather than letting tests read the real home directory",
			runtime.GOOS, got, dir)
	}
	return dir
}

// useDefaultStoreFallback takes a test OUT of the package-wide XDG sandbox and
// into the home-directory fallback, returning the home it redirected to.
//
// TestMain pins the four XDG roots for every test in this package, and
// resolveXDGDir consults the XDG variable BEFORE the home directory -- so
// ordinary tests never reach os.UserHomeDir at all, and a broken home redirect
// cannot hurt them. The tests that DO reach it are the ones deliberately
// exercising what a user with no XDG configuration gets, which means clearing
// the pin. Clearing the pin without redirecting the home is exactly the escape
// this pairing exists to prevent, so the two are one call and cannot be
// half-done.
func useDefaultStoreFallback(t *testing.T) string {
	t.Helper()
	home := redirectHomeDir(t)
	for _, key := range []string{base.XDGConfigHome, base.XDGDataHome, base.XDGStateHome, base.XDGCacheHome} {
		t.Setenv(key, "")
	}
	return home
}

// TestDefaultStoreFallbackStaysInsideTheSandbox is the check that was missing.
//
// It runs everywhere, Windows included, and is why redirectHomeDir verifies
// rather than assumes: a home redirect that quietly fails is indistinguishable
// from one that works until something asserts on a resolved path, and by then
// the failure names a path mismatch rather than its cause. Skipping this on the
// one platform where the behaviour differs is how the class stays hidden.
func TestDefaultStoreFallbackStaysInsideTheSandbox(t *testing.T) {
	home := useDefaultStoreFallback(t)

	if got, err := os.UserHomeDir(); err != nil || got != home {
		t.Fatalf("os.UserHomeDir() = %q (err %v), want the redirected %q", got, err, home)
	}

	// A PLATFORM-INDEPENDENT statement of the contract, and the only assertion
	// here that a POSIX machine can fail. os.UserHomeDir reads HOME on this
	// build, so the check above passes whether or not USERPROFILE was set --
	// which is precisely why dropping USERPROFILE stayed green on every shard
	// but Windows. Naming both variables makes that regression fail everywhere.
	for _, key := range []string{"HOME", "USERPROFILE"} {
		if got := os.Getenv(key); got != home {
			t.Errorf("%s = %q, want the redirected home %q; os.UserHomeDir reads this "+
				"variable on at least one supported platform", key, got, home)
		}
	}

	// LOCALAPPDATA is checked separately because it is not a home variable: it
	// must point INSIDE the redirected home, not at it. The Windows data-root
	// rule prefers it over the %USERPROFILE%\AppData\Local fallback, so leaving
	// it at the machine's real value is an escape that no POSIX shard can see.
	if got := os.Getenv(base.EnvLocalAppData); !base.PathInside(home, got) {
		t.Errorf("%s = %q, want a path inside the redirected home %q; the Windows "+
			"data-root rule reads this before falling back to the home directory",
			base.EnvLocalAppData, got, home)
	}

	// An empty EntireEnv is the "host supplied no plugin dirs" path, and with
	// the XDG pin cleared it falls through to the home directory. Every one of
	// the four must land in the sandbox; on Windows before this helper, all
	// four resolved under the CI runner's real profile.
	dirs, err := base.ResolvePluginDirs(EntireEnv{})
	if err != nil {
		t.Fatalf("resolve plugin dirs: %v", err)
	}
	for name, got := range map[string]string{
		base.EnvPluginConfigDir: dirs.Config,
		base.EnvPluginDataDir:   dirs.Data,
		base.EnvPluginStateDir:  dirs.State,
		base.EnvPluginCacheDir:  dirs.Cache,
	} {
		if !base.PathInside(home, got) {
			t.Errorf("%s resolved to %q, outside the redirected home %q", name, got, home)
		}
	}
}
