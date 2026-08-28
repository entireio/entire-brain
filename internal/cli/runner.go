package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// CommandRunner runs external commands. Tests replace it so export behavior
// can be verified without requiring a real Entire checkout.
type CommandRunner interface {
	Run(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error)
}

// EnvironmentCommandRunner is the narrow capability used when a child process
// needs an operation-specific environment safety boundary. Keeping it optional
// avoids making every test runner and provider adapter environment-aware while
// still letting security-sensitive callers fail closed when the capability is
// unavailable.
type EnvironmentCommandRunner interface {
	RunWithEnv(ctx context.Context, dir string, env map[string]string, name string, args ...string) ([]byte, []byte, error)
}

// CommandStreamer is an optional capability for CommandRunner implementations
// that can expose a child process's stdout incrementally instead of buffering
// the entire output. The semantic provider snapshot can grow to hundreds of
// megabytes on large repositories, so the indexer streams it record-by-record
// rather than holding it all in memory. Runners that do not implement this
// interface fall back to the buffered Run path.
type CommandStreamer interface {
	Stream(ctx context.Context, dir, name string, args ...string) (CommandStream, error)
}

// CommandStream is a started command whose stdout is consumed incrementally.
// Callers must read Stdout to completion (or drain it) before calling Wait.
type CommandStream interface {
	// Stdout returns the live stdout reader.
	Stdout() io.Reader
	// Wait blocks until the command exits, returning the captured stderr and
	// the raw exit error (unwrapped, so callers can format it).
	Wait() (stderr []byte, err error)
	// Close releases any resources. It is safe to call after Wait.
	Close() error
}

// streamStderrCap bounds how much stderr a streamed command may retain in
// memory. Provider stderr is only used for diagnostics, so a generous cap keeps
// error messages useful without letting a misbehaving provider exhaust memory.
const streamStderrCap = 256 * 1024

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
	return runExecCommand(ctx, dir, nil, name, args...)
}

func (ExecRunner) RunWithEnv(ctx context.Context, dir string, env map[string]string, name string, args ...string) ([]byte, []byte, error) {
	return runExecCommand(ctx, dir, env, name, args...)
}

func runExecCommand(ctx context.Context, dir string, env map[string]string, name string, args ...string) ([]byte, []byte, error) {
	if name == "git" {
		args = hardenedGitArgs(args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if len(env) > 0 {
		cmd.Env = mergeCommandEnv(os.Environ(), env)
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

func mergeCommandEnv(base []string, overrides map[string]string) []string {
	out := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if ok {
			if _, replaced := overrides[key]; replaced {
				continue
			}
		}
		out = append(out, entry)
	}
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := overrides[key]
		out = append(out, key+"="+value)
	}
	return out
}

// Stream starts name and exposes its stdout as a streaming reader. stderr is
// captured into a bounded buffer by exec's own copy goroutine, so reading
// stdout never blocks on stderr.
func (ExecRunner) Stream(ctx context.Context, dir, name string, args ...string) (CommandStream, error) {
	if name == "git" {
		args = hardenedGitArgs(args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr := &cappedBuffer{limit: streamStderrCap}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &execStream{cmd: cmd, stdout: stdout, stderr: stderr}, nil
}

type execStream struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr *cappedBuffer
}

func (s *execStream) Stdout() io.Reader { return s.stdout }

func (s *execStream) Wait() ([]byte, error) {
	err := s.cmd.Wait()
	return s.stderr.Bytes(), err
}

func (s *execStream) Close() error {
	// cmd.Wait closes the stdout pipe; closing again is harmless and covers the
	// case where the caller never reached Wait.
	return s.stdout.Close()
}

// cappedBuffer is an io.Writer that retains at most limit bytes. Writes past the
// cap are counted but discarded, so diagnostics keep the head of the output.
type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.limit - c.buf.Len(); room > 0 {
		if room < len(p) {
			c.buf.Write(p[:room])
		} else {
			c.buf.Write(p)
		}
	}
	return len(p), nil
}

func (c *cappedBuffer) Bytes() []byte { return c.buf.Bytes() }
