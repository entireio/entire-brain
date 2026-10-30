package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/command/connect"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/clitest"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
)

func connectTestEnv(t *testing.T, repoDir string) EntireEnv {
	t.Helper()
	return EntireEnv{
		RepoRoot:        repoDir,
		PluginConfigDir: t.TempDir(),
		PluginDataDir:   t.TempDir(),
		PluginStateDir:  t.TempDir(),
		PluginCacheDir:  t.TempDir(),
	}
}

func runConnectCommand(t *testing.T, opts Options, args ...string) (string, string, error) {
	t.Helper()
	cmd := connect.New(connectConfig(opts))
	cmd.SetArgs(args)
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

func TestConnectWithExplicitFlagsWritesBindingWithoutShellOut(t *testing.T) {
	repoDir := t.TempDir()
	env := connectTestEnv(t, repoDir)
	placementCalls := 0
	runner := clitest.CommandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		if name == "entire" && len(args) >= 2 && args[0] == "repo" && args[1] == "view" {
			placementCalls++
		}
		return nil, nil, errors.New("not available")
	})
	opts := Options{Version: "test", Env: env, Runner: runner, Now: time.Now}

	out, _, err := runConnectCommand(t, opts,
		"--repo-id", "01M2Q9WATHXVM46CD8Y4D3AX8W",
		"--api-url", "https://cell.api.example.com/api/v1")
	// This runner cannot mint a token, so the first sync does not run and
	// connect reports that instead of claiming success (COR-2202). The binding
	// below must be written regardless.
	if err == nil || !strings.Contains(err.Error(), "first sync did not run") {
		t.Fatalf("want the first-sync failure, got err=%v stdout=%q", err, out)
	}
	if placementCalls != 0 {
		t.Fatalf("explicit flags must bypass placement resolution entirely, got %d calls", placementCalls)
	}
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	binding, present, err := brainstore.ReadHostedRepoBinding(storage.BrainDir)
	if err != nil || !present {
		t.Fatalf("binding must be written: present=%v err=%v", present, err)
	}
	// The factsync client appends /api/v1/repos/... itself, so connect must
	// store the origin: a stored /api/v1 suffix would 404 every sync.
	if binding.RepoID != "01M2Q9WATHXVM46CD8Y4D3AX8W" ||
		binding.BaseURL != "https://cell.api.example.com" {
		t.Fatalf("binding mismatch: %+v", binding)
	}
}

func TestConnectResolvesViaRepoView(t *testing.T) {
	repoDir := t.TempDir()
	env := connectTestEnv(t, repoDir)
	runner := clitest.CommandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		if name == "entire" && len(args) >= 2 && args[0] == "repo" && args[1] == "view" {
			return []byte(`{"repoId":"01AAAAAAAAAAAAAAAAAAAAAAAA","apiUrl":"https://eu.api.example.com/api/v1","name":"test/repo"}` + "\nUpdate available! 1 -> 2\n"), nil, nil
		}
		return nil, nil, errors.New("not available")
	})
	opts := Options{Version: "test", Env: env, Runner: runner, Now: time.Now}

	// Placement resolves, but this runner cannot mint a token for the sync, so
	// connect reports the failed first sync; the binding must still be written.
	if _, _, err := runConnectCommand(t, opts); err == nil || !strings.Contains(err.Error(), "first sync did not run") {
		t.Fatalf("want the first-sync failure, got %v", err)
	}
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	binding, present, err := brainstore.ReadHostedRepoBinding(storage.BrainDir)
	if err != nil || !present {
		t.Fatalf("binding must be written: present=%v err=%v", present, err)
	}
	if binding.RepoID != "01AAAAAAAAAAAAAAAAAAAAAAAA" ||
		binding.BaseURL != "https://eu.api.example.com" {
		t.Fatalf("binding mismatch: %+v", binding)
	}
}

func TestConnectPlacementUnavailableIsCleanError(t *testing.T) {
	repoDir := t.TempDir()
	env := connectTestEnv(t, repoDir)
	runner := clitest.CommandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		return nil, nil, errors.New("unknown command")
	})
	opts := Options{Version: "test", Env: env, Runner: runner, Now: time.Now}

	_, _, err := runConnectCommand(t, opts)
	if err == nil {
		t.Fatal("placement-resolution failure without flags must error")
	}
	if !strings.Contains(err.Error(), "--repo-id") {
		t.Fatalf("error must point at the explicit flags, got: %v", err)
	}
	storage, storageErr := repoStoragePaths(context.Background(), runner, env, repoDir)
	if storageErr != nil {
		t.Fatal(storageErr)
	}
	if _, present, _ := brainstore.ReadHostedRepoBinding(storage.BrainDir); present {
		t.Fatal("no binding may be written on failure")
	}
}

func TestConnectPlacementErrorKeepsHostMessageAndFlagsHint(t *testing.T) {
	// COR-2207: the failure the user reads must carry the host CLI's message
	// alone — no argv echo, no "Using context" banner — with the explicit-flags
	// hint as its own line instead of a dangling "; pass ..." clause.
	repoDir := t.TempDir()
	env := connectTestEnv(t, repoDir)
	hostMessage := `no repo matching "sandbox-audit/brain-sbx" visible from your login (GitHub repos you could onboard: ` + "`entire repo mirror list --available`" + `)`
	stderr := "Using context 'auth.example.invalid'.\n" + hostMessage + "\n"
	runner := clitest.CommandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		if name == "entire" && len(args) > 0 && args[0] == "version" {
			return []byte("Entire CLI 0.12.0\n"), nil, nil
		}
		if name == "entire" && len(args) > 0 && args[0] == "repo" {
			return nil, []byte(stderr), fmt.Errorf("entire [repo view --json]: exit status 1: %s", stderr)
		}
		return nil, nil, errors.New("not available")
	})
	opts := Options{Version: "test", Env: env, Runner: runner, Now: time.Now}

	_, _, err := runConnectCommand(t, opts)
	if err == nil {
		t.Fatal("placement-resolution failure without flags must error")
	}
	want := "connect: " + hostMessage + "\nOr pass --repo-id and --api-url explicitly"
	if err.Error() != want {
		t.Fatalf("error must be the host message plus the flags hint, got:\n%s\nwant:\n%s", err.Error(), want)
	}
}

func TestConnectRejectsInvalidAPIURL(t *testing.T) {
	repoDir := t.TempDir()
	env := connectTestEnv(t, repoDir)
	runner := clitest.CommandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		return nil, nil, errors.New("not available")
	})
	opts := Options{Version: "test", Env: env, Runner: runner, Now: time.Now}

	_, _, err := runConnectCommand(t, opts,
		"--repo-id", "01M2Q9WATHXVM46CD8Y4D3AX8W",
		"--api-url", "http://evil.example/api/v1")
	if err == nil {
		t.Fatal("plaintext non-loopback URL must be rejected")
	}
	storage, storageErr := repoStoragePaths(context.Background(), runner, env, repoDir)
	if storageErr != nil {
		t.Fatal(storageErr)
	}
	if _, present, _ := brainstore.ReadHostedRepoBinding(storage.BrainDir); present {
		t.Fatal("no binding may be written for a rejected URL")
	}
}

func TestConnectRunsInitialPull(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	remoteFact := hostedSyncTestFact("team fact", now)
	fake := &hostedFake{}
	fake.seed(remoteFact, "trail-1", "acct-teammate", "teammate")
	srv := newHostedFakeServer(t, fake)

	repoDir := t.TempDir()
	env := connectTestEnv(t, repoDir)
	runner := clitest.CommandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		if name == "entire" && len(args) >= 2 && args[0] == "auth" && args[1] == "token" {
			return []byte(fakeJWT + "\n"), nil, nil
		}
		return nil, nil, errors.New("not available")
	})
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}

	out, _, err := runConnectCommand(t, opts,
		"--repo-id", "repo1", "--api-url", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"is now syncing memories",
		"sync periodically, at session end, and on git push",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output must contain %q, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "repo1") {
		t.Fatalf("banner must not leak the hosted repo id, got:\n%s", out)
	}
	assertHostedBannerLayout(t, out)
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	local, err := factstore.Load(storage.BrainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, rec := range local {
		if rec.ID == remoteFact.ID && rec.Author == "teammate" {
			found = true
		}
	}
	if !found {
		t.Fatalf("connect must perform the initial pull, got %d local records", len(local))
	}
}

func TestConnectReusesTheExistingBindingWhenPlacementUnavailable(t *testing.T) {
	// A connected repo stays re-connectable without flags even before the host
	// CLI ships repo view placement: connect falls back to the stored binding,
	// so the blind-spot note's remedy ("run entire brain connect") always pulls.
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	remoteFact := hostedSyncTestFact("team fact via reconnect", now)
	fake := &hostedFake{}
	fake.seed(remoteFact, "trail-1", "acct-teammate", "teammate")
	srv := newHostedFakeServer(t, fake)

	repoDir := t.TempDir()
	env := connectTestEnv(t, repoDir)
	runner := clitest.CommandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		if name == "entire" && len(args) >= 2 && args[0] == "auth" && args[1] == "token" {
			return []byte(fakeJWT + "\n"), nil, nil
		}
		return nil, nil, errors.New("not available")
	})
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := brainstore.WriteHostedRepoBinding(storage.BrainDir, brainstore.HostedRepoBinding{RepoID: "repo1", BaseURL: srv.URL}); err != nil {
		t.Fatal(err)
	}

	if _, _, err := runConnectCommand(t, opts); err != nil {
		t.Fatalf("connect on an already-connected repo must not require placement resolution, got %v", err)
	}
	local, err := factstore.Load(storage.BrainDir, "main")
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
		t.Fatalf("reconnect must pull, got %d local records", len(local))
	}
}

func TestConnectClearsOptOut(t *testing.T) {
	// Re-joining after a disconnect must clear the opt-out, and the opted-out
	// binding's kept coordinates must serve as the no-flag fallback.
	repoDir := t.TempDir()
	env := connectTestEnv(t, repoDir)
	fake := &hostedFake{}
	srv := newHostedFakeServer(t, fake)
	runner := clitest.CommandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		if name == "entire" && len(args) >= 2 && args[0] == "auth" && args[1] == "token" {
			return []byte(fakeJWT + "\n"), nil, nil
		}
		return nil, nil, errors.New("not available")
	})
	opts := Options{Version: "test", Env: env, Runner: runner, Now: time.Now}
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := brainstore.WriteHostedRepoBinding(storage.BrainDir, brainstore.HostedRepoBinding{RepoID: "repo1", BaseURL: srv.URL, OptedOut: true}); err != nil {
		t.Fatal(err)
	}

	if _, _, err := runConnectCommand(t, opts); err != nil {
		t.Fatal(err)
	}
	binding, present, err := brainstore.ReadHostedRepoBinding(storage.BrainDir)
	if err != nil || !present {
		t.Fatalf("binding must be present: present=%v err=%v", present, err)
	}
	if binding.OptedOut {
		t.Fatal("connect must clear the opt-out")
	}
	if binding.RepoID != "repo1" || binding.BaseURL != srv.URL {
		t.Fatalf("connect must rebind the kept coordinates, got %+v", binding)
	}
}

// Connect's headline must reflect the first sync's outcome (COR-2202): a
// failed pull means nothing syncs yet, so the command must not claim "now
// syncing memories" and must exit non-zero — while still recording the
// binding, which is durable intent and makes a plain re-run the retry.
func TestConnectReportsFirstSyncFailure(t *testing.T) {
	repoDir := t.TempDir()
	env := connectTestEnv(t, repoDir)
	fake := &hostedFake{denyFeed: true}
	srv := newHostedFakeServer(t, fake)
	runner := clitest.CommandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		if name == "entire" && len(args) >= 2 && args[0] == "auth" && args[1] == "token" {
			return []byte(fakeJWT + "\n"), nil, nil
		}
		return nil, nil, errors.New("not available")
	})
	opts := Options{Version: "test", Env: env, Runner: runner, Now: time.Now}

	out, errOut, err := runConnectCommand(t, opts, "--repo-id", "repo1", "--api-url", srv.URL)
	if err == nil {
		t.Fatalf("connect must fail when the first sync fails; stdout=%q stderr=%q", out, errOut)
	}
	if strings.Contains(out, "now syncing") {
		t.Fatalf("failed sync must not claim success: %q", out)
	}
	storage, storageErr := repoStoragePaths(context.Background(), runner, env, repoDir)
	if storageErr != nil {
		t.Fatal(storageErr)
	}
	if _, present, readErr := brainstore.ReadHostedRepoBinding(storage.BrainDir); readErr != nil || !present {
		t.Fatalf("binding must survive a failed first sync: present=%v err=%v", present, readErr)
	}
}

// Under the kill switch the binding is still recorded (the environment only
// subtracts), but the output must say nothing will sync instead of claiming
// "now syncing memories".
func TestConnectUnderNoEgressSaysNothingSyncs(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
	repoDir := t.TempDir()
	env := connectTestEnv(t, repoDir)
	runner := clitest.CommandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		return nil, nil, errors.New("not available")
	})
	opts := Options{Version: "test", Env: env, Runner: runner, Now: time.Now}

	out, _, err := runConnectCommand(t, opts, "--repo-id", "repo1", "--api-url", "https://cell.api.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "now syncing") || !strings.Contains(out, "ENTIRE_BRAIN_NO_EGRESS") {
		t.Fatalf("no-egress connect must name the kill switch, not claim syncing: %q", out)
	}
	storage, storageErr := repoStoragePaths(context.Background(), runner, env, repoDir)
	if storageErr != nil {
		t.Fatal(storageErr)
	}
	if _, present, readErr := brainstore.ReadHostedRepoBinding(storage.BrainDir); readErr != nil || !present {
		t.Fatalf("binding must be recorded under no-egress: present=%v err=%v", present, readErr)
	}
}

// Re-running connect on an already-connected repo acknowledges the existing
// connection instead of announcing a new one.
func TestConnectAcknowledgesExistingConnection(t *testing.T) {
	repoDir := t.TempDir()
	env := connectTestEnv(t, repoDir)
	fake := &hostedFake{}
	srv := newHostedFakeServer(t, fake)
	runner := clitest.CommandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		if name == "entire" && len(args) >= 2 && args[0] == "auth" && args[1] == "token" {
			return []byte(fakeJWT + "\n"), nil, nil
		}
		return nil, nil, errors.New("not available")
	})
	opts := Options{Version: "test", Env: env, Runner: runner, Now: time.Now}
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := brainstore.WriteHostedRepoBinding(storage.BrainDir, brainstore.HostedRepoBinding{RepoID: "repo1", BaseURL: srv.URL}); err != nil {
		t.Fatal(err)
	}

	out, _, err := runConnectCommand(t, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Already connected") {
		t.Fatalf("re-connect must acknowledge the existing connection: %q", out)
	}
}
