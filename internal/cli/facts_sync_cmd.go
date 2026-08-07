package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ashtom/entire-brain/internal/factgitmeta"
	"github.com/ashtom/entire-brain/internal/factsync"

	"github.com/spf13/cobra"
)

const (
	// factsBackendLocalGitmeta is the default: a bare, on-disk git-meta store
	// under the plugin cache — fully local, no network/entiredb/Postgres.
	factsBackendLocalGitmeta = "local-gitmeta"
	// factsBackendHTTP drives the hosted entire-api FactSetStore (factsync.HTTPServer).
	factsBackendHTTP = "http"
	// envFactsBackend selects the backend when --facts-backend is unset.
	envFactsBackend = "ENTIRE_BRAIN_FACTS_BACKEND"
)

// factsSyncOptions bundles the flag state for `facts sync`.
type factsSyncOptions struct {
	branch  string
	backend string
	member  string
	// http backend only (mirror publish's flag/env resolution):
	apiURL string
	token  string
	repoID string

	jsonOut bool
}

func newFactsSyncCommand(opts Options) *cobra.Command {
	var syncOpts factsSyncOptions
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Read-merge-CAS this branch's local facts into the shared fact-set head",
		Long: `sync runs the factsync read-merge-CAS loop for the current branch: pull the
current fact-set head, keep-both merge this member's local facts into it, and
compare-and-swap the result back — retrying on a losing swap.

Backends (--facts-backend, or ` + envFactsBackend + `):
  local-gitmeta  (default) a bare, on-disk git-meta store under the brain cache
                 (<cache>/brain/<repo-key>/gitmeta.git); fully local, no network,
                 entiredb, or Postgres.
  http           the hosted entire-api FactSetStore; opt-in, requires
                 --api-url/ENTIRE_API_URL, --repo-id/ENTIRE_REPO_ID,
                 --token/ENTIRE_API_TOKEN and ENTIRE_BRAIN_ALLOW_HOSTED=1.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFactsSync(cmd, opts, syncOpts)
		},
	}
	cmd.Flags().StringVar(&syncOpts.branch, "branch", "", "Branch to sync (default: current branch)")
	cmd.Flags().StringVar(&syncOpts.backend, "facts-backend", "", "Fact-set backend: local-gitmeta (default) or http")
	cmd.Flags().StringVar(&syncOpts.member, "member", "", "Member identity stamped on cross-member proposals (default: git user.email)")
	cmd.Flags().StringVar(&syncOpts.repoID, "repo-id", "", "Target repo id for the http backend (overrides ENTIRE_REPO_ID)")
	cmd.Flags().StringVar(&syncOpts.apiURL, "api-url", "", "Entire API base URL for the http backend (overrides ENTIRE_API_URL)")
	cmd.Flags().StringVar(&syncOpts.token, "token", "", "Entire API bearer token for the http backend (overrides ENTIRE_API_TOKEN)")
	cmd.Flags().BoolVar(&syncOpts.jsonOut, "json", false, "Emit the sync result as JSON")
	return cmd
}

func runFactsSync(cmd *cobra.Command, opts Options, syncOpts factsSyncOptions) error {
	ctx := cmd.Context()

	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, agentSurfaceTarget(opts, nil))
	if err != nil {
		return err
	}
	if !local {
		return fmt.Errorf("facts sync requires a local repository path")
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}

	branch := strings.TrimSpace(syncOpts.branch)
	if branch == "" {
		if current, gitErr := gitScalar(ctx, opts.Runner, repoDir, "branch", "--show-current"); gitErr == nil {
			branch = strings.TrimSpace(current)
		}
	}
	if branch == "" {
		branch = distillDefaultBranch
	}

	facts, err := loadFacts(storage.BrainDir, branch)
	if err != nil {
		return err
	}
	memberID := resolveSyncMemberID(ctx, opts.Runner, repoDir, syncOpts.member)

	srv, repoID, backendLabel, err := buildFactsSyncBackend(opts, resolveFactsBackendName(syncOpts.backend), storage, syncOpts)
	if err != nil {
		return err
	}

	// The merge runs on this runner (ADR-P1-E); the backend only stores the blob
	// and CAS-swaps the head. Sync retries on a losing swap and never drops a
	// member's fact.
	res, err := factsync.Sync(ctx, srv, repoID, branch, memberID, facts, opts.Now().UTC())
	if err != nil {
		return err
	}
	if err := persistFactsSyncProposals(storage.BrainDir, branch, res.Proposals); err != nil {
		return err
	}

	if syncOpts.jsonOut {
		return writeJSON(cmd, map[string]any{
			"branch":    branch,
			"backend":   backendLabel,
			"member":    memberID,
			"published": res.Published,
			"ref":       res.NewRef,
			"attempts":  res.Attempts,
			"proposals": res.Proposals,
			"facts":     len(facts),
		})
	}

	out := cmd.OutOrStdout()
	if res.Published {
		fmt.Fprintf(out, "synced %d local fact(s) on %s via %s: head advanced to %s (%d attempt(s))\n",
			len(facts), branch, backendLabel, res.NewRef, res.Attempts)
	} else {
		fmt.Fprintf(out, "synced %d local fact(s) on %s via %s: no change; head at %s (%d attempt(s))\n",
			len(facts), branch, backendLabel, refOrNone(res.NewRef), res.Attempts)
	}
	if len(res.Proposals) > 0 {
		fmt.Fprintf(out, "%d cross-member conflict(s) queued for review:\n", len(res.Proposals))
		for _, p := range res.Proposals {
			fmt.Fprintf(out, "  %s %s -> %s (by %s, confidence %.2f)\n", p.Action, p.CandidateID, p.TargetID, p.ProposedBy, p.Confidence)
		}
	}
	return nil
}

// persistFactsSyncProposals appends cross-member conflicts to the same durable
// queue consumed by facts review/status and the recall pending-review guard.
// The brain write lock makes the read-merge-write atomic with other local
// writers, while writeFactProposals supplies stable ordering and deduplication.
func persistFactsSyncProposals(brainDir, branch string, proposals []factProposal) error {
	if len(proposals) == 0 {
		return nil
	}
	return withBrainWriteLock(brainDir, func() error {
		existing, err := loadFactProposals(brainDir, branch)
		if err != nil {
			return fmt.Errorf("facts sync: load review queue: %w", err)
		}
		if err := writeFactProposals(brainDir, branch, append(existing, proposals...)); err != nil {
			return fmt.Errorf("facts sync: persist review queue: %w", err)
		}
		return nil
	})
}

// resolveFactsBackendName folds the --facts-backend flag, the env override, and
// the default into a single backend name.
func resolveFactsBackendName(flagVal string) string {
	if v := strings.TrimSpace(flagVal); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv(envFactsBackend)); v != "" {
		return v
	}
	return factsBackendLocalGitmeta
}

// buildFactsSyncBackend constructs the selected Server, returning it, the repoID
// to thread through Sync, and a human label for output. The local-gitmeta path
// is keyed by the repo storage key; the http path mirrors publish's opt-in gates
// and flag/env resolution.
func buildFactsSyncBackend(opts Options, backend string, storage repoStorage, syncOpts factsSyncOptions) (factsync.Server, string, string, error) {
	switch backend {
	case factsBackendLocalGitmeta:
		gitDir, err := gitmetaDirForKey(opts.Env, storage.Key)
		if err != nil {
			return nil, "", "", err
		}
		b, err := factgitmeta.NewLocalBackend(gitDir, storage.Key, opts.Now)
		if err != nil {
			return nil, "", "", err
		}
		return b, storage.Key, "local-gitmeta (" + gitDir + ")", nil

	case factsBackendHTTP:
		// The master local-only switch always wins over the hosted opt-in.
		if brainNoEgressMode() {
			return nil, "", "", fmt.Errorf("no_egress: hosted fact sync is disabled while ENTIRE_BRAIN_NO_EGRESS or ENTIRE_BRAIN_LOCAL_ONLY is set; unset it or use --facts-backend=local-gitmeta")
		}
		// Hosted egress is opt-in and fails closed (mirrors publish).
		if !envBool(envBrainAllowHosted) {
			return nil, "", "", fmt.Errorf("hosted_sync_disabled: the http fact backend is opt-in and off by default; set %s=1 to enable it, or use --facts-backend=local-gitmeta", envBrainAllowHosted)
		}
		repoID := publishFlagOrEnv(syncOpts.repoID, envRepoID)
		if repoID == "" {
			return nil, "", "", fmt.Errorf("facts sync: target repo id is required for the http backend; set --repo-id or %s", envRepoID)
		}
		baseURL := publishFlagOrEnv(syncOpts.apiURL, envAPIBaseURL)
		if baseURL == "" {
			return nil, "", "", fmt.Errorf("facts sync: API base URL is required for the http backend; set --api-url or %s", envAPIBaseURL)
		}
		token := publishFlagOrEnv(syncOpts.token, envAPIToken)
		if token == "" {
			return nil, "", "", fmt.Errorf("facts sync: API token is required for the http backend; set --token or %s", envAPIToken)
		}
		return &factsync.HTTPServer{BaseURL: baseURL, Token: token}, repoID, "http (" + strings.TrimRight(baseURL, "/") + ")", nil

	default:
		return nil, "", "", fmt.Errorf("unknown --facts-backend %q (want %q or %q)", backend, factsBackendLocalGitmeta, factsBackendHTTP)
	}
}

// resolveSyncMemberID derives the member identity stamped on cross-member
// proposals: the explicit --member, else the repo's git user.email, else
// $USER@<hostname>, else "local". A non-empty id is required (Sync rejects "").
func resolveSyncMemberID(ctx context.Context, runner CommandRunner, repoDir, override string) string {
	if v := strings.TrimSpace(override); v != "" {
		return v
	}
	if email, err := gitScalar(ctx, runner, repoDir, "config", "--get", "user.email"); err == nil {
		if e := strings.TrimSpace(email); e != "" {
			return e
		}
	}
	if u := strings.TrimSpace(os.Getenv("USER")); u != "" {
		if host, err := os.Hostname(); err == nil && host != "" {
			return u + "@" + host
		}
		return u
	}
	return "local"
}

// gitmetaDirForKey resolves the local git-meta store path for a repo key —
// <cache>/brain/<key>/gitmeta.git — mirroring brainDirForKey's key handling but
// rooted in the plugin CACHE tree (the store is a local, rebuildable sync
// staging area, not authoritative brain data).
func gitmetaDirForKey(env EntireEnv, key string) (string, error) {
	if first, _, _ := strings.Cut(key, "/"); first == workspaceDirName {
		return "", fmt.Errorf("repo key uses the reserved %q segment: %s", workspaceDirName, key)
	}
	dirs, err := resolvePluginDirs(env)
	if err != nil {
		return "", err
	}
	root := filepath.Join(dirs.Cache, "brain")
	if err := rejectBrainRootPathSymlinks(root, filepath.FromSlash(key)); err != nil {
		return "", err
	}
	return filepath.Join(root, filepath.FromSlash(key), "gitmeta.git"), nil
}

// refOrNone renders an empty ref as "(none)" for the no-change/empty case.
func refOrNone(ref string) string {
	if ref == "" {
		return "(none)"
	}
	return ref
}
