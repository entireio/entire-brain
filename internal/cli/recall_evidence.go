package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// Evidence recall has a separate opt-in envelope from ordinary fact recall.
// The evidence array contains only original source bytes and their anchors.
type evidenceRecallResult struct {
	// Set only on an empty result: names other branches holding sessions, so a
	// scan of the wrong branch is distinguishable from a genuine miss.
	BlindSpot          string         `json:"blind_spot,omitempty"`
	SchemaVersion      int            `json:"schema_version"`
	Branch             string         `json:"branch"`
	Query              string         `json:"query"`
	EffectiveEngine    string         `json:"effective_engine"`
	SelectionMode      string         `json:"selection_mode"`
	Evidence           []evidenceSpan `json:"evidence"`
	ReturnedCount      int            `json:"returned_count"`
	CandidateCount     int            `json:"candidate_count"`
	SessionsScanned    int            `json:"sessions_scanned"`
	SessionsMatched    int            `json:"sessions_matched"`
	OmittedIDs         []string       `json:"omitted_ids"`
	InputTruncated     bool           `json:"input_truncated"`
	Truncated          bool           `json:"truncated"`
	EvidenceBytes      int            `json:"evidence_bytes"`
	EvidenceByteBudget int            `json:"evidence_byte_budget"`
	RetrievalState     string         `json:"retrieval_state"`
	CoverageScope      string         `json:"coverage_scope"`
	Warnings           []string       `json:"warnings"`
}

func runRecallEvidence(cmd *cobra.Command, brainDir, branch, query string, limit, budget int, jsonOut bool) error {
	if strings.TrimSpace(query) == "" {
		return fmt.Errorf("--evidence requires a nonempty query")
	}
	if limit < 1 || limit > 128 {
		return fmt.Errorf("--evidence requires --k between 1 and 128 (candidate sessions)")
	}
	if budget < 2 || budget > 1024*1024 {
		return fmt.Errorf("--evidence-bytes must be between 2 and 1048576")
	}
	if err := cmd.Context().Err(); err != nil {
		return err
	}
	policy, err := captureRetrievalPrivacyPolicy(brainDir)
	if err != nil {
		return err
	}
	// Reuse the existing buffered privacy boundary: exclusions and cleanup
	// cannot commit between checking the source and emitting its bytes.
	// This path never invokes a provider or writes derived facts.
	return runPrivacyLinearizedPatternMutation(cmd, []retrievalPrivacyPolicy{policy}, func() error {
		c, err := collectEvidence(cmd.Context(), brainDir, branch, query, limit, budget)
		if err != nil {
			return err
		}
		spans, omitted, size, err := packDeterministicEvidence(c.Spans, budget)
		if err != nil {
			return err
		}
		out := evidenceRecallResult{
			SchemaVersion: 1, Branch: branch, Query: query,
			EffectiveEngine: "canonical_sessions_lexical_with_fact_anchors", SelectionMode: "deterministic",
			Evidence: spans, ReturnedCount: len(spans), CandidateCount: len(c.Spans),
			SessionsScanned: c.SessionsScanned, SessionsMatched: c.SessionsMatched,
			OmittedIDs: omitted, InputTruncated: c.InputTruncated, Truncated: len(omitted) > 0,
			EvidenceBytes: size, EvidenceByteBudget: budget, RetrievalState: c.State,
			CoverageScope: "bounded canonical-session candidates; no completeness, truth, or temporal eligibility guarantee; byte budget covers compact JSON evidence array only",
			Warnings:      append([]string{}, c.Warnings...),
		}
		if out.Truncated {
			out.Warnings = append(out.Warnings, "evidence byte budget omitted whole blocks")
		}
		// An empty evidence result on a branch that captured no sessions is the
		// wrong-branch trap, not an answer. `--evidence` returned before
		// reaching the blind-spot checks the other recall paths run
		// (facts_read_cmd.go:412,430), so this surface said nothing at all.
		//
		// The note is in the result's OWN unit -- sessions, which is what
		// evidence scans -- rather than the fact-branch note, which would
		// attach a claim about facts to a result that holds none.
		if out.ReturnedCount == 0 {
			out.BlindSpot = otherBranchSessionBlindSpot(brainDir, branch)
		}
		if err := cmd.Context().Err(); err != nil {
			return err
		}
		if jsonOut {
			return writeJSON(cmd, out)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%d evidence spans for %q on %s (deterministic)\n", out.ReturnedCount, query, branch)
		for _, warning := range out.Warnings {
			fmt.Fprintln(cmd.OutOrStdout(), "warning: "+warning)
		}
		// The text reader needs this as much as the JSON one; it is the only
		// output a person sees.
		if out.BlindSpot != "" {
			fmt.Fprintln(cmd.OutOrStdout(), out.BlindSpot)
		}
		for _, span := range out.Evidence {
			fmt.Fprintf(cmd.OutOrStdout(), "\n[%s] %s:%d %s UTF-8[%d:%d] sha256:%s\n%s\n", span.ID, span.Path, span.Line, span.JSONPointer, span.StartByte, span.EndByte, span.SourceSHA256, span.Text)
		}
		return nil
	})
}

// Keep whole blocks in candidate order, skipping blocks that do not fit.
// Charge the actual compact JSON representation, including escaping, anchors,
// brackets and commas. Pretty printing and envelope fields are outside the cap.
func packDeterministicEvidence(candidates []evidenceSpan, budget int) ([]evidenceSpan, []string, int, error) {
	spans, omitted := []evidenceSpan{}, []string{}
	size := 2
	for _, span := range candidates {
		encoded, err := json.Marshal(span)
		if err != nil {
			// Fail before emitting the buffered response; an encoding failure is
			// not a budget omission and must never be charged as zero bytes.
			return nil, nil, 0, fmt.Errorf("encode evidence span: %w", err)
		}
		cost := len(encoded)
		if len(spans) > 0 {
			cost++
		}
		if size+cost > budget {
			omitted = append(omitted, span.ID)
			continue
		}
		spans = append(spans, span)
		size += cost
	}
	return spans, omitted, size, nil
}
