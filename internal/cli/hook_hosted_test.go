package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func runSessionEndHook(t *testing.T, opts Options) (string, string) {
	t.Helper()
	cmd := newHookSessionEndCommand(opts)
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
	runner := commandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
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
		if err := writeHostedRepoBinding(storage.BrainDir, hostedRepoBinding{RepoID: "repo1", BaseURL: srv.URL}); err != nil {
			t.Fatal(err)
		}
	}
	return opts, storage
}

func TestSessionEndHookSyncsAndPullsWithCleanStdout(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	remoteFact := hostedSyncTestFact("team fact via hook", now)
	fake := &hostedFake{ref: "facts-remote", authors: map[string]string{remoteFact.ID: "teammate"}}
	fake.data = encodeFactsNDJSON(t, []factRecord{remoteFact})
	opts, storage := sessionEndHarness(t, fake, true)

	stdout, _ := runSessionEndHook(t, opts)
	if stdout != "" {
		t.Fatalf("session-end stdout must stay empty (machine-read contract), got %q", stdout)
	}
	local, err := loadFacts(storage.BrainDir, distillDefaultBranch)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, rec := range local {
		if rec.ID == remoteFact.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("session-end must pull teammates' facts, got %d local records", len(local))
	}
}

func TestSessionEndHookHostedFailureIsSilentSuccess(t *testing.T) {
	fake := &hostedFake{}
	opts, storage := sessionEndHarness(t, fake, true)
	// Corrupt the binding's URL so the sync step fails its gate.
	if err := writeHostedRepoBinding(storage.BrainDir, hostedRepoBinding{RepoID: "repo1", BaseURL: "ftp://nope"}); err != nil {
		t.Fatal(err)
	}
	stdout, errOut := runSessionEndHook(t, opts)
	if stdout != "" {
		t.Fatalf("stdout must stay empty on hosted failure, got %q", stdout)
	}
	if !strings.Contains(errOut, "hosted sync") {
		t.Fatalf("hosted failure must leave a stderr note, got %q", errOut)
	}
}

func TestSessionEndHookWithoutBindingMakesNoRequests(t *testing.T) {
	fake := &hostedFake{}
	opts, _ := sessionEndHarness(t, fake, false)
	stdout, _ := runSessionEndHook(t, opts)
	if stdout != "" {
		t.Fatalf("stdout must stay empty, got %q", stdout)
	}
	if fake.requestCount() != 0 {
		t.Fatalf("unconnected repo must make zero requests, got %d", fake.requestCount())
	}
}
