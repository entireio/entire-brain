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
	"sync"
	"time"
)

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

// GitCatFileBatcher is an optional capability: a runner whose git spawn policy
// matches ExecRunner's may have repeated local object reads served by a shared
// long-lived `git cat-file --batch` child (see git_catfile_batch.go). It is a
// deliberate opt-in, not an inherited seam: batching bypasses Run entirely, so
// a wrapper runner that intercepts per-call behavior must not claim it by
// accident.
type GitCatFileBatcher interface {
	CatFileBatchAllowed() bool
}

func (ExecRunner) CatFileBatchAllowed() bool { return true }

// ConcurrentCommandRunner is an optional capability: a runner whose Run holds
// no per-call state may have independent read-only commands spawned several at
// a time. Like GitCatFileBatcher this is a deliberate opt-in rather than an
// inherited seam — a wrapper runner that records calls, replays fixtures, or
// otherwise keeps unsynchronised state must not claim it by accident, and a
// caller that cannot keep its own accumulation deterministic must not ask.
type ConcurrentCommandRunner interface {
	ConcurrentRunAllowed() bool
}

// ExecRunner spawns a fresh child per call and shares nothing between them, so
// concurrent Run calls are independent. (The cat-file batch child is reached
// through CatFileBatchAllowed, not Run, and is not covered by this claim.)
func (ExecRunner) ConcurrentRunAllowed() bool { return true }

// hardenedGitInvocation is the one assembly point for how this binary spawns
// git: hardened args plus the repository filter-driver overrides merged into
// the environment. The buffered runner, the streaming runner, and the cat-file
// batch child all consume it, so a new hardening bit cannot cover one spawn
// path and miss another (see git_harden.go).
func hardenedGitInvocation(ctx context.Context, dir string, env map[string]string, args ...string) (hardenedArgs, mergedEnv []string, err error) {
	hardenedArgs = hardenedGitArgs(args...)
	overrides, err := repoFilterDriverOverrides(ctx, dir)
	if err != nil {
		return nil, nil, err
	}
	return hardenedArgs, gitConfigOverrideEnv(mergeCommandEnv(os.Environ(), env), overrides), nil
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

// commandWaitDelay bounds how long a command may keep the runner blocked after
// its context is cancelled.
//
// exec.CommandContext kills the child on cancellation, but cmd.Run and cmd.Wait
// return only once every writer of the stdout/stderr pipes has closed. A child
// that spawns a grandchild inheriting those descriptors therefore keeps the
// runner blocked for as long as the GRANDCHILD lives, long after the deadline
// passed and the child was killed. Every context deadline in this binary is
// implemented on top of this runner, so without a WaitDelay a single stray
// grandchild makes all of them unenforceable: measured at 30s of wait against a
// 300ms deadline.
//
// WaitDelay bounds waiting for process exit and inherited pipes after
// cancellation. CommandContext kills the direct child, not its process group. It is a
// backstop, not a timeout: a command that exits normally is never affected, and
// the delay only starts counting once the context is already cancelled.
const commandWaitDelay = 2 * time.Second

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
	return runExecCommand(ctx, dir, nil, name, args...)
}

func (ExecRunner) RunWithEnv(ctx context.Context, dir string, env map[string]string, name string, args ...string) ([]byte, []byte, error) {
	return runExecCommand(ctx, dir, env, name, args...)
}

func runExecCommand(ctx context.Context, dir string, env map[string]string, name string, args ...string) ([]byte, []byte, error) {
	var mergedEnv []string
	if name == "git" {
		// The filter neutralizers travel in the environment, not in argv; see
		// git_harden_filters.go for why `-c` cannot express them safely.
		var err error
		args, mergedEnv, err = hardenedGitInvocation(ctx, dir, env, args...)
		if err != nil {
			return nil, nil, err
		}
	} else if len(env) > 0 {
		mergedEnv = mergeCommandEnv(os.Environ(), env)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = commandWaitDelay
	if dir != "" {
		cmd.Dir = dir
	}
	if mergedEnv != nil {
		cmd.Env = mergedEnv
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
	var mergedEnv []string
	if name == "git" {
		var err error
		args, mergedEnv, err = hardenedGitInvocation(ctx, dir, nil, args...)
		if err != nil {
			return nil, err
		}
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = commandWaitDelay
	if dir != "" {
		cmd.Dir = dir
	}
	if mergedEnv != nil {
		cmd.Env = mergedEnv
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
//
// The mutex exists because exec's copy goroutine writes while a reader can
// call Bytes after Wait returned early: with WaitDelay set, Wait may return
// on a forced pipe close while the copier is still draining its final read.
type cappedBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := c.limit - c.buf.Len(); room > 0 {
		if room < len(p) {
			c.buf.Write(p[:room])
		} else {
			c.buf.Write(p)
		}
	}
	return len(p), nil
}

func (c *cappedBuffer) Bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Copy: the caller must not observe later writes through a shared backing
	// array.
	return bytes.Clone(c.buf.Bytes())
}
