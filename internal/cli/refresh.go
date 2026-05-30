package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
)

type refreshCommandOptions struct {
	checkpointLimit int
	entireBinary    string
	rawTranscript   bool
	scope           string
	seed            seedCommandOptions
}

func newRefreshCommand(opts Options) *cobra.Command {
	refreshOpts := refreshCommandOptions{
		checkpointLimit: defaultCheckpointLimit,
		entireBinary:    "entire",
		scope:           exportScopeAll,
		seed: seedCommandOptions{
			includeTests:       true,
			maxFileBytes:       defaultSeedMaxFileBytes,
			maxFiles:           defaultSeedMaxFiles,
			format:             "markdown+json",
			agent:              "none",
			agentQuickTimeout:  2 * time.Minute,
			agentDeepTimeout:   10 * time.Minute,
			agentTimeoutAction: "keep-quick",
			agentMaxInputBytes: defaultAgentMaxInput,
		},
	}
	cmd := &cobra.Command{
		Use:   "refresh",
		Short: "Refresh session history and seed baseline when needed",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRefresh(cmd.Context(), cmd, opts, refreshOpts)
		},
	}
	cmd.Flags().IntVar(&refreshOpts.checkpointLimit, "checkpoint-limit", defaultCheckpointLimit, "Maximum checkpoints to inspect")
	cmd.Flags().StringVar(&refreshOpts.entireBinary, "entire-binary", "entire", "Entire CLI binary to invoke")
	cmd.Flags().BoolVar(&refreshOpts.rawTranscript, "raw", false, "Export raw agent transcripts instead of normalized compact transcripts")
	cmd.Flags().StringVar(&refreshOpts.scope, "scope", exportScopeAll, "Checkpoint discovery scope: all or branch")
	cmd.Flags().BoolVar(&refreshOpts.seed.force, "force-seed", false, "Force seed refresh")
	cmd.Flags().BoolVar(&refreshOpts.seed.worktree, "worktree", false, "Include selected untracked instruction/docs files in seed")
	cmd.Flags().StringVar(&refreshOpts.seed.agent, "agent", "none", "Agent synthesis mode for seed: none, command, or codex")
	cmd.Flags().StringArrayVar(&refreshOpts.seed.agentCommand, "agent-command", nil, "Agent command argv for --agent command")
	return cmd
}

func runRefresh(ctx context.Context, cmd *cobra.Command, opts Options, refreshOpts refreshCommandOptions) error {
	repoDir, err := exportRepoDir(opts.Env)
	if err != nil {
		return err
	}
	exportCmd := &cobra.Command{Use: "export"}
	exportCmd.SetOut(io.Discard)
	exportCmd.SetErr(io.Discard)
	exportOpts := exportCommandOptions{
		outputDir:       defaultExportDir,
		checkpointLimit: refreshOpts.checkpointLimit,
		entireBinary:    refreshOpts.entireBinary,
		rawTranscript:   refreshOpts.rawTranscript,
		scope:           refreshOpts.scope,
	}
	exportErr := runExport(ctx, exportCmd, opts, exportOpts)

	storage, storageErr := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if storageErr != nil {
		return storageErr
	}
	manifest, _ := loadBrainManifest(storage.BrainDir)
	needSeed := refreshOpts.seed.force || seedNeededForBrain(manifest)
	if exportErr != nil && manifest.Sources == nil {
		needSeed = true
	}
	if needSeed {
		seedCmd := &cobra.Command{Use: "seed"}
		seedCmd.SetOut(io.Discard)
		seedCmd.SetErr(io.Discard)
		seedOpts := refreshOpts.seed
		seedOpts.update = true
		if err := runSeed(ctx, seedCmd, opts, seedOpts, repoDir); err != nil {
			return err
		}
	}
	if exportErr != nil && !needSeed {
		return exportErr
	}
	fmt.Fprintf(cmd.OutOrStdout(), "refreshed brain: %s\n", storage.BrainDir)
	return nil
}

func seedNeededForBrain(manifest *exportManifest) bool {
	if manifest == nil || manifest.Sources == nil || manifest.Sources.Seed == nil {
		return true
	}
	if manifest.Sources.Sessions == nil || len(manifest.Sources.Sessions.Sessions) == 0 {
		return true
	}
	seed := manifest.Sources.Seed
	if seed.HistoryBaseline != nil && seed.HistoryBaseline.SeedRequired {
		return false
	}
	return false
}
