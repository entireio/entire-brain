package cli

import (
	"bytes"
	"context"
	"errors"
	"github.com/entireio/entire-brain/internal/cli/command/disconnect"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/clitest"
	"strings"
	"testing"
	"time"
)

func runDisconnectCommand(t *testing.T, opts Options) (string, error) {
	t.Helper()
	cmd := disconnect.New(disconnectConfig(opts))
	cmd.SetArgs(nil)
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	err := cmd.Execute()
	return out.String(), err
}

func disconnectTestOptions(t *testing.T, repoDir string) (Options, repoStorage) {
	t.Helper()
	env := connectTestEnv(t, repoDir)
	runner := clitest.CommandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		return nil, nil, errors.New("not available")
	})
	opts := Options{Version: "test", Env: env, Runner: runner, Now: time.Now}
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	return opts, storage
}

func TestDisconnectRecordsOptOutKeepingCoordinates(t *testing.T) {
	opts, storage := disconnectTestOptions(t, t.TempDir())
	if err := brainstore.WriteHostedRepoBinding(storage.BrainDir, brainstore.HostedRepoBinding{
		RepoID: "01M2Q9WATHXVM46CD8Y4D3AX8W", BaseURL: "https://cell.api.example.com",
	}); err != nil {
		t.Fatal(err)
	}

	out, err := runDisconnectCommand(t, opts)
	if err != nil {
		t.Fatal(err)
	}
	binding, present, err := brainstore.ReadHostedRepoBinding(storage.BrainDir)
	if err != nil || !present {
		t.Fatalf("disconnect must keep the state file: present=%v err=%v", present, err)
	}
	if !binding.OptedOut {
		t.Fatal("disconnect must record the opt-out")
	}
	if binding.RepoID != "01M2Q9WATHXVM46CD8Y4D3AX8W" || binding.BaseURL != "https://cell.api.example.com" {
		t.Fatalf("disconnect must keep the coordinates for easy re-connect, got %+v", binding)
	}
	if strings.Contains(out, "01M2Q9WATHXVM46CD8Y4D3AX8W") {
		t.Fatalf("banner must not leak the hosted repo id, got:\n%s", out)
	}
	for _, want := range []string{
		"no longer syncs memories",
		"keeps working exactly as before",
		"stay in the local store",
		"resume syncing",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output must contain %q, got:\n%s", want, out)
		}
	}
	assertHostedBannerLayout(t, out)
}

// assertHostedBannerLayout pins the shared banner shape: one blank line before
// the headline, and every line left-aligned (no indentation).
func assertHostedBannerLayout(t *testing.T, out string) {
	t.Helper()
	if !strings.HasPrefix(out, "\n") {
		t.Fatalf("banner must open with a blank line, got %q", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, " ") {
			t.Fatalf("banner lines must be left-aligned, got indented line %q in:\n%s", line, out)
		}
	}
}

func TestDisconnectWhenNeverConnectedRecordsOptOut(t *testing.T) {
	opts, storage := disconnectTestOptions(t, t.TempDir())

	out, err := runDisconnectCommand(t, opts)
	if err != nil {
		t.Fatal(err)
	}
	binding, present, err := brainstore.ReadHostedRepoBinding(storage.BrainDir)
	if err != nil || !present {
		t.Fatalf("disconnect must record the opt-out even when never connected: present=%v err=%v", present, err)
	}
	if !binding.OptedOut || binding.RepoID != "" || binding.BaseURL != "" {
		t.Fatalf("never-connected opt-out must be bare, got %+v", binding)
	}
	for _, want := range []string{
		"Disconnected: this repository does not sync with a hosted brain",
		"keeps working exactly as before",
		"to join",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output must contain %q, got:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "as before.\n\nRun") {
		t.Fatalf("the hint must be its own paragraph, like the connected variant, got:\n%s", out)
	}
	assertHostedBannerLayout(t, out)
}

func TestDisconnectIsIdempotent(t *testing.T) {
	opts, storage := disconnectTestOptions(t, t.TempDir())
	if err := brainstore.WriteHostedRepoBinding(storage.BrainDir, brainstore.HostedRepoBinding{
		RepoID: "repo1", BaseURL: "https://cell.api.example.com",
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := runDisconnectCommand(t, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := runDisconnectCommand(t, opts); err != nil {
		t.Fatalf("a second disconnect must succeed, got %v", err)
	}
	binding, present, err := brainstore.ReadHostedRepoBinding(storage.BrainDir)
	if err != nil || !present || !binding.OptedOut || binding.RepoID != "repo1" {
		t.Fatalf("state must be unchanged after the second disconnect: present=%v err=%v binding=%+v", present, err, binding)
	}
}

func TestDisconnectHealsACorruptStateFile(t *testing.T) {
	opts, storage := disconnectTestOptions(t, t.TempDir())
	if err := brainstore.WriteBrainRelativeFileAtomic(storage.BrainDir, brainstore.HostedRepoBindingFile, []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := runDisconnectCommand(t, opts); err != nil {
		t.Fatalf("disconnect must succeed over a corrupt state file, got %v", err)
	}
	binding, present, err := brainstore.ReadHostedRepoBinding(storage.BrainDir)
	if err != nil || !present || !binding.OptedOut {
		t.Fatalf("disconnect must overwrite a corrupt state file with the opt-out: present=%v err=%v binding=%+v", present, err, binding)
	}
}
