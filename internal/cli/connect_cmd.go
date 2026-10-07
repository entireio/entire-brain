package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/entireio/entire-brain/internal/apiurl"

	"github.com/spf13/cobra"
)

// whereamiResult is the host CLI's `entire repo whereami --json` contract
// (COR-2029): the repo's region-local ULID, its cell API base URL, and the
// jurisdiction (saved auth context) tokens are minted against.
type whereamiResult struct {
	RepoID       string `json:"repo_id"`
	APIURL       string `json:"api_url"`
	Jurisdiction string `json:"jurisdiction"`
}

type connectOptions struct {
	repoID       string
	apiURL       string
	jurisdiction string
}

func newConnectCommand(opts Options) *cobra.Command {
	var connectOpts connectOptions
	cmd := &cobra.Command{
		Use:   "connect",
		Short: "Connect this repository to its hosted brain for automatic fact sync",
		Long: `connect binds this repository to its hosted brain: it records the repo id,
API base URL, and jurisdiction (non-secrets only — tokens are minted fresh per
sync), which enables the daemon and session-end hook to sync durable facts
automatically.

With no flags, placement is resolved through the host CLI
(entire repo whereami --json). Pass --repo-id/--api-url (and optionally
--jurisdiction) to bind explicitly.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConnect(cmd, opts, connectOpts)
		},
	}
	cmd.Flags().StringVar(&connectOpts.repoID, "repo-id", "", "Hosted repo id (region-local ULID)")
	cmd.Flags().StringVar(&connectOpts.apiURL, "api-url", "", "Entire API base URL, https://")
	cmd.Flags().StringVar(&connectOpts.jurisdiction, "jurisdiction", "", "Auth context tokens are minted against (entire auth contexts)")
	return cmd
}

func runConnect(cmd *cobra.Command, opts Options, connectOpts connectOptions) error {
	ctx := cmd.Context()
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, agentSurfaceTarget(opts, nil))
	if err != nil {
		return err
	}
	if !local {
		return fmt.Errorf("connect requires a local repository path")
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}

	binding := hostedRepoBinding{
		RepoID:       strings.TrimSpace(connectOpts.repoID),
		BaseURL:      strings.TrimSpace(connectOpts.apiURL),
		Jurisdiction: strings.TrimSpace(connectOpts.jurisdiction),
	}
	if binding.RepoID == "" || binding.BaseURL == "" {
		resolved, whereamiErr := resolveWhereami(ctx, opts.Runner, repoDir)
		if whereamiErr != nil {
			return fmt.Errorf("connect: resolve hosted placement (`entire repo whereami --json`): %w; pass --repo-id and --api-url explicitly", whereamiErr)
		}
		if binding.RepoID == "" {
			binding.RepoID = resolved.RepoID
		}
		if binding.BaseURL == "" {
			binding.BaseURL = resolved.APIURL
		}
		if binding.Jurisdiction == "" {
			binding.Jurisdiction = resolved.Jurisdiction
		}
	}
	if binding.RepoID == "" {
		return fmt.Errorf("connect: hosted repo id is empty; pass --repo-id")
	}
	validated, err := apiurl.Validate(binding.BaseURL)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	binding.BaseURL = validated

	if err := writeHostedRepoBinding(storage.BrainDir, binding); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "connected: repo %s via %s\n", binding.RepoID, binding.BaseURL)
	return nil
}

// resolveWhereami shells out to the host CLI for this repo's hosted placement.
func resolveWhereami(ctx context.Context, runner CommandRunner, repoDir string) (whereamiResult, error) {
	stdout, _, err := runner.Run(ctx, repoDir, "entire", "repo", "whereami", "--json")
	if err != nil {
		return whereamiResult{}, err
	}
	var result whereamiResult
	if err := decodeHostCLIJSON(stdout, &result); err != nil {
		return whereamiResult{}, fmt.Errorf("parse whereami json: %w", err)
	}
	return result, nil
}
