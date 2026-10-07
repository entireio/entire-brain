package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestWatchTickRunsHostedSyncAfterRefresh(t *testing.T) {
	cursorPath := filepath.Join(t.TempDir(), "watch.json")
	var refreshed, seeded, distilled, synced int
	steps := fakeWatchSteps("ck1:head1", &refreshed, &seeded, &distilled)
	steps.factsSync = func(context.Context) error { synced++; return nil }
	var out bytes.Buffer
	var agentCalls int
	watchTick(context.Background(), &out, watchCommandOptions{}, cursorPath, steps, &agentCalls)
	if refreshed != 1 || synced != 1 {
		t.Fatalf("hosted sync must run after a successful refresh, got refresh=%d sync=%d", refreshed, synced)
	}
	if !strings.Contains(out.String(), "hosted fact sync ran") {
		t.Fatalf("expected the sync note, got %q", out.String())
	}
	if readWatchCursor(t, cursorPath).LastFactsSyncAt.IsZero() {
		t.Fatal("cursor must record the sync attempt")
	}
}

func TestWatchTickSkipsHostedSyncWhenRefreshFails(t *testing.T) {
	cursorPath := filepath.Join(t.TempDir(), "watch.json")
	var refreshed, seeded, distilled, synced int
	steps := fakeWatchSteps("ck1:head1", &refreshed, &seeded, &distilled)
	steps.refresh = func(context.Context) error { return errors.New("refresh broke") }
	steps.factsSync = func(context.Context) error { synced++; return nil }
	var out bytes.Buffer
	var agentCalls int
	watchTick(context.Background(), &out, watchCommandOptions{}, cursorPath, steps, &agentCalls)
	if synced != 0 {
		t.Fatalf("hosted sync must not run when refresh failed, got %d", synced)
	}
}

func TestWatchTickHostedSyncErrorDoesNotFailTick(t *testing.T) {
	cursorPath := filepath.Join(t.TempDir(), "watch.json")
	var refreshed, seeded, distilled int
	steps := fakeWatchSteps("ck1:head1", &refreshed, &seeded, &distilled)
	steps.factsSync = func(context.Context) error { return errors.New("hosted outage") }
	var out bytes.Buffer
	var agentCalls int
	watchTick(context.Background(), &out, watchCommandOptions{}, cursorPath, steps, &agentCalls)
	if refreshed != 1 {
		t.Fatalf("tick must complete, got refresh=%d", refreshed)
	}
	if !strings.Contains(out.String(), "hosted fact sync failed (local brain unaffected)") {
		t.Fatalf("expected the non-fatal note, got %q", out.String())
	}
	// The attempt still advances the cursor: a failing hosted endpoint must not
	// turn every tick into an egress attempt.
	if readWatchCursor(t, cursorPath).LastFactsSyncAt.IsZero() {
		t.Fatal("cursor must record the failed attempt")
	}
}

func TestWatchTickThrottlesHostedSync(t *testing.T) {
	cursorPath := filepath.Join(t.TempDir(), "watch.json")
	var refreshed, seeded, distilled, synced int
	fp := "ck1:head1"
	steps := fakeWatchSteps("", &refreshed, &seeded, &distilled)
	steps.fingerprint = func(context.Context) string { return fp }
	steps.factsSync = func(context.Context) error { synced++; return nil }
	var out bytes.Buffer
	var agentCalls int
	watchTick(context.Background(), &out, watchCommandOptions{}, cursorPath, steps, &agentCalls)
	fp = "ck2:head2"
	watchTick(context.Background(), &out, watchCommandOptions{}, cursorPath, steps, &agentCalls)
	if refreshed != 2 {
		t.Fatalf("both ticks must refresh, got %d", refreshed)
	}
	if synced != 1 {
		t.Fatalf("two ticks inside hostedSyncMinInterval must sync once, got %d", synced)
	}
}

func TestWatchTickNilFactsSyncIsSkipped(t *testing.T) {
	cursorPath := filepath.Join(t.TempDir(), "watch.json")
	var refreshed, seeded, distilled int
	steps := fakeWatchSteps("ck1:head1", &refreshed, &seeded, &distilled)
	var out bytes.Buffer
	var agentCalls int
	watchTick(context.Background(), &out, watchCommandOptions{}, cursorPath, steps, &agentCalls)
	if refreshed != 1 {
		t.Fatalf("tick must run normally with no factsSync step, got refresh=%d", refreshed)
	}
	if strings.Contains(out.String(), "hosted fact sync") {
		t.Fatalf("nil factsSync must leave no trace, got %q", out.String())
	}
}

func TestWatchTickRunsHostedSyncOnNoChangeTick(t *testing.T) {
	cursorPath := filepath.Join(t.TempDir(), "watch.json")
	var refreshed, seeded, distilled, synced int
	steps := fakeWatchSteps("ck1:head1", &refreshed, &seeded, &distilled)
	steps.factsSync = func(context.Context) error { synced++; return nil }
	// Seed a cursor matching the fingerprint with a prior refresh: the idle,
	// no-change tick — the main pull scenario for a machine receiving facts.
	if err := saveWatchCursor(cursorPath, watchCursor{LastFingerprint: "ck1:head1", LastRefreshAt: steps.now()}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	var agentCalls int
	watchTick(context.Background(), &out, watchCommandOptions{}, cursorPath, steps, &agentCalls)
	if refreshed != 0 {
		t.Fatalf("no-change tick must not refresh, got %d", refreshed)
	}
	if synced != 1 {
		t.Fatalf("an idle repo must still pull teammates' facts, got sync=%d", synced)
	}
}
