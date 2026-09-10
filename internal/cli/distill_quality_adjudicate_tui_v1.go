package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"github.com/ashtom/entire-brain/internal/tui"
)

// runDistillQualityAdjudicationTUIV1 adapts the sealed Phase 2 packet types to
// the presentation-only TUI. The callback returns to this package for every
// confirmed answer, so the same validation, lock, and atomic store used by the
// plain reviewer remain authoritative.
func runDistillQualityAdjudicationTUIV1(bundleDir, adjudicatorID, queue string, limit int, input io.Reader, output io.Writer, theme tui.Theme, now func() time.Time) (distillQualityAdjudicateResultV1, error) {
	var result distillQualityAdjudicateResultV1
	if input == nil || output == nil {
		return result, fmt.Errorf("quality adjudication: input and output are required")
	}
	if now == nil {
		now = time.Now
	}
	session, err := openDistillQualityAdjudicationSessionV1(bundleDir, adjudicatorID, queue, limit)
	if err != nil {
		return result, err
	}
	defer func() { _ = session.close() }()
	if len(session.items) == 0 {
		fmt.Fprintln(output, "No unreviewed packets match this queue.")
		return session.result, nil
	}

	view, byViewID := makeDistillQualityReviewV1(session, queue)
	uiResult, err := tui.RunQualityReview(input, output, theme, view, func(itemID, admission, authority, safety, rationale string) error {
		item, ok := byViewID[itemID]
		if !ok {
			return fmt.Errorf("quality adjudication: unknown review item %q", itemID)
		}
		return session.save(item, admission, authority, safety, rationale, now())
	})
	session.result.Presented = uiResult.Presented
	session.result.Quit = uiResult.Quit
	if err != nil {
		return session.result, err
	}
	if uiResult.Saved != session.result.Reviewed {
		return session.result, fmt.Errorf("quality adjudication: terminal saved %d decisions but store committed %d", uiResult.Saved, session.result.Reviewed)
	}
	return session.result, nil
}

func makeDistillQualityReviewV1(session *distillQualityAdjudicationSessionV1, queue string) (tui.QualityReview, map[string]distillQualityAdjudicateItemV1) {
	items := make([]tui.QualityReviewItem, 0, len(session.items))
	byViewID := make(map[string]distillQualityAdjudicateItemV1, len(session.items))
	for index, item := range session.items {
		viewID := fmt.Sprintf("Item %02d", index+1)
		byViewID[viewID] = item
		items = append(items, makeDistillQualityReviewItemV1(viewID, item))
	}
	return tui.QualityReview{
		Title: "Phase 2 quality review",
		Items: items,
		Progress: tui.QualityReviewProgress{
			Reviewer: session.result.AdjudicatorID, Queue: queue,
			Total: session.result.Total, AlreadyReviewed: session.result.AlreadyReviewed,
			Remaining: session.result.Remaining,
		},
	}, byViewID
}

func makeDistillQualityReviewItemV1(viewID string, item distillQualityAdjudicateItemV1) tui.QualityReviewItem {
	filterDecision := "FILTERED"
	if item.CandidateAdmitted {
		filterDecision = "SELECTED"
	}
	view := tui.QualityReviewItem{
		ID:             viewID,
		Selector:       filterDecision,
		SelectorReason: distillQualitySelectorReasonV1(item),
		Focus:          make([]tui.QualityReviewEvidence, 0, len(item.Focus)),
		Context:        make([]tui.QualityReviewEvidence, 0, len(item.Context)),
		Judges:         make([]tui.QualityReviewJudge, 0, len(item.Aggregate.Verdicts)+len(item.Aggregate.MissingJudges)),
		Sources:        make([]tui.QualityReviewSource, 0, len(item.Sources)),
	}
	acceptance := stringSliceContainsV1(item.Cues, string(distillCandidateCueAcceptanceV1))
	for _, turn := range item.Focus {
		label := "HUMAN STATEMENT"
		if acceptance && turn.Role == "assistant" {
			label = "ASSISTANT PROPOSAL"
		} else if acceptance && turn.Role == "user" {
			label = "HUMAN CONFIRMATION"
		} else if turn.Role == "assistant" {
			label = "ASSISTANT STATEMENT"
		}
		view.Focus = append(view.Focus, tui.QualityReviewEvidence{Label: label, Text: turn.Text})
	}
	for _, turn := range item.Context {
		label := "SURROUNDING HUMAN CONTEXT"
		if turn.Role == "assistant" {
			label = "SURROUNDING ASSISTANT CONTEXT"
		}
		view.Context = append(view.Context, tui.QualityReviewEvidence{Label: label, Kind: "not the statement being judged", Text: turn.Text})
	}
	verdicts := append([]distillQualityPanelVerdictV1(nil), item.Aggregate.Verdicts...)
	sort.SliceStable(verdicts, func(left, right int) bool {
		return verdicts[left].State == distillQualityVerdictCompletedV1 && verdicts[right].State != distillQualityVerdictCompletedV1
	})
	for _, verdict := range verdicts {
		judge := tui.QualityReviewJudge{
			Name: string(verdict.Judge), Model: verdict.Model, State: string(verdict.State),
			Rationale: distillQualityHumanVerdictNoteV1(verdict.Rationale), Scores: make([]tui.QualityReviewScore, 0, len(verdict.Scores)),
		}
		for _, score := range verdict.Scores {
			summary, value := distillQualityReviewScoreSummaryV1(score)
			judge.Scores = append(judge.Scores, tui.QualityReviewScore{
				Dimension: string(score.Dimension), Label: string(score.Label),
				Summary: summary, Rationale: distillQualityHumanJudgeRationaleV1(item.Packet, score.Rationale),
			})
			switch score.Dimension {
			case distillQualityDimensionAdmissionV1:
				judge.Recommendation = value
				judge.RecommendationRationale = distillQualityHumanJudgeRationaleV1(item.Packet, score.Rationale)
			case distillQualityDimensionAuthorityV1:
				judge.Authority = value
			case distillQualityDimensionSafetyV1:
				judge.Safety = value
			}
		}
		view.Judges = append(view.Judges, judge)
	}
	for _, missing := range item.Aggregate.MissingJudges {
		view.Judges = append(view.Judges, tui.QualityReviewJudge{Name: string(missing), State: "missing", Rationale: "No advisory verdict is available; the human decision remains independent."})
	}
	for index, source := range item.Sources {
		detail := "branch: " + source.Branch
		if source.SessionID != "" {
			detail += " · session: " + source.SessionID
		}
		if source.CheckpointID != "" {
			detail += " · checkpoint: " + source.CheckpointID
		}
		view.Sources = append(view.Sources, tui.QualityReviewSource{
			Label: fmt.Sprintf("source %d", index+1), Path: filepath.ToSlash(source.TranscriptPath),
			Lines: fmt.Sprintf("lines %d–%d", source.StartLine, source.EndLine), Detail: detail,
		})
	}
	return view
}

func distillQualitySelectorReasonV1(item distillQualityAdjudicateItemV1) string {
	authority := "assistant context"
	if stringSliceContainsV1(item.Authorities, "direct_user") {
		authority = "the human's own words"
	} else if stringSliceContainsV1(item.Authorities, "direct_user_context") {
		authority = "direct human context"
	} else if stringSliceContainsV1(item.Authorities, "context_only") {
		authority = "context-only text"
	}
	if !item.CandidateAdmitted {
		return "The automatic filter kept this statement out because it found no durable-memory cue. Source: " + authority + "."
	}
	cues := make([]string, 0, len(item.Cues))
	for _, cue := range item.Cues {
		switch cue {
		case "rule":
			cues = append(cues, "rule-like wording")
		case "preference":
			cues = append(cues, "a preference")
		case "correction":
			cues = append(cues, "a correction")
		case "decision":
			cues = append(cues, "a decision")
		case "closed_negative":
			cues = append(cues, "a rejected approach")
		case "invariant":
			cues = append(cues, "an invariant")
		case "gotcha":
			cues = append(cues, "a gotcha")
		case "acceptance":
			cues = append(cues, "an explicitly accepted proposal")
		default:
			cues = append(cues, cue)
		}
	}
	matched := "a possible long-term knowledge cue"
	if len(cues) > 0 {
		matched = strings.Join(cues, ", ")
	}
	return "Selected after matching " + matched + " in " + authority + ". Verify that it is genuinely long-term knowledge."
}

func distillQualityReviewScoreSummaryV1(score distillQualityPanelScoreV1) (string, string) {
	switch score.Dimension {
	case distillQualityDimensionAdmissionV1:
		var value struct {
			ShouldAdmit       string `json:"should_admit"`
			CandidateAdmitted bool   `json:"candidate_admitted"`
			CriticalMiss      bool   `json:"critical_miss"`
		}
		if json.Unmarshal(score.RawScore, &value) == nil {
			candidate := "FILTERED"
			if value.CandidateAdmitted {
				candidate = "SELECTED"
			}
			answer := "UNSURE"
			if value.ShouldAdmit == "yes" {
				answer = "YES"
			} else if value.ShouldAdmit == "no" {
				answer = "NO"
			}
			return fmt.Sprintf("Long-term knowledge: %s · selector: %s · critical miss: %s", answer, candidate, qualityReviewBoolWordV1(value.CriticalMiss)), answer
		}
	case distillQualityDimensionAuthorityV1:
		var value struct {
			Authority string `json:"authority"`
		}
		if json.Unmarshal(score.RawScore, &value) == nil {
			answer := "UNSURE"
			if value.Authority == "adequate" {
				answer = "YES"
			} else if value.Authority == "inadequate" {
				answer = "NO"
			}
			return "Human authority: " + answer, answer
		}
	case distillQualityDimensionSafetyV1:
		var value struct {
			UnsafeMaterial string `json:"unsafe_material"`
		}
		if json.Unmarshal(score.RawScore, &value) == nil {
			answer := "UNSURE"
			if value.UnsafeMaterial == "no" {
				answer = "YES"
			} else if value.UnsafeMaterial == "yes" {
				answer = "NO"
			}
			return "Safe source: " + answer, answer
		}
	}
	return "Structured advisory details unavailable", "UNAVAILABLE"
}

func distillQualityHumanJudgeRationaleV1(packet distillQualityPanelPacketV1, value string) string {
	value = strings.TrimSpace(value)
	for _, fragment := range packet.Payload.Fragments {
		if strings.HasPrefix(value, fragment.ID) {
			value = strings.TrimSpace(strings.TrimPrefix(value, fragment.ID))
			value = strings.TrimSpace(strings.TrimPrefix(value, ":"))
		}
	}
	return value
}

func distillQualityHumanVerdictNoteV1(value string) string {
	value = strings.TrimSpace(value)
	switch value {
	case "transport_or_schema_failure_after_one_retry":
		return "The judge did not return a usable answer after one retry."
	case "no scope supplied":
		return "The judge could not evaluate this item because its request lacked scope."
	default:
		return strings.ReplaceAll(value, "_", " ")
	}
}

func qualityReviewBoolWordV1(value bool) string {
	if value {
		return "YES"
	}
	return "NO"
}

func commandInputIsTTY(cmd *cobra.Command) bool {
	if file, ok := cmd.InOrStdin().(*os.File); ok {
		return isatty.IsTerminal(file.Fd()) || isatty.IsCygwinTerminal(file.Fd())
	}
	return false
}
