package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type refreshCommandOptions struct {
	checkpointLimit  int
	entireBinary     string
	rawTranscript    bool
	scope            string
	semantic         bool
	semanticWorktree bool
	allBranches      bool
	forceAllBranches bool
	historyIndex     bool
	semBinary        string
	seed             seedCommandOptions
}

func newRefreshCommand(opts Options) *cobra.Command {
	refreshOpts := refreshCommandOptions{
		checkpointLimit: defaultCheckpointLimit,
		entireBinary:    "entire",
		semBinary:       "entire",
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
	cmd.Flags().StringVar(&refreshOpts.seed.agent, "agent", "none", "Agent synthesis mode for seed: none, command, codex, or claude-code")
	cmd.Flags().StringArrayVar(&refreshOpts.seed.agentCommand, "agent-command", nil, "Agent command argv for --agent command")
	cmd.Flags().BoolVar(&refreshOpts.semantic, "semantic", false, "Refresh the local semantic index after session and seed refresh")
	cmd.Flags().BoolVar(&refreshOpts.semanticWorktree, "semantic-worktree", false, "Allow semantic indexing of the current dirty worktree")
	cmd.Flags().BoolVar(&refreshOpts.historyIndex, "history-index", false, "Build a decision/rationale index from exported sessions")
	cmd.Flags().StringVar(&refreshOpts.semBinary, "sem-binary", "entire", "Entire CLI binary that exposes `sem` provider commands")
	cmd.Flags().BoolVar(&refreshOpts.allBranches, "all-branches", false, "Refresh recent local branch overlays without fetching remotes")
	cmd.Flags().BoolVar(&refreshOpts.forceAllBranches, "force-all-branches", false, "Allow all local branches instead of the bounded recent-branch default")
	return cmd
}

func runRefresh(ctx context.Context, cmd *cobra.Command, opts Options, refreshOpts refreshCommandOptions) error {
	if refreshOpts.allBranches && !refreshOpts.semantic {
		return errors.New("--all-branches requires --semantic")
	}
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
	if refreshOpts.historyIndex {
		if _, err := writeBrainHistoryIndexAndSource(storage.BrainDir, opts.Now().UTC()); err != nil {
			return err
		}
	}
	if refreshOpts.semantic {
		indexCmd := &cobra.Command{Use: "index"}
		indexCmd.SetOut(io.Discard)
		indexCmd.SetErr(io.Discard)
		if err := runSemanticIndex(ctx, indexCmd, opts, semanticIndexOptions{force: true, semBinary: refreshOpts.semBinary, worktree: refreshOpts.semanticWorktree}, repoDir); err != nil {
			return err
		}
	}
	if refreshOpts.allBranches {
		if err := runSemanticRefreshAllBranches(ctx, opts, refreshOpts, repoDir); err != nil {
			return err
		}
	}
	fmt.Fprintf(cmd.OutOrStdout(), "refreshed brain: %s\n", storage.BrainDir)
	return nil
}

func runSemanticRefreshAllBranches(ctx context.Context, opts Options, refreshOpts refreshCommandOptions, repoDir string) error {
	if !refreshOpts.semantic {
		return errors.New("--all-branches requires --semantic")
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}
	unlock, err := acquireSemanticIndexLock(storage.BrainDir)
	if err != nil {
		return err
	}
	defer unlock()
	manifest, err := loadBrainManifest(storage.BrainDir)
	if err != nil {
		return err
	}
	if manifest.Sources == nil || manifest.Sources.Semantic == nil || manifest.Sources.Semantic.SnapshotPath == "" {
		return errors.New("semantic index missing; run `entire brain refresh --semantic` first")
	}
	defaultBranch, _ := detectDefaultBranch(ctx, opts.Runner, repoDir)
	defaultHead := ""
	if defaultBranch != "" {
		defaultHead = strings.TrimSpace(string(runGitOutput(ctx, opts.Runner, repoDir, "rev-parse", "refs/heads/"+defaultBranch)))
		if defaultHead == "" {
			defaultHead = strings.TrimSpace(string(runGitOutput(ctx, opts.Runner, repoDir, "rev-parse", "refs/remotes/origin/"+defaultBranch)))
		}
	}
	stdout, _, err := opts.Runner.Run(ctx, repoDir, "git", "for-each-ref", "--format=%(refname:short)%00%(committerdate:unix)", "refs/heads")
	if err != nil {
		return fmt.Errorf("list local branches for semantic refresh: %w", err)
	}
	now := opts.Now().UTC()
	type branchRefresh struct {
		Branch      string `json:"branch"`
		State       string `json:"state"`
		Reason      string `json:"reason,omitempty"`
		OverlayPath string `json:"overlay_path,omitempty"`
	}
	report := struct {
		GeneratedAt      time.Time       `json:"generated_at"`
		RetentionDays    int             `json:"retention_days"`
		ForceAllBranches bool            `json:"force_all_branches"`
		Branches         []branchRefresh `json:"branches"`
	}{GeneratedAt: now, RetentionDays: 30, ForceAllBranches: refreshOpts.forceAllBranches}
	for _, line := range strings.Split(strings.TrimSpace(string(stdout)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "\x00", 2)
		if len(parts) != 2 {
			continue
		}
		branch := strings.TrimSpace(parts[0])
		if !refreshOpts.forceAllBranches {
			seconds, _ := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
			if seconds > 0 && now.Sub(time.Unix(seconds, 0).UTC()) > 30*24*time.Hour {
				report.Branches = append(report.Branches, branchRefresh{Branch: branch, State: "skipped", Reason: "older_than_30d"})
				continue
			}
		}
		if defaultHead == "" || branch == defaultBranch {
			report.Branches = append(report.Branches, branchRefresh{Branch: branch, State: "skipped", Reason: "no_branch_delta"})
			continue
		}
		head := strings.TrimSpace(string(runGitOutput(ctx, opts.Runner, repoDir, "rev-parse", "refs/heads/"+branch)))
		if head == "" || head == defaultHead {
			report.Branches = append(report.Branches, branchRefresh{Branch: branch, State: "skipped", Reason: "no_branch_delta"})
			continue
		}
		overlayPath, err := writeSemanticOverlayFile(storage.BrainDir, defaultHead, head, branch, "", now)
		if err != nil {
			return err
		}
		report.Branches = append(report.Branches, branchRefresh{Branch: branch, State: "overlay_written", OverlayPath: overlayPath})
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	rel := filepath.Join(semanticDirName, "overlays", "all-branches.json")
	if err := rejectExistingSymlinkPathComponents(storage.BrainDir, rel); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(storage.BrainDir, rel), data, 0o600); err != nil {
		return err
	}
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
