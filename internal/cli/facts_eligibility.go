package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	eligibilityEmptyProvenance = "empty_provenance"
	eligibilityExcludedSession = "excluded_session"
	eligibilityUnknownSession  = "unknown_session"
	eligibilityAtOrAfterCutoff = "at_or_after_cutoff"
)

// factEligibilityAudit records the complete candidate accounting before ranking.
// DeliveredCount is filled by recall after the eligible corpus has been ranked.
type factEligibilityAudit struct {
	PrefilterCorpusCount int            `json:"prefilter_corpus_count"`
	EligibleCount        int            `json:"eligible_count"`
	ExcludedCounts       map[string]int `json:"excluded_counts"`
	DeliveredCount       int            `json:"delivered_count"`
}

func loadSessionDates(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read session dates: %w", err)
	}
	var dates map[string]string
	if err := json.Unmarshal(raw, &dates); err != nil {
		return nil, fmt.Errorf("parse session dates: %w", err)
	}
	if dates == nil {
		return nil, fmt.Errorf("parse session dates: expected a JSON object")
	}
	return dates, nil
}

func parseEligibilityTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if strings.HasSuffix(value, "z") {
		value = strings.TrimSuffix(value, "z") + "Z"
	}
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed, nil
	}
	// Match the harness's established compatibility behavior: a timestamp with
	// no explicit offset is UTC, never local machine time.
	if parsed, err := time.ParseInLocation("2006-01-02T15:04:05.999999999", value, time.UTC); err == nil {
		return parsed, nil
	}
	return time.Time{}, fmt.Errorf("invalid RFC3339 timestamp %q", value)
}

// filterFactsByTemporalEligibility constructs the complete task-scoped candidate
// set before lexical or semantic ranking. Every provenance anchor must identify a
// known, non-excluded session strictly before cutoff; uncertainty fails closed.
func filterFactsByTemporalEligibility(
	facts []factRecord,
	sessionDates map[string]string,
	cutoffValue string,
	excludeSessionIDs []string,
) ([]factRecord, factEligibilityAudit, error) {
	cutoff, err := parseEligibilityTime(cutoffValue)
	if err != nil {
		return nil, factEligibilityAudit{}, fmt.Errorf("--eligible-before: %w", err)
	}
	excluded := make(map[string]struct{}, len(excludeSessionIDs))
	for _, id := range excludeSessionIDs {
		if id = strings.TrimSpace(id); id != "" {
			excluded[id] = struct{}{}
		}
	}
	audit := factEligibilityAudit{
		PrefilterCorpusCount: len(facts),
		ExcludedCounts: map[string]int{
			eligibilityEmptyProvenance: 0,
			eligibilityExcludedSession: 0,
			eligibilityUnknownSession:  0,
			eligibilityAtOrAfterCutoff: 0,
		},
	}
	eligible := make([]factRecord, 0, len(facts))
	for _, fact := range facts {
		if len(fact.Provenance) == 0 {
			audit.ExcludedCounts[eligibilityEmptyProvenance]++
			continue
		}
		hasEmptyAnchor := false
		hasExcludedSession := false
		hasUnknownSession := false
		hasAtOrAfterCutoff := false
		for _, anchor := range fact.Provenance {
			sessionID := strings.TrimSpace(anchor.SessionID)
			if sessionID == "" {
				hasEmptyAnchor = true
				continue
			}
			if _, found := excluded[sessionID]; found {
				hasExcludedSession = true
				continue
			}
			createdValue, found := sessionDates[sessionID]
			if !found {
				hasUnknownSession = true
				continue
			}
			created, parseErr := parseEligibilityTime(createdValue)
			if parseErr != nil {
				hasUnknownSession = true
				continue
			}
			if !created.Before(cutoff) {
				hasAtOrAfterCutoff = true
			}
		}
		// Assign exactly one deterministic reason, independent of provenance
		// anchor order, while retaining the strictest fail-closed precedence.
		reason := ""
		switch {
		case hasEmptyAnchor:
			reason = eligibilityEmptyProvenance
		case hasExcludedSession:
			reason = eligibilityExcludedSession
		case hasUnknownSession:
			reason = eligibilityUnknownSession
		case hasAtOrAfterCutoff:
			reason = eligibilityAtOrAfterCutoff
		}
		if reason != "" {
			audit.ExcludedCounts[reason]++
			continue
		}
		eligible = append(eligible, fact)
	}
	audit.EligibleCount = len(eligible)
	return eligible, audit, nil
}
