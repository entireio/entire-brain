package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

const recallEngineBM25Lexical = "bm25_lexical"

// recallRetrievalEngine is emitted with recall --json. Semantic retrieval was
// removed (trail #278: BM25 matched or beat the embedder stack end-to-end);
// the semantic/vector fields are retained in the schema and report inert
// values so existing consumers keep parsing.
type recallRetrievalEngine struct {
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

func recallRetrievalIdentity() recallRetrievalEngine {
	return recallRetrievalEngine{
		SchemaVersion:    1,
		EffectiveEngine:  recallEngineBM25Lexical,
		IdentityVerified: true,
		BM25Enabled:      true,
		BM25Applied:      true,
	}
}

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
		Short: "Retrieve durable facts matching a query for the current branch",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			query := ""
			if len(args) == 1 {
				query = args[0]
			}
			if evidence {
				for _, flag := range []string{"all", "scope", "kind", "locus", "expand", "agent", "model", "agent-command", "eligible-before", "session-dates", "exclude-session-id", "read-only-semantic-cache"} {
					if cmd.Flags().Changed(flag) {
						return fmt.Errorf("--%s is not supported with --evidence; evidence recall is deterministic and does not apply fact filters", flag)
					}
				}
			} else if cmd.Flags().Changed("evidence-bytes") {
				return fmt.Errorf("--evidence-bytes requires --evidence")
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
			if evidence {
				return runRecallEvidence(cmd, brainDir, resolvedBranch, query, limit, evidenceBytes, jsonOut)
			}
			allFacts, err := loadFacts(brainDir, resolvedBranch)
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
			if globalFactsEnabled(noGlobal) {
				globalFacts, globalErr := loadGlobalFacts(opts.Env)
				if globalErr != nil {
					// A damaged global store must not take recall down with it;
					// the repository's own facts are the answer to most queries.
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: global facts unavailable: %v\n", globalErr)
				} else {
					allFacts, globalIDs = mergeGlobalFacts(allFacts, globalFacts)
				}
			}
			var storeWarning string
			manifest, manifestErr := loadBrainManifest(brainDir)
			if manifestErr != nil {
				storeWarning = "fact source manifest unavailable: " + manifestErr.Error()
			} else if manifest != nil && manifest.Sources != nil {
				storeWarning = missingFactStoreWarningForBranch(brainDir, manifest.Sources.Facts, resolvedBranch)
			}
			candidateFacts := allFacts
			var eligibility *factEligibilityAudit
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
			facts := filterFactsByLocus(filterFactsByKind(filterFactsByScope(candidateFacts, scope), kind), locus)
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
				expandArgs = injectAgentModel(expandArgs, resolved, model)
				exp, expErr := expandQuery(cmd.Context(), defaultDistillAgentRunner(resolved), expandArgs, repoDir, query, loadExpansionCache(""))
				if expErr != nil {
					return fmt.Errorf("expand query: %w", expErr)
				}
				effectiveQuery = expandedQuery(query, exp)
			}
			// --no-semantic and --read-only-semantic-cache are vacuously
			// satisfied since the semantic arm was removed (trail #278):
			// ranking is always lexical and no vector cache exists to write.
			_, _ = noSemantic, readOnlySemanticCache
			matches := rankFacts(facts, effectiveQuery, limit, includeAll)
			// Locus drift (Phase 2 item 4): flag surfaced facts whose code
			// locus left the worktree, so the agent knows which to re-verify.
			drift := factsLocusDrift(repoDir, factsEligibleForLocusDrift(matches, globalIDs))
			// Live trust state: a surfaced fact with a pending merge/supersede
			// proposal is annotated (not collapsed — recall keeps its record
			// shape), matching the guard the unified query/search/get path
			// applies. Groups are built from the unfiltered branch set so
			// scope/kind/locus filters cannot hide a pending relationship.
			// Nothing surfaced means no annotation and no queue warning, so skip
			// the proposal read entirely on the empty-recall path.
			var proposals []factProposal
			var proposalsErr error
			var pendingReviews map[string]factReviewNotice
			if len(matches) > 0 {
				proposals, proposalsErr = loadFactProposals(brainDir, resolvedBranch)
				if proposalsErr == nil {
					localFacts := factsEligibleForLocusDrift(allFacts, globalIDs)
					pendingReviews = factsPendingReview(localFacts, proposals, matches)
				}
			}
			recordReceipt := func() {
				recordServedFacts(cmd.ErrOrStderr(), vitalityNow(opts), brainDir, resolvedBranch, "recall",
					vitalityHead(cmd.Context(), opts.Runner, repoDir), query, factRecordIDs(matches))
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
				if ids := sortedGlobalFactIDs(matches, globalIDs); len(ids) > 0 {
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
					out["warnings"] = []string{factReviewQueueUnavailableWarning}
				}
				if storeWarning != "" {
					warnings, _ := out["warnings"].([]string)
					out["warnings"] = append(warnings, storeWarning)
				}
				if len(matches) == 0 {
					if note := emptyResultBlindSpotOnBranch(brainDir, resolvedBranch); note != "" && storeWarning == "" {
						out["blind_spot"] = note
					}
				}
				if err := writeJSON(cmd, out); err != nil {
					return err
				}
				if len(matches) > 0 {
					recordReceipt()
				}
				return nil
			}
			if len(matches) == 0 {
				return writeText(cmd, func(out io.Writer) {
					fmt.Fprintf(out, "no facts for %q on %s\n", query, resolvedBranch)
					if storeWarning != "" {
						fmt.Fprintln(out, "warning: "+storeWarning)
					}
					if note := emptyResultBlindSpotOnBranch(brainDir, resolvedBranch); note != "" && storeWarning == "" {
						fmt.Fprintln(out, note)
					}
				})
			}
			if err := writeText(cmd, func(out io.Writer) {
				if storeWarning != "" {
					fmt.Fprintln(out, "warning: "+storeWarning)
				}
				if proposalsErr != nil {
					fmt.Fprintf(out, "⚠ %s\n", factReviewQueueUnavailableWarning)
				}
				for _, f := range matches {
					if globalIDs[f.ID] {
						// Without this a global convention is indistinguishable
						// from something this repository declared, and an agent
						// would cite it as a property of the codebase.
						printGlobalFactLine(out, f)
					} else {
						printFactLine(out, f)
					}
					if gone := drift[f.ID]; len(gone) > 0 {
						fmt.Fprintf(out, "  ⚠ stale locus (no longer in worktree): %s\n", strings.Join(gone, ", "))
					}
					if notice, ok := pendingReviews[f.ID]; ok {
						fmt.Fprintf(out, "  %s\n", factReviewNoticeLine(notice))
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
	cmd.Flags().IntVar(&limit, "k", 10, "Maximum facts to return, or candidate sessions with --evidence (1-128)")
	cmd.Flags().BoolVar(&includeAll, "all", false, "Include superseded and retracted facts")
	cmd.Flags().StringVar(&scope, "scope", "", "Restrict to 'local' (code/subsystem) or 'cross-cutting' (preferences/workflow) facts")
	cmd.Flags().StringVar(&kind, "kind", "", "Restrict to one kind: decision|invariant|gotcha|preference|convention")
	cmd.Flags().StringVar(&locus, "locus", "", "Restrict to facts about a code locus (a path or symbol, e.g. internal/cli/facts.go or factRecord)")
	cmd.Flags().BoolVar(&noSemantic, "no-semantic", false, "Accepted for compatibility: semantic retrieval was removed, ranking is always BM25 lexical")
	cmd.Flags().BoolVar(&expand, "expand", false, "Expand the query with agent-generated retrieval terms before ranking")
	cmd.Flags().StringVar(&agent, "agent", "auto", "Agent for --expand: auto, codex, claude-code, ollama, or command")
	cmd.Flags().StringVar(&model, "model", "", "Model for codex/claude-code/ollama expand calls")
	cmd.Flags().StringArrayVar(&agentCommand, "agent-command", nil, "Agent command argv for --agent command")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	cmd.Flags().StringVar(&eligibleBefore, "eligible-before", "", "Restrict candidates to facts whose provenance is strictly before this RFC3339 cutoff")
	cmd.Flags().StringVar(&sessionDatesPath, "session-dates", "", "JSON map of session IDs to provenance timestamps for --eligible-before")
	cmd.Flags().StringArrayVar(&excludeSessionIDs, "exclude-session-id", nil, "Exclude facts anchored to this session (repeatable)")
	cmd.Flags().BoolVar(&readOnlySemanticCache, "read-only-semantic-cache", false, "Accepted for compatibility: semantic vector caches no longer exist")
	cmd.Flags().BoolVar(&noGlobal, "no-global", false, "Exclude global facts, recalling only this repository's")
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

// factsEligibleForLocusDrift drops global facts before the drift check.
//
// Locus drift asks whether a fact's code locus still exists in THIS worktree.
// A global fact was never scoped to this repository, so the answer is
// meaningless and always alarming: any path it mentions would be reported as a
// stale locus in every repository except the one it was written in. Global
// facts are excluded here for the same reason they are kept out of this
// repository's semantic cache.
func factsEligibleForLocusDrift(matches []factRecord, globalIDs map[string]bool) []factRecord {
	if len(globalIDs) == 0 {
		return matches
	}
	eligible := make([]factRecord, 0, len(matches))
	for _, fact := range matches {
		if !globalIDs[fact.ID] {
			eligible = append(eligible, fact)
		}
	}
	return eligible
}

// printGlobalFactLine renders a fact that came from the global store rather than
// this repository.
func printGlobalFactLine(out io.Writer, f factRecord) {
	marker := ""
	switch f.Status {
	case factStatusSuperseded:
		marker = " (superseded)"
	case factStatusRetracted:
		marker = " (retracted)"
	}
	fmt.Fprintf(out, "%s %s [%s] (global)%s\n  %s\n", f.ID, factKindOrInferred(f), strings.Join(f.Paths, ","), marker, f.Text)
}

// sortedGlobalFactIDs returns the ids of the surfaced facts that are global, in
// the order they were surfaced, so JSON output is stable across runs.
func sortedGlobalFactIDs(matches []factRecord, globalIDs map[string]bool) []string {
	var ids []string
	for _, f := range matches {
		if globalIDs[f.ID] {
			ids = append(ids, f.ID)
		}
	}
	return ids
}

// printFactLine renders a fact for human-readable listings.
func printFactLine(out io.Writer, f factRecord) {
	marker := ""
	switch f.Status {
	case factStatusSuperseded:
		marker = " (superseded)"
	case factStatusRetracted:
		marker = " (retracted)"
	}
	fmt.Fprintf(out, "%s %s [%s]%s\n  %s\n", f.ID, factKindOrInferred(f), strings.Join(f.Paths, ","), marker, f.Text)
}
