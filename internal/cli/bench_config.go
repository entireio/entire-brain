package cli

import (
	"context"

	"github.com/entireio/entire-brain/internal/cli/command/bench"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/spf13/cobra"
)

// benchConfig binds repo identity resolution, the indexing engine, and the
// rusage probe at the assembly boundary. `bench scale` stays in this package
// with the retrieval engine it measures, so its constructor is injected too.
func benchConfig(opts Options) bench.Config {
	return bench.Config{Options: opts, Deps: bench.Deps{
		RepoStoragePaths: repoStoragePaths,
		RunSemanticIndex: func(ctx context.Context, cmd *cobra.Command, opts base.Options, req bench.IndexRequest, target string) error {
			return runSemanticIndex(ctx, cmd, opts, semanticIndexOptions{
				force:             req.Force,
				graphBinary:       req.GraphBinary,
				profile:           req.Profile,
				timeout:           req.Timeout,
				inactivityTimeout: req.InactivityTimeout,
				progress:          req.Progress,
			}, target)
		},
		ProcessMaxRSSBytes: processMaxRSSBytes,
		ScaleCommand:       func() *cobra.Command { return newScaleBenchCommand(opts) },
	}}
}
