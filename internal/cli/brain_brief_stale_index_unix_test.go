//go:build darwin || linux

package cli

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestBrainBriefPostIndexRejectsNamedPipe(t *testing.T) {
	repoDir := newBrainBriefPostIndexRepo(t)
	writeBrainBriefPostIndexFiles(t, repoDir, map[string]string{"internal/cli/base.go": "package cli\n"})
	indexed := commitBrainBriefPostIndexRepo(t, repoDir, "base")
	writeBrainBriefPostIndexFiles(t, repoDir, map[string]string{
		"internal/cli/compact_v3_pipe.go": "package cli\n",
	})
	current := commitBrainBriefPostIndexRepo(t, repoDir, "pipe candidate")
	pipe := filepath.Join(repoDir, "internal", "cli", "compact_v3_pipe.go")
	if err := os.Remove(pipe); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Skipf("FIFO unsupported: %v", err)
	}
	status := brainBriefPostIndexStatus(repoDir, indexed, current)

	edits, tests := brainBriefPostIndexFiles(context.Background(), ExecRunner{}, status, "fix compact-v3 checksum")
	if len(edits) != 0 || len(tests) != 0 {
		t.Fatalf("named pipe escaped regular-file boundary: edits=%v tests=%v", edits, tests)
	}
}
