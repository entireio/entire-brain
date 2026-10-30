package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
)

type gitlessRunner struct{}

func (gitlessRunner) Run(context.Context, string, string, ...string) ([]byte, []byte, error) {
	return nil, nil, errors.New("no git here")
}

func captureNotifyHostedSync(t *testing.T) *[]string {
	t.Helper()
	original := notifyHostedFactsSync
	t.Cleanup(func() { notifyHostedFactsSync = original })
	var branches []string
	notifyHostedFactsSync = func(_ context.Context, _ io.Writer, _ Options, _ string, _ repoStorage, branch string) (hostedSyncOutcome, error) {
		branches = append(branches, branch)
		return hostedSyncOutcome{Attempted: true, PullSucceeded: true}, nil
	}
	return &branches
}

func notifySyncHarness(t *testing.T, now *time.Time) (Options, repoStorage) {
	t.Helper()
	brainDir := t.TempDir()
	if err := os.MkdirAll(brainDir, 0o755); err != nil {
		t.Fatal(err)
	}
	opts := Options{Version: "test", Now: func() time.Time { return *now }}
	return opts, repoStorage{Key: "test/repo", BrainDir: brainDir}
}

func TestPushHintSyncsBranchAndDefault(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	opts, storage := notifySyncHarness(t, &now)
	calls := captureNotifyHostedSync(t)

	ran := hostedSyncOnPushHint(context.Background(), io.Discard, opts, t.TempDir(), storage, "feat-x")
	if !ran {
		t.Fatal("first push hint must sync")
	}
	if len(*calls) != 2 || (*calls)[0] != "feat-x" || (*calls)[1] != distillDefaultBranch {
		t.Fatalf("expected [feat-x %s], got %v", distillDefaultBranch, *calls)
	}
}

func TestPushHintSyncsDefaultBranchOnce(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	opts, storage := notifySyncHarness(t, &now)
	calls := captureNotifyHostedSync(t)

	if !hostedSyncOnPushHint(context.Background(), io.Discard, opts, t.TempDir(), storage, distillDefaultBranch) {
		t.Fatal("hint on the default branch must sync")
	}
	if len(*calls) != 1 || (*calls)[0] != distillDefaultBranch {
		t.Fatalf("expected one %s sync, got %v", distillDefaultBranch, *calls)
	}
}

// A recorded no-access pause (COR-2279) bails before the throttle claim, like
// an opt-out: the push beat must not spend a sync slot — or a server round
// trip — on a repo the server already denied.
func TestPushHintSkipsPausedRepo(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	opts, storage := notifySyncHarness(t, &now)
	calls := captureNotifyHostedSync(t)
	if err := brainstore.WriteHostedRepoBinding(storage.BrainDir, brainstore.HostedRepoBinding{
		RepoID: "repo1", BaseURL: "https://cell.example", NoAccessUntil: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	if hostedSyncOnPushHint(context.Background(), io.Discard, opts, t.TempDir(), storage, "feat-x") {
		t.Fatal("a paused repo must not sync on a push hint")
	}
	if len(*calls) != 0 {
		t.Fatalf("expected no sync calls, got %v", *calls)
	}
}

func TestPushHintSyncThrottles(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	opts, storage := notifySyncHarness(t, &now)
	calls := captureNotifyHostedSync(t)
	repoDir := t.TempDir()

	if !hostedSyncOnPushHint(context.Background(), io.Discard, opts, repoDir, storage, "feat-x") {
		t.Fatal("first hint must sync")
	}
	now = now.Add(memoryNotifyHostedSyncMinInterval - time.Second)
	if hostedSyncOnPushHint(context.Background(), io.Discard, opts, repoDir, storage, "feat-x") {
		t.Fatal("hint inside the interval must be throttled")
	}
	if len(*calls) != 2 {
		t.Fatalf("throttled hint must not sync, got %d calls", len(*calls))
	}
	now = now.Add(2 * time.Second)
	if !hostedSyncOnPushHint(context.Background(), io.Discard, opts, repoDir, storage, "feat-x") {
		t.Fatal("hint after the interval must sync again")
	}
	if len(*calls) != 4 {
		t.Fatalf("expected 4 calls after the interval, got %d", len(*calls))
	}
}

func TestPushHintSyncEmptyBranchFallsBackToDefault(t *testing.T) {
	// No git repository behind repoDir, so branch resolution fails and the
	// sync covers the default branch only — never an empty branch name.
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	opts, storage := notifySyncHarness(t, &now)
	opts.Runner = gitlessRunner{}
	calls := captureNotifyHostedSync(t)

	if !hostedSyncOnPushHint(context.Background(), io.Discard, opts, t.TempDir(), storage, "") {
		t.Fatal("hint without a branch must still sync")
	}
	if len(*calls) != 1 || (*calls)[0] != distillDefaultBranch {
		t.Fatalf("expected one %s sync, got %v", distillDefaultBranch, *calls)
	}
}

func TestNotifyCommandDispatchesPushWorkDetached(t *testing.T) {
	opts, _ := memoryAdminCommandFixture(t)
	originalLaunch := memoryWorkerLaunch
	memoryWorkerLaunch = func(string) error { return nil }
	t.Cleanup(func() { memoryWorkerLaunch = originalLaunch })
	originalWork := memoryPushWorkLaunch
	t.Cleanup(func() { memoryPushWorkLaunch = originalWork })
	var launches []string
	memoryPushWorkLaunch = func(_ string, branch string) error {
		launches = append(launches, branch)
		return nil
	}

	for _, event := range []string{"session_start", "checkpoint", "session_end"} {
		if _, err := execute(t, newMemoryNotifyCommand(opts), "--event", event, "--session", "s1"); err != nil {
			t.Fatal(err)
		}
	}
	if len(launches) != 0 {
		t.Fatalf("only push may dispatch upload work, got %v", launches)
	}
	out, err := execute(t, newMemoryNotifyCommand(opts), "--event", "push", "--branch", "feat-x")
	if err != nil {
		t.Fatal(err)
	}
	if len(launches) != 1 || launches[0] != "feat-x" {
		t.Fatalf("push must dispatch the hinted branch, got %v", launches)
	}
	if !strings.Contains(out, "dispatched push upload") {
		t.Fatalf("push output = %q", out)
	}
}

func TestPushWorkCommandRunsThrottledSync(t *testing.T) {
	opts, _ := memoryAdminCommandFixture(t)
	calls := captureNotifyHostedSync(t)

	if _, err := execute(t, newMemoryPushWorkCommand(opts), "--branch", "feat-x"); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 2 || (*calls)[0] != "feat-x" {
		t.Fatalf("push work must sync the hinted branch then default, got %v", *calls)
	}
}
