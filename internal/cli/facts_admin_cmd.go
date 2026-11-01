package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/cmdutil"

	"github.com/entireio/entire-brain/internal/cli/command/webhook"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	"github.com/spf13/cobra"
)

const defaultFactRetention = 30 * 24 * time.Hour

func newFactsCommand(opts Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "memories",
		Aliases: []string{"facts"},
		Short:   "Inspect, retract, and garbage-collect durable memories",
		Args:    cobra.NoArgs,
	}
	cmd.AddCommand(newFactsTreeCommand(opts))
	cmd.AddCommand(newFactsFilesCommand(opts))
	cmd.AddCommand(newFactsImportCommand(opts))
	cmd.AddCommand(newFactsStatusCommand(opts))
	cmd.AddCommand(newFactsStorageCommand(opts))
	cmd.AddCommand(newFactsSyncCommand(opts))
	cmd.AddCommand(newFactsVitalityCommand(opts))
	cmd.AddCommand(newFactsProvenanceCommand(opts))
	cmd.AddCommand(newFactsEvalCommand(opts))
	cmd.AddCommand(newFactsEvalGenCommand(opts))
	cmd.AddCommand(newFactsEvalCompareCommand(opts))
	cmd.AddCommand(newFactsRetractCommand(opts))
	cmd.AddCommand(newFactsGlobalCommand(opts))
	cmd.AddCommand(newFactsGCCommand(opts))
	cmd.AddCommand(newFactsReclassifyCommand(opts))
	cmd.AddCommand(newFactsOutlineCommand(opts))
	cmd.AddCommand(newFactsMapCommand(opts))
	return cmd
}

func newFactsRetractCommand(opts Options) *cobra.Command {
	var (
		jsonOut bool
		global  bool
	)
	cmd := &cobra.Command{
		Use:   "retract <memory-id>",
		Short: "Mark a memory as retracted (no longer true); pruned later by gc",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			factID := args[0]
			var (
				brainDir string
				err      error
			)
			// A fact that can be written and not retracted is a fact you are
			// stuck with. Global facts surface in every repository, so being
			// stuck with a wrong one is worse, not better.
			if global {
				brainDir, err = factstore.ResolveGlobalTarget(opts.Env)
			} else {
				_, brainDir, _, err = resolveFactsTarget(cmd.Context(), opts, agentSurfaceTarget(opts, nil), "")
			}
			if err != nil {
				return err
			}
			now := opts.Now().UTC()
			changed := false
			// Seeded with the id so the event is still identifiable if the
			// record cannot be re-read after the mutation.
			retracted := factRecord{ID: factID}
			if err := brainstore.WithBrainWriteLock(brainDir, func() error {
				if _, err := migrateFactStoreLocked(brainDir); err != nil {
					return err
				}
				facts, err := factstore.Load(brainDir)
				if err != nil {
					return err
				}
				found, didChange := retractFact(facts, factID, now)
				if !found {
					if global {
						return fmt.Errorf("no global memory %s", factID)
					}
					return fmt.Errorf("no memory %s", factID)
				}
				changed = didChange
				if !changed {
					return nil
				}
				// Captured under the lock so the webhook reports the kind and
				// paths of the fact that was actually retracted, rather than an
				// id a subscriber would have to look up through an API that only
				// listens on loopback.
				if i := indexOfFact(facts, factID); i >= 0 {
					retracted = facts[i]
				}
				if err := factstore.Write(brainDir, facts); err != nil {
					return err
				}
				return updateFactSourceManifestLocked(brainDir, now)
			}); err != nil {
				return err
			}
			// Only on a real transition: re-retracting an already-retracted fact
			// changed nothing, and a subscriber that fired on it would act on
			// news that is not news.
			if changed {
				if global {
					webhook.NotifyGlobalFactWebhook(cmd.Context(), cmd.ErrOrStderr(), webhook.WebhookFactRetracted, retracted, now)
				} else {
					webhook.NotifyFactWebhook(cmd.Context(), cmd.ErrOrStderr(), opts, webhook.WebhookFactRetracted, retracted.Branch, retracted, now)
				}
			}
			if jsonOut {
				return cmdutil.WriteJSON(cmd, map[string]any{"id": factID, "status": factStatusRetracted, "changed": changed})
			}
			where := "in this repository"
			if global {
				where = "globally"
			}
			if changed {
				fmt.Fprintf(cmd.OutOrStdout(), "retracted %s %s (run `memories gc --force` to prune)\n", factID, where)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "%s %s was already retracted\n", factID, where)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit the result as JSON")
	cmd.Flags().BoolVar(&global, "global", false, "Retract a global memory rather than one of this repository's")
	return cmd
}

// --- gc ---------------------------------------------------------------------

func newFactsGCCommand(opts Options) *cobra.Command {
	var (
		branch  string
		force   bool
		retain  time.Duration
		jsonOut bool
		global  bool
	)
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Prune retracted and old superseded memories; report orphans",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			var (
				brainDir       string
				resolvedBranch string
				err            error
			)
			// Without this the global store is write-only in one direction:
			// facts can be retracted there and never pruned, so it grows
			// without bound and the documented promise that global facts get
			// the same lifecycle as repository ones is not kept.
			if global {
				if strings.TrimSpace(branch) != "" {
					return fmt.Errorf("--global and --branch cannot be combined: a global memory is not on a branch")
				}
				brainDir, err = factstore.ResolveGlobalTarget(opts.Env)
			} else {
				_, brainDir, _, err = resolveFactsTarget(cmd.Context(), opts, agentSurfaceTarget(opts, nil), "")
			}
			if err != nil {
				return err
			}
			resolvedBranch = strings.TrimSpace(branch)
			return runFactsGC(cmd, opts, brainDir, resolvedBranch, force, retain, jsonOut)
		},
	}
	cmd.Flags().BoolVar(&global, "global", false, "Prune the global memory store rather than this repository's")
	cmd.Flags().StringVar(&branch, "branch", "", "Only gc memories stamped with this branch (default: all)")
	cmd.Flags().BoolVar(&force, "force", false, "Actually prune (default: dry-run reporting what would be pruned)")
	cmd.Flags().DurationVar(&retain, "retain", defaultFactRetention, "Retain superseded memories updated within this window")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit the gc summary as JSON")
	return cmd
}

func newFactsReclassifyCommand(opts Options) *cobra.Command {
	var (
		force   bool
		jsonOut bool
	)
	cmd := &cobra.Command{
		Hidden: true,
		Use:    "reclassify",
		Short:  "Backfill the deterministic KIND onto memories missing one (no agent)",
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, brainDir, _, err := resolveFactsTarget(cmd.Context(), opts, agentSurfaceTarget(opts, nil), "")
			if err != nil {
				return err
			}
			return runFactsReclassify(cmd, opts, brainDir, force, jsonOut)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "Recompute every memory's kind (default: only fill memories missing a valid kind)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit the reclassify summary as JSON")
	return cmd
}

func runFactsReclassify(cmd *cobra.Command, opts Options, brainDir string, force, jsonOut bool) error {
	now := opts.Now().UTC()
	var facts []factRecord
	changed := 0
	// The whole load-mutate-write runs under the brain write lock, like every
	// other fact mutator (remember, retract, gc, distill's flush) and like the
	// refresh-time reclassifyAllFacts. Reading the store outside it and
	// writing the mutated copy back is a lost update: a concurrent writer that
	// commits between this read and this write is silently overwritten, and
	// its facts disappear.
	if err := brainstore.WithBrainWriteLock(brainDir, func() error {
		if _, err := migrateFactStoreLocked(brainDir); err != nil {
			return err
		}
		loaded, err := factstore.Load(brainDir)
		if err != nil {
			return err
		}
		facts = loaded
		changed = factstore.Reclassify(facts, force)
		if changed > 0 {
			if err := factstore.Write(brainDir, facts); err != nil {
				return err
			}
		}
		// Always refresh the manifest, even on a no-op reclassify: a store predating
		// the by_kind field has valid kinds but a stale/absent histogram that only a
		// manifest rebuild repairs.
		return updateFactSourceManifestLocked(brainDir, now)
	}); err != nil {
		return err
	}
	byKind := map[string]int{}
	for _, f := range facts {
		if f.Status == factStatusActive {
			byKind[factstore.KindOrInferred(f)]++
		}
	}
	if jsonOut {
		return cmdutil.WriteJSON(cmd, map[string]any{"changed": changed, "facts": len(facts), "by_kind": byKind})
	}
	fmt.Fprintf(cmd.OutOrStdout(), "reclassified %d of %d memory(ies)\n", changed, len(facts))
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
		jsonOut bool
	)
	gen.agent = "auto"
	gen.minFacts = 2
	cmd := &cobra.Command{
		Use:   "outline",
		Short: "Generate the synthesized hierarchical memory outline (agent rollup summaries)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			repoDir, brainDir, _, err := resolveFactsTarget(cmd.Context(), opts, agentSurfaceTarget(opts, nil), "")
			if err != nil {
				return err
			}
			facts, err := factstore.Load(brainDir)
			if err != nil {
				return err
			}
			resolved := gen
			if resolved.agent == "auto" {
				resolved.agent = defaultRefreshAgent(cmd.Context(), opts.Runner, repoDir)
			}
			now := opts.Now().UTC()
			outline, regenerated, err := generateFactOutline(cmd.Context(), repoDir, brainDir, facts, resolved, now)
			if err != nil {
				return err
			}
			if err := writeFactOutline(brainDir, outline); err != nil {
				return err
			}
			if jsonOut {
				return cmdutil.WriteJSON(cmd, map[string]any{"nodes": len(outline.Nodes), "regenerated": regenerated})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "outline: %d node(s), %d summary(ies) regenerated\n", len(outline.Nodes), regenerated)
			return nil
		},
	}
	cmd.Flags().StringVar(&gen.agent, "agent", "auto", "Agent for summaries: auto, codex, claude-code, command, or none")
	cmd.Flags().StringArrayVar(&gen.agentCommand, "agent-command", nil, "Agent command argv for --agent command")
	cmd.Flags().StringVar(&gen.model, "model", "", "Override the agent model (pairs with --effort for a cheap run)")
	cmd.Flags().StringVar(&gen.effort, "effort", "", "Override the reasoning effort for codex/claude-code")
	cmd.Flags().IntVar(&gen.budget, "budget", 0, "Max agent summary calls this run (0 = unlimited)")
	cmd.Flags().IntVar(&gen.minFacts, "min-memories", 2, "Skip nodes whose subtree has fewer memories than this")
	cmd.Flags().BoolVar(&gen.force, "force", false, "Re-summarize every node, ignoring unchanged fingerprints")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit the outline summary as JSON")
	// --min-facts predates the facts→memories rename; keep it working.
	legacyFlagAlias(cmd.Flags(), "min-facts", "min-memories")
	return cmd
}

func newFactsMapCommand(opts Options) *cobra.Command {
	var (
		path    string
		depth   int
		jsonOut bool
	)
	cmd := &cobra.Command{
		Use:   "map",
		Short: "Render the synthesized memory outline (summaries + structure) with progressive disclosure",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, brainDir, _, err := resolveFactsTarget(cmd.Context(), opts, agentSurfaceTarget(opts, nil), "")
			if err != nil {
				return err
			}
			// Always rebuild the structure from the live facts (it is deterministic
			// and never stale), then overlay stored summaries only where the node's
			// fingerprint still matches — so a summary for a changed subtree is
			// hidden rather than shown stale, and no `facts outline` run is required
			// to see the map.
			facts, err := factstore.Load(brainDir)
			if err != nil {
				return err
			}
			outline := buildFactOutlineTree(facts, opts.Now().UTC())
			if stored, storedErr := loadFactOutline(brainDir); storedErr == nil {
				for key, n := range outline.Nodes {
					if s, ok := stored.Nodes[key]; ok && s.Fingerprint == n.Fingerprint && s.Summary != "" {
						n.Summary = s.Summary
						outline.Nodes[key] = n
					}
				}
			}
			node := path
			if _, ok := outline.Nodes[node]; !ok {
				return fmt.Errorf("no outline node %q", path)
			}
			// The outline counts what factstore.Load produced. When the manifest
			// declares more than that, "root (4 facts)" is a count of survivors
			// rendered as if it were the corpus. Qualify it before rendering,
			// and exit nonzero so a caller cannot read the map as complete.
			integrityErr := reportFactStoreIntegrity(cmd, brainDir, jsonOut)
			if jsonOut {
				if err := cmdutil.WriteJSON(cmd, outline); err != nil {
					return err
				}
				return integrityErr
			}
			var b strings.Builder
			renderFactOutline(&b, outline, node, depth)
			if b.Len() == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no memories")
				return integrityErr
			}
			fmt.Fprint(cmd.OutOrStdout(), b.String())
			return integrityErr
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "Drill into a node key (a directory tier, e.g. internal/cli)")
	cmd.Flags().IntVar(&depth, "depth", 0, "Levels to render below the node (0 = all)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit the outline as JSON")
	return cmd
}

// runFactsGC prunes over the flat store. branch is an explicit metadata
// filter: when set, only records stamped with that branch are gc candidates
// and everything else passes through untouched; empty means the whole store.
func runFactsGC(cmd *cobra.Command, opts Options, brainDir, branch string, force bool, retain time.Duration, jsonOut bool) error {
	now := opts.Now().UTC()
	gcPass := func(facts []factRecord) ([]factRecord, gcResult, error) {
		taxonomy, err := factstore.LoadTaxonomy(brainDir, now)
		if err != nil {
			return nil, gcResult{}, err
		}
		scoped := facts
		var passthrough []factRecord
		if branch != "" {
			scoped = scoped[:0:0]
			for _, f := range facts {
				if f.Branch == branch {
					scoped = append(scoped, f)
				} else {
					passthrough = append(passthrough, f)
				}
			}
		}
		result := gcFacts(scoped, taxonomy, now, retain)
		return append(passthrough, result.Kept...), result, nil
	}
	facts, err := factstore.Load(brainDir)
	if err != nil {
		return err
	}
	_, result, err := gcPass(facts)
	if err != nil {
		return err
	}
	summary := map[string]any{
		"branch":    branch,
		"pruned":    len(result.Pruned),
		"orphans":   len(result.Orphans),
		"remaining": len(result.Kept),
		"applied":   force,
	}
	if force {
		if err := brainstore.WithBrainWriteLock(brainDir, func() error {
			if _, err := migrateFactStoreLocked(brainDir); err != nil {
				return err
			}
			facts, err := factstore.Load(brainDir)
			if err != nil {
				return err
			}
			kept, res, err := gcPass(facts)
			if err != nil {
				return err
			}
			result = res
			summary["pruned"] = len(result.Pruned)
			summary["orphans"] = len(result.Orphans)
			summary["remaining"] = len(result.Kept)
			if err := factstore.Write(brainDir, kept); err != nil {
				return err
			}
			return updateFactSourceManifestLocked(brainDir, now)
		}); err != nil {
			return err
		}
	}
	if jsonOut {
		return cmdutil.WriteJSON(cmd, summary)
	}
	verb := "would prune"
	if force {
		verb = "pruned"
	}
	scope := "in this repository"
	if branch != "" {
		scope = "stamped " + branch
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s %d memory(ies) %s (%d retracted/old-superseded); %d taxonomy-orphaned memory(ies) reported (kept); %d remaining\n",
		verb, len(result.Pruned), scope, len(result.Pruned), len(result.Orphans), len(result.Kept))
	if !force && len(result.Pruned) > 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "re-run with --force to apply")
	}
	return nil
}
