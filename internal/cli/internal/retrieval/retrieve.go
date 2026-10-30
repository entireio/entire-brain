// Package retrieval is the unified retrieval layer behind the qmd-inspired
// verbs (search/vsearch/query). It ranks across the brain's text layers —
// facts, history, docs — and merges the per-source ranked lists with RRF. Each
// layer keeps its own best ranker; RRF fuses them by rank so their incomparable
// raw scores don't fight.
//
// The engine owns the read-side closure: source routing, per-layer candidate
// budgets, guard-filtered inputs, fusion, and recency weighting. The pieces
// that bind cli-private machinery — the session read-guard composition, the
// store-specific rankers, the conversation arms, and declared-index
// inspection — are injected through Deps, mirroring the brainreview seam.
package retrieval

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/docstore"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	"github.com/entireio/entire-brain/internal/cli/internal/historystore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
)

// Deps are the cli-bound seams the engine consumes without owning: guard
// composition stays with the privacy layer, rankers with their stores, and the
// conversation arms with the session-view machinery. No cli-private type
// crosses this boundary.
type Deps struct {
	// LoadSessionReadGuard builds the exclusion guard; a nil manifest defers
	// the manifest-derived transcript paths until the caller resolves one.
	LoadSessionReadGuard func(brainDir string, manifest *bmanifest.Manifest) (brainstore.SessionReadGuard, error)
	// GuardFactRecords filters a fact slice through the exclusion guard.
	GuardFactRecords func(guard brainstore.SessionReadGuard, facts []factmerge.Record) []factmerge.Record
	// GuardBlocksRecord reports whether the guard excludes a history record.
	GuardBlocksRecord func(guard brainstore.SessionReadGuard, record historystore.Record) bool
	// LoadFactProposals loads the pending fact-review queue.
	LoadFactProposals func(brainDir, branch string) ([]factmerge.Proposal, error)
	// RankFacts ranks facts against a query (BM25), highest score first.
	RankFacts func(facts []factmerge.Record, query string, limit int, includeAll bool) []factmerge.Record
	// RankHistoryLexical is the common BM25-only history path: it hydrates its
	// small result window from the fresh FTS payload table, then merges the
	// short-term tier. Legacy/stale/corrupt caches fall back through the
	// verified full JSON index and substring scorer.
	RankHistoryLexical func(brainDir string, source *bmanifest.HistorySource, kind, query string, limit int) ([]ScoredHistoryRecord, error)
	// RankGuardedHistory is the full history path for brains with live
	// tombstones: the predicate filters candidate generation, which the FTS
	// fast path cannot do.
	RankGuardedHistory func(brainDir string, source *bmanifest.HistorySource, kind, query string, limit int, pred func(historystore.Record) bool) ([]ScoredHistoryRecord, error)
	// RankDocs ranks doc chunks (FTS with the in-memory lexical fallback).
	RankDocs func(brainDir string, index docstore.Index, query string, limit int) []docstore.ScoredRecord
	// InspectDeclaredIndex reports a declared-but-unreadable derived index.
	InspectDeclaredIndex func(brainDir string, source brainstore.DeclaredIndexSource) brainstore.DeclaredIndexDefect
	// RetrieveConversation is the explicit conversation-source arm.
	RetrieveConversation func(brainDir, query string, limit int, mode Mode, opts Options) ([]Result, error)
	// RetrieveConversationMultiConcept is the multi-concept conversation arm.
	RetrieveConversationMultiConcept func(brainDir, query string, limit int, mode Mode, opts Options) ([]Result, error)
}

type Result struct {
	Source               string   `json:"source"` // fact | fact-review | history | conversation | doc | consolidation | theme | workspace_pattern | workspace_graph
	ID                   string   `json:"id"`     // prefixed, addressable by get/multi-get
	Path                 string   `json:"path,omitempty"`
	Heading              string   `json:"heading,omitempty"`
	Line                 int      `json:"line,omitempty"`
	Text                 string   `json:"text"`
	Score                float64  `json:"score,omitempty"` // omitted for unranked results (get/multi-get); RRF scores are always > 0
	VerificationRequired bool     `json:"verification_required,omitempty"`
	Caveats              []Caveat `json:"caveats,omitempty"`
	RelatedIDs           []string `json:"related_ids,omitempty"`

	// Source provenance; available fields vary by source. Fact timestamps
	// retain their distinct creation and revision meanings.
	EndLine   int    `json:"end_line,omitempty"` // inclusive 1-based source range end
	Branch    string `json:"branch,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Agent     string `json:"agent,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
	Truncated bool   `json:"truncated,omitempty"` // expanded/projected text is bounded, not complete
	// MatchedTerms lists the query tokens that actually hit this record, so a
	// weak match is diagnosable instead of opaque (conversation search only).
	MatchedTerms []string `json:"matched_terms,omitempty"`

	// Session navigation (additive; conversation source only).
	SessionRef      string             `json:"session_ref,omitempty"`
	TargetID        string             `json:"target_id,omitempty"`
	ContextBefore   int                `json:"context_before,omitempty"`
	ContextAfter    int                `json:"context_after,omitempty"`
	OmittedBefore   int                `json:"omitted_before,omitempty"`
	OmittedAfter    int                `json:"omitted_after,omitempty"`
	Turns           []ConversationTurn `json:"turns,omitempty"`
	NextTurn        int                `json:"next_turn,omitempty"`
	PacketTruncated bool               `json:"packet_truncated,omitempty"`

	// Abstract status on session outlines (additive; never a generation
	// call).
	AbstractStatus string           `json:"abstract_status,omitempty"`
	AbstractIssue  string           `json:"abstract_issue,omitempty"`
	Abstract       *SessionAbstract `json:"abstract,omitempty"`

	// Multi-concept session coverage (additive; heading session_coverage).
	Concepts          []string       `json:"concepts,omitempty"`
	ConceptMatches    []ConceptMatch `json:"concept_matches,omitempty"`
	EvidenceIDs       []string       `json:"evidence_ids,omitempty"`
	WorstRank         int            `json:"worst_rank,omitempty"`
	RankSum           int            `json:"rank_sum,omitempty"`
	Approximate       bool           `json:"approximate,omitempty"`
	ResponseTruncated bool           `json:"response_truncated,omitempty"`
}

// ScoredHistoryRecord pairs a history record with its query score for ranking.
type ScoredHistoryRecord struct {
	Record historystore.Record
	Score  int
	Order  int
}

// ConversationTurn is one bounded outline or context entry. Outline entries
// carry the request excerpt; context entries carry the expanded bounded text.
// Neither ever contains hidden reasoning or raw tool output.
type ConversationTurn struct {
	ConversationID  string   `json:"conversation_id"`
	TurnOrdinal     int      `json:"turn_ordinal"`
	CreatedAt       string   `json:"created_at,omitempty"`
	Request         string   `json:"request,omitempty"`
	Text            string   `json:"text,omitempty"`
	RangeIncomplete bool     `json:"range_incomplete,omitempty"`
	ToolNames       []string `json:"tool_names,omitempty"`
	Truncated       bool     `json:"truncated,omitempty"`
	Caveats         []Caveat `json:"caveats,omitempty"`
}

// AbstractStatement is one evidence-linked claim; the evidence ids must cite
// exchanges of the same session.
type AbstractStatement struct {
	Text        string   `json:"text"`
	EvidenceIDs []string `json:"evidence_ids"`
}

type AbstractCoverageRange struct {
	StartTurn int `json:"start_turn"`
	EndTurn   int `json:"end_turn"`
}

type SessionAbstract struct {
	SchemaVersion int       `json:"schema_version"`
	SessionRef    string    `json:"session_ref"`
	SessionDigest string    `json:"session_digest"`
	GeneratedAt   time.Time `json:"generated_at"`
	Generator     struct {
		Kind            string `json:"kind"` // local | hosted
		Provider        string `json:"provider"`
		Model           string `json:"model"`
		ContractVersion int    `json:"contract_version"`
	} `json:"generator"`
	Overview       AbstractStatement   `json:"overview"`
	Outcomes       []AbstractStatement `json:"outcomes,omitempty"`
	Decisions      []AbstractStatement `json:"decisions,omitempty"`
	Unresolved     []AbstractStatement `json:"unresolved,omitempty"`
	InputTruncated bool                `json:"input_truncated,omitempty"`
	Coverage       struct {
		TotalTurns     int                     `json:"total_turns"`
		IncludedTurns  int                     `json:"included_turns"`
		IncludedRanges []AbstractCoverageRange `json:"included_ranges,omitempty"`
	} `json:"coverage"`
}

// ConceptMatch names the best supporting exchange for one concept inside a
// matching session.
type ConceptMatch struct {
	Concept        string   `json:"concept"`
	ConversationID string   `json:"conversation_id"`
	Rank           int      `json:"rank"` // 1-based rank in the concept's own list
	MatchedTerms   []string `json:"matched_terms,omitempty"`
	Arm            string   `json:"arm"` // lexical | semantic | fused
}

type Mode int

const (
	ModeLexical Mode = iota // search
	ModeVector              // vsearch
	ModeHybrid              // query
)

// Options is the shared retrieval-options contract; extend it
// rather than widening positional signatures (cross-cutting rule of the
// conversational-memory plan).
type Options struct {
	// Source selects the layer(s) to rank: "" or "all" is the default set.
	Source string
	// Recency turns on time-weighted ranking: a soft multiplier on the fused
	// score, never a filter. Off by default because switching it on silently
	// would reorder every existing caller's results for a reason they never
	// asked about. RecencyHalfLife overrides the default; zero uses it.
	// See recency.go.
	Recency         bool
	RecencyHalfLife time.Duration
	// Now is the clock recency measures age against. Zero means time.Now;
	// tests set it so a ranking assertion is not a race with the wall clock.
	Now time.Time
	// Structured conversation-source filters (Phase 2). Zero values mean no
	// filter. After/Before bound the session time (After inclusive, Before
	// exclusive); records without a session time are excluded whenever a time
	// filter is set, so a filter can never leak an unprovable record into
	// scope. SessionID and Agent are exact (Agent case-insensitive) matches.
	// Branch filters on the captured branch. All five apply only when
	// Source == "conversation"; supplying After/Before/SessionID/Agent with
	// another source is a structured error, never silently ignored.
	After     time.Time
	Before    time.Time
	SessionID string
	Agent     string
	Branch    string
	// Concepts are the additional multi-concept queries (one to four,
	// joined with the primary query for session-scoped AND coverage). Valid
	// only with Source == "conversation".
	Concepts []string
	// IncludeAbstract adds a bounded top-level preview envelope after ranking.
	// It never changes rank and never invokes a provider.
	IncludeAbstract bool
	// GlobalFacts are facts recorded outside any repository. They are merged
	// into the fact layer before ranking and go through every filter and the
	// same scoring as repository facts, so a preference recorded once applies
	// everywhere rather than only on the CLI surface that loaded it. Callers
	// that should not see them (--no-global, ENTIRE_BRAIN_NO_GLOBAL_FACTS)
	// simply leave this nil.
	GlobalFacts []factmerge.Record
	// NoGlobalFacts is the per-request off switch, matching recall's
	// --no-global. The environment switch is checked alongside it.
	NoGlobalFacts bool
}

// HasConversationOnlyFilters reports filters that have no meaning outside the
// conversation source. Branch is excluded: it is a long-standing facts-branch
// selector on every retrieval surface and doubles as the conversation branch
// filter when that source is selected.
func (o Options) HasConversationOnlyFilters() bool {
	return !o.After.IsZero() || !o.Before.IsZero() ||
		strings.TrimSpace(o.SessionID) != "" || strings.TrimSpace(o.Agent) != "" ||
		len(o.Concepts) > 0
}

// ParseTimeFilter parses a time filter: RFC3339 or a plain
// YYYY-MM-DD day (interpreted as UTC midnight). Empty means unset.
func ParseTimeFilter(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", value); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("time filter must be RFC3339 or YYYY-MM-DD (got %q)", value)
}

// ParseSource validates a user/client-supplied source selector,
// normalizing "" to "all".
func ParseSource(value string) (string, error) {
	source := strings.ToLower(strings.TrimSpace(value))
	switch source {
	case "":
		return brainstore.RetrievalSourceAll, nil
	case brainstore.RetrievalSourceAll, brainstore.RetrievalSourceFact, brainstore.RetrievalSourceHistory, brainstore.RetrievalSourceConversation, brainstore.RetrievalSourceDoc:
		return source, nil
	default:
		return "", fmt.Errorf("source must be one of all, fact, history, conversation, doc (got %q)", value)
	}
}

// Unified ranks across facts + history + docs, lexically: BM25 for
// facts (facts_bm25.go), BM25/FTS with an in-memory fallback for history and
// docs. Semantic retrieval was removed (trail #278), so every mode ranks the
// same way; vsearch and --semantic survive only as compatibility aliases.
// Missing layers are skipped, not errors.
func Unified(deps Deps, repoDir, brainDir, branch, query string, limit int, mode Mode) ([]Result, error) {
	return UnifiedWithOptions(deps, repoDir, brainDir, branch, query, limit, mode, Options{})
}

func UnifiedWithOptions(deps Deps, repoDir, brainDir, branch, query string, limit int, mode Mode, opts Options) ([]Result, error) {
	if limit <= 0 {
		limit = 10
	}
	source := opts.Source
	if source == "" {
		source = brainstore.RetrievalSourceAll
	}
	if opts.IncludeAbstract && source != brainstore.RetrievalSourceConversation {
		return nil, fmt.Errorf(`include_abstract requires source "conversation" (got %q)`, source)
	}
	if source == brainstore.RetrievalSourceConversation {
		if opts.Recency {
			return nil, fmt.Errorf("recency is not supported for source conversation")
		}
		if len(opts.Concepts) > 0 {
			return deps.RetrieveConversationMultiConcept(brainDir, query, limit, mode, opts)
		}
		return deps.RetrieveConversation(brainDir, query, limit, mode, opts)
	}
	if opts.HasConversationOnlyFilters() {
		return nil, fmt.Errorf(`after/before/session/agent filters require source "conversation" (got %q)`, source)
	}
	includeFacts := source == brainstore.RetrievalSourceAll || source == brainstore.RetrievalSourceFact
	includeHistory := source == brainstore.RetrievalSourceAll || source == brainstore.RetrievalSourceHistory
	includeDocs := source == brainstore.RetrievalSourceAll || source == brainstore.RetrievalSourceDoc
	// Preserve the existing 2x per-layer candidate budget. The facts arm adds a
	// bounded collapse reserve before applying trust state, so unrelated
	// history/doc RRF candidates never change merely because a proposal entered
	// the review queue without converting the entire fact corpus on every query.
	candidateLimit := limit * 2
	if candidateLimit < limit { // integer overflow guard for unreasonable inputs
		candidateLimit = limit
	}
	// Exclusion guard: consulted at this retrieval boundary so a
	// tombstoned session's derived records are unreadable immediately, even
	// mid-cleanup. Empty guard (the normal case) changes nothing.
	guard, err := deps.LoadSessionReadGuard(brainDir, nil)
	if err != nil {
		return nil, err
	}
	var guardPred func(historystore.Record) bool
	if !guard.Empty() {
		// Path-based exclusion needs the manifest: blocksFactAnchor falls back
		// to the transcript path when a record lost its session id, and that
		// map is populated only from the manifest. Resolve it HERE, before the
		// facts arm filters below. This used to happen inside the history arm,
		// after GuardFactRecords had already run, so an excluded session's fact
		// whose only anchor had no session id survived on query/search/vsearch
		// while brief, dash, handoff, viz, and the pattern corpus all hid it.
		// Only tombstoned brains pay the manifest read.
		manifest, err := bmanifest.Load(brainDir)
		if err != nil {
			return nil, err
		}
		guard, err = deps.LoadSessionReadGuard(brainDir, manifest)
		if err != nil {
			return nil, err
		}
		guardPred = func(r historystore.Record) bool { return !deps.GuardBlocksRecord(guard, r) }
	}
	// factstore.Load surfaces corrupt NDJSON as a hard error; propagate it rather than
	// presenting a broken store as "no results".
	var all []factmerge.Record
	var globalFactIDs map[string]bool
	var active []factmerge.Record
	var proposals []factmerge.Proposal
	var proposalsErr error
	if includeFacts {
		var err error
		all, err = factstore.Load(brainDir, branch)
		if err != nil {
			return nil, err
		}
		if len(opts.GlobalFacts) > 0 {
			all, globalFactIDs = MergeGlobalFacts(all, opts.GlobalFacts)
		}
		all = deps.GuardFactRecords(guard, all)
		active = make([]factmerge.Record, 0, len(all))
		for _, f := range all {
			if f.Status == factmerge.StatusActive {
				active = append(active, f)
			}
		}
		proposals, proposalsErr = deps.LoadFactProposals(brainDir, branch)
	}

	var lists [][]Result

	// Facts.
	if includeFacts && len(active) > 0 {
		factLimit := min(len(active), candidateLimit)
		var reviewGroups []FactReviewGroup
		if proposalsErr == nil {
			reviewGroups = BuildFactReviewGroups(FactsEligibleForLocusDrift(active, globalFactIDs), proposals)
			factLimit = guardedFactCandidateLimitForGroups(len(active), reviewGroups, candidateLimit)
		}
		factResults := FactsToUnified(deps.RankFacts(active, query, factLimit, false))
		// Repository proposals and locus checks apply only to repository facts.
		localFacts := FactsEligibleForLocusDrift(all, globalFactIDs)
		for i := range factResults {
			if globalFactIDs[factResults[i].ID] {
				factResults[i].Heading += " (global)"
			}
		}
		if proposalsErr == nil {
			factResults = guardUnifiedFactResultsWithGroups(repoDir, localFacts, reviewGroups, factResults, candidateLimit)
		} else {
			factResults = guardUnifiedFactResultsWithGroups(repoDir, localFacts, nil, factResults, candidateLimit)
			factResults = AnnotateProposalStateUnavailable(factResults)
		}
		if len(factResults) > 0 {
			lists = append(lists, factResults)
		}
	}

	// History — BM25 (lexical / hybrid), plus the gated semantic arm (hybrid /
	// vector; see history_vec.go). The FTS index is an optimization, never
	// load-bearing (see history_fts.go): on a build/open/query failure fall
	// back to the in-memory substring scorer. But a corrupt manifest, or a history
	// index the manifest declares yet is missing/unreadable, is a real storage
	// problem — surface it rather than returning silently-incomplete results. A
	// brain with no history source (nil) is legitimately skipped.
	if includeHistory {
		// Self-echo filtering happens after lexical ranking. Over-fetch a bounded
		// reserve so discarded `entire brain ...` tool-call echoes do not leave
		// the history arm artificially short.
		historyCandidateLimit := candidateLimit
		if candidateLimit <= int(^uint(0)>>1)/3 {
			historyCandidateLimit = candidateLimit * 3
		}
		manifest, err := bmanifest.Load(brainDir)
		if err != nil {
			return nil, err
		}
		if manifest.Sources != nil && manifest.Sources.History != nil {
			source := manifest.Sources.History
			// The FTS-payload fast path hydrates its result window without
			// loading index.json, but it cannot apply an exclusion predicate
			// during candidate generation and does not resolve the two-tier
			// duplicate winner. A brain with live tombstones therefore takes
			// the full path below; an unguarded brain, the common
			// case, keeps the fast path.
			if guardPred == nil {
				scored, rankErr := deps.RankHistoryLexical(brainDir, source, "history", query, historyCandidateLimit)
				if rankErr != nil {
					return nil, fmt.Errorf("load history index: %w", rankErr)
				}
				scored = filterHistoryRetrievalSelfEchoes(scored, query)
				if len(scored) > candidateLimit {
					scored = scored[:candidateLimit]
				}
				if len(scored) > 0 {
					lists = append(lists, HistoryToUnified(scored))
				}
			} else {
				scored, loadErr := deps.RankGuardedHistory(brainDir, source, "history", query, historyCandidateLimit, guardPred)
				if loadErr != nil {
					return nil, fmt.Errorf("load history index: %w", loadErr)
				}
				scored = filterHistoryRetrievalSelfEchoes(scored, query)
				if len(scored) > candidateLimit {
					scored = scored[:candidateLimit]
				}
				if len(scored) > 0 {
					lists = append(lists, HistoryToUnified(scored))
				}
			}
		}
	}

	// Docs — lexical (search/query) and/or vector (vsearch/query). In hybrid mode
	// docs join BOTH arms, mirroring facts. A never-built doc index is skipped; a
	// corrupt/unreadable one is a real storage problem and is surfaced.
	var docIdx docstore.Index
	var derr error
	if includeDocs {
		docIdx, derr = docstore.Load(brainDir)
	} else {
		derr = os.ErrNotExist
	}
	switch {
	case derr == nil && len(docIdx.Records) > 0:
		scored := deps.RankDocs(brainDir, docIdx, query, candidateLimit)
		if len(scored) > 0 {
			lists = append(lists, DocsToUnified(scored))
		}
	case includeDocs && derr != nil && os.IsNotExist(derr):
		// A doc index the manifest DECLARES and the store no longer holds is
		// data loss, not an un-built layer, and skipping it silently is how
		// `vsearch` answered "no results for \"Greet\"" with exit 0 over a brain
		// that had returned six doc hits for that exact query minutes earlier —
		// while `search`, which reaches history first, exited 1. Two retrieval
		// verbs disagreeing about the same broken brain is itself the defect, so
		// both fail here, and for the reason the comment above already gave: a
		// real storage problem is surfaced.
		//
		// A brain whose manifest declares NO docs source keeps the silent skip.
		// That is a brain that has not been refreshed, not a broken one.
		if defect := deps.InspectDeclaredIndex(brainDir, brainstore.DeclaredIndexDocs); !defect.OK() {
			return nil, defect
		}
	case derr != nil && !os.IsNotExist(derr):
		return nil, fmt.Errorf("load doc index: %w", derr)
	}

	fusionLimit := limit
	if opts.Recency {
		// Retain every candidate through weighting: a fresh result below the
		// raw RRF cutoff may belong in the final page. The sum is an upper
		// bound on the number of distinct results after fusion.
		fusionLimit = 0
		for _, list := range lists {
			fusionLimit += len(list)
		}
	}
	fused := RRFMergeUnified(lists, fusionLimit)
	if opts.Recency {
		now := opts.Now
		if now.IsZero() {
			now = time.Now()
		}
		halfLife := opts.RecencyHalfLife
		if halfLife <= 0 {
			halfLife = defaultRecencyHalfLife
		}
		// Inspect the full candidate pool: weighting may demote every dated
		// candidate out of the final page, which is still a successful use.
		if dated := applyRecency(fused, now, halfLife); dated == 0 {
			for i := range fused {
				fused[i].Caveats = append(fused[i].Caveats, Caveat{
					Kind:    "recency_unavailable",
					Message: "Recency weighting was requested, but none of the retrieved candidates has a usable timestamp; scores are unchanged.",
				})
			}
		}
	}
	if len(fused) > limit {
		fused = fused[:limit]
	}
	return fused, nil
}

func filterHistoryRetrievalSelfEchoes(scored []ScoredHistoryRecord, query string) []ScoredHistoryRecord {
	needle := strings.ToLower(strings.TrimSpace(query))
	if needle == "" {
		return scored
	}
	out := scored[:0]
	for _, item := range scored {
		kind := strings.ToLower(strings.TrimSpace(item.Record.Kind))
		summary := strings.ToLower(item.Record.Summary)
		brainInvocation := strings.Contains(summary, "entire brain search") ||
			strings.Contains(summary, "entire brain query") ||
			strings.Contains(summary, "entire brain brief") ||
			strings.Contains(summary, "brain_search") ||
			strings.Contains(summary, "brain_query") ||
			strings.Contains(summary, "brain_brief")
		if kind == "tool_call" && brainInvocation && strings.Contains(summary, needle) {
			continue
		}
		out = append(out, item)
	}
	return out
}

func FactsToUnified(facts []factmerge.Record) []Result {
	// fact ids are already prefixed "fact:" (factmerge.RecordID); history likewise.
	out := make([]Result, len(facts))
	for i, f := range facts {
		out[i] = Result{Source: "fact", ID: f.ID, Path: strings.Join(f.Paths, ","), Heading: factstore.KindOrInferred(f), Text: f.Text}
		if !f.CreatedAt.IsZero() {
			out[i].CreatedAt = f.CreatedAt.UTC().Format(time.RFC3339)
		}
		if !f.UpdatedAt.IsZero() {
			out[i].UpdatedAt = f.UpdatedAt.UTC().Format(time.RFC3339)
		}
	}
	return out
}

func HistoryToUnified(scored []ScoredHistoryRecord) []Result {
	out := make([]Result, len(scored))
	for i, s := range scored {
		out[i] = Result{Source: "history", ID: s.Record.ID, Path: s.Record.Path, Line: s.Record.Line, Heading: s.Record.Kind, Text: s.Record.Summary}
		// Classic history records carry no time. Leaving CreatedAt empty is
		// the honest answer: recency treats undated records as neutral, not old.
		out[i].CreatedAt = s.Record.CreatedAt
	}
	return out
}

func DocToUnified(r docstore.Record) Result {
	result := Result{Source: "doc", ID: "doc:" + r.ID, Path: r.Path, Line: r.Line, Heading: r.Heading, Text: r.Text}
	if r.Historical {
		result.VerificationRequired = true
		result.Caveats = []Caveat{{
			Kind:    CaveatHistoricalDocument,
			Message: "This document is explicitly historical or superseded; prefer current operational documentation.",
			Paths:   []string{r.Path},
			Action:  "Verify against the current README and active implementation before relying on it.",
		}}
	}
	return result
}

func DocsToUnified(scored []docstore.ScoredRecord) []Result {
	out := make([]Result, len(scored))
	for i, s := range scored {
		out[i] = DocToUnified(s.Record)
	}
	return out
}

// RRFK is the Reciprocal Rank Fusion constant (canonical 60): survives the
// semantic removal because lexical two-tier history merging still rank-fuses.
const RRFK = 60.0

func RRFMergeUnified(lists [][]Result, limit int) []Result {
	fused := map[string]float64{}
	rec := map[string]Result{}
	for _, list := range lists {
		for rank, r := range list {
			fused[r.ID] += 1.0 / (RRFK + float64(rank+1))
			if _, ok := rec[r.ID]; !ok {
				rec[r.ID] = r
			}
		}
	}
	out := make([]Result, 0, len(rec))
	for id, r := range rec {
		r.Score = fused[id]
		out = append(out, r)
	}
	sortUnifiedByScore(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// sortUnifiedByScore orders by score descending, breaking ties on id so the
// order is stable across runs. Recency reweighting re-sorts with the same
// comparison, so a reweighted list and a fused list are ordered by one rule.
func sortUnifiedByScore(out []Result) {
	sort.Slice(out, func(a, b int) bool {
		if out[a].Score != out[b].Score {
			return out[a].Score > out[b].Score
		}
		return out[a].ID < out[b].ID
	})
}

// MergeGlobalFacts appends global facts to a repository's, dropping any whose
// id a repository fact already has.
//
// The repository's copy wins on a collision. Fact ids are content-derived, so a
// collision means the same statement is recorded in both places; the local one
// is the more specific claim and carries that repo's provenance, which is the
// evidence somebody would check.
func MergeGlobalFacts(repoFacts, global []factmerge.Record) ([]factmerge.Record, map[string]bool) {
	// Only a live repository fact shadows. The precedence above is justified by
	// the local copy being the more specific claim carrying checkable
	// provenance; a retracted or superseded fact makes no claim, so it has
	// nothing to shadow with. Letting it shadow anyway would mean retracting a
	// local duplicate silently removed the global statement from this one repo.
	seen := make(map[string]bool, len(repoFacts))
	for _, fact := range repoFacts {
		switch fact.Status {
		case "", factmerge.StatusActive:
			seen[fact.ID] = true
		}
	}
	merged := repoFacts
	globalIDs := map[string]bool{}
	for _, fact := range global {
		if seen[fact.ID] {
			continue
		}
		// A retracted or superseded global fact is filtered by the same code
		// that filters repository ones; nothing special happens here.
		merged = append(merged, fact)
		globalIDs[fact.ID] = true
	}
	return merged, globalIDs
}

// FactsEligibleForLocusDrift drops global facts before the drift check.
//
// Locus drift asks whether a fact's code locus still exists in THIS worktree.
// A global fact was never scoped to this repository, so the answer is
// meaningless and always alarming: any path it mentions would be reported as a
// stale locus in every repository except the one it was written in. Global
// facts are excluded here for the same reason they are kept out of this
// repository's semantic cache.
func FactsEligibleForLocusDrift(matches []factmerge.Record, globalIDs map[string]bool) []factmerge.Record {
	if len(globalIDs) == 0 {
		return matches
	}
	eligible := make([]factmerge.Record, 0, len(matches))
	for _, fact := range matches {
		if !globalIDs[fact.ID] {
			eligible = append(eligible, fact)
		}
	}
	return eligible
}
