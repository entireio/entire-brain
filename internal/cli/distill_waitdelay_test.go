package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

const evidencePipeHelperEnv = "ENTIRE_BRAIN_EVIDENCE_PIPE_HELPER"

func TestEvidenceProviderPipeHelper(t *testing.T) {
	switch os.Getenv(evidencePipeHelperEnv) {
	case "normal":
		fmt.Print("{}")
		os.Exit(0)
	case "grandchild":
		time.Sleep(15 * time.Second)
		os.Exit(0)
	case "parent":
		self, err := os.Executable()
		if err != nil {
			os.Exit(2)
		}
		child := exec.Command(self, "-test.run=^TestEvidenceProviderPipeHelper$")
		child.Env = mergeCommandEnv(os.Environ(), map[string]string{evidencePipeHelperEnv: "grandchild"})
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if child.Start() != nil {
			os.Exit(3)
		}
		if os.WriteFile(os.Getenv("ENTIRE_BRAIN_EVIDENCE_HELPER_PID"), []byte(strconv.Itoa(child.Process.Pid)), 0600) != nil {
			os.Exit(4)
		}
		time.Sleep(15 * time.Second)
		os.Exit(0)
	default:
		t.Skip("subprocess helper")
	}
}

func TestDistillAgentTimeoutBoundsInheritedPipes(t *testing.T) {
	t.Setenv(evidencePipeHelperEnv, "parent")
	pidPath := t.TempDir() + string(os.PathSeparator) + "child.pid"
	t.Setenv("ENTIRE_BRAIN_EVIDENCE_HELPER_PID", pidPath)
	t.Cleanup(func() {
		if b, err := os.ReadFile(pidPath); err == nil {
			if pid, err := strconv.Atoi(string(b)); err == nil {
				if process, err := os.FindProcess(pid); err == nil {
					_ = process.Kill()
				}
			}
		}
	})
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = execDistillAgent(context.Background(), t.TempDir(), []string{self, "-test.run=^TestEvidenceProviderPipeHelper$"}, nil, 500*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout, got %v", err)
	}
	if _, err := os.Stat(pidPath); err != nil {
		t.Fatalf("grandchild probe did not start: %v", err)
	}
	if elapsed := time.Since(start); elapsed > commandWaitDelay+5*time.Second {
		t.Fatalf("inherited pipes extended a 500ms timeout to %s", elapsed)
	}
}

func TestDistillAgentWaitDelayKeepsNormalOutput(t *testing.T) {
	t.Setenv(evidencePipeHelperEnv, "normal")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	out, err := execDistillAgent(context.Background(), t.TempDir(), []string{self, "-test.run=^TestEvidenceProviderPipeHelper$"}, nil, 5*time.Second)
	if err != nil || out != "{}" {
		t.Fatalf("normal provider: %q %v", out, err)
	}
}
