package retrieve

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/cmdutil"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	"github.com/entireio/entire-brain/internal/cli/internal/retrieval"
	"github.com/spf13/cobra"
)

// retrieve.go wires the qmd-inspired verbs over the unified text index:
// search (lexical), query (BM25 lexical; semantic retrieval was removed in
// trail #278), vsearch (compatibility alias for query), and get/multi-get (fetch by
// id). These verbs subsumed the old per-source inspect kinds (facts/docs/history
// text). What remains under `inspect` is only what the verbs can't do: symbol-graph
// traversal (code/context/impact/changes/tests/boundaries) and regression analysis.

func NewSearch(cfg Config) *cobra.Command {
	return NewRetrieve(cfg, "search", retrieval.ModeLexical, "Lexical keyword search across facts, history, and docs (BM25 ranking; facts add locus and closed-negative boosts)")
}

func NewVsearch(cfg Config) *cobra.Command {
	return NewRetrieve(cfg, "vsearch", retrieval.ModeHybrid, "Compatibility alias for query: semantic retrieval was removed (trail #278); runs the same BM25 lexical ranking")
}

func NewQuery(cfg Config) *cobra.Command {
	return NewRetrieve(cfg, "query", retrieval.ModeHybrid, "Search the brain (BM25 lexical ranking)")
}

func NewRetrieve(cfg Config, use string, mode retrieval.Mode, short string) *cobra.Command {
	var selection QuerySelection
	var jsonOut bool
	var format string
	var limit int
	var branch string
	var patterns, includeAbstract, noGlobal bool
	var source, after, before, session, agent string
	var concepts []string
	var recency bool
	var recencyHalfLife string
	cmd := &cobra.Command{
		Use:   use + " <query>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			wantJSON, err := outputWantsJSON(jsonOut, format)
			if err != nil {
				return err
			}
			ropts, err := BuildRetrievalOptions(source, after, before, session, agent, branch, concepts)
			if err != nil {
				return fmt.Errorf("--%s", err.Error())
			}
			ropts.IncludeAbstract = includeAbstract
			ropts.NoGlobalFacts = noGlobal
			ropts, err = retrieval.WithRecencyOptions(ropts, recency, recencyHalfLife)
			if err != nil {
				return err
			}
			selectedMode, err := selection.Mode(mode)
			if err != nil {
				return err
			}
			WarnRemovedSemanticAlias(cmd, use, selection.Semantic)
			return Run(cmd.Context(), cmd, cfg, args[0], selectedMode, limit, branch, ropts, wantJSON, patterns, use)
		},
	}
	ConfigureQueryCommand(cmd, use, &selection, 0)
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	cmd.Flags().IntVar(&limit, "limit", 10, "Maximum results")
	cmd.Flags().IntVarP(&limit, "number", "n", 10, "Maximum results (QMD-style alias for --limit)")
	cmd.Flags().StringVar(&format, "format", "", "Output format: json or cli (QMD-style alias for --json)")
	cmd.Flags().StringVar(&branch, "branch", "", "Branch for memories (default: current); with --source conversation also filters exchanges to that captured branch")
	cmd.Flags().BoolVar(&noGlobal, "no-global", false, "Exclude global memories from this request")
	cmd.Flags().BoolVar(&patterns, "patterns", false, "Also surface relevant pattern:/theme: pointers (does not change facts/history/docs ranking)")
	cmd.Flags().StringVar(&source, "source", "", "Restrict retrieval to one source: all, fact, history, conversation, or doc (default all; conversation is experimental opt-in)")
	cmd.Flags().StringVar(&after, "after", "", "Conversation source only: sessions at or after this time (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().StringVar(&before, "before", "", "Conversation source only: sessions before this time (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().StringVar(&session, "session", "", "Conversation source only: exchanges from this session id (also disables the per-session diversity cap)")
	cmd.Flags().StringVar(&agent, "agent", "", "Conversation source only: exchanges captured by this agent/harness (e.g. \"Claude Code\", \"Codex\")")
	cmd.Flags().StringArrayVar(&concepts, "concept", nil, "Conversation source only: additional concept (repeatable, up to 4); sessions must match the query AND every concept")
	cmd.Flags().BoolVar(&includeAbstract, "include-abstract", false, "Conversation source only: include bounded evidence-linked session previews (never affects ranking)")
	cmd.Flags().BoolVar(&recency, "recency", false, "Prefer recent dated results (not supported for source conversation)")
	cmd.Flags().StringVar(&recencyHalfLife, "recency-half-life", "", "Recency half-life as a positive Go duration, e.g. 720h (default 2160h; requires --recency)")
	return cmd
}

// BuildRetrievalOptions validates and assembles the shared retrieval-options
// contract from CLI flags. branch doubles as the conversation
// branch filter; error text names bare flag words so the CLI can prefix "--".
func BuildRetrievalOptions(source, after, before, session, agent, branch string, concepts []string) (retrieval.Options, error) {
	parsedSource, err := retrieval.ParseSource(source)
	if err != nil {
		return retrieval.Options{}, err
	}
	if len(concepts) > retrieval.ConversationConceptsMaxTotal-1 {
		return retrieval.Options{}, fmt.Errorf("concept: at most %d additional concepts (got %d)", retrieval.ConversationConceptsMaxTotal-1, len(concepts))
	}
	trimmedConcepts := make([]string, 0, len(concepts))
	for _, concept := range concepts {
		concept = strings.TrimSpace(concept)
		if concept == "" {
			return retrieval.Options{}, fmt.Errorf("concept: concepts must be non-empty")
		}
		trimmedConcepts = append(trimmedConcepts, concept)
	}
	if len(trimmedConcepts) > 0 && parsedSource != brainstore.RetrievalSourceConversation {
		return retrieval.Options{}, fmt.Errorf(`concept: concepts require source "conversation" (got %q)`, parsedSource)
	}
	afterTime, err := retrieval.ParseTimeFilter(after)
	if err != nil {
		return retrieval.Options{}, fmt.Errorf("after: %s", err.Error())
	}
	beforeTime, err := retrieval.ParseTimeFilter(before)
	if err != nil {
		return retrieval.Options{}, fmt.Errorf("before: %s", err.Error())
	}
	if !afterTime.IsZero() && !beforeTime.IsZero() && !afterTime.Before(beforeTime) {
		return retrieval.Options{}, fmt.Errorf("after (%s) must be earlier than before (%s)", after, before)
	}
	return retrieval.Options{
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
// "query"). ropts carries the
// validated source selector and structured filters; branch (the raw flag)
// still selects the facts branch via the resolve-target seam.
func Run(ctx context.Context, cmd *cobra.Command, cfg Config, query string, mode retrieval.Mode, limit int, branch string, ropts retrieval.Options, jsonOut, patterns bool, surface string) error {
	// Reject --limit <= 0 rather than silently defaulting, so a typo like
	// `--limit 0` is an explicit error (matching the rest of the CLI surface).
	if limit <= 0 {
		return fmt.Errorf("--limit must be greater than 0")
	}
	if len(ropts.Concepts) > 0 {
		if err := retrieval.ValidateRetrievalLimit(limit); err != nil {
			return fmt.Errorf("--%s", err.Error())
		}
	}
	repoDir, brainDir, resolvedBranch, err := cfg.Deps.ResolveTarget(ctx, cfg.Options, branch)
	if err != nil {
		return err
	}
	privacyPolicy, err := brainstore.CaptureRetrievalPrivacyPolicy(brainDir)
	if err != nil {
		return err
	}
	if patterns {
		privacyPolicy.RequireDerivedClean = true
		if err := cfg.Deps.RequirePatternsReadable(brainDir); err != nil {
			return err
		}
	}
	// Facts recorded outside any repository merge into the fact layer before
	// ranking, so a preference recorded once applies on every retrieval surface
	// rather than only on the one command that happened to load them. They go
	// through the same guard, filters and scoring as repository facts.
	if globalFacts, globalErr := factstore.GlobalFactsForRead(cfg.Options.Env, ropts.NoGlobalFacts); globalErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: global memories unavailable: %v\n", globalErr)
	} else {
		ropts.GlobalFacts = globalFacts
	}
	results, err := cfg.Deps.RetrieveUnified(repoDir, brainDir, resolvedBranch, query, limit, mode, ropts)
	if err != nil {
		return err
	}
	multiConcept := len(ropts.Concepts) > 0
	policyIdentityBeforeExtras := privacyPolicy.Identity
	factIDs := unifiedFactIDs(results)
	recordReceipt := func() {
		cfg.Deps.RecordServedFacts(cmd.ErrOrStderr(), ctx, cfg.Options, brainDir, resolvedBranch, surface,
			repoDir, query, factIDs)
	}
	// Discoverability only: pattern/theme pointers never enter the facts/history/
	// docs ranking — they are a separate, capped, opt-in section so default
	// retrieval quality is unchanged by construction.
	var related []retrieval.RelatedPatternRef
	if patterns {
		related, err = cfg.Deps.RelatedPatternPointers(brainDir, query)
		if err != nil {
			return err
		}
	}
	var abstractPreviews []retrieval.SessionAbstractPreview
	var abstractPreviewsTruncated bool
	if ropts.IncludeAbstract {
		if ropts.Source != brainstore.RetrievalSourceConversation {
			return fmt.Errorf(`--include-abstract requires --source conversation (got %q)`, ropts.Source)
		}
		abstractPreviews, abstractPreviewsTruncated, err = cfg.Deps.AbstractPreviews(brainDir, results)
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
			out := map[string]any{"query": query, "branch": resolvedBranch, "results": retrieval.CompactResults(results, query)}
			if ropts.IncludeAbstract {
				out["abstract_previews"] = abstractPreviews
				out["abstract_previews_truncated"] = abstractPreviewsTruncated
			}
			hints := cfg.Deps.TaskHints(repoDir, results, query)
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
				if note := cfg.Deps.EmptyResultBlindSpot(brainDir, resolvedBranch); note != "" {
					out["blind_spot"] = note
				}
			}
			serialized, err := cmdutil.JSONOutputBytes(out)
			if err != nil {
				return err
			}
			if err := cfg.Deps.WriteRetrievalResponse(cmd.OutOrStdout(), serialized, privacyPolicy); err != nil {
				return err
			}
			if len(factIDs) > 0 {
				recordReceipt()
			}
			return nil
		}
		var rendered bytes.Buffer
		if err := cmdutil.WriteTextToWriter(&rendered, func(out io.Writer) {
			if len(results) == 0 {
				fmt.Fprintf(out, "no results for %q\n", query)
				if note := cfg.Deps.EmptyResultBlindSpot(brainDir, resolvedBranch); note != "" {
					fmt.Fprintln(out, note)
				}
			}
			hints := cfg.Deps.TaskHints(repoDir, results, query)
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
				ex := base.TruncateString(strings.Join(strings.Fields(result.Text), " "), 200)
				label := result.Source
				if result.VerificationRequired {
					label += " verify"
				}
				fmt.Fprintf(out, "[%s] %s  %s\n    %s\n", label, result.ID, retrieval.ResultLocation(result), ex)
				retrieval.PrintResultCaveats(out, result)
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
		if err := cfg.Deps.WriteRetrievalResponse(cmd.OutOrStdout(), rendered.Bytes(), privacyPolicy); err != nil {
			return err
		}
		if len(factIDs) > 0 {
			recordReceipt()
		}
		return nil
	}
	// Ranking may be slow enough for a concurrent exclusion to land after its
	// first guard snapshot. Revalidate immediately before transport assembly;
	// no JSON or text bytes are emitted before this check succeeds.
	results, policyAfter, err := cfg.Deps.RevalidateResponsePrivacy(brainDir, results)
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
	extras := retrieval.TransportExtras{
		AbstractPreviews:          abstractPreviews,
		AbstractPreviewsTruncated: abstractPreviewsTruncated,
		Hints:                     cfg.Deps.TaskHints(repoDir, results, query),
		Related:                   related,
	}
	if len(results) == 0 {
		extras.BlindSpot = cfg.Deps.EmptyResultBlindSpot(brainDir, resolvedBranch)
	}
	// Preserve the established JSON `text` field
	// surfaces. Excerpt is additive; existing consumers must not be forced to
	// switch to a second brain_get round trip.
	data, err := cfg.Deps.BoundedResponseBytes(query, resolvedBranch, results, extras, jsonOut)
	if err != nil {
		return err
	}
	if err := cfg.Deps.WriteRetrievalResponse(cmd.OutOrStdout(), data, brainstore.RetrievalPrivacyPolicy{BrainDir: brainDir, Identity: policyAfter}); err != nil {
		return err
	}
	if len(factIDs) > 0 {
		recordReceipt()
	}
	return nil
}

func NewGet(cfg Config) *cobra.Command {
	var jsonOut bool
	var format string
	var branch string
	var contextBefore, contextAfter, afterTurn, outlineLimit int
	cmd := &cobra.Command{
		Use:   "get <id>",
		Short: "Fetch a brain item by ID",
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
			gopts := retrieval.GetOptions{
				ContextBefore: contextBefore, ContextAfter: contextAfter,
				AfterTurn: afterTurn, OutlineLimit: outlineLimit,
				ContextSet: cmd.Flags().Changed("context-before") || cmd.Flags().Changed("context-after"),
				OutlineSet: cmd.Flags().Changed("after-turn") || cmd.Flags().Changed("limit"),
			}
			missing, err := RunGet(cmd.Context(), cmd, cfg, []string{args[0]}, branch, wantJSON, gopts, "get")
			if err != nil {
				return err
			}
			return retrievalMissingFailure(missing)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	cmd.Flags().StringVar(&format, "format", "", "Output format: json or cli (QMD-style alias for --json)")
	cmd.Flags().StringVar(&branch, "branch", "", "Branch for memories (default: current)")
	cmd.Flags().IntVar(&contextBefore, "context-before", 0, "Adjacent earlier exchanges to include (conversation: ids only, 0-3)")
	cmd.Flags().IntVar(&contextAfter, "context-after", 0, "Adjacent later exchanges to include (conversation: ids only, 0-3)")
	cmd.Flags().IntVar(&afterTurn, "after-turn", 0, "Outline cursor: entries after this turn ordinal (conversation-session: ids only)")
	cmd.Flags().IntVar(&outlineLimit, "limit", 0, "Outline entries per page, max 50 (conversation-session: ids only)")
	return cmd
}

func NewMultiGet(cfg Config) *cobra.Command {
	var jsonOut bool
	var format string
	var branch string
	cmd := &cobra.Command{
		Use:   "multi-get <id>...",
		Short: "Fetch multiple brain items by ID",
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
			missing, err := RunGet(cmd.Context(), cmd, cfg, args, branch, wantJSON, retrieval.GetOptions{}, "multi-get")
			if err != nil {
				return err
			}
			return retrievalMissingFailure(missing)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	cmd.Flags().StringVar(&format, "format", "", "Output format: json or cli (QMD-style alias for --json)")
	cmd.Flags().StringVar(&branch, "branch", "", "Branch for memories (default: current)")
	return cmd
}

// ErrRetrievalIDsMissing is the gate `get` and `multi-get` close when an id the
// caller named was not in the brain.
var ErrRetrievalIDsMissing = errors.New("requested ids were not found")

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
// cmdutil.RenderedCommandError because the ids were named on the way out;
// repeating them on stderr would say it twice.
func retrievalMissingFailure(missing []string) error {
	if len(missing) == 0 {
		return nil
	}
	return cmdutil.RenderedCommandError{Err: fmt.Errorf("%w: %s", ErrRetrievalIDsMissing, strings.Join(missing, ", "))}
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

// RunGet emits the requested items and REPORTS which ids were not found rather
// than failing on them. The miss is a result, not an error: the JSON payload
// keeps carrying `missing`. Turning a miss into an exit code is the CLI's job,
// one layer up -- see retrievalMissingFailure.
func RunGet(ctx context.Context, cmd *cobra.Command, cfg Config, ids []string, branch string, jsonOut bool, gopts retrieval.GetOptions, surface string) ([]string, error) {
	repoDir, brainDir, resolvedBranch, err := cfg.Deps.ResolveTarget(ctx, cfg.Options, branch)
	if err != nil {
		return nil, err
	}
	privacyPolicy, err := brainstore.CaptureRetrievalPrivacyPolicy(brainDir)
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
		if err := cfg.Deps.RequirePatternsReadable(brainDir); err != nil {
			return nil, err
		}
	}
	// An id that query or search just handed back and get reports missing is a
	// worse seam than not returning it at all.
	if globalFacts, globalErr := factstore.GlobalFactsForRead(cfg.Options.Env, false); globalErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: global memories unavailable: %v\n", globalErr)
	} else {
		gopts.GlobalFacts = globalFacts
	}
	found, missing, err := cfg.Deps.GetUnifiedBatch(repoDir, brainDir, resolvedBranch, ids, gopts)
	if err != nil {
		return nil, err
	}
	factIDs := unifiedFactIDs(found)
	recordReceipt := func() {
		cfg.Deps.RecordServedFacts(cmd.ErrOrStderr(), ctx, cfg.Options, brainDir, resolvedBranch, surface,
			repoDir, "", factIDs)
	}
	// Normalize empty collections to [] so --json emits arrays, not null, matching
	// the repo's JSON contract (see TestInspectCodeEmptyResultsEmitArrayNotNull).
	if found == nil {
		found = []retrieval.Result{}
	}
	if missing == nil {
		missing = []string{}
	}
	// Computed BEFORE the output split, because both surfaces need it.
	//
	// These notes lived only in the text block, so `get --json` never carried
	// them. The agent surface, the one most likely to act on a "--branch" hint,
	// was the one surface that could not see it. The integrity note had the
	// same gap before this change and inherits the fix.
	missNote := getMissNote(brainDir, resolvedBranch, missing)
	if jsonOut {
		payload := map[string]any{"branch": resolvedBranch, "results": found, "missing": missing}
		if missNote != "" {
			payload["blind_spot"] = missNote
		}
		serialized, err := cmdutil.JSONOutputBytes(payload)
		if err != nil {
			return nil, err
		}
		if err := cfg.Deps.WriteRetrievalResponse(cmd.OutOrStdout(), serialized, privacyPolicy); err != nil {
			return nil, err
		}
		if len(factIDs) > 0 {
			recordReceipt()
		}
		return missing, nil
	}
	var rendered bytes.Buffer
	if err := cmdutil.WriteTextToWriter(&rendered, func(out io.Writer) {
		for _, r := range found {
			label := r.Source
			if r.VerificationRequired {
				label += " verify"
			}
			fmt.Fprintf(out, "[%s] %s  %s\n%s\n", label, r.ID, retrieval.ResultLocation(r), r.Text)
			retrieval.PrintResultCaveats(out, r)
			printConversationTurns(out, r)
			fmt.Fprintln(out)
		}
		for _, id := range missing {
			fmt.Fprintf(out, "not found: %s\n", id)
		}
		// "not found" for an id the caller is already holding makes the same
		// claim the empty-result note makes: that the item was never there.
		// When the fact store cannot produce what the manifest declares, that
		// claim is wrong — and `get fact:<id>` is usually the exact moment a
		// user discovers the loss.
		if missNote != "" {
			fmt.Fprintf(out, "%s\n", missNote)
		}
	}); err != nil {
		return nil, err
	}
	if err := cfg.Deps.WriteRetrievalResponse(cmd.OutOrStdout(), rendered.Bytes(), privacyPolicy); err != nil {
		return nil, err
	}
	if len(factIDs) > 0 {
		recordReceipt()
	}
	return missing, nil
}

// unifiedFactIDs extracts the fact-layer ids from unified retrieval results.
// Only source "fact" is a served fact; review groups, history, and docs carry
// no per-fact vitality.
func unifiedFactIDs(results []retrieval.Result) []string {
	var ids []string
	for _, r := range results {
		if r.Source == "fact" {
			ids = append(ids, r.ID)
		}
	}
	return ids
}

// printConversationTurns renders outline or context entries for the text
// surface; JSON callers get the structured turns directly.
func printConversationTurns(out io.Writer, r retrieval.Result) {
	for _, turn := range r.Turns {
		body := turn.Request
		if body == "" {
			body = turn.Text
		}
		fmt.Fprintf(out, "  turn %d [%s] %s\n", turn.TurnOrdinal, turn.ConversationID, strings.Join(strings.Fields(body), " "))
		for _, caveat := range turn.Caveats {
			if caveat.Kind != retrieval.CaveatHistoricalConversation {
				fmt.Fprintf(out, "    caveat %s: %s\n", caveat.Kind, caveat.Message)
			}
		}
	}
	if r.NextTurn > 0 {
		fmt.Fprintf(out, "  next turn: %d\n", r.NextTurn)
	}
	if r.PacketTruncated {
		fmt.Fprintln(out, "  packet truncated: byte cap reached")
	}
}

// WarnRemovedSemanticAlias prints the one-line deprecation notice when a
// caller reaches retrieval through a removed-semantic surface (the vsearch
// verb or --semantic). stderr only, so JSON/CSV consumers are unaffected.
func WarnRemovedSemanticAlias(cmd *cobra.Command, use string, semanticFlag bool) {
	if use != "vsearch" && !semanticFlag {
		return
	}
	fmt.Fprintln(cmd.ErrOrStderr(), "warning: semantic retrieval was removed (trail #278); running the standard BM25 lexical ranking")
}

// QuerySelection is shared by local and workspace query commands.
type QuerySelection struct {
	text     string
	keyword  bool
	Semantic bool
}

func (s QuerySelection) Mode(fallback retrieval.Mode) (retrieval.Mode, error) {
	if s.keyword && s.Semantic {
		return fallback, fmt.Errorf("--keyword and --semantic are mutually exclusive")
	}
	if s.keyword {
		return retrieval.ModeLexical, nil
	}
	if s.Semantic {
		// Semantic retrieval was removed (trail #278); the flag is honored as
		// a compatibility alias for the default BM25 ranking.
		return retrieval.ModeHybrid, nil
	}
	return fallback, nil
}

func ConfigureQueryCommand(cmd *cobra.Command, use string, selection *QuerySelection, prefixArgs int) {
	if use != "query" {
		cmd.Hidden = true // Keep search/vsearch callable for existing integrations.
		return
	}
	cmd.Use = "query [query]"
	if prefixArgs == 1 {
		cmd.Use = "query <workspace> [query]"
	}
	cmd.Long = cmd.Short + ". Supply the text positionally or with --query. Flags may appear before or after the text."
	cmd.Flags().StringVar(&selection.text, "query", "", "Query text (alternative to the positional query)")
	cmd.Flags().BoolVar(&selection.keyword, "keyword", false, "Match keywords and identifiers only")
	cmd.Flags().BoolVar(&selection.Semantic, "semantic", false, "Compatibility alias: semantic retrieval was removed; ranking is BM25 lexical")
	cmd.MarkFlagsMutuallyExclusive("keyword", "semantic")
	cmd.Args = func(cmd *cobra.Command, args []string) error {
		if cmd.Flags().Changed("query") {
			if len(args) != prefixArgs {
				if len(args) < prefixArgs {
					return fmt.Errorf("workspace is required")
				}
				return fmt.Errorf("supply either a positional query or --query, not both")
			}
			if strings.TrimSpace(selection.text) == "" {
				return fmt.Errorf("query must not be empty")
			}
		} else {
			if err := cobra.ExactArgs(prefixArgs+1)(cmd, args); err != nil {
				return err
			}
			if strings.TrimSpace(args[prefixArgs]) == "" {
				return fmt.Errorf("query must not be empty")
			}
		}
		return nil
	}
	run := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if cmd.Flags().Changed("query") {
			args = append(append([]string(nil), args...), selection.text)
		}
		return run(cmd, args)
	}
}

// missingFactIDs reports whether any of these ids is fact-shaped.
//
// `get` and `multi-get` are unified surfaces that take fact, review, history,
// conversation, doc, pattern and theme ids. The branch note is about where
// FACTS live, so it belongs only on a miss that could be a fact: on
// `get conversation:<id>` it would be noise attached to an unrelated answer.
//
// A bare id with no prefix is treated as a fact id, matching how the rest of
// the codebase resolves them (facts_eval.go:902).
func missingFactIDs(ids []string) bool {
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if strings.HasPrefix(id, "fact:") || !strings.Contains(id, ":") {
			return true
		}
	}
	return false
}

// getMissNote is the one note a miss carries, computed once for both output
// formats.
//
// It lived inside the text block, so `get --json` carried neither this nor
// the integrity warning, making the agent surface the one consumer that could
// not see it.
//
// INTEGRITY WINS. A store that cannot produce what its manifest declares must
// never be reported as merely the wrong branch, or the caller goes hunting for
// a fact that is actually gone.
//
// The branch note is gated on a fact-shaped id because get and multi-get are
// unified surfaces: a missing conversation has nothing to do with which branch
// holds facts, and a note about facts there is noise on an unrelated answer.
func getMissNote(brainDir, branch string, missing []string) string {
	if len(missing) == 0 {
		return ""
	}
	if integrity := factstore.InspectBrain(brainDir); !integrity.OK() {
		return fmt.Sprintf("note: \"not found\" is NOT evidence of absence — %s", integrity.Warning())
	}
	if !missingFactIDs(missing) {
		return ""
	}
	return factstore.OtherBranchBlindSpot(brainDir, branch)
}
