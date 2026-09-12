package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ashtom/entire-brain/internal/factsync"
	"github.com/ashtom/entire-brain/internal/hostedbrain"
)

// `facts proposals` is the user-facing surface for the CROSS-MEMBER review queue: the
// conflicts another member's `facts sync` kept both sides of and routed for a human to
// settle. It is the hosted twin of `facts review` (which resolves this member's own
// low-confidence, on-disk proposals) and shares its verbs: list, show, apply, reject.
//
// Hosted egress is opt-in and off by default, exactly like `publish` (ADR-P1-A, no
// implicit egress): a network call happens only when BOTH gates are satisfied — the
// user explicitly runs one of these commands, AND ENTIRE_BRAIN_ALLOW_HOSTED is truthy.
// The master local-only switch (ENTIRE_BRAIN_NO_EGRESS / ENTIRE_BRAIN_LOCAL_ONLY)
// always wins. Every gate is checked BEFORE any resolution or HTTP request, so a
// refusal is guaranteed egress-free and the default behaviour stays strictly local.

// factsProposalsOptions bundles the flag state shared by every `facts proposals` verb.
type factsProposalsOptions struct {
	branch  string
	repoID  string
	apiURL  string
	token   string
	jsonOut bool
}

func newFactsProposalsCommand(opts Options) *cobra.Command {
	var pOpts factsProposalsOptions

	cmd := &cobra.Command{
		Use:   "proposals",
		Short: "Review the hosted cross-member conflict queue (opt-in)",
		Long: `proposals lists and settles the OPEN cross-member review proposals on the
hosted fact-set: contradictions two members' facts raised, kept both-sides by the
keep-both merge and routed to a human rather than silently overwritten.

Accepting a proposal applies its merge/supersede (the losing fact is retained as
superseded, never deleted); rejecting it keeps both facts active and clears the
conflict cross-link. Either way the resolved fact set and the shrunken open set are
pushed under compare-and-swap, so every member converges.

This surface is HOSTED and opt-in: nothing leaves your machine unless you both run
one of these commands AND set ` + envBrainAllowHosted + `=1. Use ` + "`facts review`" + ` for this
member's own local, low-confidence proposals — that path is fully offline.

The target repo, API base URL, and bearer token resolve from --repo-id /` + " " + envRepoID + `,
--api-url / ` + envAPIBaseURL + `, and --token / ` + envAPIToken + `.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}

	cmd.PersistentFlags().StringVar(&pOpts.branch, "branch", "", "Branch whose open proposals to review (default: current branch)")
	cmd.PersistentFlags().StringVar(&pOpts.repoID, "repo-id", "", "Target repo id (overrides "+envRepoID+")")
	cmd.PersistentFlags().StringVar(&pOpts.apiURL, "api-url", "", "Entire API base URL (overrides "+envAPIBaseURL+")")
	cmd.PersistentFlags().StringVar(&pOpts.token, "token", "", "Entire API bearer token (overrides "+envAPIToken+")")
	cmd.PersistentFlags().BoolVar(&pOpts.jsonOut, "json", false, "Emit the result as JSON")

	cmd.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "List the open cross-member proposals for a branch",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				return runFactsProposalsList(cmd, opts, pOpts)
			},
		},
		&cobra.Command{
			Use:   "show <proposal>",
			Short: "Show one open proposal (by id, unambiguous id prefix, or candidate fact id)",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return runFactsProposalsShow(cmd, opts, pOpts, args[0])
			},
		},
		&cobra.Command{
			Use:   "apply <proposal>",
			Short: "Accept a proposal: apply its merge/supersede and converge the hosted head",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return runFactsProposalsResolve(cmd, opts, pOpts, args[0], factsync.Accept)
			},
		},
		&cobra.Command{
			Use:   "reject <proposal>",
			Short: "Reject a proposal: keep both facts active and clear the conflict link",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return runFactsProposalsResolve(cmd, opts, pOpts, args[0], factsync.Reject)
			},
		},
	)
	var repairApply bool
	var repairRef string
	repairCmd := &cobra.Command{Use: "repair", Short: "Preview invalid proposal IDs; optionally remove them from the reviewed queue version", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if repairApply && repairRef == "" {
				return fmt.Errorf("--apply requires --if-ref from the repair preview")
			}
			if !repairApply && repairRef != "" {
				return fmt.Errorf("--if-ref requires --apply")
			}
			target, err := resolveHostedProposalTarget(cmd.Context(), opts, pOpts)
			if err != nil {
				return err
			}
			result, err := target.client.RepairProposalIDs(cmd.Context(), target.repoID, target.branch, repairApply, repairRef)
			if err != nil {
				return err
			}
			if pOpts.jsonOut {
				return writeJSON(cmd, result)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "queue ref: %q; invalid proposals: %d; removed: %d\n", result.Ref, len(result.Invalid), result.Removed)
			for _, p := range result.Invalid {
				fmt.Fprintf(cmd.OutOrStdout(), "  invalid id: %q (derived: %q)\n", p.ID, factsync.ProposalID(p.Proposal))
			}
			if !repairApply && len(result.Invalid) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "Review these entries, then use repair --apply --if-ref %q to remove them. Facts are unchanged.\n", result.Ref)
			}
			return nil
		}}
	repairCmd.Flags().BoolVar(&repairApply, "apply", false, "Remove invalid proposals from the reviewed queue version")
	repairCmd.Flags().StringVar(&repairRef, "if-ref", "", "Queue ref reported by the repair preview")
	cmd.AddCommand(repairCmd)
	return cmd
}

// hostedProposalTarget is the resolved hosted target for one `facts proposals` run.
type hostedProposalTarget struct {
	client  *hostedbrain.Client
	repoID  string
	branch  string
	baseURL string
}

// resolveHostedProposalTarget enforces the two-gate opt-in and resolves the hosted
// target. It performs NO network call, so every refusal below is egress-free.
func resolveHostedProposalTarget(ctx context.Context, opts Options, pOpts factsProposalsOptions) (hostedProposalTarget, error) {
	// The master local-only switch always wins over the hosted opt-in.
	if brainNoEgressMode() {
		return hostedProposalTarget{}, fmt.Errorf("no_egress: hosted proposal review is disabled while ENTIRE_BRAIN_NO_EGRESS or ENTIRE_BRAIN_LOCAL_ONLY is set; unset it, or use 'facts review' for this member's local proposals")
	}
	// Gate 2: the explicit hosted opt-in must be truthy; anything else fails closed.
	if !envBool(envBrainAllowHosted) {
		return hostedProposalTarget{}, fmt.Errorf("hosted_proposals_disabled: the hosted cross-member proposal queue is opt-in and off by default; nothing was sent. Set %s=1 to enable it, or use 'facts review' for this member's local proposals", envBrainAllowHosted)
	}

	repoID := publishFlagOrEnv(pOpts.repoID, envRepoID)
	if repoID == "" {
		return hostedProposalTarget{}, fmt.Errorf("facts proposals: target repo id is required; set --repo-id or %s", envRepoID)
	}
	baseURL := publishFlagOrEnv(pOpts.apiURL, envAPIBaseURL)
	if baseURL == "" {
		return hostedProposalTarget{}, fmt.Errorf("facts proposals: API base URL is required; set --api-url or %s", envAPIBaseURL)
	}
	token := publishFlagOrEnv(pOpts.token, envAPIToken)
	if token == "" {
		return hostedProposalTarget{}, fmt.Errorf("facts proposals: API token is required; set --token or %s", envAPIToken)
	}

	return hostedProposalTarget{
		client:  &hostedbrain.Client{BaseURL: baseURL, Token: token},
		repoID:  repoID,
		branch:  resolveProposalsBranch(ctx, opts, pOpts.branch),
		baseURL: strings.TrimRight(baseURL, "/"),
	}, nil
}

// resolveProposalsBranch folds --branch, the repo's current branch, and the default
// into the branch whose open set is reviewed. The repo is only consulted for its
// branch name — the review target itself is the hosted repo id, so this works from a
// checkout without a built brain.
func resolveProposalsBranch(ctx context.Context, opts Options, flagVal string) string {
	if b := strings.TrimSpace(flagVal); b != "" {
		return b
	}
	repoDir := opts.Env.RepoRoot
	if repoDir == "" {
		repoDir = "."
	}
	if current, err := gitScalar(ctx, opts.Runner, repoDir, "branch", "--show-current"); err == nil {
		if b := strings.TrimSpace(current); b != "" {
			return b
		}
	}
	return distillDefaultBranch
}

func runFactsProposalsList(cmd *cobra.Command, opts Options, pOpts factsProposalsOptions) error {
	ctx := cmd.Context()
	target, err := resolveHostedProposalTarget(ctx, opts, pOpts)
	if err != nil {
		return err
	}
	proposals, err := target.client.ListProposals(ctx, target.repoID, target.branch)
	if err != nil {
		return err
	}
	if pOpts.jsonOut {
		if proposals == nil {
			proposals = []factsync.OpenProposal{}
		}
		return writeJSON(cmd, map[string]any{
			"branch":    target.branch,
			"repo_id":   target.repoID,
			"host":      target.baseURL,
			"open":      len(proposals),
			"proposals": proposals,
		})
	}
	out := cmd.OutOrStdout()
	if len(proposals) == 0 {
		fmt.Fprintf(out, "no open cross-member proposals on %s\n", target.branch)
		return nil
	}
	fmt.Fprintf(out, "%d open cross-member proposal(s) on %s:\n", len(proposals), target.branch)
	for _, p := range proposals {
		fmt.Fprintf(out, "  %s  %s\n", p.ID, describeProposal(p))
	}
	fmt.Fprintf(out, "resolve with: facts proposals apply|reject <proposal>\n")
	return nil
}

func runFactsProposalsShow(cmd *cobra.Command, opts Options, pOpts factsProposalsOptions, ref string) error {
	ctx := cmd.Context()
	target, err := resolveHostedProposalTarget(ctx, opts, pOpts)
	if err != nil {
		return err
	}
	// Resolve the caller's reference (id, prefix, or candidate fact id) against the
	// open set, then fetch the canonical record by its exact id.
	open, err := target.client.ListProposals(ctx, target.repoID, target.branch)
	if err != nil {
		return err
	}
	found, err := factsync.FindProposal(open, ref)
	if err != nil {
		return err
	}
	proposal, err := target.client.GetProposal(ctx, target.repoID, target.branch, found.ID)
	if err != nil {
		return err
	}
	if pOpts.jsonOut {
		return writeJSON(cmd, map[string]any{
			"branch":   target.branch,
			"repo_id":  target.repoID,
			"host":     target.baseURL,
			"proposal": proposal,
		})
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "%s\n", proposal.ID)
	fmt.Fprintf(out, "  branch:     %s\n", target.branch)
	fmt.Fprintf(out, "  action:     %s\n", proposal.Proposal.Action)
	fmt.Fprintf(out, "  candidate:  %s\n", proposal.Proposal.CandidateID)
	fmt.Fprintf(out, "  target:     %s\n", proposal.Proposal.TargetID)
	fmt.Fprintf(out, "  proposedBy: %s\n", proposalMemberOrUnknown(proposal))
	fmt.Fprintf(out, "  confidence: %.2f\n", proposal.Proposal.Confidence)
	return nil
}

func runFactsProposalsResolve(cmd *cobra.Command, opts Options, pOpts factsProposalsOptions, ref string, decision factsync.Decision) error {
	ctx := cmd.Context()
	target, err := resolveHostedProposalTarget(ctx, opts, pOpts)
	if err != nil {
		return err
	}
	now := opts.Now().UTC()
	var res factsync.ResolveOpenResult
	if decision == factsync.Accept {
		res, err = target.client.ApplyProposal(ctx, target.repoID, target.branch, ref, now)
	} else {
		res, err = target.client.RejectProposal(ctx, target.repoID, target.branch, ref, now)
	}
	if err != nil {
		return err
	}
	// The hosted set is now the settled truth; drop this member's local
	// review-queue copy of the same conflict so `facts review`, `facts status`,
	// and the pending-review guard stop listing a proposal a member just
	// settled — and so a stale local apply cannot later override this
	// resolution. Best-effort: the hosted resolution has already succeeded.
	if pruneErr := pruneLocalProposalCopy(ctx, opts, target.branch, decision, res.Proposal); pruneErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: resolved on the hosted queue, but the local review-queue copy was not pruned: %v\n", pruneErr)
	}
	if pOpts.jsonOut {
		return writeJSON(cmd, map[string]any{
			"branch":   target.branch,
			"repo_id":  target.repoID,
			"host":     target.baseURL,
			"resolved": res,
		})
	}
	out := cmd.OutOrStdout()
	verb := "applied"
	detail := "candidate merged/superseded target; the losing fact is retained"
	if decision == factsync.Reject {
		verb = "rejected"
		detail = "both facts stay active; conflict link cleared"
	}
	fmt.Fprintf(out, "%s %s on %s (%s)\n", verb, res.Proposal.ID, target.branch, detail)
	fmt.Fprintf(out, "  facts head: %s (%d attempt(s))\n", refOrNone(res.FactsRef), res.Attempts)
	fmt.Fprintf(out, "  %d open proposal(s) remain\n", res.Remaining)
	return nil
}

// pruneLocalProposalCopy removes the local review-queue copy of a proposal that
// was just settled on the hosted queue, AND mirrors the settlement into this
// member's local facts. Outside a local repository there is no local queue to
// prune, which is success, not an error.
//
// Mirroring here is what makes the member's own decision visible immediately.
// `facts sync` reconciles settlements eventually, but without this the member who
// just ran `facts proposals apply` would keep seeing the pre-settlement state in
// `facts list`/recall until their next sync — their local view contradicting the
// decision they had just made.
func pruneLocalProposalCopy(ctx context.Context, opts Options, branch string, decision factsync.Decision, settled factsync.OpenProposal) error {
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, agentSurfaceTarget(opts, nil))
	if err != nil || !local {
		return err
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}
	// The settled id deliberately STAYS in the shared-proposal ledger: the
	// ledger records "this conflict has reached the hosted set", and keeping it
	// suppresses a republish should an identical entry ever re-derive locally.
	return withBrainWriteLock(storage.BrainDir, func() error {
		queue, err := loadFactProposals(storage.BrainDir, branch)
		if err != nil {
			return err
		}
		// Mirror first: a crash between the two writes then leaves the queue entry
		// in place, and the next sync redoes the (idempotent) mirror rather than
		// losing it.
		facts, factsErr := loadFacts(storage.BrainDir, branch)
		if factsErr != nil {
			return factsErr
		}
		now := opts.Now().UTC()
		if updated, changed := applySettlementLocally(facts, settled.Proposal, decision == factsync.Accept, now); changed {
			if err := writeFacts(storage.BrainDir, branch, updated); err != nil {
				return err
			}
			if err := updateFactSourceManifestLocked(storage.BrainDir, now); err != nil {
				return err
			}
		}
		next := make([]factProposal, 0, len(queue))
		for _, p := range queue {
			if factsync.ProposalID(p) == settled.ID {
				continue
			}
			next = append(next, p)
		}
		if len(next) == len(queue) {
			return nil
		}
		return writeFactProposals(storage.BrainDir, branch, next)
	})
}

// describeProposal renders one open proposal as a single review line.
func describeProposal(p factsync.OpenProposal) string {
	return fmt.Sprintf("%s %s -> %s (by %s, confidence %.2f)",
		p.Proposal.Action, p.Proposal.CandidateID, p.Proposal.TargetID, proposalMemberOrUnknown(p), p.Proposal.Confidence)
}

// proposalMemberOrUnknown renders the routing member, tolerating a proposal raised by
// an older sync that predates ProposedBy.
func proposalMemberOrUnknown(p factsync.OpenProposal) string {
	if m := strings.TrimSpace(p.Proposal.ProposedBy); m != "" {
		return m
	}
	return "(unknown member)"
}
