package facts

import (
	"fmt"
	"io"
	"strings"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/cmdutil"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"github.com/entireio/entire-brain/internal/cli/internal/retrieval"
	"github.com/spf13/cobra"
)

// RecallEngineBM25Lexical names recall's only retrieval engine in its JSON
// identity report.
const RecallEngineBM25Lexical = "bm25_lexical"

// RecallRetrievalEngine is emitted with recall --json. Semantic retrieval was
// removed (trail #278: BM25 matched or beat the embedder stack end-to-end);
// the semantic/vector fields are retained in the schema and report inert
// values so existing consumers keep parsing.
type RecallRetrievalEngine struct {
	SchemaVersion         int    `json:"schema_version"`
	EffectiveEngine       string `json:"effective_engine,omitempty"`
	IdentityVerified      bool   `json:"identity_verified"`
	SemanticRequested     bool   `json:"semantic_requested"`
	SemanticApplied       bool   `json:"semantic_applied"`
	SemanticAvailable     bool   `json:"semantic_available"`
	BM25Enabled           bool   `json:"bm25_enabled"`
	BM25Applied           bool   `json:"bm25_applied"`
	FallbackUsed          bool   `json:"fallback_used"`
	IdentityFailureReason string `json:"identity_failure_reason,omitempty"`
}

func recallRetrievalIdentity() RecallRetrievalEngine {
	return RecallRetrievalEngine{
		SchemaVersion:    1,
		EffectiveEngine:  RecallEngineBM25Lexical,
		IdentityVerified: true,
		BM25Enabled:      true,
		BM25Applied:      true,
	}
}

// recallMaxK bounds recall's -k.
const recallMaxK = 128

func NewRecall(cfg Config) *cobra.Command {
	var evidence bool
	var evidenceBytes int
	var (
		branch                string
		limit                 int
		includeAll            bool
		scope                 string
		kind                  string
		locus                 string
		noSemantic            bool
		expand                bool
		agent                 string
		model                 string
		agentCommand          []string
		jsonOut               bool
		eligibleBefore        string
		sessionDatesPath      string
		excludeSessionIDs     []string
		readOnlySemanticCache bool
		noGlobal              bool
	)
	cmd := &cobra.Command{
		Use:   "recall <query>",
		Short: "Retrieve durable memories matching a query for the current branch",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			query := ""
			if len(args) == 1 {
				query = args[0]
			}
			if evidence {
				for _, flag := range []string{"all", "scope", "kind", "locus", "expand", "agent", "model", "agent-command", "eligible-before", "session-dates", "exclude-session-id", "read-only-semantic-cache"} {
					if cmd.Flags().Changed(flag) {
						return fmt.Errorf("--%s is not supported with --evidence; evidence recall is deterministic and does not apply memory filters", flag)
					}
				}
			} else if cmd.Flags().Changed("evidence-bytes") {
				return fmt.Errorf("--evidence-bytes requires --evidence")
			}
			if err := factstore.ValidateScopeFlag(scope); err != nil {
				return err
			}
			if err := factstore.ValidateKindFlag(kind); err != nil {
				return err
			}
			repoDir, brainDir, resolvedBranch, err := cfg.Deps.ResolveFactsTarget(cmd.Context(), cfg.Options, cfg.Deps.AgentSurfaceTarget(cfg.Options, nil), branch)
			if err != nil {
				return err
			}
			if evidence {
				return cfg.Deps.RunRecallEvidence(cmd, brainDir, resolvedBranch, query, limit, evidenceBytes, jsonOut)
			}
			allFacts, err := factstore.Load(brainDir, resolvedBranch)
			if err != nil {
				return err
			}
			// Facts that are not about this repository — preferences, team
			// conventions, environment details — merge in here, so something
			// recorded once applies everywhere. They go through every filter
			// and the same ranking as repository facts; the only difference is
			// that they are labelled, because acting on a global convention as
			// though this repo had declared it is a different thing.
			globalIDs := map[string]bool{}
			if factstore.GlobalFactsEnabled(noGlobal) {
				globalFacts, globalErr := factstore.LoadGlobal(cfg.Options.Env)
				if globalErr != nil {
					// A damaged global store must not take recall down with it;
					// the repository's own facts are the answer to most queries.
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: global memories unavailable: %v\n", globalErr)
				} else {
					allFacts, globalIDs = retrieval.MergeGlobalFacts(allFacts, globalFacts)
				}
			}
			var storeWarning string
			manifest, manifestErr := bmanifest.Load(brainDir)
			if manifestErr != nil {
				storeWarning = "memory source manifest unavailable: " + manifestErr.Error()
			} else if manifest != nil && manifest.Sources != nil {
				storeWarning = factstore.MissingStoreWarningForBranch(brainDir, manifest.Sources.Facts, resolvedBranch)
			}
			candidateFacts := allFacts
			var eligibility *FactEligibilityAudit
			if eligibleBefore != "" || sessionDatesPath != "" || len(excludeSessionIDs) > 0 {
				if eligibleBefore == "" || sessionDatesPath == "" {
					return fmt.Errorf("--eligible-before and --session-dates must be supplied together")
				}
				dates, loadErr := loadSessionDates(sessionDatesPath)
				if loadErr != nil {
					return loadErr
				}
				filtered, audit, filterErr := filterFactsByTemporalEligibility(allFacts, dates, eligibleBefore, excludeSessionIDs)
				if filterErr != nil {
					return filterErr
				}
				candidateFacts = filtered
				eligibility = &audit
			}
			facts := factstore.FilterByLocus(factstore.FilterByKind(factstore.FilterByScope(candidateFacts, scope), kind), locus)
			effectiveQuery := query
			if expand && strings.TrimSpace(query) != "" {
				resolved := agent
				if resolved == "auto" {
					resolved = cfg.Deps.DefaultRefreshAgent(cmd.Context(), cfg.Options.Runner, repoDir)
				}
				expanded, expErr := cfg.Deps.ExpandQuery(cmd.Context(), resolved, agentCommand, model, repoDir, query)
				if expErr != nil {
					return expErr
				}
				effectiveQuery = expanded
			}
			// --no-semantic and --read-only-semantic-cache are vacuously
			// satisfied since the semantic arm was removed (trail #278):
			// ranking is always lexical and no vector cache exists to write.
			_, _ = noSemantic, readOnlySemanticCache
			matches := cfg.Deps.RankFacts(facts, effectiveQuery, limit, includeAll)
			// Locus drift (Phase 2 item 4): flag surfaced facts whose code
			// locus left the worktree, so the agent knows which to re-verify.
			drift := retrieval.FactsLocusDrift(repoDir, retrieval.FactsEligibleForLocusDrift(matches, globalIDs))
			// Live trust state: a surfaced fact with a pending merge/supersede
			// proposal is annotated (not collapsed — recall keeps its record
			// shape), matching the guard the unified query/search/get path
			// applies. Groups are built from the unfiltered branch set so
			// scope/kind/locus filters cannot hide a pending relationship.
			// Nothing surfaced means no annotation and no queue warning, so skip
			// the proposal read entirely on the empty-recall path.
			var proposals []factmerge.Proposal
			var proposalsErr error
			var pendingReviews map[string]retrieval.FactReviewNotice
			if len(matches) > 0 {
				proposals, proposalsErr = cfg.Deps.LoadFactProposals(brainDir, resolvedBranch)
				if proposalsErr == nil {
					localFacts := retrieval.FactsEligibleForLocusDrift(allFacts, globalIDs)
					pendingReviews = retrieval.FactsPendingReview(localFacts, proposals, matches)
				}
			}
			recordReceipt := func() {
				cfg.Deps.RecordServedFacts(cmd.ErrOrStderr(), cmd.Context(), cfg.Options, brainDir, resolvedBranch, "recall",
					repoDir, query, matches)
			}
			if jsonOut {
				// Recall's JSON envelope is fact-centric (top-level `facts`), so
				// its fact-scoped keys stay unprefixed: `pending_reviews` and
				// `locus_drift`. The brief report is a multi-domain object and
				// prefixes the same concepts as `facts_pending_review` /
				// `facts_locus_drift` to disambiguate. This per-surface split is
				// intentional and kept as-is; we do not unify the keys.
				out := map[string]any{"branch": resolvedBranch, "query": query, "facts": matches}
				// A consumer that renders these has to be able to tell which
				// facts came from outside this repository; the ids are the
				// smallest way to say so without changing the fact shape that
				// every existing reader binds to.
				if ids := SortedGlobalFactIDs(matches, globalIDs); len(ids) > 0 {
					out["global_fact_ids"] = ids
				}
				engine := recallRetrievalIdentity()
				out["retrieval_engine"] = engine
				if engine.EffectiveEngine != "" {
					out["effective_engine"] = engine.EffectiveEngine
				}
				if eligibility != nil {
					eligibility.DeliveredCount = len(matches)
					out["eligibility"] = eligibility
				}
				if len(drift) > 0 {
					out["locus_drift"] = drift
				}
				if len(pendingReviews) > 0 {
					out["pending_reviews"] = pendingReviews
				}
				if proposalsErr != nil && len(matches) > 0 {
					out["warnings"] = []string{retrieval.FactReviewQueueUnavailableWarning}
				}
				if storeWarning != "" {
					warnings, _ := out["warnings"].([]string)
					out["warnings"] = append(warnings, storeWarning)
				}
				if len(matches) == 0 {
					if note := cfg.Deps.EmptyResultBlindSpot(brainDir, resolvedBranch); note != "" && storeWarning == "" {
						out["blind_spot"] = note
					}
				}
				if err := cmdutil.WriteJSON(cmd, out); err != nil {
					return err
				}
				if len(matches) > 0 {
					recordReceipt()
				}
				return nil
			}
			if len(matches) == 0 {
				return cmdutil.WriteText(cmd, func(out io.Writer) {
					fmt.Fprintf(out, "no memories for %q on %s\n", query, resolvedBranch)
					if storeWarning != "" {
						fmt.Fprintln(out, "warning: "+storeWarning)
					}
					if note := cfg.Deps.EmptyResultBlindSpot(brainDir, resolvedBranch); note != "" && storeWarning == "" {
						fmt.Fprintln(out, note)
					}
				})
			}
			if err := cmdutil.WriteText(cmd, func(out io.Writer) {
				if storeWarning != "" {
					fmt.Fprintln(out, "warning: "+storeWarning)
				}
				if proposalsErr != nil {
					fmt.Fprintf(out, "⚠ %s\n", retrieval.FactReviewQueueUnavailableWarning)
				}
				for _, f := range matches {
					if globalIDs[f.ID] {
						// Without this a global convention is indistinguishable
						// from something this repository declared, and an agent
						// would cite it as a property of the codebase.
						PrintGlobalFactLine(out, f)
					} else {
						PrintFactLine(out, f)
					}
					if gone := drift[f.ID]; len(gone) > 0 {
						fmt.Fprintf(out, "  ⚠ stale locus (no longer in worktree): %s\n", strings.Join(gone, ", "))
					}
					if notice, ok := pendingReviews[f.ID]; ok {
						fmt.Fprintf(out, "  %s\n", retrieval.FactReviewNoticeLine(notice))
					}
				}
			}); err != nil {
				return err
			}
			recordReceipt()
			return nil
		},
	}
	cmd.Flags().BoolVar(&evidence, "evidence", false, "Experimental: return original source spans using deterministic retrieval; no model call")
	cmd.Flags().IntVar(&evidenceBytes, "evidence-bytes", 8192, "Maximum compact JSON bytes in the evidence array, including citations; enclosing metadata is separate")
	cmd.Flags().StringVar(&branch, "branch", "", "Branch to recall from (default: current branch)")
	cmd.Flags().IntVar(&limit, "k", 10, fmt.Sprintf("Maximum memories to return, or candidate sessions with --evidence (1-%d)", recallMaxK))
	cmd.Flags().BoolVar(&includeAll, "all", false, "Include superseded and retracted memories")
	cmd.Flags().StringVar(&scope, "scope", "", "Restrict to 'local' (code/subsystem) or 'cross-cutting' (preferences/workflow) memories")
	cmd.Flags().StringVar(&kind, "kind", "", "Restrict to one kind: decision|invariant|gotcha|preference|convention")
	cmd.Flags().StringVar(&locus, "locus", "", "Restrict to memories about a code locus (a path or symbol, e.g. internal/cli/facts.go or factRecord)")
	cmd.Flags().BoolVar(&noSemantic, "no-semantic", false, "Accepted for compatibility: semantic retrieval was removed, ranking is always BM25 lexical")
	cmd.Flags().BoolVar(&expand, "expand", false, "Expand the query with agent-generated retrieval terms before ranking")
	cmd.Flags().StringVar(&agent, "agent", "auto", "Agent for --expand: auto, codex, claude-code, ollama, or command")
	cmd.Flags().StringVar(&model, "model", "", "Model for codex/claude-code/ollama expand calls")
	cmd.Flags().StringArrayVar(&agentCommand, "agent-command", nil, "Agent command argv for --agent command")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	cmd.Flags().StringVar(&eligibleBefore, "eligible-before", "", "Restrict candidates to memories whose provenance is strictly before this RFC3339 cutoff")
	cmd.Flags().StringVar(&sessionDatesPath, "session-dates", "", "JSON map of session IDs to provenance timestamps for --eligible-before")
	cmd.Flags().StringArrayVar(&excludeSessionIDs, "exclude-session-id", nil, "Exclude memories anchored to this session (repeatable)")
	cmd.Flags().BoolVar(&readOnlySemanticCache, "read-only-semantic-cache", false, "Accepted for compatibility: semantic vector caches no longer exist")
	cmd.Flags().BoolVar(&noGlobal, "no-global", false, "Exclude global memories, recalling only this repository's")
	return cmd
}

// PrintGlobalFactLine renders a fact that came from the global store rather than
// this repository.
func PrintGlobalFactLine(out io.Writer, f factmerge.Record) {
	marker := ""
	switch f.Status {
	case factmerge.StatusSuperseded:
		marker = " (superseded)"
	case factmerge.StatusRetracted:
		marker = " (retracted)"
	}
	fmt.Fprintf(out, "%s %s [%s] (global)%s\n  %s\n", f.ID, factstore.KindOrInferred(f), strings.Join(f.Paths, ","), marker, f.Text)
}

// SortedGlobalFactIDs returns the ids of the surfaced facts that are global, in
// the order they were surfaced, so JSON output is stable across runs.
func SortedGlobalFactIDs(matches []factmerge.Record, globalIDs map[string]bool) []string {
	var ids []string
	for _, f := range matches {
		if globalIDs[f.ID] {
			ids = append(ids, f.ID)
		}
	}
	return ids
}

// PrintFactLine renders a fact for human-readable listings.
func PrintFactLine(out io.Writer, f factmerge.Record) {
	marker := ""
	switch f.Status {
	case factmerge.StatusSuperseded:
		marker = " (superseded)"
	case factmerge.StatusRetracted:
		marker = " (retracted)"
	}
	fmt.Fprintf(out, "%s %s [%s]%s\n  %s\n", f.ID, factstore.KindOrInferred(f), strings.Join(f.Paths, ","), marker, f.Text)
}
