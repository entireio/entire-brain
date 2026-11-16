package cli

import "strings"

const brainBriefActionChecklistGuidance = "Treat action_checklist as the first-pass current-code inventory; edit listed files first, and broaden only when the checklist is missing, ambiguous, or validation fails."

type brainBriefActionFamily uint8

const (
	brainBriefActionFamilyLimit brainBriefActionFamily = 1 << iota
	brainBriefActionFamilyMetadata
	brainBriefActionFamilyPreviousResponse
)

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
