package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
		filterDecision = "ADMITTED"
	}
	view := tui.QualityReviewItem{
		ID:        viewID,
		Candidate: fmt.Sprintf("Candidate filter: %s\nAttention: %s", filterDecision, reportDistillQualityAttentionTextV1(item.Aggregate)),
		Stratum:   item.Stratum,
		Evidence:  make([]tui.QualityReviewEvidence, 0, len(item.Packet.Payload.Fragments)),
		Judges:    make([]tui.QualityReviewJudge, 0, len(item.Aggregate.Verdicts)+len(item.Aggregate.MissingJudges)),
		Sources:   make([]tui.QualityReviewSource, 0, len(item.Sources)),
	}
	for _, fragment := range item.Packet.Payload.Fragments {
		view.Evidence = append(view.Evidence, tui.QualityReviewEvidence{Label: fragment.ID, Kind: "redacted evidence", Text: fragment.Text})
	}
	for _, verdict := range item.Aggregate.Verdicts {
		judge := tui.QualityReviewJudge{
			Name: string(verdict.Judge), Model: verdict.Model, State: string(verdict.State),
			Rationale: verdict.Rationale, Scores: make([]tui.QualityReviewScore, 0, len(verdict.Scores)),
		}
		for _, score := range verdict.Scores {
			judge.Scores = append(judge.Scores, tui.QualityReviewScore{
				Dimension: string(score.Dimension), Label: string(score.Label),
				Summary: distillQualityReviewScoreSummaryV1(score), Rationale: score.Rationale,
			})
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

func distillQualityReviewScoreSummaryV1(score distillQualityPanelScoreV1) string {
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
				candidate = "ADMITTED"
			}
			return fmt.Sprintf("Judge says admit: %s · candidate: %s · critical miss: %s", strings.ToUpper(value.ShouldAdmit), candidate, qualityReviewBoolWordV1(value.CriticalMiss))
		}
	case distillQualityDimensionAuthorityV1:
		var value struct {
			Authority string `json:"authority"`
		}
		if json.Unmarshal(score.RawScore, &value) == nil {
			return "Authority: " + strings.ToUpper(value.Authority)
		}
	case distillQualityDimensionSafetyV1:
		var value struct {
			UnsafeMaterial string `json:"unsafe_material"`
		}
		if json.Unmarshal(score.RawScore, &value) == nil {
			return "Unsafe material: " + strings.ToUpper(value.UnsafeMaterial)
		}
	}
	return "Structured advisory details unavailable"
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
