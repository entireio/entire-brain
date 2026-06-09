package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// retrieve_cmd.go wires the qmd-aligned verbs over the unified text index:
// search (lexical), vsearch (vector), query (hybrid), and get/multi-get (fetch by
// id). Symbol/code navigation stays under `inspect code/context/impact`; the
// specialist `inspect <kind>` commands remain as the pre-distillation fallback.

func newSearchCommand(opts Options) *cobra.Command {
	return newRetrieveCommand(opts, "search", modeLexical, "Lexical (BM25) search across facts, history, and docs")
}

func newVsearchCommand(opts Options) *cobra.Command {
	return newRetrieveCommand(opts, "vsearch", modeVector, "Vector (semantic) search across facts and docs")
}

func newQueryCommand(opts Options) *cobra.Command {
	return newRetrieveCommand(opts, "query", modeHybrid, "Hybrid (lexical+vector, RRF) search across the brain")
}

func newRetrieveCommand(opts Options, use string, mode retrievalMode, short string) *cobra.Command {
	var jsonOut bool
	var limit int
	var branch string
	cmd := &cobra.Command{
		Use:   use + " <query>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRetrieve(cmd.Context(), cmd, opts, args[0], mode, limit, branch, jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	cmd.Flags().IntVar(&limit, "limit", 10, "Maximum results")
	cmd.Flags().StringVar(&branch, "branch", "", "Branch for facts (default: current)")
	return cmd
}

func runRetrieve(ctx context.Context, cmd *cobra.Command, opts Options, query string, mode retrievalMode, limit int, branch string, jsonOut bool) error {
	_, brainDir, resolvedBranch, err := resolveFactsTarget(ctx, opts, agentSurfaceTarget(opts, nil), branch)
	if err != nil {
		return err
	}
	results := retrieveUnified(brainDir, resolvedBranch, query, limit, mode)
	if jsonOut {
		return writeJSON(cmd, map[string]any{"query": query, "branch": resolvedBranch, "results": results})
	}
	if len(results) == 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "no results for %q\n", query)
		return nil
	}
	for _, r := range results {
		ex := strings.Join(strings.Fields(r.Text), " ")
		if len(ex) > 200 {
			ex = ex[:200]
		}
		loc := r.Path
		if r.Line > 0 {
			loc = fmt.Sprintf("%s:%d", r.Path, r.Line)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "[%s] %s  %s\n    %s\n", r.Source, r.ID, loc, ex)
	}
	return nil
}

func newGetCommand(opts Options) *cobra.Command {
	var jsonOut bool
	var branch string
	cmd := &cobra.Command{
		Use:   "get <id>",
		Short: "Fetch one item in full by id (fact:… | history:… | doc:…)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGet(cmd.Context(), cmd, opts, []string{args[0]}, branch, jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	cmd.Flags().StringVar(&branch, "branch", "", "Branch for facts (default: current)")
	return cmd
}

func newMultiGetCommand(opts Options) *cobra.Command {
	var jsonOut bool
	var branch string
	cmd := &cobra.Command{
		Use:   "multi-get <id>...",
		Short: "Fetch multiple items by id",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGet(cmd.Context(), cmd, opts, args, branch, jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	cmd.Flags().StringVar(&branch, "branch", "", "Branch for facts (default: current)")
	return cmd
}

func runGet(ctx context.Context, cmd *cobra.Command, opts Options, ids []string, branch string, jsonOut bool) error {
	_, brainDir, resolvedBranch, err := resolveFactsTarget(ctx, opts, agentSurfaceTarget(opts, nil), branch)
	if err != nil {
		return err
	}
	found, missing := getUnifiedBatch(brainDir, resolvedBranch, ids)
	if missing == nil {
		missing = []string{}
	}
	if jsonOut {
		return writeJSON(cmd, map[string]any{"results": found, "missing": missing})
	}
	for _, r := range found {
		loc := r.Path
		if r.Line > 0 {
			loc = fmt.Sprintf("%s:%d", r.Path, r.Line)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "[%s] %s  %s\n%s\n\n", r.Source, r.ID, loc, r.Text)
	}
	for _, id := range missing {
		fmt.Fprintf(cmd.OutOrStdout(), "not found: %s\n", id)
	}
	return nil
}
