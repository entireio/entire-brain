package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const defaultFactRetention = 30 * 24 * time.Hour

func newFactsCommand(opts Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "facts",
		Short: "Review, promote, and garbage-collect durable facts",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newFactsTreeCommand(opts))
	cmd.AddCommand(newFactsStatusCommand(opts))
	cmd.AddCommand(newFactsSyncCommand(opts))
	cmd.AddCommand(newFactsVitalityCommand(opts))
	cmd.AddCommand(newFactsEvalCommand(opts))
	cmd.AddCommand(newFactsEvalGenCommand(opts))
	cmd.AddCommand(newFactsEvalCompareCommand(opts))
	cmd.AddCommand(newFactsReviewCommand(opts))
	cmd.AddCommand(newFactsPromoteCommand(opts))
	cmd.AddCommand(newFactsRetractCommand(opts))
	cmd.AddCommand(newFactsGCCommand(opts))
	cmd.AddCommand(newFactsReclassifyCommand(opts))
	cmd.AddCommand(newFactsOutlineCommand(opts))
	cmd.AddCommand(newFactsMapCommand(opts))
	return cmd
}

func newFactsRetractCommand(opts Options) *cobra.Command {
	var (
		branch  string
		jsonOut bool
	)
	cmd := &cobra.Command{
		Use:   "retract <fact-id>",
		Short: "Mark a fact as retracted (no longer true); pruned later by gc",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			factID := args[0]
			_, brainDir, resolvedBranch, err := resolveFactsTarget(cmd.Context(), opts, agentSurfaceTarget(opts, nil), branch)
			if err != nil {
				return err
			}
			now := opts.Now().UTC()
			changed := false
			if err := withBrainWriteLock(brainDir, func() error {
				facts, err := loadFacts(brainDir, resolvedBranch)
				if err != nil {
					return err
				}
				found, didChange := retractFact(facts, factID, now)
				if !found {
					return fmt.Errorf("no fact %s on %s", factID, resolvedBranch)
				}
				changed = didChange
				if !changed {
					return nil
				}
				if err := writeFacts(brainDir, resolvedBranch, facts); err != nil {
					return err
				}
				return updateFactSourceManifestLocked(brainDir, now)
			}); err != nil {
				return err
			}
			if jsonOut {
				return writeJSON(cmd, map[string]any{"id": factID, "branch": resolvedBranch, "status": factStatusRetracted, "changed": changed})
			}
			if changed {
				fmt.Fprintf(cmd.OutOrStdout(), "retracted %s on %s (run `facts gc --force` to prune)\n", factID, resolvedBranch)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "%s on %s was already retracted\n", factID, resolvedBranch)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "", "Branch the fact belongs to (default: current branch)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit the result as JSON")
	return cmd
}

// --- review ---------------------------------------------------------------

func newFactsReviewCommand(opts Options) *cobra.Command {
	var (
		branch    string
		jsonOut   bool
		apply     string
		reject    string
		applyAll  bool
		rejectAll bool
	)
	cmd := &cobra.Command{
		Use:   "review",
		Short: "Resolve queued low-confidence merge/supersede proposals",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, brainDir, resolvedBranch, err := resolveFactsTarget(cmd.Context(), opts, agentSurfaceTarget(opts, nil), branch)
			if err != nil {
				return err
			}
			return runFactsReview(cmd, opts, brainDir, resolvedBranch, factsReviewActions{apply: apply, reject: reject, applyAll: applyAll, rejectAll: rejectAll, jsonOut: jsonOut})
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "", "Branch to review (default: current branch)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "List pending proposals as JSON")
	cmd.Flags().StringVar(&apply, "apply", "", "Apply the proposal for this candidate fact id")
	cmd.Flags().StringVar(&reject, "reject", "", "Reject the proposal for this candidate fact id")
	cmd.Flags().BoolVar(&applyAll, "apply-all", false, "Apply every pending proposal")
	cmd.Flags().BoolVar(&rejectAll, "reject-all", false, "Reject every pending proposal")
	return cmd
}

type factsReviewActions struct {
	apply, reject       string
	applyAll, rejectAll bool
	jsonOut             bool
}

func runFactsReview(cmd *cobra.Command, opts Options, brainDir, branch string, act factsReviewActions) error {
	now := opts.Now().UTC()

	// No resolution requested: list pending proposals.
	if act.apply == "" && act.reject == "" && !act.applyAll && !act.rejectAll {
		proposals, err := loadFactProposals(brainDir, branch)
		if err != nil {
			return err
		}
		facts, err := loadFacts(brainDir, branch)
		if err != nil {
			return err
		}
		if act.jsonOut {
			return writeJSON(cmd, map[string]any{"branch": branch, "proposals": proposals})
		}
		if len(proposals) == 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "no pending proposals on %s\n", branch)
			return nil
		}
		for _, p := range proposals {
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s -> %s (confidence %.2f)\n", p.Action, p.CandidateID, p.TargetID, p.Confidence)
			printProposalContext(cmd, facts, p)
		}
		return nil
	}

	resolved, remaining := 0, 0
	if err := withBrainWriteLock(brainDir, func() error {
		proposals, err := loadFactProposals(brainDir, branch)
		if err != nil {
			return err
		}
		facts, err := loadFacts(brainDir, branch)
		if err != nil {
			return err
		}
		keep := proposals[:0:0]
		for _, p := range proposals {
			applyThis := act.applyAll || p.CandidateID == act.apply
			rejectThis := act.rejectAll || p.CandidateID == act.reject
			switch {
			case applyThis:
				updated, applyErr := applyProposal(facts, p, now)
				if applyErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "skip stale proposal %s: %v\n", p.CandidateID, applyErr)
					resolved++ // drop the stale proposal
					continue
				}
				facts = updated
				resolved++
			case rejectThis:
				facts = rejectProposal(facts, p)
				resolved++
			default:
				keep = append(keep, p)
			}
		}
		if resolved == 0 {
			return fmt.Errorf("no proposal matched")
		}
		if err := writeFacts(brainDir, branch, facts); err != nil {
			return err
		}
		if err := writeFactProposals(brainDir, branch, keep); err != nil {
			return err
		}
		remaining = len(keep)
		return updateFactSourceManifestLocked(brainDir, now)
	}); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "resolved %d proposal(s) on %s; %d remaining\n", resolved, branch, remaining)
	return nil
}

func printProposalContext(cmd *cobra.Command, facts []factRecord, p factProposal) {
	if i := indexOfFact(facts, p.CandidateID); i >= 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "  candidate: %s\n", truncateString(facts[i].Text, 100))
	}
	if i := indexOfFact(facts, p.TargetID); i >= 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "  target:    %s\n", truncateString(facts[i].Text, 100))
	}
}

// --- promote ---------------------------------------------------------------

func newFactsPromoteCommand(opts Options) *cobra.Command {
	var (
		from     string
		into     string
		strategy string
		jsonOut  bool
	)
	cmd := &cobra.Command{
		Use:   "promote",
		Short: "Carry a branch's active facts into another branch",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(from) == "" {
				return fmt.Errorf("--from <branch> is required")
			}
			switch strategy {
			case "keep-both", "prefer-source", "prefer-target":
			default:
				return fmt.Errorf("--strategy must be keep-both, prefer-source, or prefer-target")
			}
			repoDir, brainDir, _, err := resolveFactsTarget(cmd.Context(), opts, agentSurfaceTarget(opts, nil), "")
			if err != nil {
				return err
			}
			target := into
			if strings.TrimSpace(target) == "" {
				if current, gitErr := gitScalar(cmd.Context(), opts.Runner, repoDir, "branch", "--show-current"); gitErr == nil {
					target = strings.TrimSpace(current)
				}
			}
			if strings.TrimSpace(target) == "" {
				return fmt.Errorf("--into <branch> is required (could not detect current branch)")
			}
			if target == from {
				return fmt.Errorf("--from and --into must differ")
			}
			return runFactsPromote(cmd, opts, brainDir, from, target, strategy, jsonOut)
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "Source branch to promote facts from (required)")
	cmd.Flags().StringVar(&into, "into", "", "Target branch (default: current branch)")
	cmd.Flags().StringVar(&strategy, "strategy", "keep-both", "Conflict strategy: keep-both, prefer-source, or prefer-target")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit the result summary as JSON")
	return cmd
}

func runFactsPromote(cmd *cobra.Command, opts Options, brainDir, from, into, strategy string, jsonOut bool) error {
	now := opts.Now().UTC()
	var promoted, proposalCount int
	if err := withBrainWriteLock(brainDir, func() error {
		source, err := loadFacts(brainDir, from)
		if err != nil {
			return err
		}
		target, err := loadFacts(brainDir, into)
		if err != nil {
			return err
		}
		merged, proposals, promotedCount := promoteFacts(source, target, strategy, into, now)
		promoted = promotedCount
		proposalCount = len(proposals)
		if err := writeFacts(brainDir, into, merged); err != nil {
			return err
		}
		if len(proposals) > 0 {
			existing, _ := loadFactProposals(brainDir, into)
			if err := writeFactProposals(brainDir, into, dedupeProposals(append(existing, proposals...))); err != nil {
				return err
			}
		}
		return updateFactSourceManifestLocked(brainDir, now)
	}); err != nil {
		return err
	}
	summary := map[string]any{"from": from, "into": into, "strategy": strategy, "promoted": promoted, "proposals": proposalCount}
	if jsonOut {
		return writeJSON(cmd, summary)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "promoted %d fact(s) from %s into %s (%s); %d conflict(s) queued for review\n", promoted, from, into, strategy, proposalCount)
	return nil
}

// --- gc ---------------------------------------------------------------------

func newFactsGCCommand(opts Options) *cobra.Command {
	var (
		branch  string
		force   bool
		retain  time.Duration
		jsonOut bool
	)
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Prune retracted and old superseded facts; report orphans",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, brainDir, resolvedBranch, err := resolveFactsTarget(cmd.Context(), opts, agentSurfaceTarget(opts, nil), branch)
			if err != nil {
				return err
			}
			return runFactsGC(cmd, opts, brainDir, resolvedBranch, force, retain, jsonOut)
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "", "Branch to gc (default: current branch)")
	cmd.Flags().BoolVar(&force, "force", false, "Actually prune (default: dry-run reporting what would be pruned)")
	cmd.Flags().DurationVar(&retain, "retain", defaultFactRetention, "Retain superseded facts updated within this window")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit the gc summary as JSON")
	return cmd
}

func newFactsReclassifyCommand(opts Options) *cobra.Command {
	var (
		branch  string
		force   bool
		jsonOut bool
	)
	cmd := &cobra.Command{
		Use:   "reclassify",
		Short: "Backfill the deterministic KIND onto facts missing one (no agent)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, brainDir, resolvedBranch, err := resolveFactsTarget(cmd.Context(), opts, agentSurfaceTarget(opts, nil), branch)
			if err != nil {
				return err
			}
			return runFactsReclassify(cmd, opts, brainDir, resolvedBranch, force, jsonOut)
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "", "Branch to reclassify (default: current branch)")
	cmd.Flags().BoolVar(&force, "force", false, "Recompute every fact's kind (default: only fill facts missing a valid kind)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit the reclassify summary as JSON")
	return cmd
}

func runFactsReclassify(cmd *cobra.Command, opts Options, brainDir, branch string, force, jsonOut bool) error {
	facts, err := loadFacts(brainDir, branch)
	if err != nil {
		return err
	}
	changed := reclassifyFacts(facts, force)
	now := opts.Now().UTC()
	if changed > 0 {
		if err := writeFacts(brainDir, branch, facts); err != nil {
			return err
		}
	}
	// Always refresh the manifest, even on a no-op reclassify: a store predating
	// the by_kind field has valid kinds but a stale/absent histogram that only a
	// manifest rebuild repairs.
	if err := updateFactSourceManifest(brainDir, now); err != nil {
		return err
	}
	byKind := map[string]int{}
	for _, f := range facts {
		if f.Status == factStatusActive {
			byKind[factKindOrInferred(f)]++
		}
	}
	if jsonOut {
		return writeJSON(cmd, map[string]any{"branch": branch, "changed": changed, "facts": len(facts), "by_kind": byKind})
	}
	fmt.Fprintf(cmd.OutOrStdout(), "reclassified %d of %d fact(s) on %s\n", changed, len(facts), branch)
	for _, kind := range []string{factKindDecision, factKindInvariant, factKindGotcha, factKindPreference, factKindConvention} {
		if byKind[kind] > 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "  %-11s %d\n", kind, byKind[kind])
		}
	}
	return nil
}

func newFactsOutlineCommand(opts Options) *cobra.Command {
	var (
		gen     outlineGenOptions
		branch  string
		jsonOut bool
	)
	gen.agent = "auto"
	gen.minFacts = 2
	cmd := &cobra.Command{
		Use:   "outline",
		Short: "Generate the synthesized hierarchical fact outline (agent rollup summaries)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			repoDir, brainDir, resolvedBranch, err := resolveFactsTarget(cmd.Context(), opts, agentSurfaceTarget(opts, nil), branch)
			if err != nil {
				return err
			}
			facts, err := loadFacts(brainDir, resolvedBranch)
			if err != nil {
				return err
			}
			resolved := gen
			if resolved.agent == "auto" {
				resolved.agent = defaultRefreshAgent(cmd.Context(), opts.Runner, repoDir)
			}
			now := opts.Now().UTC()
			outline, regenerated, err := generateFactOutline(cmd.Context(), repoDir, brainDir, resolvedBranch, facts, resolved, now)
			if err != nil {
				return err
			}
			if err := writeFactOutline(brainDir, resolvedBranch, outline); err != nil {
				return err
			}
			if jsonOut {
				return writeJSON(cmd, map[string]any{"branch": resolvedBranch, "nodes": len(outline.Nodes), "regenerated": regenerated})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "outline for %s: %d node(s), %d summary(ies) regenerated\n", resolvedBranch, len(outline.Nodes), regenerated)
			return nil
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "", "Branch to outline (default: current branch)")
	cmd.Flags().StringVar(&gen.agent, "agent", "auto", "Agent for summaries: auto, codex, claude-code, command, or none")
	cmd.Flags().StringArrayVar(&gen.agentCommand, "agent-command", nil, "Agent command argv for --agent command")
	cmd.Flags().StringVar(&gen.model, "model", "", "Override the agent model (pairs with --effort for a cheap run)")
	cmd.Flags().StringVar(&gen.effort, "effort", "", "Override the reasoning effort for codex/claude-code")
	cmd.Flags().IntVar(&gen.budget, "budget", 0, "Max agent summary calls this run (0 = unlimited)")
	cmd.Flags().IntVar(&gen.minFacts, "min-facts", 2, "Skip nodes whose subtree has fewer facts than this")
	cmd.Flags().BoolVar(&gen.force, "force", false, "Re-summarize every node, ignoring unchanged fingerprints")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit the outline summary as JSON")
	return cmd
}

func newFactsMapCommand(opts Options) *cobra.Command {
	var (
		branch  string
		path    string
		depth   int
		jsonOut bool
	)
	cmd := &cobra.Command{
		Use:   "map",
		Short: "Render the synthesized fact outline (summaries + structure) with progressive disclosure",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, brainDir, resolvedBranch, err := resolveFactsTarget(cmd.Context(), opts, agentSurfaceTarget(opts, nil), branch)
			if err != nil {
				return err
			}
			// Always rebuild the structure from the live facts (it is deterministic
			// and never stale), then overlay stored summaries only where the node's
			// fingerprint still matches — so a summary for a changed subtree is
			// hidden rather than shown stale, and no `facts outline` run is required
			// to see the map.
			facts, err := loadFacts(brainDir, resolvedBranch)
			if err != nil {
				return err
			}
			outline := buildFactOutlineTree(resolvedBranch, facts, opts.Now().UTC())
			if stored, storedErr := loadFactOutline(brainDir, resolvedBranch); storedErr == nil {
				for key, n := range outline.Nodes {
					if s, ok := stored.Nodes[key]; ok && s.Fingerprint == n.Fingerprint && s.Summary != "" {
						n.Summary = s.Summary
						outline.Nodes[key] = n
					}
				}
			}
			node := path
			if _, ok := outline.Nodes[node]; !ok {
				return fmt.Errorf("no outline node %q on %s", path, resolvedBranch)
			}
			if jsonOut {
				return writeJSON(cmd, outline)
			}
			var b strings.Builder
			renderFactOutline(&b, outline, node, depth)
			if b.Len() == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "no facts on %s\n", resolvedBranch)
				return nil
			}
			fmt.Fprint(cmd.OutOrStdout(), b.String())
			return nil
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "", "Branch to map (default: current branch)")
	cmd.Flags().StringVar(&path, "path", "", "Drill into a node key (a directory tier, e.g. internal/cli)")
	cmd.Flags().IntVar(&depth, "depth", 0, "Levels to render below the node (0 = all)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit the outline as JSON")
	return cmd
}

func runFactsGC(cmd *cobra.Command, opts Options, brainDir, branch string, force bool, retain time.Duration, jsonOut bool) error {
	facts, err := loadFacts(brainDir, branch)
	if err != nil {
		return err
	}
	now := opts.Now().UTC()
	taxonomy, err := loadFactTaxonomy(brainDir, now)
	if err != nil {
		return err
	}
	result := gcFacts(facts, taxonomy, now, retain)

	summary := map[string]any{
		"branch":    branch,
		"pruned":    len(result.Pruned),
		"orphans":   len(result.Orphans),
		"remaining": len(result.Kept),
		"applied":   force,
	}
	if force {
		if err := withBrainWriteLock(brainDir, func() error {
			facts, err := loadFacts(brainDir, branch)
			if err != nil {
				return err
			}
			taxonomy, err := loadFactTaxonomy(brainDir, now)
			if err != nil {
				return err
			}
			result = gcFacts(facts, taxonomy, now, retain)
			summary["pruned"] = len(result.Pruned)
			summary["orphans"] = len(result.Orphans)
			summary["remaining"] = len(result.Kept)
			if err := writeFacts(brainDir, branch, result.Kept); err != nil {
				return err
			}
			return updateFactSourceManifestLocked(brainDir, now)
		}); err != nil {
			return err
		}
	}
	if jsonOut {
		return writeJSON(cmd, summary)
	}
	verb := "would prune"
	if force {
		verb = "pruned"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s %d fact(s) on %s (%d retracted/old-superseded); %d taxonomy-orphaned fact(s) reported (kept); %d remaining\n",
		verb, len(result.Pruned), branch, len(result.Pruned), len(result.Orphans), len(result.Kept))
	if !force && len(result.Pruned) > 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "re-run with --force to apply")
	}
	return nil
}
