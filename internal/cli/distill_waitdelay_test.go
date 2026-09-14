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

const distillPipeHelperEnv = "ENTIRE_BRAIN_DISTILL_PIPE_HELPER"

func TestDistillAgentPipeHelper(t *testing.T) {
	switch os.Getenv(distillPipeHelperEnv) {
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
		child := exec.Command(self, "-test.run=^TestDistillAgentPipeHelper$")
		child.Env = mergeCommandEnv(os.Environ(), map[string]string{distillPipeHelperEnv: "grandchild"})
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if child.Start() != nil {
			os.Exit(3)
		}
		if os.WriteFile(os.Getenv("ENTIRE_BRAIN_DISTILL_HELPER_PID"), []byte(strconv.Itoa(child.Process.Pid)), 0600) != nil {
			os.Exit(4)
		}
		time.Sleep(15 * time.Second)
		os.Exit(0)
	default:
		t.Skip("subprocess helper")
	}
}

func TestDistillAgentTimeoutBoundsInheritedPipes(t *testing.T) {
	t.Setenv(distillPipeHelperEnv, "parent")
	pidPath := t.TempDir() + string(os.PathSeparator) + "child.pid"
	t.Setenv("ENTIRE_BRAIN_DISTILL_HELPER_PID", pidPath)
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
	_, err = execDistillAgent(context.Background(), t.TempDir(), []string{self, "-test.run=^TestDistillAgentPipeHelper$"}, nil, 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout, got %v", err)
	}
	if _, err := os.Stat(pidPath); err != nil {
		t.Fatalf("grandchild probe did not start: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second+distillAgentWaitDelay+3*time.Second {
		t.Fatalf("inherited pipes extended a 2s timeout to %s", elapsed)
	}
}

func TestDistillAgentWaitDelayKeepsNormalOutput(t *testing.T) {
	t.Setenv(distillPipeHelperEnv, "normal")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	out, err := execDistillAgent(context.Background(), t.TempDir(), []string{self, "-test.run=^TestDistillAgentPipeHelper$"}, nil, 5*time.Second)
	if err != nil || out != "{}" {
		t.Fatalf("normal provider: %q %v", out, err)
	}
}
