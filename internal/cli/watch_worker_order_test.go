package cli

import (
	"context"
	"io"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/command/watch"
	"github.com/spf13/cobra"
)

func TestWatchRefreshDefersWorkerToTickReconciliation(t *testing.T) {
	opts, repoDir, _, launches := watchCommandFixture(t)
	opts.Runner.(*fakeCommandRunner).responses[fakeRoutedCheckpointListKey("entire", true)] = fakeCommandResponse{stdout: `[]`}
	cmd := &cobra.Command{}
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	steps := watchStepsForRepo(cmd, opts, watch.DefaultFlags(), repoDir, opts.Now)
	if err := steps.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*launches) != 0 {
		t.Fatalf("refresh launched a worker before the tick's gated writers finished: %v", *launches)
	}
	if err := steps.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertMemoryWorkerLaunches(t, *launches, repoDir, 1)
}

func TestStandaloneRefreshStillLaunchesMemoryWorker(t *testing.T) {
	for _, setup := range []bool{false, true} {
		name := "workspace refresh"
		if setup {
			name = "setup"
		}
		t.Run(name, func(t *testing.T) {
			opts, repoDir, _, launches := watchCommandFixture(t)
			opts.Runner.(*fakeCommandRunner).responses[fakeRoutedCheckpointListKey("entire", true)] = fakeCommandResponse{stdout: `[]`}
			cmd := &cobra.Command{}
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			var err error
			if setup {
				memoryReports := 0
				err = watchDeterministicRefreshComponents(context.Background(), cmd, opts, repoDir, true, true, func(component string, componentErr error) {
					if component == brainComponentMemory {
						memoryReports++
						if componentErr != nil {
							t.Fatal(componentErr)
						}
					}
				}, false)
				if memoryReports != 1 {
					t.Fatalf("setup memory reports = %d, want 1", memoryReports)
				}
			} else {
				err = watchDeterministicRefresh(context.Background(), cmd, opts, repoDir, false)
			}
			if err != nil {
				t.Fatal(err)
			}
			assertMemoryWorkerLaunches(t, *launches, repoDir, 1)
		})
	}
}
