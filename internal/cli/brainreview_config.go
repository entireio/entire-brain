package cli

import (
	"context"

	"github.com/entireio/entire-brain/internal/cli/command/brainreview"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

// brainReviewConfig binds the shared regression engine, brain/repo status
// resolution, and the retrieval privacy transport at the assembly boundary.
// Command packages never import cli, and no mutable hook is copied.
func brainReviewConfig(opts Options) brainreview.Config {
	return brainreview.Config{Options: opts, Deps: brainreview.Deps{
		AgentSurfaceTarget: agentSurfaceTarget,
		BrainStatus: func(ctx context.Context, opts base.Options, target string) (string, string, *bmanifest.SemanticSource, error) {
			status, err := buildBrainStatusReport(ctx, opts, target)
			if err != nil {
				return "", "", nil, err
			}
			var semSource *semanticSourceManifest
			if status.Manifest != nil && status.Manifest.Sources != nil {
				semSource = status.Manifest.Sources.Semantic
			}
			return status.Brain.Path, status.Repo.Root, semSource, nil
		},
		DetectAnomalies:                 detectRegressionAnomaliesCapped,
		RuntimeTraceFacts:               semanticRuntimeTraceFacts,
		RequirePrivacyDerivedRead:       requirePrivacyDerivedRead,
		RequirePatternCorpusAvailable:   requirePatternCorpusAvailable,
		LoadReviewPatternContextChecked: loadReviewPatternContextChecked,
		BufferRetrievalOutput:           bufferRetrievalCommandOutput,
	}}
}
