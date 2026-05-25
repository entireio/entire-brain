package cli

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// CommandRunner runs external commands. Tests replace it so export behavior
// can be verified without requiring a real Entire checkout.
type CommandRunner interface {
	Run(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		if stderr.Len() > 0 {
			return stdout.Bytes(), stderr.Bytes(), fmt.Errorf("%s %v: %w: %s", name, args, err, stderr.String())
		}
		return stdout.Bytes(), stderr.Bytes(), fmt.Errorf("%s %v: %w", name, args, err)
	}
	return stdout.Bytes(), stderr.Bytes(), nil
}
