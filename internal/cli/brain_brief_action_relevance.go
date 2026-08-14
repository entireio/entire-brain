package cli

import (
	"strings"
	"unicode/utf8"
)

const brainBriefActionChecklistGuidance = "Treat action_checklist as the first-pass current-code inventory; edit listed files first, and broaden only when the checklist is missing, ambiguous, or validation fails."

type brainBriefActionFamily uint8

const (
	brainBriefActionFamilyLimit brainBriefActionFamily = 1 << iota
	brainBriefActionFamilyMetadata
	brainBriefActionFamilyPreviousResponse
)

// These terms describe the generic task/retrieval surface or the action-family
// trigger itself. They may contribute to the overlap count, but cannot by
// themselves prove that a history excerpt is about the same code concern.
var brainBriefActionGenericOverlapTerms = map[string]struct{}{
	"action": {}, "api": {}, "behavior": {}, "brain": {}, "brief": {},
	"cli": {}, "code": {}, "context": {}, "file": {}, "history": {},
	"issue": {}, "limit": {},
	"metadata": {}, "model": {}, "normalization": {}, "normalize": {},
	"oversized": {}, "perception": {}, "previous": {}, "query": {},
	"regression": {}, "repository": {}, "response": {}, "responses": {},
	"sql": {}, "state": {}, "strings": {}, "task": {}, "test": {}, "update": {},
	"values": {}, "workflow": {},
}

// brainBriefActionFamiliesForReport keeps task-specific code scanners from
// being activated by an unrelated lexical history hit. The task is always
// authoritative. A history excerpt may reveal an otherwise implicit action
// family only when it first proves relevance to the task through a strong
// identifier or multiple significant terms. Matches are considered one at a
// time so unrelated excerpts cannot combine fragments into a false intent.
func brainBriefActionFamiliesForReport(report brainBriefReport, task string) brainBriefActionFamily {
	taskContext := strings.ToLower(task)
	families := brainBriefActionFamiliesForContext(taskContext)
	for _, match := range report.History.Matches {
		if !brainBriefActionHistoryRelevant(task, match.Excerpt) {
			continue
		}
		families |= brainBriefActionFamiliesForContext(taskContext + "\n" + strings.ToLower(match.Excerpt))
	}
	return families
}

func brainBriefActionFamiliesForContext(context string) brainBriefActionFamily {
	var families brainBriefActionFamily
	if strings.Contains(context, "normalizelimit") ||
		strings.Contains(context, "max_query_limit") ||
		(strings.Contains(context, "query limit") && strings.Contains(context, "limit normalization")) ||
		(strings.Contains(context, "normalize") && strings.Contains(context, "limit")) ||
		(strings.Contains(context, "oversized") && strings.Contains(context, "limit")) {
		families |= brainBriefActionFamilyLimit
	}
	if strings.Contains(context, "metadata.step") ||
		strings.Contains(context, "metadata values must be strings") ||
		strings.Contains(context, "invalid_type") ||
		(strings.Contains(context, "metadata") && strings.Contains(context, "responses api")) {
		families |= brainBriefActionFamilyMetadata
	}
	if strings.Contains(context, "previousresponseid") ||
		strings.Contains(context, "previous_response_id") ||
		(strings.Contains(context, "self-contained") && strings.Contains(context, "perception")) ||
		(strings.Contains(context, "stale") && strings.Contains(context, "model state")) {
		families |= brainBriefActionFamilyPreviousResponse
	}
	return families
}

func brainBriefActionHistoryRelevant(task, excerpt string) bool {
	excerpt = strings.TrimSpace(excerpt)
	if excerpt == "" {
		return false
	}
	lowerExcerpt := strings.ToLower(excerpt)
	strongIdentifiers := brainBriefActionStrongIdentifiers(task)
	for _, identifier := range strongIdentifiers {
		if strings.Contains(lowerExcerpt, strings.ToLower(identifier)) {
			return true
		}
	}
	// When the task names a strong symbol/constant, that anchor is the safer
	// relevance boundary. Do not let two generic prose terms bind history about
	// another subsystem to the named-code task.
	if len(strongIdentifiers) > 0 {
		return false
	}

	taskTerms := brainBriefFileMatchTerms(task)
	if len(taskTerms) < 2 {
		return false
	}
	excerptTerms := brainBriefFileMatchTerms(excerpt)
	if len(excerptTerms) == 0 {
		return false
	}
	present := make(map[string]struct{}, len(excerptTerms))
	for _, term := range excerptTerms {
		present[term] = struct{}{}
	}
	required := 2
	if len(taskTerms) >= 5 {
		required = 3
	}
	hits := 0
	domainHit := false
	for _, term := range taskTerms {
		if _, ok := present[term]; !ok {
			continue
		}
		hits++
		if _, generic := brainBriefActionGenericOverlapTerms[term]; !generic {
			domainHit = true
		}
	}
	return hits >= required && domainHit
}

func brainBriefActionStrongIdentifiers(task string) []string {
	queries := brainBriefRawHistoryQueries(task)
	identifiers := queries[:0]
	for _, identifier := range queries {
		// Short all-caps words such as SQL/CLI are identifier-like to the history
		// tokenizer but are not strong enough to bind an action invariant to one
		// task. Real symbol/constant anchors remain well above this floor.
		if utf8.RuneCountInString(strings.Trim(identifier, "_")) >= 6 {
			identifiers = append(identifiers, identifier)
		}
	}
	return identifiers
}
