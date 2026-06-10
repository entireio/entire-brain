package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// resolveFactsTarget resolves the repo, brain directory, and the branch facts
// are scoped to (the live git branch unless overridden). It is shared by the
// fact read/write commands.
func resolveFactsTarget(ctx context.Context, opts Options, target, branchOverride string) (repoDir, brainDir, branch string, err error) {
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		return "", "", "", err
	}
	if !local {
		return "", "", "", fmt.Errorf("facts require a local repository path: %s", target)
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return "", "", "", err
	}
	branch = strings.TrimSpace(branchOverride)
	if branch == "" {
		if current, gitErr := gitScalar(ctx, opts.Runner, repoDir, "branch", "--show-current"); gitErr == nil {
			branch = strings.TrimSpace(current)
		}
	}
	if branch == "" {
		branch = distillDefaultBranch
	}
	return repoDir, storage.BrainDir, branch, nil
}

func newRecallCommand(opts Options) *cobra.Command {
	var (
		branch       string
		limit        int
		includeAll   bool
		scope        string
		kind         string
		locus        string
		noSemantic   bool
		expand       bool
		agent        string
		agentCommand []string
		jsonOut      bool
	)
	cmd := &cobra.Command{
		Use:   "recall <query>",
		Short: "Retrieve durable facts matching a query for the current branch",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			query := ""
			if len(args) == 1 {
				query = args[0]
			}
			if err := validateScopeFlag(scope); err != nil {
				return err
			}
			if err := validateKindFlag(kind); err != nil {
				return err
			}
			repoDir, brainDir, resolvedBranch, err := resolveFactsTarget(cmd.Context(), opts, agentSurfaceTarget(opts, nil), branch)
			if err != nil {
				return err
			}
			allFacts, err := loadFacts(brainDir, resolvedBranch)
			if err != nil {
				return err
			}
			facts := filterFactsByLocus(filterFactsByKind(filterFactsByScope(allFacts, scope), kind), locus)
			effectiveQuery := query
			if expand && strings.TrimSpace(query) != "" {
				resolved := agent
				if resolved == "auto" {
					resolved = defaultRefreshAgent(cmd.Context(), opts.Runner, repoDir)
				}
				expandArgs, expErr := distillAgentCommandArgs(resolved, agentCommand, queryExpansionPrompt())
				if expErr != nil {
					return fmt.Errorf("expand agent: %w", expErr)
				}
				exp, expErr := expandQuery(cmd.Context(), execDistillAgent, expandArgs, repoDir, query, loadExpansionCache(""))
				if expErr != nil {
					return fmt.Errorf("expand query: %w", expErr)
				}
				effectiveQuery = expandedQuery(query, exp)
			}
			// Semantic rerank is on by default (the measured Phase D win); it
			// degrades silently to lexical when the embedder can't load, so a
			// missing/corrupt model never breaks recall. The disk-backed cache
			// avoids re-embedding the branch on every invocation.
			var rr *semanticReranker
			if !noSemantic {
				if e := defaultEmbedder(); e != nil {
					rr = newSemanticRerankerForBranch(e, brainDir, resolvedBranch)
				}
			}
			matches := rankFactsFused(facts, effectiveQuery, limit, includeAll, rr)
			if rr != nil {
				rr.retain(allFacts) // keep every present fact's vector; prune only departed facts
				_ = rr.flush()      // best-effort cache persist
			}
			// Locus drift (Phase 2 item 4): flag surfaced facts whose code
			// locus left the worktree, so the agent knows which to re-verify.
			drift := factsLocusDrift(repoDir, matches)
			if jsonOut {
				out := map[string]any{"branch": resolvedBranch, "query": query, "facts": matches}
				if len(drift) > 0 {
					out["locus_drift"] = drift
				}
				if len(matches) == 0 {
					if note := emptyResultBlindSpot(brainDir); note != "" {
						out["blind_spot"] = note
					}
				}
				return writeJSON(cmd, out)
			}
			if len(matches) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "no facts for %q on %s\n", query, resolvedBranch)
				if note := emptyResultBlindSpot(brainDir); note != "" {
					fmt.Fprintln(cmd.OutOrStdout(), note)
				}
				return nil
			}
			for _, f := range matches {
				printFactLine(cmd, f)
				if gone := drift[f.ID]; len(gone) > 0 {
					fmt.Fprintf(cmd.OutOrStdout(), "  ⚠ stale locus (no longer in worktree): %s\n", strings.Join(gone, ", "))
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "", "Branch to recall from (default: current branch)")
	cmd.Flags().IntVar(&limit, "k", 10, "Maximum facts to return")
	cmd.Flags().BoolVar(&includeAll, "all", false, "Include superseded and retracted facts")
	cmd.Flags().StringVar(&scope, "scope", "", "Restrict to 'local' (code/subsystem) or 'cross-cutting' (preferences/workflow) facts")
	cmd.Flags().StringVar(&kind, "kind", "", "Restrict to one kind: decision|invariant|gotcha|preference|convention")
	cmd.Flags().StringVar(&locus, "locus", "", "Restrict to facts about a code locus (a path or symbol, e.g. internal/cli/facts.go or factRecord)")
	cmd.Flags().BoolVar(&noSemantic, "no-semantic", false, "Disable embedding rerank; rank with lexical + taxonomy only")
	cmd.Flags().BoolVar(&expand, "expand", false, "Expand the query with agent-generated retrieval terms before ranking")
	cmd.Flags().StringVar(&agent, "agent", "auto", "Agent for --expand: auto, codex, claude-code, or command")
	cmd.Flags().StringArrayVar(&agentCommand, "agent-command", nil, "Agent command argv for --agent command")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	return cmd
}

// validateScopeFlag rejects an unrecognized --scope value.
func validateScopeFlag(scope string) error {
	switch scope {
	case "", factScopeLocal, factScopeCrossCutting:
		return nil
	default:
		return fmt.Errorf("--scope must be %q or %q", factScopeLocal, factScopeCrossCutting)
	}
}

// validateKindFlag rejects an unrecognized --kind value (empty = no filter).
func validateKindFlag(kind string) error {
	if strings.TrimSpace(kind) == "" || validFactKind(kind) {
		return nil
	}
	return fmt.Errorf("--kind must be one of decision|invariant|gotcha|preference|convention")
}

func newInspectBlameCommand(opts Options) *cobra.Command {
	var (
		branch  string
		jsonOut bool
	)
	cmd := &cobra.Command{
		Use:   "blame <fact-id>",
		Short: "Show the source anchors a fact was derived from",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			factID := args[0]
			_, brainDir, resolvedBranch, err := resolveFactsTarget(cmd.Context(), opts, agentSurfaceTarget(opts, nil), branch)
			if err != nil {
				return err
			}
			facts, err := loadFacts(brainDir, resolvedBranch)
			if err != nil {
				return err
			}
			i := indexOfFact(facts, factID)
			if i < 0 {
				return fmt.Errorf("no fact %s on %s", factID, resolvedBranch)
			}
			fact := facts[i]
			if jsonOut {
				return writeJSON(cmd, fact)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s [%s] %s\n", fact.ID, factKindOrInferred(fact), strings.Join(fact.Paths, ","), fact.Text)
			fmt.Fprintf(cmd.OutOrStdout(), "  origin=%s status=%s\n", fact.Origin, fact.Status)
			for _, a := range fact.Provenance {
				verified := "unsigned"
				if a.Verified {
					verified = "verified"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "  - session=%s checkpoint=%s commit=%s %s:%d (%s)\n",
					valueOrUnset(a.SessionID), valueOrUnset(a.CheckpointID), valueOrUnset(a.Commit), valueOrUnset(a.Transcript), a.Line, verified)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "", "Branch the fact belongs to (default: current branch)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	return cmd
}

// printFactLine renders a fact for human-readable listings.
func printFactLine(cmd *cobra.Command, f factRecord) {
	marker := ""
	switch f.Status {
	case factStatusSuperseded:
		marker = " (superseded)"
	case factStatusRetracted:
		marker = " (retracted)"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s %s [%s]%s\n  %s\n", f.ID, factKindOrInferred(f), strings.Join(f.Paths, ","), marker, f.Text)
}
