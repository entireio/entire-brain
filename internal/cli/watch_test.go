package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatchShouldDistill(t *testing.T) {
	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	hourAgo := now.Add(-time.Hour)
	cases := []struct {
		name    string
		w       watchCommandOptions
		cursor  watchCursor
		spent   int
		wantRun bool
	}{
		{"disabled", watchCommandOptions{distill: false, distillEvery: time.Hour}, watchCursor{}, 0, false},
		{"enabled first run (no prior distill)", watchCommandOptions{distill: true, distillEvery: time.Hour}, watchCursor{}, 0, true},
		{"interval not elapsed", watchCommandOptions{distill: true, distillEvery: time.Hour}, watchCursor{LastDistillAt: now.Add(-30 * time.Minute)}, 0, false},
		{"interval elapsed", watchCommandOptions{distill: true, distillEvery: time.Hour}, watchCursor{LastDistillAt: hourAgo}, 0, true},
		{"budget reached", watchCommandOptions{distill: true, distillEvery: 0, budget: 1}, watchCursor{}, 1, false},
		{"budget not reached", watchCommandOptions{distill: true, distillEvery: 0, budget: 2}, watchCursor{}, 1, true},
	}
	for _, tc := range cases {
		if got, _ := watchShouldDistill(tc.w, tc.cursor, tc.spent, now); got != tc.wantRun {
			t.Errorf("%s: shouldDistill = %v, want %v", tc.name, got, tc.wantRun)
		}
	}
}

// fakeWatchSteps records calls so the loop's behaviour can be asserted without a real refresh/distill.
func fakeWatchSteps(fp string, refreshed, distilled *int) watchSteps {
	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	return watchSteps{
		now:         func() time.Time { return now },
		fingerprint: func(context.Context) string { return fp },
		refresh:     func(context.Context) error { *refreshed++; return nil },
		distill:     func(context.Context) error { *distilled++; return nil },
	}
}

func readWatchCursor(t *testing.T, path string) watchCursor {
	t.Helper()
	var c watchCursor
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("parse cursor: %v", err)
	}
	return c
}

func TestWatchLoopOnceRefreshesNoDistillByDefault(t *testing.T) {
	cursor := filepath.Join(t.TempDir(), "watch.json")
	var refreshed, distilled int
	w := watchCommandOptions{once: true, distill: false} // default: distill OFF
	if err := watchLoop(context.Background(), &bytes.Buffer{}, w, cursor, fakeWatchSteps("ck1:head1", &refreshed, &distilled)); err != nil {
		t.Fatalf("watchLoop: %v", err)
	}
	if refreshed != 1 {
		t.Fatalf("deterministic refresh should run once, got %d", refreshed)
	}
	if distilled != 0 {
		t.Fatalf("DEFAULT must spend ZERO agent tokens (no distill), got %d distill calls", distilled)
	}
	got := readWatchCursor(t, cursor)
	if got.LastFingerprint != "ck1:head1" || got.LastRefreshAt.IsZero() {
		t.Fatalf("cursor not persisted: %+v", got)
	}
}

func TestWatchLoopNoChangeIsNoop(t *testing.T) {
	cursor := filepath.Join(t.TempDir(), "watch.json")
	// Seed a cursor matching the fingerprint the steps will report, with a prior refresh.
	if err := saveWatchCursor(cursor, watchCursor{LastFingerprint: "ck1:head1", LastRefreshAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	var refreshed, distilled int
	w := watchCommandOptions{once: true, distill: true, distillEvery: 0}
	if err := watchLoop(context.Background(), &bytes.Buffer{}, w, cursor, fakeWatchSteps("ck1:head1", &refreshed, &distilled)); err != nil {
		t.Fatalf("watchLoop: %v", err)
	}
	if refreshed != 0 || distilled != 0 {
		t.Fatalf("unchanged fingerprint must be a no-op, got refresh=%d distill=%d", refreshed, distilled)
	}
}

func TestWatchLoopDistillWhenEnabledAndElapsed(t *testing.T) {
	cursor := filepath.Join(t.TempDir(), "watch.json")
	var refreshed, distilled int
	w := watchCommandOptions{once: true, distill: true, distillEvery: 0} // elapsed always true
	if err := watchLoop(context.Background(), &bytes.Buffer{}, w, cursor, fakeWatchSteps("ck2:head2", &refreshed, &distilled)); err != nil {
		t.Fatalf("watchLoop: %v", err)
	}
	if refreshed != 1 || distilled != 1 {
		t.Fatalf("expected one refresh + one distill, got refresh=%d distill=%d", refreshed, distilled)
	}
	if got := readWatchCursor(t, cursor); got.LastDistillAt.IsZero() {
		t.Fatalf("distill timestamp must advance so a restart does not re-spend: %+v", got)
	}
}

func TestWatchFingerprintUsesRefs(t *testing.T) {
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "rev-parse", "--verify", "--quiet", v1MainRef):   {stdout: "ckSHA\n"},
		fakeCommandKey("git", "rev-parse", "--verify", "--quiet", v1OriginRef): {stdout: "originSHA\n"},
		fakeCommandKey("git", "rev-parse", "--verify", "--quiet", "HEAD"):      {stdout: "headSHA\n"},
	}}
	if got := watchFingerprint(context.Background(), runner, "/repo"); got != "ckSHA:originSHA:headSHA" {
		t.Fatalf("fingerprint = %q, want ckSHA:originSHA:headSHA (incl. fetched origin checkpoint ref)", got)
	}
}

func TestWatchTickSkipsDistillWhenRefreshFails(t *testing.T) {
	cursorPath := filepath.Join(t.TempDir(), "watch.json")
	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	var refreshed, distilled int
	steps := watchSteps{
		now:         func() time.Time { return now },
		fingerprint: func(context.Context) string { return "ck:orig:head" },
		refresh:     func(context.Context) error { refreshed++; return errors.New("export failed") },
		distill:     func(context.Context) error { distilled++; return nil },
	}
	w := watchCommandOptions{distill: true, distillEvery: 0} // distill WOULD fire if the block were reached
	calls := 0
	watchTick(context.Background(), &bytes.Buffer{}, w, cursorPath, steps, &calls)
	if refreshed != 1 {
		t.Fatalf("refresh should be attempted once, got %d", refreshed)
	}
	if distilled != 0 {
		t.Fatalf("distill MUST NOT run after a failed refresh (no token spend on an unrefreshed brain), got %d", distilled)
	}
	if calls != 0 {
		t.Fatalf("budget counter must not advance on failed refresh, got %d", calls)
	}
	// Cursor must NOT advance, so the next tick retries the free refresh and never re-distills on failure.
	if c := loadWatchCursor(cursorPath); !c.LastRefreshAt.IsZero() || c.LastFingerprint != "" {
		t.Fatalf("cursor must not advance on failed refresh (else thrash): %+v", c)
	}
}

func TestWatchLoopStopsOnContextCancel(t *testing.T) {
	cursorPath := filepath.Join(t.TempDir(), "watch.json")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled — the daemon loop must exit cleanly without ticking
	var refreshed, distilled int
	w := watchCommandOptions{interval: time.Hour} // not --once: would loop forever if cancel weren't honored
	if err := watchLoop(ctx, &bytes.Buffer{}, w, cursorPath, fakeWatchSteps("ck", &refreshed, &distilled)); err != nil {
		t.Fatalf("graceful shutdown must return nil, got %v", err)
	}
	if refreshed != 0 {
		t.Fatalf("a cancelled context must not tick, got %d refreshes", refreshed)
	}
}
