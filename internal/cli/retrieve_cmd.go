package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// retrieve_cmd.go wires the qmd-inspired verbs over the unified text index:
// search (lexical), vsearch (vector), query (hybrid), and get/multi-get (fetch by
// id). These verbs subsumed the old per-source inspect kinds (facts/docs/history
// text). What remains under `inspect` is only what the verbs can't do: symbol-graph
// traversal (code/context/impact/changes/tests/boundaries) and regression analysis.

func newSearchCommand(opts Options) *cobra.Command {
	return newRetrieveCommand(opts, "search", modeLexical, "Lexical keyword search across facts, history, and docs (BM25 for history and docs; token-overlap for facts)")
}

func newVsearchCommand(opts Options) *cobra.Command {
	return newRetrieveCommand(opts, "vsearch", modeVector, "Vector (semantic) search across facts and docs (and history when a Gemma-class embedder is configured)")
}

func newQueryCommand(opts Options) *cobra.Command {
	return newRetrieveCommand(opts, "query", modeHybrid, "Hybrid (lexical+vector, RRF) search across the brain")
}

func newRetrieveCommand(opts Options, use string, mode retrievalMode, short string) *cobra.Command {
	var jsonOut bool
	var format string
	var limit int
	var branch string
	var patterns bool
	cmd := &cobra.Command{
		Use:   use + " <query>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			wantJSON, err := outputWantsJSON(jsonOut, format)
			if err != nil {
				return err
			}
			return runRetrieve(cmd.Context(), cmd, opts, args[0], mode, limit, branch, wantJSON, patterns)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	cmd.Flags().IntVar(&limit, "limit", 10, "Maximum results")
	cmd.Flags().IntVarP(&limit, "number", "n", 10, "Maximum results (QMD-style alias for --limit)")
	cmd.Flags().StringVar(&format, "format", "", "Output format: json or cli (QMD-style alias for --json)")
	cmd.Flags().StringVar(&branch, "branch", "", "Branch for facts (default: current)")
	cmd.Flags().BoolVar(&patterns, "patterns", false, "Also surface relevant pattern:/theme: pointers (does not change facts/history/docs ranking)")
	return cmd
}

func runRetrieve(ctx context.Context, cmd *cobra.Command, opts Options, query string, mode retrievalMode, limit int, branch string, jsonOut, patterns bool) error {
	// Reject --limit <= 0 rather than silently defaulting, so a typo like
	// `--limit 0` is an explicit error (matching the rest of the CLI surface). The
	// MCP path passes a validated positive limit, so it's unaffected.
	if limit <= 0 {
		return fmt.Errorf("--limit must be greater than 0")
	}
	_, brainDir, resolvedBranch, err := resolveFactsTarget(ctx, opts, agentSurfaceTarget(opts, nil), branch)
	if err != nil {
		return err
	}
	results, err := retrieveUnified(brainDir, resolvedBranch, query, limit, mode)
	if err != nil {
		return err
	}
	// Discoverability only: pattern/theme pointers never enter the facts/history/
	// docs ranking — they are a separate, capped, opt-in section so default
	// retrieval quality is unchanged by construction.
	var related []relatedPatternRef
	if patterns {
		related = relatedPatternPointers(brainDir, query, patternPointerCap)
	}
	if jsonOut {
		out := map[string]any{"query": query, "branch": resolvedBranch, "results": results}
		if len(related) > 0 {
			out["related_patterns"] = related
		}
		if len(results) == 0 {
			if note := emptyResultBlindSpot(brainDir); note != "" {
				out["blind_spot"] = note
			}
		}
		return writeJSON(cmd, out)
	}
	if len(results) == 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "no results for %q\n", query)
		if note := emptyResultBlindSpot(brainDir); note != "" {
			fmt.Fprintln(cmd.OutOrStdout(), note)
		}
		// still show related pattern pointers if any
	}
	for _, r := range results {
		ex := truncateString(strings.Join(strings.Fields(r.Text), " "), 200)
		loc := r.Path
		if r.Line > 0 {
			loc = fmt.Sprintf("%s:%d", r.Path, r.Line)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "[%s] %s  %s\n    %s\n", r.Source, r.ID, loc, ex)
	}
	for _, p := range related {
		fmt.Fprintf(cmd.OutOrStdout(), "related [%s] %s  %s\n", p.Type, p.ID, p.Title)
	}
	return nil
}

func newGetCommand(opts Options) *cobra.Command {
	var jsonOut bool
	var format string
	var branch string
	cmd := &cobra.Command{
		Use:   "get <id>",
		Short: "Fetch one item in full by id (fact:… | history:… | doc:… | pattern:…)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			wantJSON, err := outputWantsJSON(jsonOut, format)
			if err != nil {
				return err
			}
			return runGet(cmd.Context(), cmd, opts, []string{args[0]}, branch, wantJSON)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	cmd.Flags().StringVar(&format, "format", "", "Output format: json or cli (QMD-style alias for --json)")
	cmd.Flags().StringVar(&branch, "branch", "", "Branch for facts (default: current)")
	return cmd
}

func newMultiGetCommand(opts Options) *cobra.Command {
	var jsonOut bool
	var format string
	var branch string
	cmd := &cobra.Command{
		Use:   "multi-get <id>...",
		Short: "Fetch multiple items by id",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			wantJSON, err := outputWantsJSON(jsonOut, format)
			if err != nil {
				return err
			}
			return runGet(cmd.Context(), cmd, opts, args, branch, wantJSON)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	cmd.Flags().StringVar(&format, "format", "", "Output format: json or cli (QMD-style alias for --json)")
	cmd.Flags().StringVar(&branch, "branch", "", "Branch for facts (default: current)")
	return cmd
}

func outputWantsJSON(jsonOut bool, format string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "":
		return jsonOut, nil
	case "json":
		return true, nil
	case "cli":
		return false, nil
	default:
		return false, fmt.Errorf("--format must be json or cli")
	}
}

func runGet(ctx context.Context, cmd *cobra.Command, opts Options, ids []string, branch string, jsonOut bool) error {
	_, brainDir, resolvedBranch, err := resolveFactsTarget(ctx, opts, agentSurfaceTarget(opts, nil), branch)
	if err != nil {
		return err
	}
	found, missing, err := getUnifiedBatch(brainDir, resolvedBranch, ids)
	if err != nil {
		return err
	}
	// Normalize empty collections to [] so --json emits arrays, not null, matching
	// the repo's JSON contract (see TestInspectCodeEmptyResultsEmitArrayNotNull).
	if found == nil {
		found = []unifiedResult{}
	}
	if missing == nil {
		missing = []string{}
	}
	if jsonOut {
		return writeJSON(cmd, map[string]any{"branch": resolvedBranch, "results": found, "missing": missing})
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
