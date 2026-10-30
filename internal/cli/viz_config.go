package cli

import (
	"context"

	"github.com/entireio/entire-brain/internal/cli/command/viz"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

func vizConfig(opts Options) viz.Config {
	return viz.Config{Options: opts, Deps: vizDeps()}
}

// vizDeps binds the cli-private seams the viz server needs; the transport and
// regression tests that construct a viz.Server directly reuse it.
func vizDeps() viz.Deps {
	return viz.Deps{
		AgentSurfaceTarget: agentSurfaceTarget,
		ResolveFactsTarget: resolveFactsTarget,
		BrainManifest: func(ctx context.Context, opts Options, target string) (*bmanifest.Manifest, error) {
			status, err := buildBrainStatusReport(ctx, opts, target)
			if err != nil {
				return nil, err
			}
			return status.Manifest, nil
		},
		RetrieveUnified:         retrieveUnified,
		CommitRetrievalResponse: commitVizRetrievalResponse,
	}
}
