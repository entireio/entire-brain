//go:build !windows

package cli

import (
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Detached backfills must lead a new session and process group.
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
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })

	session, err := unix.Getsid(pid)
	if err != nil {
		t.Fatal(err)
	}
	if session != pid {
		t.Fatalf("child session=%d, want child pid %d", session, pid)
	}
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

// A recorded pid that some UNRELATED process inherited — the ordinary situation
// after a reboot, where low pids are handed out again within seconds — used to
// read as "your backfill is still running". `setup` then skipped the backfill
// for as long as that stranger lived, and `status` reported a pass that did not
// exist, so the repo's facts were never backfilled again and nothing said why.
func TestBackfillRunningRejectsAReusedPid(t *testing.T) {
	brainDir := t.TempDir()
	// os.Getpid() is alive by construction and is emphatically NOT a detached
	// backfill child: it is the test binary. That is exactly the shape of a
	// reused pid.
	if backfillRunning(brainDir, os.Getpid()) {
		t.Fatal("a live pid that holds no distill pass lock is a STALE record, not a running backfill")
	}
}

// The other direction: while a distill pass genuinely holds the lock, the
// backfill must read as running, or the re-entrancy guard stops guarding and a
// second `setup` doubles the token spend on the same pending sessions.
func TestBackfillRunningSeesAHeldDistillPassLock(t *testing.T) {
	brainDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(brainDir, brainLockDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireFileLock(filepath.Join(brainDir, brainLockDirName, brainDistillLockName), "distill_pass_locked", 0)
	if err != nil {
		t.Fatalf("take the pass lock: %v", err)
	}
	defer func() { _ = lock.Close() }()
	if !backfillRunning(brainDir, os.Getpid()) {
		t.Fatal("a live pid AND a held distill pass lock is a running backfill")
	}
}

// A pid nothing owns is gone whatever the lock says; the lock is a
// disambiguator for live pids, not a replacement for the liveness check.
func TestBackfillRunningRejectsADeadPid(t *testing.T) {
	if backfillRunning(t.TempDir(), 0) {
		t.Fatal("pid 0 is not a running backfill")
	}
}

// No brain directory means no lock to consult. Answering "dead" there would
// invent a fact; the honest fallback is the pid answer.
func TestBackfillRunningFallsBackToThePidWithoutABrain(t *testing.T) {
	if !backfillRunning("", os.Getpid()) {
		t.Fatal("with no brain to check, a live pid must not be reported as a dead backfill")
	}
}
