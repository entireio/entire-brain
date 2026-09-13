package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type evidenceRecallResult struct {
	SelectorProtocol   string                    `json:"selector_protocol"`
	SelectorInputBytes int                       `json:"selector_input_bytes"`
	SchemaVersion      int                       `json:"schema_version"`
	Branch             string                    `json:"branch"`
	Query              string                    `json:"query"`
	EffectiveEngine    string                    `json:"effective_engine"`
	SelectionMode      string                    `json:"selection_mode"`
	Agent              string                    `json:"agent"`
	Model              string                    `json:"model,omitempty"`
	RequestSHA256      string                    `json:"request_sha256"`
	Evidence           []evidenceSpan            `json:"evidence"`
	ReturnedCount      int                       `json:"returned_count"`
	CandidateCount     int                       `json:"candidate_count"`
	SessionsScanned    int                       `json:"sessions_scanned"`
	SessionsMatched    int                       `json:"sessions_matched"`
	OmittedIDs         []string                  `json:"omitted_ids"`
	SuppressedIDs      []string                  `json:"suppressed_ids"`
	InputTruncated     bool                      `json:"input_truncated"`
	Truncated          bool                      `json:"truncated"`
	EvidenceBytes      int                       `json:"evidence_bytes"`
	EvidenceByteBudget int                       `json:"evidence_byte_budget"`
	RetrievalState     string                    `json:"retrieval_state"`
	CoverageScope      string                    `json:"coverage_scope"`
	Warnings           []string                  `json:"warnings"`
	SelectorSeconds    float64                   `json:"selector_seconds"`
	TotalSeconds       float64                   `json:"total_seconds"`
	TokenUsage         *distillTokenUsageSummary `json:"token_usage,omitempty"`
}

func runRecallEvidence(cmd *cobra.Command, opts Options, repoDir, brainDir, branch, query string, limit, budget int, agent, model string, command []string, jsonOut bool, run distillAgentRunner) error {
	if strings.TrimSpace(query) == "" {
		return fmt.Errorf("--evidence requires a nonempty query")
	}
	if limit < 1 || limit > 128 {
		return fmt.Errorf("--evidence requires --k between 1 and 128 (candidate sessions)")
	}
	if budget < 2 || budget > 1024*1024 {
		return fmt.Errorf("--evidence-bytes must be between 2 and 1048576")
	}
	if agent == "auto" {
		agent = defaultRefreshAgent(cmd.Context(), opts.Runner, repoDir)
	}
	// Reject unsupported configuration before touching source text. Provider
	// outages/invalid responses fall back; policy denials remain hard errors.
	var args []string
	var err error
	if agent != "none" {
		args, err = evidenceSelectorArgs(agent, model, command)
		if err != nil {
			return err
		}
	}
	policy, err := captureRetrievalPrivacyPolicy(brainDir)
	if err != nil {
		return err
	}
	// Same linearized egress/output boundary as pattern model calls: exclusions
	// cannot commit between final policy validation and provider transmission.
	return runPrivacyLinearizedPatternMutation(cmd, []retrievalPrivacyPolicy{policy}, func() error {
		started := time.Now()
		c, err := collectEvidence(cmd.Context(), brainDir, branch, query, limit)
		if err != nil {
			return err
		}
		request := makeEvidenceRequest(query, branch, c)
		out := evidenceRecallResult{SchemaVersion: 1, SelectorProtocol: "sparse_v2", Branch: branch, Query: query, EffectiveEngine: "canonical_sessions_lexical_with_fact_anchors", SelectionMode: "deterministic", Agent: agent, Model: model, RequestSHA256: request.RequestSHA256, CandidateCount: len(c.Spans), SessionsScanned: c.SessionsScanned, SessionsMatched: c.SessionsMatched, InputTruncated: c.InputTruncated, EvidenceByteBudget: budget, RetrievalState: c.State, CoverageScope: "supplied candidate blocks only; selected relations are query-scoped and are not stored facts; byte budget covers compact JSON evidence array only", Warnings: append([]string{}, c.Warnings...)}
		var selection *evidenceSelection
		if agent != "none" && len(c.Spans) > 0 && c.State != "unavailable" {
			payload, _ := json.Marshal(request)
			out.SelectorInputBytes = len(payload)
			dir, err := os.MkdirTemp("", "brain-evidence-selector-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(dir)
			if run == nil {
				run = defaultDistillAgentRunner(agent)
			}
			callCtx, usage := withDistillUsageCollector(cmd.Context())
			callStart := time.Now()
			response, callErr := run(callCtx, dir, args, payload, 180*time.Second)
			out.SelectorSeconds = time.Since(callStart).Seconds()
			out.TokenUsage = usage.summary(1)
			if cmd.Context().Err() != nil {
				return cmd.Context().Err()
			}
			if callErr != nil {
				out.SelectionMode = "fallback"
				out.Warnings = append(out.Warnings, evidenceFallbackWarning("failed or timed out"))
			} else {
				validated, validationErr := validateEvidenceSelection(request, response)
				if validationErr != nil {
					out.SelectionMode = "fallback"
					out.Warnings = append(out.Warnings, evidenceFallbackWarning("returned invalid output"))
				} else {
					selection = &validated
					out.SelectionMode = "model"
				}
			}
		}
		packet := packEvidence(c.Spans, selection, budget)
		out.Evidence = packet.Spans
		out.EvidenceBytes = packet.Bytes
		out.ReturnedCount = len(packet.Spans)
		out.OmittedIDs = packet.OmittedIDs
		out.SuppressedIDs = packet.SuppressedIDs
		out.Truncated = len(packet.OmittedIDs) > 0
		if out.Truncated {
			out.Warnings = append(out.Warnings, "evidence byte budget omitted whole blocks or required groups")
		}
		out.TotalSeconds = time.Since(started).Seconds()
		if jsonOut {
			return writeJSON(cmd, out)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%d evidence spans for %q on %s (%s)\n", out.ReturnedCount, query, branch, out.SelectionMode)
		for _, w := range out.Warnings {
			fmt.Fprintln(cmd.OutOrStdout(), "warning: "+w)
		}
		for _, s := range out.Evidence {
			fmt.Fprintf(cmd.OutOrStdout(), "\n[%s] %s:%d %s UTF-8[%d:%d] sha256:%s\n%s\n", s.ID, s.Path, s.Line, s.JSONPointer, s.StartByte, s.EndByte, s.SourceSHA256, s.Text)
		}
		return nil
	})
}
