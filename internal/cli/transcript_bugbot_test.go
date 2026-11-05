package cli

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/historystore"
	"github.com/spf13/cobra"
)

// Bundle discovery uses the semantic fixture, while checkpoint object reads
// use its real Git repository and the production no-lazy-fetch environment.
type bundleCheckpointRunner struct {
	ExecRunner
	discovery CommandRunner
}

func (r bundleCheckpointRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
	return r.discovery.Run(ctx, dir, name, args...)
}

func TestFilelessRefreshPreparesHistoryOutsideWriteLock(t *testing.T) {
	opts, brain, _ := filelessCheckpointFixture(t)
	originalWrite := historyProjectionWriteFile
	checked := false
	historyProjectionWriteFile = func(dir, rel string, data []byte, mode os.FileMode) error {
		if !checked && strings.HasPrefix(rel, historystore.StagingDir+"/") {
			checked = true
			unlock, err := brainstore.AcquireBrainWriteLockTimeout(brain, 100*time.Millisecond)
			if err != nil {
				t.Fatalf("history preparation holds the brain write lock: %v", err)
			}
			unlock()
		}
		return originalWrite(dir, rel, data, mode)
	}
	t.Cleanup(func() { historyProjectionWriteFile = originalWrite })
	cmd := &cobra.Command{}
	cmd.SetOut(io.Discard)
	err := runExport(context.Background(), cmd, opts, exportCommandOptions{scope: exportScopeAll, entireBinary: "entire", rawTranscript: true, historyIndex: true})
	if !checked {
		t.Fatal("history was not prepared")
	}
	if err != nil {
		t.Fatal(err)
	}
}
