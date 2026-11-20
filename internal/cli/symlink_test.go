package cli

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Windows refuses os.Symlink for an unprivileged account without Developer
// Mode: "A required privilege is not held by the client". Twenty-nine tests in
// this package treated that as a test failure, so a plain `go test ./...` on an
// ordinary Windows machine produced a wall of red that said nothing about the
// product. These helpers are the idiom internal/agentsetup already uses.
//
// Deliberately NOT a blanket `runtime.GOOS == "windows"` skip. Developer Mode,
// an elevated shell and the CI runners all do provide symlinks, and Windows is
// exactly where the os.Root and path semantics these tests cover most need
// exercising. Probe the capability; never assume it from the platform.

// skipIfSymlinksUnrepresentable skips when this process cannot create a symlink
// at all. Call it at the narrowest scope that needs one — if only one subtest
// creates symlinks, call it inside that subtest, so the rest still runs.
func skipIfSymlinksUnrepresentable(t *testing.T) {
	t.Helper()
	if err := os.Symlink("target", filepath.Join(t.TempDir(), "link")); err != nil {
		t.Skipf("symlinks unavailable on this host: %v", err)
	}
}

// symlinkForTest creates a symlink a test depends on, skipping rather than
// failing when the host withholds the privilege, and failing for every other
// reason. The privilege case is matched specifically rather than swallowing
// every error, so a genuinely broken fixture still fails loudly.
func symlinkForTest(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if isSymlinkPrivilegeError(err) {
			t.Skipf("symlinks unavailable on this host: %v", err)
		}
		t.Fatalf("symlink %s -> %s: %v", link, target, err)
	}
}

// isSymlinkPrivilegeError reports the host refusing the privilege rather than
// the link being impossible for an ordinary reason.
//
// ERROR_PRIVILEGE_NOT_HELD (1314) is named numerically because the constant
// lives in golang.org/x/sys/windows, which this file must not import: it has to
// compile on every platform. syscall.Errno is an integer error on all of them,
// so the comparison is simply false everywhere else.
func isSymlinkPrivilegeError(err error) bool {
	const windowsErrorPrivilegeNotHeld = syscall.Errno(1314)
	var errno syscall.Errno
	if errors.As(err, &errno) && errno == windowsErrorPrivilegeNotHeld {
		return true
	}
	return errors.Is(err, os.ErrPermission)
}

// The classifier decides skip-versus-fail, and on a POSIX machine no real call
// ever hands it the Windows errno — so without this it would ship untested on
// the one platform it exists for. os.Symlink wraps its errno in *os.LinkError,
// which is the shape errors.As has to see through.
func TestIsSymlinkPrivilegeError(t *testing.T) {
	t.Parallel()

	const privilegeNotHeld = syscall.Errno(1314) // Windows ERROR_PRIVILEGE_NOT_HELD

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "windows privilege refusal, wrapped as os.Symlink returns it",
			err:  &os.LinkError{Op: "symlink", Old: "target", New: "link", Err: privilegeNotHeld},
			want: true,
		},
		{"bare windows errno", privilegeNotHeld, true},
		{"posix permission denied", &os.LinkError{Op: "symlink", Err: syscall.EACCES}, true},
		{"posix operation not permitted", &os.LinkError{Op: "symlink", Err: syscall.EPERM}, true},
		// A broken fixture must still fail loudly rather than vanish into a skip.
		{"file exists", &os.LinkError{Op: "symlink", Err: syscall.EEXIST}, false},
		{"no such file", &os.LinkError{Op: "symlink", Err: syscall.ENOENT}, false},
		{"not a syscall error at all", errors.New("some other failure"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isSymlinkPrivilegeError(tc.err); got != tc.want {
				t.Fatalf("isSymlinkPrivilegeError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
