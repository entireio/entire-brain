package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/command/watch"
	"github.com/spf13/cobra"
)

func TestWatchMaintenanceDoesNotContactHostedService(t *testing.T) {
	for _, mode := range []string{"idle", "refresh", "failed_refresh"} {
		t.Run(mode, func(t *testing.T) {
			fake := &hostedFake{}
			opts, _, repoDir, _ := hostedSyncHarness(t, fake, true)
			now := opts.Now()
			flags := watch.DefaultFlags()
			steps := watchStepsForRepo(&cobra.Command{}, opts, flags, repoDir, func() time.Time { return now })
			steps.Delta = nil
			steps.Reconcile = nil
			steps.Fingerprint = func(context.Context) string { return "head" }
			refreshes := 0
			steps.Refresh = func(context.Context) error {
				refreshes++
				if mode == "failed_refresh" {
					return errors.New("refresh failed")
				}
				return nil
			}
			path := filepath.Join(t.TempDir(), "watch.json")
			if mode == "idle" {
				if err := watch.SaveCursor(path, watch.Cursor{LastFingerprint: "head", LastRefreshAt: now}); err != nil {
					t.Fatal(err)
				}
			}
			var out bytes.Buffer
			calls := 0
			watch.Tick(context.Background(), &out, flags, path, steps, &calls)
			now = now.Add(24 * time.Hour)
			watch.Tick(context.Background(), &out, flags, path, steps, &calls)
			if fake.requestCount() != 0 {
				t.Fatalf("%s made %d hosted requests", mode, fake.requestCount())
			}
			if mode == "refresh" && refreshes != 1 {
				t.Fatalf("refreshes=%d", refreshes)
			}
		})
	}
}
