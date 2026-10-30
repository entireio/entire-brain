package cli

import "github.com/entireio/entire-brain/internal/cli/internal/retrieval"

// Type aliases for the unified retrieval engine extracted to retrieval:
// struct identity and JSON wire shapes are unchanged, the cli call sites keep
// their old lowercase names.
type (
	unifiedResult         = retrieval.Result
	retrievalMode         = retrieval.Mode
	retrievalOptions      = retrieval.Options
	retrievalCaveat       = retrieval.Caveat
	factReviewGroup       = retrieval.FactReviewGroup
	factReviewNotice      = retrieval.FactReviewNotice
	scoredHistoryRecord   = retrieval.ScoredHistoryRecord
	conversationTurn      = retrieval.ConversationTurn
	sessionAbstract       = retrieval.SessionAbstract
	abstractStatement     = retrieval.AbstractStatement
	abstractCoverageRange = retrieval.AbstractCoverageRange
	conceptMatch          = retrieval.ConceptMatch
)

// Payload shapes shared with the extracted retrieve command surface
// (internal/cli/command/retrieve); same alias contract as above.
type (
	compactUnifiedResult     = retrieval.CompactResult
	retrievalTaskHints       = retrieval.TaskHints
	brainBriefAction         = retrieval.BriefAction
	sessionAbstractPreview   = retrieval.SessionAbstractPreview
	relatedPatternRef        = retrieval.RelatedPatternRef
	retrievalTransportExtras = retrieval.TransportExtras
	getOptions               = retrieval.GetOptions
)
