// payload.go holds the response-payload shapes and projections shared by the
// retrieval command surface (internal/cli/command/retrieve) and the cli-side
// transport/workspace assembly: the compact locator projection, the optional
// transport extras, and the shared result renderers. Moved from package cli
// (COR-2268); JSON wire shapes are unchanged.
package retrieval

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/semstore"
)

const retrievalExcerptBytes = 600

// ConversationConceptsMaxTotal bounds primary query plus extra concepts.
const ConversationConceptsMaxTotal = 5

// ConversationConceptResultMax is shared by the core, CLI, workspace,
// transports. It bounds candidate/result allocations before any
// ranking work starts; transport byte budgets are enforced separately on
// the exact serialized response shape.
const ConversationConceptResultMax = 50

func ValidateRetrievalLimit(limit int) error {
	if limit < 1 || limit > ConversationConceptResultMax {
		return fmt.Errorf("limit must be between 1 and %d (got %d)", ConversationConceptResultMax, limit)
	}
	return nil
}

type CompactResult struct {
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
	Text                 string   `json:"text"`
	Excerpt              string   `json:"excerpt,omitempty"`
	Score                float64  `json:"score,omitempty"`
	VerificationRequired bool     `json:"verification_required,omitempty"`
	Caveats              []Caveat `json:"caveats,omitempty"`
	RelatedIDs           []string `json:"related_ids,omitempty"`

	// Source provenance; available fields vary by source.
	EndLine      int      `json:"end_line,omitempty"`
	Branch       string   `json:"branch,omitempty"`
	SessionID    string   `json:"session_id,omitempty"`
	Agent        string   `json:"agent,omitempty"`
	CreatedAt    string   `json:"created_at,omitempty"`
	UpdatedAt    string   `json:"updated_at,omitempty"`
	Truncated    bool     `json:"truncated,omitempty"`
	MatchedTerms []string `json:"matched_terms,omitempty"`
	// SessionRef names the virtual session so a caller can fetch the
	// outline or adjacent context.
	SessionRef string `json:"session_ref,omitempty"`
	// Multi-concept session coverage (additive).
	Concepts          []string       `json:"concepts,omitempty"`
	ConceptMatches    []ConceptMatch `json:"concept_matches,omitempty"`
	EvidenceIDs       []string       `json:"evidence_ids,omitempty"`
	WorstRank         int            `json:"worst_rank,omitempty"`
	RankSum           int            `json:"rank_sum,omitempty"`
	Approximate       bool           `json:"approximate,omitempty"`
	ResponseTruncated bool           `json:"response_truncated,omitempty"`
}

func CompactResults(results []Result, query string) []CompactResult {
	if results == nil {
		return []CompactResult{}
	}
	out := make([]CompactResult, len(results))
	for i, result := range results {
		out[i] = CompactResult{
			Source: result.Source, ID: result.ID, Path: result.Path, Heading: result.Heading, Line: result.Line,
			Text: result.Text, Excerpt: distinctRetrievalExcerpt(result.Text, query), Score: result.Score,
			VerificationRequired: result.VerificationRequired, Caveats: result.Caveats, RelatedIDs: result.RelatedIDs,
			EndLine: result.EndLine, Branch: result.Branch, SessionID: result.SessionID,
			Agent: result.Agent, CreatedAt: result.CreatedAt, UpdatedAt: result.UpdatedAt, Truncated: result.Truncated,
			MatchedTerms: result.MatchedTerms, SessionRef: result.SessionRef,
			Concepts: result.Concepts, ConceptMatches: result.ConceptMatches,
			EvidenceIDs: result.EvidenceIDs, WorstRank: result.WorstRank, RankSum: result.RankSum,
			Approximate: result.Approximate, ResponseTruncated: result.ResponseTruncated,
		}
	}
	return out
}

// ResultExcerpt centers a compact result on the densest query-term
// window. Ranked retrieval is a locator surface; get/multi-get own full bodies.
// distinctRetrievalExcerpt returns the bounded excerpt only when it adds
// information over Text; an identical projection is omitted.
func distinctRetrievalExcerpt(text, query string) string {
	excerpt := ResultExcerpt(text, query, retrievalExcerptBytes)
	if excerpt == strings.Join(strings.Fields(text), " ") {
		return ""
	}
	return excerpt
}

func ResultExcerpt(value, query string, maxBytes int) string {
	text := strings.Join(strings.Fields(value), " ")
	if maxBytes <= 0 || text == "" {
		return ""
	}
	if len(text) <= maxBytes {
		return text
	}
	lower, originalOffsets := foldedTextOffsets(text)
	terms := semstore.QueryTokens(query)
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

// ResultLocation renders a result's source anchor, including the
// inclusive end line for range-bearing results (conversation exchanges).
func ResultLocation(r Result) string {
	switch {
	case r.Line > 0 && r.EndLine > r.Line:
		return fmt.Sprintf("%s:%d-%d", r.Path, r.Line, r.EndLine)
	case r.Line > 0:
		return fmt.Sprintf("%s:%d", r.Path, r.Line)
	default:
		return r.Path
	}
}

func PrintResultCaveats(out io.Writer, result Result) {
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

type BriefAction struct {
	Kind     string `json:"kind,omitempty"`
	File     string `json:"file,omitempty"`
	Symbol   string `json:"symbol,omitempty"`
	Action   string `json:"action"`
	Evidence string `json:"evidence,omitempty"`
}

type TaskHints struct {
	LikelyEditFiles []string
	LikelyTestFiles []string
	ActionChecklist []BriefAction
}

type SessionAbstractPreview struct {
	SessionRef     string             `json:"session_ref"`
	AbstractStatus string             `json:"abstract_status"`
	AbstractIssue  string             `json:"abstract_issue,omitempty"`
	Overview       *AbstractStatement `json:"overview,omitempty"`
	InputTruncated bool               `json:"input_truncated,omitempty"`
}

type RelatedPatternRef struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	Title string `json:"title"`
}

// TransportExtras is the already-bounded, optional material carried
// beside ranked rows. The final response budget is still measured against the
// complete serialized shape: these fields, query/branch, indentation, and an
// envelope all count.
type TransportExtras struct {
	AbstractPreviews          []SessionAbstractPreview
	AbstractPreviewsTruncated bool
	Hints                     TaskHints
	Related                   []RelatedPatternRef
	BlindSpot                 string
}

// GetOptions is the navigation contract for get: context counts for a
// conversation: target, cursor and limit for a conversation-session: target.
// Options are type-specific and never silently ignored.
type GetOptions struct {
	ContextBefore int
	ContextAfter  int
	AfterTurn     int
	OutlineLimit  int
	// set marks which options the caller supplied explicitly.
	ContextSet bool
	OutlineSet bool
	// GlobalFacts are facts from the global store, so an id that query or
	// search returned can be fetched back by get. Nil means the caller did not
	// offer them and get sees only this repository's facts, as before.
	GlobalFacts []factmerge.Record
}

func (o GetOptions) Navigation() bool { return o.ContextSet || o.OutlineSet }
