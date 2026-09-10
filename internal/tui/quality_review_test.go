package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func qualityReviewFixture() QualityReview {
	return QualityReview{
		Title: "Phase 2 human adjudication",
		Progress: QualityReviewProgress{
			Reviewer: "reviewer-7", Queue: "calibration", Total: 4,
			AlreadyReviewed: 1, Remaining: 3,
		},
		Items: []QualityReviewItem{
			{
				ID:             "packet-001",
				Selector:       "SELECTED",
				SelectorReason: "The automatic filter matched rule-like wording in the human's own words.",
				Focus:          []QualityReviewEvidence{{Label: "HUMAN STATEMENT", Kind: "statement selected by the automatic filter", Text: "Keep write operations explicit and never infer consent from a status question."}},
				Context:        []QualityReviewEvidence{{Label: "SURROUNDING ASSISTANT CONTEXT", Kind: "not the statement being judged", Text: "This large assistant answer is context, not the proposition a human should judge."}},
				Judges: []QualityReviewJudge{{Name: "claude", Model: "test-model", State: "completed", Recommendation: "NO", Authority: "YES", Safety: "YES", RecommendationRationale: "This looks like a one-off request.", Scores: []QualityReviewScore{
					{Dimension: "admission", Label: "concern", Summary: "May be one-off context", Rationale: "The wording is broad but the source is narrow."},
					{Dimension: "safety", Label: "pass", Summary: "No injection signal"},
				}, Rationale: "Advisory only; inspect the evidence."}},
				Sources: []QualityReviewSource{{Label: "session", Path: "sessions/main/a.jsonl", Lines: "lines 10–14", Detail: "Local source record."}},
			},
			{ID: "packet-002", Selector: "FILTERED", Focus: []QualityReviewEvidence{{Label: "HUMAN STATEMENT", Text: "Second statement"}}},
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

func TestQualityReviewWideAndNarrowViewsAreReadable(t *testing.T) {
	m := NewQualityReviewModel(qualityReviewFixture(), themes["default"], func(string, string, string, string, string) error { return nil })
	m = updateQuality(t, m, tea.WindowSizeMsg{Width: 120, Height: 34})
	wide := m.View()
	for _, want := range []string{"Phase 2 human adjudication", "Overall reviewed 1/4", "Decision: 1/3 · Long-term value", "Queue", "Statement under review", "HUMAN STATEMENT", "Independent model advice", "long-term: NO", "This looks like a one-off request", "Surrounding context", "Hidden (1 turn"} {
		if !strings.Contains(wide, want) {
			t.Errorf("wide view missing %q:\n%s", want, wide)
		}
	}
	if strings.Contains(wide, `{"should_admit"`) {
		t.Errorf("wide view exposed raw score material:\n%s", wide)
	}
	if strings.Contains(wide, "large assistant answer") || strings.Contains(wide, "sessions/main") {
		t.Errorf("wide view exposed optional context by default:\n%s", wide)
	}
	m = updateQuality(t, m, qualityKey("c"))
	if toggled := m.detailView(); !strings.Contains(toggled, "large assistant answer") || !strings.Contains(toggled, "sessions/main") {
		t.Errorf("context toggle did not reveal optional context and source:\n%s", toggled)
	}
	m = updateQuality(t, m, qualityKey("c"))

	m = updateQuality(t, m, tea.WindowSizeMsg{Width: 60, Height: 28})
	narrow := m.View()
	for _, want := range []string{"item: 1/2", "packet-001", "Statement under review", "Long-term value"} {
		if !strings.Contains(narrow, want) {
			t.Errorf("narrow view missing %q:\n%s", want, narrow)
		}
	}
	if strings.Contains(narrow, "\nQueue\n") {
		t.Errorf("narrow view wasted space on a one-row queue:\n%s", narrow)
	}
	if m.viewport.Width != 56 {
		t.Errorf("narrow detail width = %d, want 56", m.viewport.Width)
	}
}

func TestQualityReviewDecisionSavesOnlyAfterExplicitConfirm(t *testing.T) {
	var calls []string
	m := NewQualityReviewModel(qualityReviewFixture(), themes["default"], func(id, admission, authority, safety, rationale string) error {
		calls = append(calls, strings.Join([]string{id, admission, authority, safety, rationale}, ":"))
		return nil
	})
	m = updateQuality(t, m, tea.WindowSizeMsg{Width: 110, Height: 30})
	for _, key := range []string{"y", "n", "u", "s"} {
		m = updateQuality(t, m, qualityKey(key))
	}
	if len(calls) != 0 || m.stage != qualityReviewConfirm {
		t.Fatalf("saved before confirmation: calls=%v stage=%v", calls, m.stage)
	}
	m = updateQuality(t, m, qualityKey("y"))
	if got, want := calls, []string{"packet-001:yes:no:unclear:"}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("save payload = %v, want %v", got, want)
	}
	if m.position != 1 || m.saved != 1 || m.stage != qualityReviewAdmission {
		t.Fatalf("did not immediately advance after save: position=%d saved=%d stage=%v", m.position, m.saved, m.stage)
	}
	if result := m.Result(); result.Presented != 2 || result.Saved != 1 || result.Quit {
		t.Errorf("result = %+v", result)
	}
}

func TestQualityReviewSaveErrorRetainsItemAndDecision(t *testing.T) {
	calls := 0
	m := NewQualityReviewModel(qualityReviewFixture(), themes["default"], func(string, string, string, string, string) error {
		calls++
		return errors.New("disk full")
	})
	m = updateQuality(t, m, tea.WindowSizeMsg{Width: 110, Height: 30})
	for _, key := range []string{"y", "y", "y", "s", "y"} {
		m = updateQuality(t, m, qualityKey(key))
	}
	if calls != 1 || m.position != 0 || m.saved != 0 || m.stage != qualityReviewConfirm || m.err == nil {
		t.Fatalf("save error lost decision: calls=%d position=%d saved=%d stage=%v err=%v", calls, m.position, m.saved, m.stage, m.err)
	}
	if !strings.Contains(m.View(), "SAVE ERROR: disk full") {
		t.Errorf("save failure was not visible: %s", m.View())
	}
}

func TestQualityReviewSkipAndQuitNeverSavePartialDecision(t *testing.T) {
	calls := 0
	m := NewQualityReviewModel(qualityReviewFixture(), themes["default"], func(string, string, string, string, string) error { calls++; return nil })
	m = updateQuality(t, m, tea.WindowSizeMsg{Width: 110, Height: 30})
	m = updateQuality(t, m, qualityKey("y")) // admission is deliberately partial
	m = updateQuality(t, m, qualityKey("x"))
	if calls != 0 || m.position != 1 || m.stage != qualityReviewAdmission {
		t.Fatalf("skip persisted or did not advance: calls=%d position=%d stage=%v", calls, m.position, m.stage)
	}
	m = updateQuality(t, m, qualityKey("q"))
	if calls != 0 || !m.Result().Quit {
		t.Fatalf("quit persisted partial state: calls=%d result=%+v", calls, m.Result())
	}
}

func TestQualityReviewScrollResizeAndNoAdvisoryDefaults(t *testing.T) {
	review := qualityReviewFixture()
	long := strings.Repeat("Detailed evidence for a human reviewer. ", 100)
	review.Items[0].Focus = append(review.Items[0].Focus, QualityReviewEvidence{Label: "MORE OF THE HUMAN STATEMENT", Text: long})
	m := NewQualityReviewModel(review, themes["default"], func(string, string, string, string, string) error { return nil })
	m = updateQuality(t, m, tea.WindowSizeMsg{Width: 100, Height: 18})
	if m.decision != (QualityReviewDecision{}) {
		t.Fatalf("advisory data populated a decision: %+v", m.decision)
	}
	before := m.viewport.YOffset
	m = updateQuality(t, m, tea.KeyMsg{Type: tea.KeyDown})
	if m.viewport.YOffset <= before {
		t.Errorf("down did not scroll detail: before=%d after=%d", before, m.viewport.YOffset)
	}
	scrolled := m.viewport.YOffset
	m = updateQuality(t, m, qualityKey("y"))
	if m.viewport.YOffset != scrolled {
		t.Errorf("answering a decision moved away from evidence: before=%d after=%d", scrolled, m.viewport.YOffset)
	}
	m = updateQuality(t, m, tea.WindowSizeMsg{Width: 64, Height: 22})
	if m.viewport.Width != 60 || m.viewport.Height < 3 {
		t.Errorf("resize did not recalculate narrow viewport: %+v", m.viewport)
	}
}

func TestQualityReviewAnswersKeepEvidencePositionAndBoundQueue(t *testing.T) {
	review := qualityReviewFixture()
	for index := 3; index <= 40; index++ {
		review.Items = append(review.Items, QualityReviewItem{ID: fmt.Sprintf("packet-%03d", index), Selector: "FILTERED"})
	}
	m := NewQualityReviewModel(review, themes["default"], func(string, string, string, string, string) error { return nil })
	m = updateQuality(t, m, tea.WindowSizeMsg{Width: 100, Height: 18})
	if got := strings.Count(m.queueView(30), "\n"); got > m.viewport.Height-1 {
		t.Fatalf("queue rendered %d rows into a %d-row pane", got, m.viewport.Height)
	}
	m.viewport.YOffset = 1
	m = updateQuality(t, m, qualityKey("y"))
	if m.viewport.YOffset != 1 {
		t.Fatalf("answering admission moved evidence viewport to %d", m.viewport.YOffset)
	}
	if view := m.View(); !strings.Contains(view, "long-term=yes") {
		t.Fatalf("fixed decision line did not preserve prior answer:\n%s", view)
	}
}

func TestQualityReviewRationaleEditorAcceptsPrintableQAndX(t *testing.T) {
	m := NewQualityReviewModel(qualityReviewFixture(), themes["default"], func(string, string, string, string, string) error { return nil })
	m = updateQuality(t, m, tea.WindowSizeMsg{Width: 100, Height: 24})
	for _, key := range []string{"y", "y", "y", "e", "q", "x"} {
		m = updateQuality(t, m, qualityKey(key))
	}
	got := m.rationale.Value()
	if m.quit || got != "qx" {
		t.Fatalf("rationale text = %q quit=%v; printable q/x must be typeable", got, m.quit)
	}
	if view := m.View(); !strings.Contains(view, "Ctrl-C quit") || strings.Contains(view, "q quit (unsaved)") {
		t.Fatalf("rationale footer described inactive shortcuts:\n%s", view)
	}
}

func TestQualityReviewMakesTerminalControlsInert(t *testing.T) {
	review := qualityReviewFixture()
	review.Items[0].Focus[0].Text = "safe\x1b]52;c;clipboard\a\rrewritten\u202etrick"
	m := NewQualityReviewModel(review, themes["default"], func(string, string, string, string, string) error { return nil })
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
