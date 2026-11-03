package retrieval

import (
	"sort"

	"github.com/entireio/entire-brain/factmerge"
)

const (
	CaveatStaleLocus              = "stale_locus"
	CaveatCurrentCodeUnavailable  = "current_code_unavailable"
	CaveatHistoricalDocument      = "historical_document"
	CaveatHistoricalConversation  = "historical_conversation"
	CaveatConversationSourceStale = "conversation_source_stale"
	// Distinct expansion failure states: a too-large or unreadable
	// source is not the same contract failure as a changed digest.
	CaveatConversationSourceTooLarge   = "conversation_source_too_large"
	CaveatConversationSourceUnreadable = "conversation_source_unreadable"
)

// Caveat is a machine-readable reason an agent must verify a memory
// result against current evidence before relying on it.
type Caveat struct {
	Kind       string   `json:"kind"`
	Message    string   `json:"message"`
	Paths      []string `json:"paths,omitempty"`
	ReviewID   string   `json:"review_id,omitempty"`
	Action     string   `json:"action,omitempty"`
	Confidence float64  `json:"confidence,omitempty"`
}

// AnnotateUnifiedFactResults annotates ranked fact results with current-code
// locus drift and enforces the caller's limit. (The pending-review collapse
// that used to live here left with the fact-proposal framework.)
func AnnotateUnifiedFactResults(repoDir string, facts []factmerge.Record, ranked []Result, limit int) []Result {
	if limit <= 0 || len(ranked) == 0 {
		return ranked
	}
	factByID := make(map[string]factmerge.Record, len(facts))
	for _, fact := range facts {
		factByID[fact.ID] = fact
	}
	out := make([]Result, 0, min(limit, len(ranked)))
	for _, result := range ranked {
		if result.Source == "fact" {
			if fact, ok := factByID[result.ID]; ok {
				out = append(out, AnnotateFactLocusTrust(repoDir, fact, result))
			} else {
				out = append(out, result)
			}
		} else {
			out = append(out, result)
		}
		if len(out) >= limit {
			break
		}
	}
	return out
}

func AnnotateCurrentCodeUnavailable(results []Result) []Result {
	return annotateFactResultCaveat(results, Caveat{
		Kind:    CaveatCurrentCodeUnavailable,
		Message: "The repository worktree could not be safely resolved, so current-code locus verification was not performed.",
	})
}

func annotateFactResultCaveat(results []Result, caveat Caveat) []Result {
	for i := range results {
		if results[i].Source != "fact" {
			continue
		}
		results[i].VerificationRequired = true
		results[i].Caveats = append(results[i].Caveats, caveat)
	}
	return results
}

func AnnotateFactLocusTrust(repoDir string, fact factmerge.Record, result Result) Result {
	drift := FactLocusDrift(repoDir, fact)
	if len(drift) == 0 {
		return result
	}
	result.VerificationRequired = true
	result.Caveats = append(result.Caveats, staleLocusCaveat(uniqueSortedStrings(drift)))
	return result
}

func staleLocusCaveat(paths []string) Caveat {
	return Caveat{
		Kind:    CaveatStaleLocus,
		Message: "One or more referenced code files are absent from the current worktree; verify whether the fact was invalidated or moved.",
		Paths:   paths,
	}
}

func sortedStringSet(values map[string]struct{}) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func uniqueSortedStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	sort.Strings(values)
	out := values[:0]
	for _, value := range values {
		if len(out) == 0 || out[len(out)-1] != value {
			out = append(out, value)
		}
	}
	return out
}
