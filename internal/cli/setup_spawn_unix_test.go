//go:build !windows

package cli

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestSpawnDetachedPutsTheChildInItsOwnSession covers the property spawn's own
// doc comment calls "the whole point", and which nothing tested: deleting
// `cmd.SysProcAttr = detachedSysProcAttr()` left all 41 setup tests passing
// while making every background backfill die with the shell that started it —
// the exact failure the detached design exists to prevent, and one that is
// invisible in `status` because the pid is recorded before the shell exits.
func TestSpawnDetachedPutsTheChildInItsOwnSession(t *testing.T) {
	dir := t.TempDir()
	pid, err := spawnDetached(setupBackfillPlan{
		Binary:  "/bin/sh",
		Args:    []string{"-c", "sleep 30"},
		Dir:     dir,
		LogPath: filepath.Join(dir, "backfill.log"),
		Env:     os.Environ(),
	})
	if err != nil {
		t.Fatalf("spawnDetached: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	childGroup, err := syscall.Getpgid(pid)
	if err != nil {
		t.Fatalf("getpgid(child): %v", err)
	}
	ownGroup, err := syscall.Getpgid(0)
	if err != nil {
		t.Fatalf("getpgid(self): %v", err)
	}
	if childGroup == ownGroup {
		t.Fatalf("the backfill shares this process group (%d): a Ctrl-C or the shell's SIGHUP kills it", childGroup)
	}
	if childGroup != pid {
		t.Fatalf("setsid must make the child its own group leader: pgid %d, pid %d", childGroup, pid)
	}
}

// TestSpawnDetachedRefusesASymlinkedLogPath: the backfill log is the one write
// in this package that does not go through writeFileAtomic (a log is appended
// to, not replaced), and it lives at a predictable per-repo path. Without
// O_NOFOLLOW anything that can plant a symlink there redirects the child's
// stdout+stderr into a file of its choosing and appends to it as the user, for
// the hours the pass runs.
func TestSpawnDetachedRefusesASymlinkedLogPath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "victim")
	if err := os.WriteFile(target, []byte("important\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "backfill.log")
	if err := os.Symlink(target, logPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	pid, err := spawnDetached(setupBackfillPlan{
		Binary:  "/bin/sh",
		Args:    []string{"-c", "echo pwned"},
		Dir:     dir,
		LogPath: logPath,
		Env:     os.Environ(),
	})
	if err == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatal("spawnDetached followed a symlinked log path and pointed an hours-long background process's output at it")
	}
	data, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != "important\n" {
		t.Fatalf("the symlink target was written through: %q", string(data))
	}
}
