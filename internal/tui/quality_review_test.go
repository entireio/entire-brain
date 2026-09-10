package tui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func qualityReviewFixture() QualityReview {
	return QualityReview{
		Title:    "Phase 2 quality review",
		Progress: QualityReviewProgress{Reviewer: "reviewer-7", Queue: "calibration", Total: 4, AlreadyReviewed: 1, Remaining: 3},
		Items: []QualityReviewItem{
			{
				ID:             "packet-001",
				Selector:       "SELECTED",
				SelectorReason: "The filter matched rule-like wording in the human's own words.",
				Focus:          []QualityReviewEvidence{{Label: "HUMAN STATEMENT", Text: "Keep write operations explicit and never infer consent from a status question."}},
				Context:        []QualityReviewEvidence{{Label: "CONTEXT", Text: "This must not be shown in the simple reviewer."}},
				Sources:        []QualityReviewSource{{Label: "session", Path: "sessions/main/a.jsonl"}},
				PanelSummary:   "Panel majority: FILTER WRONG (1/1 available).",
				Judges: []QualityReviewJudge{
					{Name: "claude", Position: "disagrees_with_filter", Why: "This looks like a one-off request."},
					{Name: "offline", State: "missing", Rationale: "No response."},
					{Name: "broken", State: "invalid", Recommendation: "YES"},
					{Name: "incomplete", State: "completed", Rationale: "No decision."},
				},
			},
			{ID: "packet-002", Selector: "FILTERED", Focus: []QualityReviewEvidence{{Text: "Second statement."}}, Judges: []QualityReviewJudge{{Name: "cursor", Position: "agrees_with_filter", Why: "It is a durable project preference."}}},
		},
	}
}

func updateQuality(t *testing.T, m QualityReviewModel, msg tea.Msg) QualityReviewModel {
	t.Helper()
	got, _ := m.Update(msg)
	updated, ok := got.(QualityReviewModel)
	if !ok {
		t.Fatalf("Update returned %T, want QualityReviewModel", got)
	}
	return updated
}

func qualityKey(key string) tea.KeyMsg {
	if key == "enter" {
		return tea.KeyMsg{Type: tea.KeyEnter}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
}

func TestQualityReviewRendersOneStatementAndCompactAvailablePanel(t *testing.T) {
	m := NewQualityReviewModel(qualityReviewFixture(), themes["default"], func(string, string) error { return nil })
	m = updateQuality(t, m, tea.WindowSizeMsg{Width: 110, Height: 28})
	view := m.View()
	for _, want := range []string{
		"Statement under review", "Keep write operations explicit", "Filter decision", "SELECTED",
		"Advisory panel", "Panel majority: FILTER WRONG (1/1 available)", "claude: DISAGREES WITH FILTER", "Why: This looks like a one-off request.",
		"Do you agree with the judges' majority?", "y agree · n disagree",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q:\n%s", want, view)
		}
	}
	for _, absent := range []string{"Queue", "offline", "broken", "incomplete", "This must not be shown", "sessions/main"} {
		if strings.Contains(view, absent) {
			t.Errorf("simple view unexpectedly contained %q:\n%s", absent, view)
		}
	}
}

func TestQualityReviewAgreementSavesImmediatelyAndAdvances(t *testing.T) {
	var calls []string
	m := NewQualityReviewModel(qualityReviewFixture(), themes["default"], func(id, answer string) error {
		calls = append(calls, id+":"+answer)
		return nil
	})
	m = updateQuality(t, m, tea.WindowSizeMsg{Width: 100, Height: 24})
	m = updateQuality(t, m, qualityKey("y"))
	if got, want := calls, []string{"packet-001:agree"}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("save calls = %v, want %v", got, want)
	}
	if m.position != 1 || m.saved != 1 || m.decision != (QualityReviewDecision{}) || m.stage != qualityReviewAsk {
		t.Fatalf("save did not reset next statement: position=%d saved=%d decision=%+v stage=%d", m.position, m.saved, m.decision, m.stage)
	}
	if result := m.Result(); result != (QualityReviewResult{Presented: 2, Saved: 1}) {
		t.Errorf("result = %+v", result)
	}
}

func TestQualityReviewArrowSelectionAndEnterSave(t *testing.T) {
	var got string
	m := NewQualityReviewModel(qualityReviewFixture(), themes["default"], func(_ string, answer string) error { got = answer; return nil })
	m = updateQuality(t, m, tea.WindowSizeMsg{Width: 100, Height: 24})
	if m.answer != -1 || strings.Contains(m.View(), "[AGREE]") || strings.Contains(m.View(), "[DISAGREE]") {
		t.Fatalf("review opened with a selected answer: answer=%d view=%s", m.answer, m.View())
	}
	m = updateQuality(t, m, tea.KeyMsg{Type: tea.KeyRight})
	if m.answer != 1 || !strings.Contains(m.View(), "[DISAGREE]") {
		t.Fatalf("right did not select disagree: answer=%d", m.answer)
	}
	m = updateQuality(t, m, qualityKey("enter"))
	if got != "disagree" || m.position != 1 {
		t.Fatalf("enter result = %q position=%d", got, m.position)
	}
	m = updateQuality(t, m, tea.KeyMsg{Type: tea.KeyLeft})
	if m.answer != 0 || !strings.Contains(m.View(), "[AGREE]") {
		t.Fatalf("left did not select agree: answer=%d", m.answer)
	}
}

func TestQualityReviewSplitPanelAsksAboutTheFilterAndStoresYesNo(t *testing.T) {
	review := qualityReviewFixture()
	review.Items = review.Items[:1]
	review.Items[0].AnswerKind = QualityReviewFilterCorrect
	review.Items[0].Question = "Is the filter decision correct?"
	var answer string
	m := NewQualityReviewModel(review, themes["default"], func(_ string, got string) error { answer = got; return nil })
	m = updateQuality(t, m, tea.WindowSizeMsg{Width: 100, Height: 24})
	if view := m.View(); !strings.Contains(view, "Is the filter decision correct?") || !strings.Contains(view, "y yes · n no") {
		t.Fatalf("split question was not rendered: %s", view)
	}
	m = updateQuality(t, m, qualityKey("n"))
	if answer != "no" || m.saved != 1 {
		t.Fatalf("split answer = %q saved=%d", answer, m.saved)
	}
}

func TestQualityReviewSaveErrorRetainsStatementAndSelection(t *testing.T) {
	calls := 0
	m := NewQualityReviewModel(qualityReviewFixture(), themes["default"], func(string, string) error {
		calls++
		return errors.New("disk full")
	})
	m = updateQuality(t, m, tea.WindowSizeMsg{Width: 100, Height: 24})
	m = updateQuality(t, m, qualityKey("n"))
	if calls != 1 || m.position != 0 || m.saved != 0 || m.decision.Answer != "disagree" || m.err == nil {
		t.Fatalf("save error lost review: calls=%d position=%d saved=%d decision=%+v err=%v", calls, m.position, m.saved, m.decision, m.err)
	}
	if !strings.Contains(m.View(), "SAVE ERROR: disk full") {
		t.Errorf("save failure was not visible: %s", m.View())
	}
}

func TestQualityReviewScrollResizeSkipAndQuit(t *testing.T) {
	review := qualityReviewFixture()
	review.Items[0].Focus[0].Text = strings.Repeat("Detailed statement evidence. ", 180)
	m := NewQualityReviewModel(review, themes["default"], func(string, string) error { return nil })
	m = updateQuality(t, m, tea.WindowSizeMsg{Width: 64, Height: 18})
	if m.viewport.Width != 60 || m.viewport.Height < 3 {
		t.Fatalf("resize = viewport %dx%d", m.viewport.Width, m.viewport.Height)
	}
	before := m.viewport.YOffset
	m = updateQuality(t, m, tea.KeyMsg{Type: tea.KeyDown})
	if m.viewport.YOffset <= before {
		t.Errorf("down did not scroll: before=%d after=%d", before, m.viewport.YOffset)
	}
	m = updateQuality(t, m, qualityKey("x"))
	if m.position != 1 || m.saved != 0 || m.skipped != 1 {
		t.Fatalf("skip = position=%d saved=%d skipped=%d", m.position, m.saved, m.skipped)
	}
	m = updateQuality(t, m, qualityKey("q"))
	if !m.Result().Quit {
		t.Fatal("q did not quit")
	}
}

func TestQualityReviewMakesTerminalControlsInert(t *testing.T) {
	review := qualityReviewFixture()
	review.Items[0].Focus[0].Text = "safe\x1b]52;c;clipboard\a\rrewritten\u202etrick"
	m := NewQualityReviewModel(review, themes["default"], func(string, string) error { return nil })
	m = updateQuality(t, m, tea.WindowSizeMsg{Width: 110, Height: 30})
	view := m.View()
	for _, forbidden := range []string{"\x1b", "\a", "\r", "\u202e"} {
		if strings.Contains(view, forbidden) {
			t.Fatalf("view retained terminal control %q: %q", forbidden, view)
		}
	}
	if !strings.Contains(view, "safe�]52;c;clipboard��rewritten�trick") {
		t.Fatalf("view did not preserve visibly inert evidence: %q", view)
	}
}
