package watch

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestWatchTickLeavesLegacySyncCursorUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watch.json")
	var refreshed, seeded, distilled, agentCalls int
	steps := fakeWatchSteps("same-head", &refreshed, &seeded, &distilled)
	lastSync := steps.Now().Add(-24 * time.Hour)
	if err := SaveCursor(path, Cursor{LastFingerprint: "same-head", LastRefreshAt: steps.Now(), LastFactsSyncAt: lastSync}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	Tick(context.Background(), &out, DefaultFlags(), path, steps, &agentCalls)
	if refreshed != 0 || !LoadCursor(path).LastFactsSyncAt.Equal(lastSync) {
		t.Fatalf("idle tick changed state: %s", out.String())
	}
}
