package cli

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/command/hook"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/clitest"
)

func runSessionEndHook(t *testing.T, opts Options) (string, string) {
	t.Helper()
	cmd := hook.NewSessionEnd(hookCommandConfig(opts))
	cmd.SetArgs([]string{"--session", "s1"})
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("session-end must never fail, got %v", err)
	}
	return out.String(), errOut.String()
}

func sessionEndHarness(t *testing.T, fake *hostedFake, bind bool) (Options, repoStorage) {
	t.Helper()
	// The real launch re-execs this test binary as `memory worker --once` with
	// its cwd inside the temp dir — on Windows that handle blocks TempDir
	// cleanup, and a spawned worker is never what a unit test wants.
	oldLaunch := memoryWorkerLaunch
	memoryWorkerLaunch = func(string) error { return nil }
	t.Cleanup(func() { memoryWorkerLaunch = oldLaunch })
	repoDir := t.TempDir()
	env := connectTestEnv(t, repoDir)
	runner := clitest.CommandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		if name == "entire" && len(args) >= 2 && args[0] == "auth" && args[1] == "token" {
			return []byte(fakeJWT + "\n"), nil, nil
		}
		return nil, nil, errors.New("not available")
	})
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if bind {
		srv := newHostedFakeServer(t, fake)
		if err := brainstore.WriteHostedRepoBinding(storage.BrainDir, brainstore.HostedRepoBinding{RepoID: "repo1", BaseURL: srv.URL}); err != nil {
			t.Fatal(err)
		}
	}
	return opts, storage
}

func TestSessionEndHookDoesNotSync(t *testing.T) {
	fake := &hostedFake{}
	opts, _ := sessionEndHarness(t, fake, true)
	stdout, stderr := runSessionEndHook(t, opts)
	if stdout != "" || stderr != "" {
		t.Fatalf("legacy hook output: %q %q", stdout, stderr)
	}
	if fake.requestCount() != 0 {
		t.Fatal("session-end contacted hosted service")
	}
}
