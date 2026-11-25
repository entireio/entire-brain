package cli

import (
	"context"
	"io"
	"sync"

	"github.com/entireio/entire-brain/internal/cli/workspace"
)

// workspaceRetrievalWorkers bounds simultaneously loaded member corpora.
// Keep this independent of workspace size; more members must not create more
// active readers or one waiting goroutine per repository. Two trades some of
// the four-worker latency gain for a smaller peak-memory increase; remeasure
// with BenchmarkWorkspaceRetrievalWorkers when changing this limit.
const workspaceRetrievalWorkers = 2

// retrieveWorkspaceMembers follows the indexed worker pattern used by checkpoint
// export. Each worker owns one result position at a time, and only the caller
// renders results. Member errors are data, so they never cancel healthy peers.
func retrieveWorkspaceMembers(ctx context.Context, repos []workspace.Repo, workers int, retrieve func(context.Context, workspace.Repo) workspaceRetrieveResult) ([]workspaceRetrieveResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(repos) == 0 {
		return nil, nil
	}
	workers = min(max(workers, 1), len(repos))
	results := make([]workspaceRetrieveResult, len(repos))
	positions := make(chan int)
	var done sync.WaitGroup
	for range workers {
		done.Add(1)
		go func() {
			defer done.Done()
			for i := range positions {
				if ctx.Err() != nil {
					return
				}
				results[i] = retrieve(ctx, repos[i])
			}
		}()
	}
dispatch:
	for i := range repos {
		if ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
			break dispatch
		case positions <- i:
		}
	}
	close(positions)
	done.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

// Check immediately before output as well as after worker completion: cancellation
// during graph lookup, rendering, or encoding must not become a successful query.
func writeWorkspaceRetrievalResponse(ctx context.Context, out io.Writer, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := writeRetrievalResponseBytes(out, data); err != nil {
		return err
	}
	return ctx.Err()
}
