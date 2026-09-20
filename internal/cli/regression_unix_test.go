//go:build darwin || linux

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestRegressionScanHistoryClosesFilesDuringLargeWalk(t *testing.T) {
	brainDir := t.TempDir()
	sessionsDir := filepath.Join(brainDir, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 160; i++ {
		path := filepath.Join(sessionsDir, fmt.Sprintf("%03d.jsonl", i))
		if err := os.WriteFile(path, []byte(`{"text":"ordinary session line"}`+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(sessionsDir, "999.jsonl")
	if err := os.WriteFile(target, []byte(`{"text":"in pkg/review_context.go the scope range is scopeBaseRef+\"..HEAD\""}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var original syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &original); err != nil {
		t.Skipf("get rlimit: %v", err)
	}
	if original.Cur <= 96 {
		t.Skipf("soft file descriptor limit is already low (%d)", original.Cur)
	}
	low := original
	low.Cur = 96
	if low.Cur > low.Max {
		t.Skipf("cannot lower soft file descriptor limit below hard limit %d", low.Max)
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &low); err != nil {
		t.Skipf("lower rlimit: %v", err)
	}
	t.Cleanup(func() {
		if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &original); err != nil {
			t.Errorf("restore file descriptor limit: %v", err)
		}
	})

	changes, _, files, err := regressionScanHistory(brainDir, []string{"scopeBaseRef"})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) == 0 {
		t.Fatalf("late signal file was not scanned under low descriptor limit; files=%v", files)
	}
	if _, ok := files["pkg/review_context.go"]; !ok {
		t.Fatalf("late signal file was scanned but file hint was not retained: changes=%+v files=%v", changes, files)
	}
}
