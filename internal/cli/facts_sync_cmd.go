package cli

import (
	"context"
	"errors"
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
	// envMemoriesBackend selects the backend when --memories-backend is unset.
	envMemoriesBackend = "ENTIRE_BRAIN_MEMORIES_BACKEND"
	// envFactsBackend is the pre-rename spelling, honored as a fallback the
	// same way legacyFlagAlias keeps --facts-backend working (COR-2193).
	envFactsBackend = "ENTIRE_BRAIN_FACTS_BACKEND"
)

// factsSyncOptions bundles the flag state for `facts sync`.
type factsSyncOptions struct {
	// branch, when set, is the local branch whose history decides which
	// teammate memories the pull adopts (resolveSyncBranchTip). Empty means
	// the checkout's HEAD.
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

The pull adopts a teammate's memory when one of its anchor commits is
reachable from the checkout's HEAD. --branch NAME judges reachability from
that local branch instead, so a scheduled worker can sync a branch that is not
checked out. The branch is resolved to its commit before anything is sent,
and a branch with no local ref fails the sync rather than falling back to
HEAD. The push half always uploads the repository's whole local store.

ENTIRE_BRAIN_NO_EGRESS (or its deprecated alias ENTIRE_BRAIN_LOCAL_ONLY)
suppresses all hosted sync regardless of defaults and flags.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFactsSync(cmd, opts, syncOpts)
		},
	}
	cmd.Flags().StringVar(&syncOpts.branch, "branch", "", "Local branch whose history decides which teammate memories are adopted (default: the checkout's HEAD)")
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
	backend, backendSource := resolveFactsBackendName(syncOpts.backend)
	switch backend {
	case "", factsBackendHTTP:
	case factsBackendLocalGitmeta:
		return fmt.Errorf("memories sync: the %q backend was removed (COR-2236): memory sync is hosted-only; run '%s' to sync with the team, or drop %s (memories stay local otherwise)",
			factsBackendLocalGitmeta, base.BrainInvocation(os.LookupEnv, "connect"), backendSource)
	default:
		return fmt.Errorf("unknown memories backend %q (from %s): only %q (the hosted per-fact wire) is supported; the local-gitmeta backend was removed (COR-2236)", backend, backendSource, factsBackendHTTP)
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

	branch, visibleFrom, err := resolveSyncBranch(ctx, opts.Runner, repoDir, syncOpts.branch, cmd.Flags().Changed("branch"))
	if err != nil {
		return err
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
		return runHostedFactsSync(cmd, opts, syncOpts, repoDir, storage, branch, visibleFrom)
	}

	// Explicit http: the master kill switch still wins over the opt-in.
	if base.BrainNoEgressMode() {
		return fmt.Errorf("no_egress: hosted memory sync is disabled while %s is set; unset it to sync", base.NoEgressToggleName())
	}
	return runHostedFactsSync(cmd, opts, syncOpts, repoDir, storage, branch, visibleFrom)
}

// runHostedFactsSync is the manual flavor of hostedFactsSyncAndPull: the same
// per-fact exchange (batch-upsert the store, pull the repo delta feed,
// rebuild the store), but the user explicitly asked for this
// sync, so failures surface in the exit code and output instead of degrading
// to a stderr note (COR-2202 precedent).
func runHostedFactsSync(cmd *cobra.Command, opts Options, syncOpts factsSyncOptions, repoDir string, storage repoStorage, branch, visibleFrom string) error {
	ctx := cmd.Context()
	errW := cmd.ErrOrStderr()
	target, err := resolveManualHostedTarget(ctx, opts, storage.BrainDir, repoDir, "memories sync", syncOpts.repoID, syncOpts.apiURL, syncOpts.token)
	if err != nil {
		return err
	}
	srv := &factsync.HTTPServer{BaseURL: target.baseURL, Token: target.token, Version: opts.Version}
	backendLabel := "hosted (" + strings.TrimRight(target.baseURL, "/") + ")"

	counts, skipped, pushErr := pushHostedFacts(ctx, storage, srv, target.repoID)
	if skipped > 0 {
		fmt.Fprintf(errW, "warning: skipped %d unpublishable record(s) whose content id does not re-derive (kept locally, not uploaded)\n", skipped)
	}
	// The pull runs even after a failed push: the team's memories still land,
	// and the error below reports exactly which half failed.
	kept, total, manifestErr, pullErr := pullHostedFacts(ctx, opts, repoDir, storage, srv, target.repoID, target.baseURL, target.token, visibleFrom)
	if pushErr == nil && pullErr == nil {
		// Both halves completed: record the sync marker, same rule as the
		// background sync (the blind-spot note keys off its absence).
		if err := writeHostedFactsBinding(storage.BrainDir, target.repoID, target.baseURL); err != nil {
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

	// The exchange above is repository-scoped, so global memories never ride
	// it (#570). Report how many stayed on this machine: without it a sync
	// that left them behind reads exactly like one that had nothing to send.
	// Reading the global store cannot fail a sync that already completed, but
	// an unreadable store is not an empty one: JSON reports the count as
	// unknown (null) with the reason, never as zero.
	globalLocal, globalErr := localOnlyGlobalFactCount(opts.Env)
	if globalErr != nil {
		fmt.Fprintf(errW, "warning: could not read the global memory store to count what stays on this machine: %v\n", globalErr)
	}

	if syncOpts.jsonOut {
		payload := map[string]any{
			"branch":                branch,
			"backend":               backendLabel,
			"pushed_added":          counts.Added,
			"pushed_changed":        counts.Changed,
			"pushed_unchanged":      counts.Unchanged,
			"skipped_unpublishable": skipped,
			"feed_facts":            total,
			"feed_kept":             kept,
			"global_not_synced":     globalLocal,
		}
		if globalErr != nil {
			payload["global_not_synced"] = nil
			payload["global_not_synced_error"] = globalErr.Error()
		}
		return cmdutil.WriteJSON(cmd, payload)
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "synced %s via %s: pushed added=%d changed=%d unchanged=%d; feed facts=%d kept=%d\n",
		branch, backendLabel, counts.Added, counts.Changed, counts.Unchanged, total, kept)
	if globalLocal > 0 {
		fmt.Fprintf(out, "not synced: %d global memory(ies); %s\n", globalLocal, globalFactsStayLocalNote())
	}
	return nil
}

// resolveSyncBranch returns the branch name the sync reports and visibleFrom,
// the revision the pull judges anchor reachability against. Without --branch
// visibleFrom stays HEAD, read when the pull runs, and the label is the
// checked-out branch. An explicit branch is resolved to its commit here, once,
// before any token mint or network call: a checkout that moves mid-sync cannot
// change the answer, and a missing branch fails before anything is uploaded.
func resolveSyncBranch(ctx context.Context, runner CommandRunner, repoDir, raw string, explicit bool) (branch, visibleFrom string, err error) {
	branch = strings.TrimSpace(raw)
	if branch != "" {
		tip, err := resolveSyncBranchTip(ctx, runner, repoDir, branch)
		if err != nil {
			return "", "", err
		}
		return branch, tip, nil
	}
	if explicit {
		// `--branch "$BRANCH"` with an unset variable must not quietly become
		// a sync of whatever the checkout is on.
		return "", "", errors.New("memories sync: --branch is empty; name a local branch, or leave the flag out to use the checkout's HEAD")
	}
	if current, gitErr := base.GitScalar(ctx, runner, repoDir, "branch", "--show-current"); gitErr == nil {
		branch = strings.TrimSpace(current)
	}
	if branch == "" {
		branch = distillDefaultBranch
	}
	return branch, "HEAD", nil
}

// resolveSyncBranchTip resolves an explicit `memories sync --branch` value to
// the commit its local ref names. Only a local branch counts: a name git would
// reject as a branch (revision syntax such as main~1 included) or one with no
// refs/heads entry is an error, never a silent fallback to the checkout, since
// judging reachability from the wrong commit adopts the wrong teammate
// memories into the store.
//
// It runs before the no-egress gate, so it reads only the ref (show-ref),
// never an object: peeling the tip with rev-parse could lazy-fetch a missing
// promisor object in a partial clone.
func resolveSyncBranchTip(ctx context.Context, runner CommandRunner, repoDir, branch string) (string, error) {
	ref := "refs/heads/" + branch
	if _, err := base.GitScalar(ctx, runner, repoDir, "check-ref-format", ref); err != nil {
		return "", fmt.Errorf("memories sync: --branch %q is not a valid branch name", branch)
	}
	tip, err := base.GitScalar(ctx, runner, repoDir, "show-ref", "--verify", "--hash", ref)
	if err != nil || tip == "" {
		return "", fmt.Errorf("memories sync: --branch %q has no local branch (%s); fetch or create it first (sync does not fall back to the checkout's HEAD)", branch, ref)
	}
	return tip, nil
}

// resolveFactsBackendName folds the --memories-backend flag and the env
// overrides into an explicit backend name plus the source it came from, so a
// rejection can tell the user which setting to drop. Empty means "no explicit
// choice": the caller requires a connected repo and syncs hosted.
func resolveFactsBackendName(flagVal string) (name, source string) {
	if v := strings.TrimSpace(flagVal); v != "" {
		return v, "--memories-backend"
	}
	if v := strings.TrimSpace(os.Getenv(envMemoriesBackend)); v != "" {
		return v, envMemoriesBackend
	}
	return strings.TrimSpace(os.Getenv(envFactsBackend)), envFactsBackend
}
