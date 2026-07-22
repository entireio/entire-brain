package cli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"
)

const (
	retrievalDefaultLimit = 5
	retrievalExcerptBytes = 600
)

type compactUnifiedResult struct {
	Source               string            `json:"source"`
	ID                   string            `json:"id"`
	Path                 string            `json:"path,omitempty"`
	Heading              string            `json:"heading,omitempty"`
	Line                 int               `json:"line,omitempty"`
	Excerpt              string            `json:"excerpt"`
	Score                float64           `json:"score,omitempty"`
	VerificationRequired bool              `json:"verification_required,omitempty"`
	Caveats              []retrievalCaveat `json:"caveats,omitempty"`
	RelatedIDs           []string          `json:"related_ids,omitempty"`
}

type retrievalTaskHints struct {
	LikelyEditFiles []string
	LikelyTestFiles []string
	ActionChecklist []brainBriefAction
}

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
			return runRetrieve(cmd.Context(), cmd, opts, args[0], mode, limit, branch, wantJSON, patterns, use)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	cmd.Flags().IntVar(&limit, "limit", retrievalDefaultLimit, "Maximum results")
	cmd.Flags().IntVarP(&limit, "number", "n", retrievalDefaultLimit, "Maximum results (QMD-style alias for --limit)")
	cmd.Flags().StringVar(&format, "format", "", "Output format: json or cli (QMD-style alias for --json)")
	cmd.Flags().StringVar(&branch, "branch", "", "Branch for facts (default: current)")
	cmd.Flags().BoolVar(&patterns, "patterns", false, "Also surface relevant pattern:/theme: pointers (does not change facts/history/docs ranking)")
	return cmd
}

// surface names the read surface for serve receipts ("search"/"vsearch"/
// "query" from the CLI, "mcp:brain_*" from the MCP server).
func runRetrieve(ctx context.Context, cmd *cobra.Command, opts Options, query string, mode retrievalMode, limit int, branch string, jsonOut, patterns bool, surface string) error {
	// Reject --limit <= 0 rather than silently defaulting, so a typo like
	// `--limit 0` is an explicit error (matching the rest of the CLI surface). The
	// MCP path passes a validated positive limit, so it's unaffected.
	if limit <= 0 {
		return fmt.Errorf("--limit must be greater than 0")
	}
	repoDir, brainDir, resolvedBranch, err := resolveFactsTarget(ctx, opts, agentSurfaceTarget(opts, nil), branch)
	if err != nil {
		return err
	}
	results, err := retrieveUnified(repoDir, brainDir, resolvedBranch, query, limit, mode)
	if err != nil {
		return err
	}
	factIDs := unifiedFactIDs(results)
	recordReceipt := func() {
		recordServedFacts(cmd.ErrOrStderr(), vitalityNow(opts), brainDir, resolvedBranch, surface,
			vitalityHead(ctx, opts.Runner, repoDir), query, factIDs)
	}
	// Discoverability only: pattern/theme pointers never enter the facts/history/
	// docs ranking — they are a separate, capped, opt-in section so default
	// retrieval quality is unchanged by construction.
	var related []relatedPatternRef
	if patterns {
		related = relatedPatternPointers(brainDir, query, patternPointerCap)
	}
	if jsonOut {
		out := map[string]any{"query": query, "branch": resolvedBranch, "results": compactUnifiedResults(results, query)}
		hints := retrievalHintsForResults(repoDir, results, query)
		if len(hints.LikelyEditFiles) > 0 {
			out["likely_edit_files"] = hints.LikelyEditFiles
		}
		if len(hints.LikelyTestFiles) > 0 {
			out["likely_test_files"] = hints.LikelyTestFiles
		}
		if len(hints.ActionChecklist) > 0 {
			out["action_checklist"] = hints.ActionChecklist
		}
		if len(related) > 0 {
			out["related_patterns"] = related
		}
		if len(results) == 0 {
			if note := emptyResultBlindSpot(brainDir); note != "" {
				out["blind_spot"] = note
			}
		}
		if err := writeJSON(cmd, out); err != nil {
			return err
		}
		if len(factIDs) > 0 {
			recordReceipt()
		}
		return nil
	}
	if err := writeText(cmd, func(out io.Writer) {
		if len(results) == 0 {
			fmt.Fprintf(out, "no results for %q\n", query)
			if note := emptyResultBlindSpot(brainDir); note != "" {
				fmt.Fprintln(out, note)
			}
			// still show related pattern pointers if any
		}
		hints := retrievalHintsForResults(repoDir, results, query)
		for _, file := range hints.LikelyEditFiles {
			fmt.Fprintf(out, "edit_file %s\n", file)
		}
		for _, file := range hints.LikelyTestFiles {
			fmt.Fprintf(out, "test_file %s\n", file)
		}
		for _, action := range hints.ActionChecklist {
			fmt.Fprintf(out, "action %s %s: %s\n", action.File, action.Symbol, action.Action)
			if action.Validation != nil {
				fmt.Fprintf(out, "  validate: %s (complete_on_pass=%t)\n", action.Validation.Command, action.Validation.CompleteOnPass)
			}
		}
		for _, r := range results {
			ex := truncateString(strings.Join(strings.Fields(r.Text), " "), 200)
			loc := r.Path
			if r.Line > 0 {
				loc = fmt.Sprintf("%s:%d", r.Path, r.Line)
			}
			label := r.Source
			if r.VerificationRequired {
				label += " verify"
			}
			fmt.Fprintf(out, "[%s] %s  %s\n    %s\n", label, r.ID, loc, ex)
			printRetrievalCaveats(out, r)
		}
		for _, p := range related {
			fmt.Fprintf(out, "related [%s] %s  %s\n", p.Type, p.ID, p.Title)
		}
	}); err != nil {
		return err
	}
	if len(factIDs) > 0 {
		recordReceipt()
	}
	return nil
}

func retrievalHintsForResults(repoRoot string, results []unifiedResult, query string) retrievalTaskHints {
	report := brainBriefReport{}
	for _, result := range results {
		if result.Source != "history" && result.Source != "doc" {
			continue
		}
		report.History.Matches = append(report.History.Matches, brainTextMatch{Excerpt: result.Text})
	}
	editFiles, testFiles, _ := brainBriefLikelyFileGroups(repoRoot, report, query)
	seen := make(map[string]struct{}, len(editFiles))
	for _, file := range editFiles {
		seen[file] = struct{}{}
	}
	var siblingImplementations []string
	for _, testFile := range testFiles {
		for _, candidate := range retrievalSiblingImplementationCandidates(testFile) {
			if _, exists := seen[candidate]; exists || !brainBriefRepoFileExists(repoRoot, candidate) {
				continue
			}
			seen[candidate] = struct{}{}
			siblingImplementations = append(siblingImplementations, candidate)
		}
	}
	editFiles = append(siblingImplementations, editFiles...)
	if len(editFiles) > 3 {
		editFiles = editFiles[:3]
	}
	if len(testFiles) > 3 {
		testFiles = testFiles[:3]
	}
	report.LikelyEditFiles = editFiles
	report.LikelyTestFiles = testFiles
	actions := brainBriefActionChecklist(repoRoot, report, query)
	if len(actions) > 0 {
		editFiles = brainBriefActionFiles(actions)
		if actionTestFiles := brainBriefActionTestFiles(actions); len(actionTestFiles) > 0 {
			testFiles = actionTestFiles
		}
	}
	return retrievalTaskHints{LikelyEditFiles: editFiles, LikelyTestFiles: testFiles, ActionChecklist: actions}
}

func retrievalSiblingImplementationCandidates(testFile string) []string {
	ext := filepath.Ext(testFile)
	switch {
	case strings.HasSuffix(testFile, "_test.go"):
		return []string{strings.TrimSuffix(testFile, "_test.go") + ".go"}
	case strings.HasSuffix(testFile, "_test.py"):
		return []string{strings.TrimSuffix(testFile, "_test.py") + ".py"}
	case strings.Contains(testFile, ".test"+ext):
		return []string{strings.Replace(testFile, ".test"+ext, ext, 1)}
	case strings.Contains(testFile, ".spec"+ext):
		return []string{strings.Replace(testFile, ".spec"+ext, ext, 1)}
	default:
		return nil
	}
}

func compactUnifiedResults(results []unifiedResult, query string) []compactUnifiedResult {
	if results == nil {
		return []compactUnifiedResult{}
	}
	out := make([]compactUnifiedResult, len(results))
	for i, result := range results {
		out[i] = compactUnifiedResult{
			Source: result.Source, ID: result.ID, Path: result.Path, Heading: result.Heading, Line: result.Line,
			Excerpt: retrievalResultExcerpt(result.Text, query, retrievalExcerptBytes), Score: result.Score,
			VerificationRequired: result.VerificationRequired, Caveats: result.Caveats, RelatedIDs: result.RelatedIDs,
		}
	}
	return out
}

// retrievalResultExcerpt centers a compact result on the densest query-term
// window. Ranked retrieval is a locator surface; get/multi-get own full bodies.
func retrievalResultExcerpt(value, query string, maxBytes int) string {
	text := strings.Join(strings.Fields(value), " ")
	if maxBytes <= 0 || text == "" {
		return ""
	}
	if len(text) <= maxBytes {
		return text
	}
	lower := strings.ToLower(text)
	terms := semanticQueryTokens(query)
	bestStart, bestScore := 0, -1
	for _, anchor := range terms {
		for searchAt := 0; searchAt < len(lower); {
			rel := strings.Index(lower[searchAt:], anchor)
			if rel < 0 {
				break
			}
			at := searchAt + rel
			start := max(0, at-maxBytes/3)
			end := min(len(lower), start+maxBytes)
			window := lower[start:end]
			score := 0
			for _, term := range terms {
				if strings.Contains(window, term) {
					score += len(term)
				}
			}
			if score > bestScore {
				bestStart, bestScore = start, score
			}
			searchAt = at + len(anchor)
		}
	}
	start := bestStart
	end := min(len(text), start+maxBytes)
	for start > 0 && !utf8.RuneStart(text[start]) {
		start--
	}
	for end < len(text) && !utf8.RuneStart(text[end]) {
		end++
	}
	excerpt := strings.TrimSpace(text[start:end])
	if start > 0 {
		excerpt = "..." + excerpt
	}
	if end < len(text) {
		excerpt += "..."
	}
	return excerpt
}

func newGetCommand(opts Options) *cobra.Command {
	var jsonOut bool
	var format string
	var branch string
	cmd := &cobra.Command{
		Use:   "get <id>",
		Short: "Fetch one item in full by id (fact:… | review:… | history:… | doc:… | pattern:… | theme:…)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			wantJSON, err := outputWantsJSON(jsonOut, format)
			if err != nil {
				return err
			}
			return runGet(cmd.Context(), cmd, opts, []string{args[0]}, branch, wantJSON, "get")
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
			return runGet(cmd.Context(), cmd, opts, args, branch, wantJSON, "multi-get")
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

func runGet(ctx context.Context, cmd *cobra.Command, opts Options, ids []string, branch string, jsonOut bool, surface string) error {
	repoDir, brainDir, resolvedBranch, err := resolveFactsTarget(ctx, opts, agentSurfaceTarget(opts, nil), branch)
	if err != nil {
		return err
	}
	found, missing, err := getUnifiedBatch(repoDir, brainDir, resolvedBranch, ids)
	if err != nil {
		return err
	}
	factIDs := unifiedFactIDs(found)
	recordReceipt := func() {
		recordServedFacts(cmd.ErrOrStderr(), vitalityNow(opts), brainDir, resolvedBranch, surface,
			vitalityHead(ctx, opts.Runner, repoDir), "", factIDs)
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
		if err := writeJSON(cmd, map[string]any{"branch": resolvedBranch, "results": found, "missing": missing}); err != nil {
			return err
		}
		if len(factIDs) > 0 {
			recordReceipt()
		}
		return nil
	}
	if err := writeText(cmd, func(out io.Writer) {
		for _, r := range found {
			loc := r.Path
			if r.Line > 0 {
				loc = fmt.Sprintf("%s:%d", r.Path, r.Line)
			}
			label := r.Source
			if r.VerificationRequired {
				label += " verify"
			}
			fmt.Fprintf(out, "[%s] %s  %s\n%s\n", label, r.ID, loc, r.Text)
			printRetrievalCaveats(out, r)
			fmt.Fprintln(out)
		}
		for _, id := range missing {
			fmt.Fprintf(out, "not found: %s\n", id)
		}
	}); err != nil {
		return err
	}
	if len(factIDs) > 0 {
		recordReceipt()
	}
	return nil
}

func printRetrievalCaveats(out io.Writer, result unifiedResult) {
	for _, caveat := range result.Caveats {
		fmt.Fprintf(out, "    verify: %s\n", caveat.Message)
		details := make([]string, 0, 4)
		if len(caveat.Paths) > 0 {
			details = append(details, "paths="+strings.Join(caveat.Paths, ","))
		}
		if caveat.ReviewID != "" {
			details = append(details, "review="+caveat.ReviewID)
		}
		if caveat.Action != "" {
			details = append(details, "action="+caveat.Action)
			details = append(details, fmt.Sprintf("confidence=%.2f", caveat.Confidence))
		}
		if len(details) > 0 {
			fmt.Fprintf(out, "      details: %s\n", strings.Join(details, "  "))
		}
	}
}
