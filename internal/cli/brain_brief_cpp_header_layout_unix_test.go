//go:build darwin || linux

package cli

import (
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
)

func TestBrainBriefCxxHeaderLayoutGuidanceRejectsNamedPipe(t *testing.T) {
	repoDir := t.TempDir()
	writeBrainBriefCxxLayoutFile(t, repoDir, "src/auth/token.cpp", "bool token();\n")
	header := filepath.Join(repoDir, "include", "auth", "token.h")
	if err := os.MkdirAll(filepath.Dir(header), 0o700); err != nil {
		t.Fatalf("mkdir header parent: %v", err)
	}
	if err := syscall.Mkfifo(header, 0o600); err != nil {
		t.Fatalf("create header-shaped named pipe: %v", err)
	}
	report := brainBriefReport{
		LikelyEditFiles: []string{"src/auth/token.cpp"},
		LikelyFiles:     []string{"src/auth/token.cpp"},
	}
	brainBriefAddCxxHeaderLayoutGuidance(repoDir, "change the token signature", &report)
	if !slices.Equal(report.LikelyEditFiles, []string{"src/auth/token.cpp"}) {
		t.Fatalf("named pipe was surfaced as a header: %+v", report.LikelyEditFiles)
	}
}
