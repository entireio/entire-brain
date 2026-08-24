package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFileLockContentionAndReacquire(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "locks", "write.lock")
	lock, err := acquireFileLock(lockPath, "brain_locked", 0)
	if err != nil {
		t.Fatalf("acquire lock: %v", err)
	}
	if _, err := acquireFileLock(lockPath, "brain_locked", 25*time.Millisecond); err == nil ||
		!strings.Contains(err.Error(), "brain_locked") || !errors.Is(err, errFileLockTimeout) {
		t.Fatalf("typed contention err = %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	reacquired, err := acquireFileLock(lockPath, "brain_locked", time.Second)
	if err != nil {
		t.Fatalf("reacquire after unlock with lock file still present: %v", err)
	}
	if err := reacquired.Close(); err != nil {
		t.Fatalf("unlock reacquired: %v", err)
	}
	parent, basename, err := fileLockIdentity(lockPath)
	if err != nil {
		t.Fatalf("lock identity: %v", err)
	}
	processFileLocks.Lock()
	retained := processFileLocks.find(parent, basename) != nil
	processFileLocks.Unlock()
	if retained {
		t.Fatal("released and timed-out lock retained a process-local lock entry")
	}
}

func TestFileLockSerializesSameProcessSharedMemory(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "locks", "write.lock")
	firstAcquired := make(chan struct{})
	firstDone := make(chan struct{})
	secondAcquired := make(chan struct{})
	releaseFirst := make(chan struct{})
	var shared int
	var wait sync.WaitGroup

	wait.Add(1)
	go func() {
		defer wait.Done()
		defer close(firstDone)
		lock, err := acquireFileLock(lockPath, "brain_locked", time.Second)
		if err != nil {
			t.Errorf("first acquire: %v", err)
			return
		}
		defer func() {
			if err := lock.Close(); err != nil {
				t.Errorf("first release: %v", err)
			}
		}()
		close(firstAcquired)
		<-releaseFirst
		shared++
	}()

	wait.Add(1)
	go func() {
		defer wait.Done()
		select {
		case <-firstAcquired:
		case <-firstDone:
			return
		}
		lock, err := acquireFileLock(lockPath, "brain_locked", time.Second)
		if err != nil {
			t.Errorf("second acquire: %v", err)
			return
		}
		defer func() {
			if err := lock.Close(); err != nil {
				t.Errorf("second release: %v", err)
			}
		}()
		close(secondAcquired)
		shared++
	}()

	earlySecond := false
	select {
	case <-secondAcquired:
		earlySecond = true
	case <-firstDone:
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)
	wait.Wait()
	if earlySecond {
		t.Fatal("second lock acquired before the first lock was released")
	}
	if shared != 2 {
		t.Fatalf("shared critical-section updates = %d, want 2", shared)
	}
}

func TestFileLockSerializesParentDirectoryAliases(t *testing.T) {
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	aliasDir := filepath.Join(dir, "alias")
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, aliasDir); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	realPath := filepath.Join(realDir, "locks", "write.lock")
	aliasPath := filepath.Join(aliasDir, "locks", "write.lock")
	if err := os.MkdirAll(filepath.Dir(realPath), 0o700); err != nil {
		t.Fatal(err)
	}
	realParent, realBasename, err := fileLockIdentity(realPath)
	if err != nil {
		t.Fatalf("real lock identity: %v", err)
	}
	aliasParent, aliasBasename, err := fileLockIdentity(aliasPath)
	if err != nil {
		t.Fatalf("alias lock identity: %v", err)
	}
	if realBasename != aliasBasename || !os.SameFile(realParent, aliasParent) {
		t.Fatal("lock path aliases must have the same parent identity and basename")
	}
	lock, err := acquireFileLock(realPath, "brain_locked", time.Second)
	if err != nil {
		t.Fatalf("acquire through real path: %v", err)
	}
	defer func() {
		if err := lock.Close(); err != nil {
			t.Errorf("release real path lock: %v", err)
		}
	}()
	if _, err := acquireFileLock(aliasPath, "brain_locked", 25*time.Millisecond); err == nil ||
		!errors.Is(err, errFileLockTimeout) {
		t.Fatalf("alias contention err = %v", err)
	}
}

func TestFileLockRejectsSymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "locks", "write.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "outside")
	if err := os.WriteFile(target, []byte("do not clobber"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, lockPath); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := acquireFileLock(lockPath, "brain_locked", 0); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink lock err = %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "do not clobber" {
		t.Fatalf("symlink target was modified: %q", data)
	}
}

func TestFileLockRejectsHardlinkTarget(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "locks", "write.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(dir, "shared")
	if err := os.WriteFile(shared, []byte("do not clobber"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(shared, lockPath); err != nil {
		t.Skipf("hardlink unsupported: %v", err)
	}
	if _, err := acquireFileLock(lockPath, "brain_locked", 0); err == nil || !strings.Contains(err.Error(), "hardlink") {
		t.Fatalf("hardlink lock err = %v", err)
	}
	data, err := os.ReadFile(shared)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "do not clobber" {
		t.Fatalf("hardlink target was modified: %q", data)
	}
}

func TestFileLockContentionAcrossProcesses(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "locks", "write.lock")
	readyPath := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command(os.Args[0], "-test.run=TestFileLockSubprocessHelper", "-test.v=false")
	cmd.Env = append(os.Environ(),
		"ENTIRE_BRAIN_LOCK_HELPER=1",
		"ENTIRE_BRAIN_LOCK_PATH="+lockPath,
		"ENTIRE_BRAIN_LOCK_READY="+readyPath,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(readyPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("helper did not acquire lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := acquireFileLock(lockPath, "brain_locked", 50*time.Millisecond); err == nil || !strings.Contains(err.Error(), "brain_locked") {
		t.Fatalf("cross-process contention err = %v", err)
	}
}

func TestFileLockSubprocessHelper(t *testing.T) {
	if os.Getenv("ENTIRE_BRAIN_LOCK_HELPER") != "1" {
		return
	}
	lock, err := acquireFileLock(os.Getenv("ENTIRE_BRAIN_LOCK_PATH"), "brain_locked", time.Second)
	if err != nil {
		t.Fatalf("helper acquire lock: %v", err)
	}
	if err := writeFileAtomic(os.Getenv("ENTIRE_BRAIN_LOCK_READY"), []byte("ready\n"), 0o600); err != nil {
		t.Fatalf("helper ready: %v", err)
	}
	time.Sleep(5 * time.Second)
	_ = lock.Close()
}
