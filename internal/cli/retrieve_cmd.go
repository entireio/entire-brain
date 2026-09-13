package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"
)

const retrievalExcerptBytes = 600

type compactUnifiedResult struct {
	Source  string `json:"source"`
	ID      string `json:"id"`
	Path    string `json:"path,omitempty"`
	Heading string `json:"heading,omitempty"`
	Line    int    `json:"line,omitempty"`
	// Text is retained for JSON compatibility. Excerpt is the bounded locator
	// projection newer agents may prefer before calling get/multi-get; it is
	// omitted when it would duplicate Text byte for byte, which is the common
	// case for short records. Duplicating both doubled every result and agents
	// that page output through `head` lost the tail of the ranking: a decisive
	// record at rank 6 was observed cut off by exactly this.
	Text                 string            `json:"text"`
	Excerpt              string            `json:"excerpt,omitempty"`
	Score                float64           `json:"score,omitempty"`
	VerificationRequired bool              `json:"verification_required,omitempty"`
	Caveats              []retrievalCaveat `json:"caveats,omitempty"`
	RelatedIDs           []string          `json:"related_ids,omitempty"`

	// Conversation-exchange provenance (experimental, additive; empty for
	// every other source).
	EndLine      int      `json:"end_line,omitempty"`
	Branch       string   `json:"branch,omitempty"`
	SessionID    string   `json:"session_id,omitempty"`
	Agent        string   `json:"agent,omitempty"`
	CreatedAt    string   `json:"created_at,omitempty"`
	Truncated    bool     `json:"truncated,omitempty"`
	MatchedTerms []string `json:"matched_terms,omitempty"`
	// SessionRef names the virtual session so a caller can fetch the
	// outline or adjacent context.
	SessionRef string `json:"session_ref,omitempty"`
	// Multi-concept session coverage (additive).
	Concepts          []string       `json:"concepts,omitempty"`
	ConceptMatches    []conceptMatch `json:"concept_matches,omitempty"`
	EvidenceIDs       []string       `json:"evidence_ids,omitempty"`
	WorstRank         int            `json:"worst_rank,omitempty"`
	RankSum           int            `json:"rank_sum,omitempty"`
	Approximate       bool           `json:"approximate,omitempty"`
	ResponseTruncated bool           `json:"response_truncated,omitempty"`
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
	return newRetrieveCommand(opts, "vsearch", modeVector, "Vector (semantic) search across facts and docs (history and conversation join with a Gemma-class embedder on the brain_cgo build)")
}

func newQueryCommand(opts Options) *cobra.Command {
	return newRetrieveCommand(opts, "query", modeHybrid, "Hybrid (lexical+vector, RRF) search across the brain")
}

func newRetrieveCommand(opts Options, use string, mode retrievalMode, short string) *cobra.Command {
	var jsonOut bool
	var format string
	var limit int
	var branch string
	var patterns, includeAbstract bool
	var source, after, before, session, agent string
	var concepts []string
	cmd := &cobra.Command{
		Use:   use + " <query>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			wantJSON, err := outputWantsJSON(jsonOut, format)
			if err != nil {
				return err
			}
			ropts, err := buildRetrievalOptions(source, after, before, session, agent, branch, concepts)
			if err != nil {
				return fmt.Errorf("--%s", err.Error())
			}
			ropts.IncludeAbstract = includeAbstract
			return runRetrieve(cmd.Context(), cmd, opts, args[0], mode, limit, branch, ropts, wantJSON, patterns, use)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	cmd.Flags().IntVar(&limit, "limit", 10, "Maximum results")
	cmd.Flags().IntVarP(&limit, "number", "n", 10, "Maximum results (QMD-style alias for --limit)")
	cmd.Flags().StringVar(&format, "format", "", "Output format: json or cli (QMD-style alias for --json)")
	cmd.Flags().StringVar(&branch, "branch", "", "Branch for facts (default: current); with --source conversation also filters exchanges to that captured branch")
	cmd.Flags().BoolVar(&patterns, "patterns", false, "Also surface relevant pattern:/theme: pointers (does not change facts/history/docs ranking)")
	cmd.Flags().StringVar(&source, "source", "", "Restrict retrieval to one source: all, fact, history, conversation, or doc (default all; conversation is experimental opt-in)")
	cmd.Flags().StringVar(&after, "after", "", "Conversation source only: sessions at or after this time (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().StringVar(&before, "before", "", "Conversation source only: sessions before this time (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().StringVar(&session, "session", "", "Conversation source only: exchanges from this session id (also disables the per-session diversity cap)")
	cmd.Flags().StringVar(&agent, "agent", "", "Conversation source only: exchanges captured by this agent/harness (e.g. \"Claude Code\", \"Codex\")")
	cmd.Flags().StringArrayVar(&concepts, "concept", nil, "Conversation source only: additional concept (repeatable, up to 4); sessions must match the query AND every concept")
	cmd.Flags().BoolVar(&includeAbstract, "include-abstract", false, "Conversation source only: include bounded evidence-linked session previews (never affects ranking)")
	return cmd
}

// buildRetrievalOptions validates and assembles the shared retrieval-options
// contract from CLI flags or MCP arguments. branch doubles as the conversation
// branch filter; error text names bare flag words so the CLI can prefix "--".
func buildRetrievalOptions(source, after, before, session, agent, branch string, concepts []string) (retrievalOptions, error) {
	parsedSource, err := parseRetrievalSource(source)
	if err != nil {
		return retrievalOptions{}, err
	}
	if len(concepts) > conversationConceptsMaxTotal-1 {
		return retrievalOptions{}, fmt.Errorf("concept: at most %d additional concepts (got %d)", conversationConceptsMaxTotal-1, len(concepts))
	}
	trimmedConcepts := make([]string, 0, len(concepts))
	for _, concept := range concepts {
		concept = strings.TrimSpace(concept)
		if concept == "" {
			return retrievalOptions{}, fmt.Errorf("concept: concepts must be non-empty")
		}
		trimmedConcepts = append(trimmedConcepts, concept)
	}
	if len(trimmedConcepts) > 0 && parsedSource != retrievalSourceConversation {
		return retrievalOptions{}, fmt.Errorf(`concept: concepts require source "conversation" (got %q)`, parsedSource)
	}
	afterTime, err := parseRetrievalTimeFilter(after)
	if err != nil {
		return retrievalOptions{}, fmt.Errorf("after: %s", err.Error())
	}
	beforeTime, err := parseRetrievalTimeFilter(before)
	if err != nil {
		return retrievalOptions{}, fmt.Errorf("before: %s", err.Error())
	}
	if !afterTime.IsZero() && !beforeTime.IsZero() && !afterTime.Before(beforeTime) {
		return retrievalOptions{}, fmt.Errorf("after (%s) must be earlier than before (%s)", after, before)
	}
	return retrievalOptions{
		Source:    parsedSource,
		After:     afterTime,
		Before:    beforeTime,
		SessionID: strings.TrimSpace(session),
		Agent:     strings.TrimSpace(agent),
		Branch:    strings.TrimSpace(branch),
		Concepts:  trimmedConcepts,
	}, nil
}

// surface names the read surface for serve receipts ("search"/"vsearch"/
// "query" from the CLI, "mcp:brain_*" from the MCP server). ropts carries the
// validated source selector and structured filters; branch (the raw flag)
// still selects the facts branch via resolveFactsTarget.
func runRetrieve(ctx context.Context, cmd *cobra.Command, opts Options, query string, mode retrievalMode, limit int, branch string, ropts retrievalOptions, jsonOut, patterns bool, surface string) error {
	// Reject --limit <= 0 rather than silently defaulting, so a typo like
	// `--limit 0` is an explicit error (matching the rest of the CLI surface). The
	// MCP path passes a validated positive limit, so it's unaffected.
	if limit <= 0 {
		return fmt.Errorf("--limit must be greater than 0")
	}
	if len(ropts.Concepts) > 0 {
		if err := validateRetrievalLimit(limit); err != nil {
			return fmt.Errorf("--%s", err.Error())
		}
	}
	repoDir, brainDir, resolvedBranch, err := resolveFactsTarget(ctx, opts, agentSurfaceTarget(opts, nil), branch)
	if err != nil {
		return err
	}
	privacyPolicy, err := captureRetrievalPrivacyPolicy(brainDir)
	if err != nil {
		return err
	}
	if patterns {
		privacyPolicy.RequireDerivedClean = true
		if err := requirePrivacyDerivedRead(brainDir); err != nil {
			return err
		}
		if err := requirePatternCorpusAvailable(brainDir); err != nil {
			return err
		}
	}
	results, err := retrieveUnifiedWithOptions(repoDir, brainDir, resolvedBranch, query, limit, mode, ropts)
	if err != nil {
		return err
	}
	multiConcept := len(ropts.Concepts) > 0
	policyIdentityBeforeExtras := privacyPolicy.Identity
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
		related, err = relatedPatternPointersChecked(brainDir, query, patternPointerCap)
		if err != nil {
			return err
		}
	}
	var abstractPreviews []sessionAbstractPreview
	var abstractPreviewsTruncated bool
	if ropts.IncludeAbstract {
		if ropts.Source != retrievalSourceConversation {
			return fmt.Errorf(`--include-abstract requires --source conversation (got %q)`, ropts.Source)
		}
		abstractPreviews, abstractPreviewsTruncated, err = abstractPreviewsForResults(brainDir, results)
		if err != nil {
			return err
		}
	}
	if !multiConcept {
		// Preserve the pre-multi-concept payload byte-for-byte, but buffer it so the shared
		// final privacy check runs after assembly and before the first write. The
		// 128 KiB response contract and proof rendering still belong only to
		// multi-concept coverage.
		if jsonOut {
			out := map[string]any{"query": query, "branch": resolvedBranch, "results": compactUnifiedResults(results, query)}
			if ropts.IncludeAbstract {
				out["abstract_previews"] = abstractPreviews
				out["abstract_previews_truncated"] = abstractPreviewsTruncated
			}
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
			serialized, err := jsonOutputBytes(out)
			if err != nil {
				return err
			}
			if err := writeRetrievalResponseBytes(cmd.OutOrStdout(), serialized, privacyPolicy); err != nil {
				return err
			}
			if len(factIDs) > 0 {
				recordReceipt()
			}
			return nil
		}
		var rendered bytes.Buffer
		if err := writeTextToWriter(&rendered, func(out io.Writer) {
			if len(results) == 0 {
				fmt.Fprintf(out, "no results for %q\n", query)
				if note := emptyResultBlindSpot(brainDir); note != "" {
					fmt.Fprintln(out, note)
				}
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
			}
			for _, result := range results {
				ex := truncateString(strings.Join(strings.Fields(result.Text), " "), 200)
				label := result.Source
				if result.VerificationRequired {
					label += " verify"
				}
				fmt.Fprintf(out, "[%s] %s  %s\n    %s\n", label, result.ID, unifiedResultLocation(result), ex)
				printRetrievalCaveats(out, result)
			}
			for _, pattern := range related {
				fmt.Fprintf(out, "related [%s] %s  %s\n", pattern.Type, pattern.ID, pattern.Title)
			}
			for _, preview := range abstractPreviews {
				text := ""
				if preview.Overview != nil {
					text = "  " + strings.Join(strings.Fields(preview.Overview.Text), " ")
				}
				fmt.Fprintf(out, "abstract [%s] %s%s\n", preview.AbstractStatus, preview.SessionRef, text)
				if preview.AbstractIssue != "" {
					fmt.Fprintf(out, "  automatic enqueue issue: %s\n", preview.AbstractIssue)
				}
			}
			if abstractPreviewsTruncated {
				fmt.Fprintln(out, "abstract previews truncated")
			}
		}); err != nil {
			return err
		}
		if err := writeRetrievalResponseBytes(cmd.OutOrStdout(), rendered.Bytes(), privacyPolicy); err != nil {
			return err
		}
		if len(factIDs) > 0 {
			recordReceipt()
		}
		return nil
	}
	// Ranking may be slow enough for a concurrent exclusion to land after its
	// first guard snapshot. Revalidate immediately before transport assembly;
	// no JSON, text, or MCP bytes are emitted before this check succeeds.
	results, policyAfter, err := revalidateRetrievalResponsePrivacy(brainDir, results)
	if err != nil {
		return err
	}
	if policyAfter != policyIdentityBeforeExtras {
		// Optional derived material was read under a superseded privacy policy.
		// Rows were freshly revalidated above; discard the stale extras instead
		// of allowing a completed fast cleanup to make them look current.
		related = nil
		abstractPreviews = nil
		abstractPreviewsTruncated = true
	}
	extras := retrievalTransportExtras{
		AbstractPreviews:          abstractPreviews,
		AbstractPreviewsTruncated: abstractPreviewsTruncated,
		Hints:                     retrievalHintsForResults(repoDir, results, query),
		Related:                   related,
	}
	if len(results) == 0 {
		extras.BlindSpot = emptyResultBlindSpot(brainDir)
	}
	if jsonOut {
		// Preserve the established JSON `text` field on both CLI and MCP
		// surfaces. Excerpt is additive; existing consumers must not be forced to
		// switch to a second brain_get round trip.
		out, err := boundedRetrievalJSONPayload(ctx, query, resolvedBranch, results, extras, surface)
		if err != nil {
			return err
		}
		serialized, err := jsonOutputBytes(out)
		if err != nil {
			return err
		}
		if err := writeRetrievalResponseBytes(cmd.OutOrStdout(), serialized, retrievalPrivacyPolicy{BrainDir: brainDir, Identity: policyAfter}); err != nil {
			return err
		}
		if len(factIDs) > 0 {
			recordReceipt()
		}
		return nil
	}
	text, err := boundedSingleRepoRetrievalText(query, results, extras)
	if err != nil {
		return err
	}
	if err := writeRetrievalResponseBytes(cmd.OutOrStdout(), text, retrievalPrivacyPolicy{BrainDir: brainDir, Identity: policyAfter}); err != nil {
		return err
	}
	if len(factIDs) > 0 {
		recordReceipt()
	}
	return nil
}

func retrievalHintsForResults(repoRoot string, results []unifiedResult, query string) retrievalTaskHints {
	_ = query
	seen := map[string]struct{}{}
	var editFiles, testFiles []string
	addPath := func(path string) {
		clean, ok := cleanBrainBriefHistoryFile(repoRoot, path)
		if !ok || !brainBriefRepoFileExists(repoRoot, clean) {
			return
		}
		if _, ok := seen[clean]; ok {
			return
		}
		seen[clean] = struct{}{}
		if brainBriefLikelyTestFile(clean) {
			testFiles = append(testFiles, clean)
		} else {
			editFiles = append(editFiles, clean)
		}
	}
	for _, result := range results {
		if result.Source != "history" && result.Source != "doc" {
			continue
		}
		addPath(result.Path)
		for _, path := range extractBrainBriefPaths(result.Text) {
			addPath(path)
		}
	}

	// Pair an indexed test locator with its conventional implementation path.
	// This uses only the paths contained in indexed results plus bounded stat
	// calls; ordinary retrieval never scans or reads the live source tree.
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
	return retrievalTaskHints{LikelyEditFiles: editFiles, LikelyTestFiles: testFiles}
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
			Text: result.Text, Excerpt: distinctRetrievalExcerpt(result.Text, query), Score: result.Score,
			VerificationRequired: result.VerificationRequired, Caveats: result.Caveats, RelatedIDs: result.RelatedIDs,
			EndLine: result.EndLine, Branch: result.Branch, SessionID: result.SessionID,
			Agent: result.Agent, CreatedAt: result.CreatedAt, Truncated: result.Truncated,
			MatchedTerms: result.MatchedTerms, SessionRef: result.SessionRef,
			Concepts: result.Concepts, ConceptMatches: result.ConceptMatches,
			EvidenceIDs: result.EvidenceIDs, WorstRank: result.WorstRank, RankSum: result.RankSum,
			Approximate: result.Approximate, ResponseTruncated: result.ResponseTruncated,
		}
	}
	return out
}

// retrievalResultExcerpt centers a compact result on the densest query-term
// window. Ranked retrieval is a locator surface; get/multi-get own full bodies.
// distinctRetrievalExcerpt returns the bounded excerpt only when it adds
// information over Text; an identical projection is omitted.
func distinctRetrievalExcerpt(text, query string) string {
	excerpt := retrievalResultExcerpt(text, query, retrievalExcerptBytes)
	if excerpt == strings.Join(strings.Fields(text), " ") {
		return ""
	}
	return excerpt
}

func retrievalResultExcerpt(value, query string, maxBytes int) string {
	text := strings.Join(strings.Fields(value), " ")
	if maxBytes <= 0 || text == "" {
		return ""
	}
	if len(text) <= maxBytes {
		return text
	}
	lower, originalOffsets := foldedTextOffsets(text)
	terms := semanticQueryTokens(query)
	bestStart, bestScore := 0, -1
	for _, anchor := range terms {
		for searchAt := 0; searchAt < len(lower); {
			rel := strings.Index(lower[searchAt:], anchor)
			if rel < 0 {
				break
			}
			at := searchAt + rel
			originalAt := originalOffsets[min(at, len(originalOffsets)-1)]
			start := max(0, originalAt-maxBytes/3)
			end := min(len(text), start+maxBytes)
			for start > 0 && !utf8.RuneStart(text[start]) {
				start--
			}
			for end < len(text) && !utf8.RuneStart(text[end]) {
				end++
			}
			window := strings.ToLower(text[start:end])
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

// foldedTextOffsets returns a lowercase search string plus a byte-offset map
// back to the original UTF-8 text. Unicode case folding can change byte length
// (for example, K -> k), so offsets into strings.ToLower(text) must never be
// applied directly to text.
func foldedTextOffsets(text string) (string, []int) {
	var folded strings.Builder
	offsets := make([]int, 0, len(text)+1)
	for originalAt, r := range text {
		lowerRune := strings.ToLower(string(r))
		folded.WriteString(lowerRune)
		for range []byte(lowerRune) {
			offsets = append(offsets, originalAt)
		}
	}
	offsets = append(offsets, len(text))
	return folded.String(), offsets
}

func newGetCommand(opts Options) *cobra.Command {
	var jsonOut bool
	var format string
	var branch string
	var contextBefore, contextAfter, afterTurn, outlineLimit int
	cmd := &cobra.Command{
		Use:   "get <id>",
		Short: "Fetch one item in full by id (fact:… | review:… | history:… | conversation:… | conversation-session:… | doc:… | pattern:… | theme:…)",
		Long: `get fetches one item in full by id (fact: | review: | history: | conversation: |
conversation-session: | doc: | pattern: | theme:).

Exit code: 0 when the id was found, 1 when it was not. A miss still prints
` + "`not found: <id>`" + ` (or a JSON body whose "missing" array names it) before
exiting, so the result is readable either way.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			wantJSON, err := outputWantsJSON(jsonOut, format)
			if err != nil {
				return err
			}
			gopts := getOptions{
				ContextBefore: contextBefore, ContextAfter: contextAfter,
				AfterTurn: afterTurn, OutlineLimit: outlineLimit,
				ContextSet: cmd.Flags().Changed("context-before") || cmd.Flags().Changed("context-after"),
				OutlineSet: cmd.Flags().Changed("after-turn") || cmd.Flags().Changed("limit"),
			}
			missing, err := runGet(cmd.Context(), cmd, opts, []string{args[0]}, branch, wantJSON, gopts, "get")
			if err != nil {
				return err
			}
			return retrievalMissingFailure(missing)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	cmd.Flags().StringVar(&format, "format", "", "Output format: json or cli (QMD-style alias for --json)")
	cmd.Flags().StringVar(&branch, "branch", "", "Branch for facts (default: current)")
	cmd.Flags().IntVar(&contextBefore, "context-before", 0, "Adjacent earlier exchanges to include (conversation: ids only, 0-3)")
	cmd.Flags().IntVar(&contextAfter, "context-after", 0, "Adjacent later exchanges to include (conversation: ids only, 0-3)")
	cmd.Flags().IntVar(&afterTurn, "after-turn", 0, "Outline cursor: entries after this turn ordinal (conversation-session: ids only)")
	cmd.Flags().IntVar(&outlineLimit, "limit", 0, "Outline entries per page, max 50 (conversation-session: ids only)")
	return cmd
}

func newMultiGetCommand(opts Options) *cobra.Command {
	var jsonOut bool
	var format string
	var branch string
	cmd := &cobra.Command{
		Use:   "multi-get <id>...",
		Short: "Fetch multiple items by id",
		Long: `multi-get fetches several items by id in one pass.

Exit code: 0 only when EVERY id was found; 1 if any was missing, including a
partial hit. Each id is a request the caller made, and get is the one-id case of
this command, so a partial miss cannot mean success here and failure there. The
items that were found are printed first either way, and the ids that were not
are named on stdout (or in the JSON "missing" array), so the partial answer
survives the nonzero exit.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			wantJSON, err := outputWantsJSON(jsonOut, format)
			if err != nil {
				return err
			}
			missing, err := runGet(cmd.Context(), cmd, opts, args, branch, wantJSON, getOptions{}, "multi-get")
			if err != nil {
				return err
			}
			return retrievalMissingFailure(missing)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	cmd.Flags().StringVar(&format, "format", "", "Output format: json or cli (QMD-style alias for --json)")
	cmd.Flags().StringVar(&branch, "branch", "", "Branch for facts (default: current)")
	return cmd
}

// errRetrievalIDsMissing is the gate `get` and `multi-get` close when an id the
// caller named was not in the brain.
var errRetrievalIDsMissing = errors.New("requested ids were not found")

// retrievalMissingFailure turns "part of what you asked for is not here" into an
// exit code, AFTER the results and the `not found:` lines have been written.
//
// `show`, the sibling verb, already exits 1 for the one id it takes, while
// `get` and `multi-get` exited 0 whether they found everything, something or
// nothing -- so nothing downstream could branch without parsing stdout.
//
// ANY missing id fails, including a partial hit. Every id in the argument list
// is a request the caller made, and `get` is the one-id case of `multi-get`, so
// a rule that forgave a partial miss would make the SAME id exit 0 when asked
// for alongside a hit and 1 when asked for alone. Nothing is withheld to say
// it: the found results and the per-id `not found:` lines (or the JSON
// `missing` array) are already out, so a caller that wants the partial answer
// still has it and can ignore the code.
//
// renderedCommandError because the ids were named on the way out; repeating
// them on stderr would say it twice.
func retrievalMissingFailure(missing []string) error {
	if len(missing) == 0 {
		return nil
	}
	return renderedCommandError{err: fmt.Errorf("%w: %s", errRetrievalIDsMissing, strings.Join(missing, ", "))}
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

// runGet emits the requested items and REPORTS which ids were not found rather
// than failing on them. The miss is a result, not an error: the MCP tools share
// this body, and a JSON-RPC tool result has to keep carrying `missing` in its
// payload. Turning a miss into an exit code is the CLI's job, one layer up --
// see retrievalMissingFailure.
func runGet(ctx context.Context, cmd *cobra.Command, opts Options, ids []string, branch string, jsonOut bool, gopts getOptions, surface string) ([]string, error) {
	repoDir, brainDir, resolvedBranch, err := resolveFactsTarget(ctx, opts, agentSurfaceTarget(opts, nil), branch)
	if err != nil {
		return nil, err
	}
	privacyPolicy, err := captureRetrievalPrivacyPolicy(brainDir)
	if err != nil {
		return nil, err
	}
	requestedPattern := false
	for _, id := range ids {
		if strings.HasPrefix(id, "pattern:") || strings.HasPrefix(id, "theme:") {
			privacyPolicy.RequireDerivedClean = true
			requestedPattern = true
			break
		}
	}
	if requestedPattern {
		if err := requirePrivacyDerivedRead(brainDir); err != nil {
			return nil, err
		}
		if err := requirePatternCorpusAvailable(brainDir); err != nil {
			return nil, err
		}
	}
	found, missing, err := getUnifiedBatchOptions(repoDir, brainDir, resolvedBranch, ids, gopts)
	if err != nil {
		return nil, err
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
		serialized, err := jsonOutputBytes(map[string]any{"branch": resolvedBranch, "results": found, "missing": missing})
		if err != nil {
			return nil, err
		}
		if err := writeRetrievalResponseBytes(cmd.OutOrStdout(), serialized, privacyPolicy); err != nil {
			return nil, err
		}
		if len(factIDs) > 0 {
			recordReceipt()
		}
		return missing, nil
	}
	var rendered bytes.Buffer
	if err := writeTextToWriter(&rendered, func(out io.Writer) {
		for _, r := range found {
			label := r.Source
			if r.VerificationRequired {
				label += " verify"
			}
			fmt.Fprintf(out, "[%s] %s  %s\n%s\n", label, r.ID, unifiedResultLocation(r), r.Text)
			printRetrievalCaveats(out, r)
			printConversationTurns(out, r)
			fmt.Fprintln(out)
		}
		for _, id := range missing {
			fmt.Fprintf(out, "not found: %s\n", id)
		}
	}); err != nil {
		return nil, err
	}
	if err := writeRetrievalResponseBytes(cmd.OutOrStdout(), rendered.Bytes(), privacyPolicy); err != nil {
		return nil, err
	}
	if len(factIDs) > 0 {
		recordReceipt()
	}
	return missing, nil
}

// unifiedResultLocation renders a result's source anchor, including the
// inclusive end line for range-bearing results (conversation exchanges).
func unifiedResultLocation(r unifiedResult) string {
	switch {
	case r.Line > 0 && r.EndLine > r.Line:
		return fmt.Sprintf("%s:%d-%d", r.Path, r.Line, r.EndLine)
	case r.Line > 0:
		return fmt.Sprintf("%s:%d", r.Path, r.Line)
	default:
		return r.Path
	}
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
