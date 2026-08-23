package cli

import (
	"fmt"
	"strings"
)

// distillCandidateExtractionProtocolV2 is the wire contract between a packed
// candidate extraction request and its agent response. It is deliberately
// separate from the candidate-card schema: cards can evolve independently as
// long as their stable candidate IDs continue to be supplied to this parser.
const distillCandidateExtractionProtocolV2 = 2

// distillCandidateMemberFactV2 is a parsed extraction result, not a durable
// fact. The command layer attaches provenance, applies taxonomy policy, and
// constructs factRecords only after this protocol boundary has completed.
type distillCandidateMemberFactV2 struct {
	Kind string
	Path string
	Text string
}

// distillCandidateMemberResultV2 is one completed candidate from a packed
// extraction call. Exactly one of NoFacts or Facts is set. Results are returned
// in the caller's expected-candidate order, rather than the model's line order.
type distillCandidateMemberResultV2 struct {
	CandidateID string
	NoFacts     bool
	Facts       []distillCandidateMemberFactV2
}

// parseDistillCandidateMemberResultsV2 validates and attributes a complete
// multi-candidate extraction response. Its accepted wire forms are exactly:
//
//	<candidate_id>\t<kind>\t<path>\t<fact>
//	<candidate_id>\tNO_FACTS
//
// expectedCandidateIDs is an ordered, unique set. A candidate with facts may
// have several fact lines (and therefore repeats its ID); a NO_FACTS candidate
// has exactly one sentinel line. Every expected candidate must have one of
// those two completions. This makes a packed call atomic: unknown IDs, a
// missing completion, duplicate expected IDs, prose, blank lines, a sentinel
// preceding facts, or more than factsMaxPerChunk facts for any individual
// candidate reject the whole response. A single redundant trailing NO_FACTS
// after valid facts is tolerated as a content-free local-model quirk.
//
// The function is intentionally pure and does not inspect a taxonomy or create
// fact records. That lets the caller apply its normal provenance and durable
// fact policy only after protocol completion is proven.
func parseDistillCandidateMemberResultsV2(expectedCandidateIDs []string, output string) ([]distillCandidateMemberResultV2, error) {
	_, err := validateDistillCandidateExpectedIDsV2(expectedCandidateIDs)
	if err != nil {
		return nil, err
	}

	// A final newline is conventional command output, not a blank protocol
	// record. Any other blank line is a malformed member response.
	output = strings.TrimSuffix(output, "\n")
	output = strings.TrimSuffix(output, "\r")
	if output == "" {
		return nil, fmt.Errorf("candidate extraction protocol v%d: blank output", distillCandidateExtractionProtocolV2)
	}

	byID := make(map[string]*distillCandidateMemberResultV2, len(expectedCandidateIDs))
	trailingEmpty := make(map[string]bool, len(expectedCandidateIDs))
	for _, id := range expectedCandidateIDs {
		byID[id] = &distillCandidateMemberResultV2{CandidateID: id}
	}

	for lineNumber, raw := range strings.Split(output, "\n") {
		line := strings.TrimSuffix(raw, "\r")
		if line == "" || strings.TrimSpace(line) == "" {
			return nil, distillCandidateProtocolLineErrorV2(lineNumber+1, "blank line")
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 2 && len(fields) != 4 {
			return nil, distillCandidateProtocolLineErrorV2(lineNumber+1, "expected candidate_id, NO_FACTS or candidate_id, kind, path, fact")
		}
		candidateID := fields[0]
		if candidateID == "" || strings.TrimSpace(candidateID) != candidateID {
			return nil, distillCandidateProtocolLineErrorV2(lineNumber+1, "invalid candidate ID")
		}
		result, ok := byID[candidateID]
		if !ok {
			return nil, distillCandidateProtocolLineErrorV2(lineNumber+1, "unknown candidate ID "+fmt.Sprintf("%q", candidateID))
		}

		if len(fields) == 2 {
			if fields[1] != "NO_FACTS" {
				return nil, distillCandidateProtocolLineErrorV2(lineNumber+1, "two-column response must use NO_FACTS")
			}
			if result.NoFacts || trailingEmpty[candidateID] {
				return nil, distillCandidateProtocolLineErrorV2(lineNumber+1, "duplicate or conflicting completion for candidate "+fmt.Sprintf("%q", candidateID))
			}
			if len(result.Facts) > 0 {
				// Some line-oriented local models append a redundant empty marker
				// after an otherwise complete fact result. It carries no conflicting
				// attribution or data, so tolerate exactly this trailing form. The
				// inverse (NO_FACTS before a fact), duplicate sentinels, missing IDs,
				// and every other partial/mixed response still fail closed.
				trailingEmpty[candidateID] = true
				continue
			}
			result.NoFacts = true
			continue
		}

		kind, path, text := fields[1], fields[2], fields[3]
		if trailingEmpty[candidateID] {
			return nil, distillCandidateProtocolLineErrorV2(lineNumber+1, "fact follows trailing NO_FACTS for candidate "+fmt.Sprintf("%q", candidateID))
		}
		if strings.TrimSpace(kind) != kind || kind != strings.ToLower(kind) || !validFactKind(kind) {
			return nil, distillCandidateProtocolLineErrorV2(lineNumber+1, "invalid fact kind")
		}
		if strings.TrimSpace(path) != path || !validFactPath(path) {
			return nil, distillCandidateProtocolLineErrorV2(lineNumber+1, "invalid fact path")
		}
		if strings.TrimSpace(text) == "" {
			return nil, distillCandidateProtocolLineErrorV2(lineNumber+1, "blank fact")
		}
		if result.NoFacts {
			return nil, distillCandidateProtocolLineErrorV2(lineNumber+1, "NO_FACTS conflicts with facts for candidate "+fmt.Sprintf("%q", candidateID))
		}
		if len(result.Facts) >= factsMaxPerChunk {
			return nil, distillCandidateProtocolLineErrorV2(lineNumber+1, fmt.Sprintf("candidate %q produced more than %d facts", candidateID, factsMaxPerChunk))
		}
		result.Facts = append(result.Facts, distillCandidateMemberFactV2{Kind: kind, Path: path, Text: text})
	}

	results := make([]distillCandidateMemberResultV2, 0, len(expectedCandidateIDs))
	for _, candidateID := range expectedCandidateIDs {
		result := byID[candidateID]
		if !result.NoFacts && len(result.Facts) == 0 {
			return nil, fmt.Errorf("candidate extraction protocol v%d: missing completion for candidate %q", distillCandidateExtractionProtocolV2, candidateID)
		}
		results = append(results, *result)
	}
	return results, nil
}

func validateDistillCandidateExpectedIDsV2(candidateIDs []string) (map[string]struct{}, error) {
	if len(candidateIDs) == 0 {
		return nil, fmt.Errorf("candidate extraction protocol v%d: no expected candidate IDs", distillCandidateExtractionProtocolV2)
	}
	expected := make(map[string]struct{}, len(candidateIDs))
	for _, candidateID := range candidateIDs {
		if candidateID == "" || strings.TrimSpace(candidateID) != candidateID || strings.ContainsAny(candidateID, "\t\r\n") {
			return nil, fmt.Errorf("candidate extraction protocol v%d: invalid expected candidate ID %q", distillCandidateExtractionProtocolV2, candidateID)
		}
		if _, exists := expected[candidateID]; exists {
			return nil, fmt.Errorf("candidate extraction protocol v%d: duplicate expected candidate ID %q", distillCandidateExtractionProtocolV2, candidateID)
		}
		expected[candidateID] = struct{}{}
	}
	return expected, nil
}

func distillCandidateProtocolLineErrorV2(line int, detail string) error {
	return fmt.Errorf("candidate extraction protocol v%d line %d: %s", distillCandidateExtractionProtocolV2, line, detail)
}
