package cli

import (
	"context"
	"github.com/spf13/cobra"
	"io"
)

// Phase adapters return operation errors. Rendering and failure policy stay in runRefresh.
func runRefreshExportPhase(ctx context.Context, opts Options, refresh refreshCommandOptions, outputExplicit bool, progress func(exportProgress)) error {
	cmd := &cobra.Command{Use: "export"}
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	exportOpts := exportCommandOptions{outputDir: defaultExportDir, outputExplicit: outputExplicit, checkpointLimit: refresh.checkpointLimit, entireBinary: refresh.entireBinary, rawTranscript: refresh.rawTranscript, scope: refresh.scope, progress: progress}
	if outputExplicit {
		exportOpts.outputDir = refresh.outputDir
	}
	return runExport(ctx, cmd, opts, exportOpts)
}

func runRefreshSeedPhase(ctx context.Context, opts Options, seed seedCommandOptions, repoDir, brainDir string, outputExplicit bool, progress func(string)) error {
	cmd := &cobra.Command{Use: "seed"}
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	seed.update = true
	seed.outputExplicit = outputExplicit
	seed.progress = progress
	if outputExplicit {
		seed.outputDir = brainDir
	}
	return runSeed(ctx, cmd, opts, seed, repoDir)
}

func runRefreshSemanticPhase(ctx context.Context, opts Options, index semanticIndexOptions, repoDir string) error {
	cmd := &cobra.Command{Use: "index"}
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	return runSemanticIndex(ctx, cmd, opts, index, repoDir)
}
