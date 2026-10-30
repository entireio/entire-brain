package cli

import (
	"fmt"
	"github.com/entireio/entire-brain/internal/cli/internal/cmdutil"
	"os"
	"strings"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/factsync"
	"github.com/spf13/cobra"
)

const (
	// factsBackendLocalGitmeta is the REMOVED local git-meta backend
	// (COR-2236). The name is kept so the removed value fails with a clear
	// message instead of "unknown backend".
	factsBackendLocalGitmeta = "local-gitmeta"
	// factsBackendHTTP is the hosted per-fact wire (factsync perfact.go) —
	// the only sync backend.
	factsBackendHTTP = "http"
	// envFactsBackend selects the backend when --memories-backend is unset.
	envFactsBackend = "ENTIRE_BRAIN_FACTS_BACKEND"
)

// factsSyncOptions bundles the flag state for `facts sync`.
type factsSyncOptions struct {
	branch  string
	backend string
	// explicit hosted target (flag wins over its ENTIRE_* env variable):
	apiURL string
	token  string
	repoID string

	jsonOut bool
}

func newFactsSyncCommand(opts Options) *cobra.Command {
	var syncOpts factsSyncOptions
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Sync this branch's local memories with the team's hosted memory set",
		Long: `sync exchanges this branch's local memories with the team's hosted memory set
over the per-fact wire: push this branch's memories, pull the team's delta feed.
It is the same exchange the background sync runs, except failures fail the
command instead of degrading silently.

Memory sync is hosted-only. The default target is the connected binding
('` + base.BrainInvocation(os.LookupEnv, "connect") + `'); on an unconnected repo a plain sync fails and
memories stay local. The local-gitmeta backend was removed (COR-2236):
--memories-backend accepts only "http", and the explicit target resolves
flag > env > connected binding (--api-url/ENTIRE_API_URL,
--repo-id/ENTIRE_REPO_ID, --token/ENTIRE_API_TOKEN; on a connected repo the
binding supplies the target and the token is minted automatically).

ENTIRE_BRAIN_NO_EGRESS (or its deprecated alias ENTIRE_BRAIN_LOCAL_ONLY)
suppresses all hosted sync regardless of defaults and flags.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFactsSync(cmd, opts, syncOpts)
		},
	}
	cmd.Flags().StringVar(&syncOpts.branch, "branch", "", "Branch to sync (default: current branch)")
	cmd.Flags().StringVar(&syncOpts.backend, "memories-backend", "", `Memory-set backend; only "http" (the hosted per-fact wire) is supported`)
	cmd.Flags().StringVar(&syncOpts.repoID, "repo-id", "", "Target repo id (overrides ENTIRE_REPO_ID)")
	cmd.Flags().StringVar(&syncOpts.apiURL, "api-url", "", "Entire API base URL, https:// (overrides ENTIRE_API_URL)")
	cmd.Flags().StringVar(&syncOpts.token, "token", "", "Entire API bearer token (overrides ENTIRE_API_TOKEN)")
	cmd.Flags().BoolVar(&syncOpts.jsonOut, "json", false, "Emit the sync result as JSON")
	// --facts-backend predates the facts→memories rename; keep it working.
	legacyFlagAlias(cmd.Flags(), "facts-backend", "memories-backend")
	return cmd
}

func runFactsSync(cmd *cobra.Command, opts Options, syncOpts factsSyncOptions) error {
	ctx := cmd.Context()

	// Backend validation first: a removed or unknown value must fail the same
	// way on every repo, before any state is read.
	backend := resolveFactsBackendName(syncOpts.backend)
	switch backend {
	case "", factsBackendHTTP:
	case factsBackendLocalGitmeta:
		return fmt.Errorf("memories sync: the %q backend was removed (COR-2236): memory sync is hosted-only; run '%s' to sync with the team, or drop --memories-backend/%s (memories stay local otherwise)",
			factsBackendLocalGitmeta, base.BrainInvocation(os.LookupEnv, "connect"), envFactsBackend)
	default:
		return fmt.Errorf("unknown --memories-backend %q: only %q (the hosted per-fact wire) is supported; the local-gitmeta backend was removed (COR-2236)", backend, factsBackendHTTP)
	}

	repoDir, local, err := base.ResolveLocalTargetRepoDir(ctx, opts.Runner, agentSurfaceTarget(opts, nil))
	if err != nil {
		return err
	}
	if !local {
		return fmt.Errorf("memories sync requires a local repository path")
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}

	branch := strings.TrimSpace(syncOpts.branch)
	if branch == "" {
		if current, gitErr := base.GitScalar(ctx, opts.Runner, repoDir, "branch", "--show-current"); gitErr == nil {
			branch = strings.TrimSpace(current)
		}
	}
	if branch == "" {
		branch = distillDefaultBranch
	}

	if backend == "" {
		// No explicit backend: connectedness decides. Connect was the consent
		// moment (ADR-P1-A reconciliation, COR-2233), so a connected repo syncs
		// hosted; an unconnected one has nothing to sync against and fails with
		// the remedy. The no-egress kill switch always wins — it suppresses the
		// sync rather than failing it, because the suppression is the user's
		// own configuration doing exactly what it promises.
		binding, present, bindErr := brainstore.ReadHostedRepoBinding(storage.BrainDir)
		if bindErr != nil {
			return fmt.Errorf("memories sync: hosted binding unreadable: %w; run '%s' to rebind", bindErr, base.BrainInvocation(os.LookupEnv, "connect"))
		}
		if !present || binding.OptedOut {
			return fmt.Errorf("memories sync: this repo is not connected to Brainstore, so there is nothing to sync with (memories stay local); run '%s' to connect, or pass --memories-backend=http with an explicit --repo-id/--api-url/--token target", base.BrainInvocation(os.LookupEnv, "connect"))
		}
		if base.BrainNoEgressMode() {
			// A suppressed sync is the user's own kill switch working, so it
			// exits 0 — but it must still answer in the caller's format.
			if syncOpts.jsonOut {
				return cmdutil.WriteJSON(cmd, map[string]any{
					"branch":  branch,
					"skipped": true,
					"reason":  base.NoEgressToggleName() + " is set",
				})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "memories sync: skipped: %s is set; unset it to sync with Brainstore (memories stay local meanwhile)\n", base.NoEgressToggleName())
			return nil
		}
		return runHostedFactsSync(cmd, opts, syncOpts, repoDir, storage, branch)
	}

	// Explicit http: the master kill switch still wins over the opt-in.
	if base.BrainNoEgressMode() {
		return fmt.Errorf("no_egress: hosted memory sync is disabled while %s is set; unset it to sync", base.NoEgressToggleName())
	}
	return runHostedFactsSync(cmd, opts, syncOpts, repoDir, storage, branch)
}

// runHostedFactsSync is the manual flavor of hostedFactsSyncAndPull: the same
// per-fact exchange (batch-upsert this branch's store, pull the repo delta
// feed, rebuild the branch store), but the user explicitly asked for this
// sync, so failures surface in the exit code and output instead of degrading
// to a stderr note (COR-2202 precedent).
func runHostedFactsSync(cmd *cobra.Command, opts Options, syncOpts factsSyncOptions, repoDir string, storage repoStorage, branch string) error {
	ctx := cmd.Context()
	errW := cmd.ErrOrStderr()
	target, err := resolveManualHostedTarget(ctx, opts, storage.BrainDir, repoDir, "memories sync", syncOpts.repoID, syncOpts.apiURL, syncOpts.token)
	if err != nil {
		return err
	}
	srv := &factsync.HTTPServer{BaseURL: target.baseURL, Token: target.token, Version: opts.Version}
	backendLabel := "hosted (" + strings.TrimRight(target.baseURL, "/") + ")"

	counts, skipped, pushErr := pushHostedFacts(ctx, storage, srv, target.repoID, branch)
	if skipped > 0 {
		fmt.Fprintf(errW, "warning: skipped %d unpublishable record(s) whose content id does not re-derive (kept locally, not uploaded)\n", skipped)
	}
	// The pull runs even after a failed push: the team's memories still land,
	// and the error below reports exactly which half failed.
	kept, total, manifestErr, pullErr := pullHostedFacts(ctx, opts, repoDir, storage, srv, target.repoID, target.baseURL, target.token, branch)
	if pushErr == nil && pullErr == nil {
		// Both halves completed: record the per-branch marker, same rule as the
		// background sync (the blind-spot note keys off its absence).
		if err := writeHostedFactsBinding(storage.BrainDir, branch, target.repoID, target.baseURL); err != nil {
			return fmt.Errorf("memories sync: record hosted target: %w", err)
		}
	}
	if manifestErr != nil {
		fmt.Fprintf(errW, "warning: memories updated, but the source manifest could not be refreshed: %v\n", manifestErr)
	}
	switch {
	case pushErr != nil && pullErr != nil:
		return fmt.Errorf("memories sync: push to %s failed: %v; pull failed: %v", backendLabel, pushErr, pullErr)
	case pushErr != nil:
		return fmt.Errorf("memories sync: pulled %d memory(ies) from %s (%d kept), but the push failed (local brain unaffected): %w", total, backendLabel, kept, pushErr)
	case pullErr != nil:
		return fmt.Errorf("memories sync: pushed %d memory(ies) to %s, but the pull failed (local brain unaffected): %w", counts.Added+counts.Changed+counts.Unchanged, backendLabel, pullErr)
	}

	if syncOpts.jsonOut {
		return cmdutil.WriteJSON(cmd, map[string]any{
			"branch":                branch,
			"backend":               backendLabel,
			"pushed_added":          counts.Added,
			"pushed_changed":        counts.Changed,
			"pushed_unchanged":      counts.Unchanged,
			"skipped_unpublishable": skipped,
			"feed_facts":            total,
			"feed_kept":             kept,
		})
	}
	fmt.Fprintf(cmd.OutOrStdout(), "synced %s via %s: pushed added=%d changed=%d unchanged=%d; feed facts=%d kept=%d\n",
		branch, backendLabel, counts.Added, counts.Changed, counts.Unchanged, total, kept)
	return nil
}

// resolveFactsBackendName folds the --memories-backend flag and the env
// override into an explicit backend name. Empty means "no explicit choice":
// the caller requires a connected repo and syncs hosted.
func resolveFactsBackendName(flagVal string) string {
	if v := strings.TrimSpace(flagVal); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv(envFactsBackend))
}
