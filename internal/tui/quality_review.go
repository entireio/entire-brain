package tui

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// QualityReview is the IO-free data required to review one statement at a time.
type QualityReview struct {
	Title    string
	Items    []QualityReviewItem
	Progress QualityReviewProgress
}

// QualityReviewItem is one statement and its precomputed panel position.
// Context and Sources are retained for adapter compatibility but deliberately
// not shown in this streamlined review screen.
type QualityReviewItem struct {
	ID, Selector, SelectorReason string
	PanelSummary                 string
	Question                     string
	AnswerKind                   QualityReviewAnswerKind
	Focus                        []QualityReviewEvidence
	Context                      []QualityReviewEvidence
	Judges                       []QualityReviewJudge
	Sources                      []QualityReviewSource
}

type QualityReviewEvidence struct{ Label, Kind, Text string }

// QualityReviewJudge is a display-ready, valid judge result supplied by the
// adapter. Position must be agrees_with_filter, disagrees_with_filter, or
// unsure; all other rows are omitted.
type QualityReviewJudge struct {
	Name     string
	Position string
	Why      string

	// Deprecated adapter fields. They are retained temporarily to avoid a broad
	// presentation-model migration, but this screen only renders Position/Why.
	Model, State, Recommendation, Authority, Safety, RecommendationRationale, Rationale string
	Scores                                                                              []QualityReviewScore
}
type QualityReviewScore struct{ Dimension, Label, Summary, Rationale string }
type QualityReviewSource struct{ Label, Path, Lines, Detail string }
type QualityReviewProgress struct {
	Reviewer, Queue                   string
	Total, AlreadyReviewed, Remaining int
}

// QualityReviewAnswerKind controls the binary question at the bottom. The
// zero value is panel agreement. Split panels use filter correctness.
type QualityReviewAnswerKind string

const (
	QualityReviewPanelAgreement QualityReviewAnswerKind = "panel_agreement"
	QualityReviewFilterCorrect  QualityReviewAnswerKind = "filter_correct"
)

// QualityReviewDecision is exposed for model tests. The simplified screen
// sets only Answer; legacy fields remain source-compatible and unused.
type QualityReviewDecision struct {
	Answer                       string
	Admission, Authority, Safety QualityReviewAnswer
	Rationale                    string
}
type QualityReviewAnswer string

const (
	QualityReviewYes     QualityReviewAnswer = "yes"
	QualityReviewNo      QualityReviewAnswer = "no"
	QualityReviewUnclear QualityReviewAnswer = "unclear"
)

// QualityReviewSaveFunc persists the sole human response. answer is
// "agree"/"disagree" for panel_agreement and "yes"/"no" for filter_correct.
type QualityReviewSaveFunc func(itemID, answer string) error
type QualityReviewResult struct {
	Presented, Saved int
	Quit             bool
}

func RunQualityReview(input io.Reader, output io.Writer, theme Theme, review QualityReview, save QualityReviewSaveFunc) (QualityReviewResult, error) {
	if input == nil || output == nil {
		return QualityReviewResult{}, errors.New("quality review: input and output are required")
	}
	if save == nil {
		return QualityReviewResult{}, errors.New("quality review: save callback is required")
	}
	p := tea.NewProgram(NewQualityReviewModel(review, theme, save), tea.WithAltScreen(), tea.WithInput(input), tea.WithOutput(output))
	model, err := p.Run()
	if err != nil {
		return QualityReviewResult{}, err
	}
	if result, ok := model.(QualityReviewModel); ok {
		return result.Result(), nil
	}
	return QualityReviewResult{}, errors.New("quality review: unexpected terminal model")
}

type qualityReviewStage int

const (
	qualityReviewAsk qualityReviewStage = iota
	qualityReviewDone
)

type QualityReviewModel struct {
	review                   QualityReview
	theme                    Theme
	save                     QualityReviewSaveFunc
	stage                    qualityReviewStage
	position, skipped, saved int
	quit                     bool
	decision                 QualityReviewDecision
	err                      error
	answer                   int // -1 none, 0 positive, 1 negative
	viewport                 viewport.Model
	width, height            int
	ready                    bool
}

func NewQualityReviewModel(review QualityReview, theme Theme, save QualityReviewSaveFunc) QualityReviewModel {
	review.Items = append([]QualityReviewItem(nil), review.Items...)
	m := QualityReviewModel{review: review, theme: theme, save: save, answer: -1, viewport: viewport.New(70, 20), width: 100, height: 32}
	if len(review.Items) == 0 {
		m.stage = qualityReviewDone
	}
	m.resize()
	m.refreshDetail()
	return m
}
func (m QualityReviewModel) Init() tea.Cmd { return nil }

func (m QualityReviewModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height, m.ready = msg.Width, msg.Height, true
		m.resize()
		m.refreshDetail()
		return m, nil
	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC || msg.String() == "q" {
			m.quit = true
			return m, tea.Quit
		}
		if m.stage == qualityReviewDone {
			if msg.Type == tea.KeyEnter || msg.String() == "esc" {
				return m, tea.Quit
			}
			return m, nil
		}
		if msg.String() == "x" {
			m.advanceSkipped()
			return m, nil
		}
		if applyQualityScroll(&m.viewport, msg) {
			return m, nil
		}
		return m.updateAnswer(msg)
	}
	return m, nil
}

func (m QualityReviewModel) updateAnswer(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	switch msg.Type {
	case tea.KeyLeft:
		m.answer = 0
		return m, nil
	case tea.KeyRight:
		m.answer = 1
		return m, nil
	case tea.KeyEnter:
		if m.answer == 0 {
			key = "y"
		}
		if m.answer == 1 {
			key = "n"
		}
	}
	answer := m.answerForKey(key)
	if answer == "" {
		return m, nil
	}
	m.decision.Answer = answer
	if m.save == nil {
		m.err = errors.New("quality review: save callback is unavailable")
	} else if err := m.save(m.current().ID, answer); err != nil {
		m.err = err
	} else {
		m.err = nil
		m.advanceSaved()
	}
	m.refreshDetail()
	return m, nil
}
func (m QualityReviewModel) answerForKey(key string) string {
	if m.current().AnswerKind == QualityReviewFilterCorrect {
		if key == "y" {
			return "yes"
		}
		if key == "n" {
			return "no"
		}
		return ""
	}
	if key == "y" {
		return "agree"
	}
	if key == "n" {
		return "disagree"
	}
	return ""
}
func (m *QualityReviewModel) advanceSaved()   { m.saved++; m.position++; m.resetDecision() }
func (m *QualityReviewModel) advanceSkipped() { m.skipped++; m.position++; m.resetDecision() }
func (m *QualityReviewModel) resetDecision() {
	m.decision = QualityReviewDecision{}
	m.answer = -1
	m.viewport.GotoTop()
	if m.position >= len(m.review.Items) {
		m.stage = qualityReviewDone
	} else {
		m.stage = qualityReviewAsk
	}
	m.refreshDetail()
}
func (m QualityReviewModel) current() QualityReviewItem {
	if m.position >= 0 && m.position < len(m.review.Items) {
		return m.review.Items[m.position]
	}
	return QualityReviewItem{}
}
func applyQualityScroll(model *viewport.Model, msg tea.KeyMsg) bool {
	switch msg.String() {
	case "up", "k":
		model.LineUp(1)
	case "down", "j":
		model.LineDown(1)
	case "pgup":
		model.ViewUp()
	case "pgdown", " ":
		model.ViewDown()
	case "home":
		model.GotoTop()
	case "end":
		model.GotoBottom()
	default:
		return false
	}
	return true
}
func (m *QualityReviewModel) resize() {
	if m.width < 30 {
		m.width = 30
	}
	if m.height < 10 {
		m.height = 10
	}
	m.viewport.Width, m.viewport.Height = m.width-4, m.height-6
	if m.viewport.Height < 3 {
		m.viewport.Height = 3
	}
}
func (m *QualityReviewModel) refreshDetail() { m.viewport.SetContent(m.detailView()) }

func (m QualityReviewModel) View() string {
	if !m.ready {
		return "Loading quality review…"
	}
	title := m.review.Title
	if title == "" {
		title = "Quality review"
	}
	header := m.theme.titleStyle().Render(qualityTerminalText(title)) + "\n" + lipgloss.NewStyle().Width(m.width).Render(m.theme.dimStyle().Render(m.progressText()))
	body := m.theme.paneStyle(true).Width(m.width - 2).Height(m.viewport.Height).Render(m.viewport.View())
	footer := m.questionView()
	if m.err != nil {
		footer = m.theme.flagStyle().Render("SAVE ERROR: "+qualityTerminalText(m.err.Error())+" — this statement is still unsaved.") + "\n" + footer
	}
	return header + "\n" + body + "\n" + footer
}
func (m QualityReviewModel) questionView() string {
	if m.stage == qualityReviewDone {
		return m.theme.dimStyle().Render("Review complete. Press Enter or Esc to exit.")
	}
	item := m.current()
	question := strings.TrimSpace(item.Question)
	if question == "" {
		question = "Do you agree with the judges' majority?"
	}
	choices, shortcut := []string{"agree", "disagree"}, "y agree · n disagree"
	if item.AnswerKind == QualityReviewFilterCorrect {
		question, choices, shortcut = item.Question, []string{"yes", "no"}, "y yes · n no"
		if question == "" {
			question = "Is the filter decision correct?"
		}
	}
	if m.answer >= 0 && m.answer < len(choices) {
		choices[m.answer] = "[" + strings.ToUpper(choices[m.answer]) + "]"
	}
	return m.theme.headingStyle().Render(question) + "  " + m.theme.textStyle().Render(strings.Join(choices, " · ")) + "\n" + m.theme.dimStyle().Render("←/→ choose · Enter save · "+shortcut+" · ↑↓ scroll · x skip · q quit")
}
func (m QualityReviewModel) progressText() string {
	total := m.review.Progress.Total
	if total < m.review.Progress.AlreadyReviewed+len(m.review.Items) {
		total = m.review.Progress.AlreadyReviewed + len(m.review.Items)
	}
	s := fmt.Sprintf("Reviewed %d/%d", m.review.Progress.AlreadyReviewed+m.saved, total)
	if m.review.Progress.Reviewer != "" {
		s += " · reviewer: " + qualityTerminalText(m.review.Progress.Reviewer)
	}
	if len(m.review.Items) > 0 && m.stage != qualityReviewDone {
		s += fmt.Sprintf(" · statement %d/%d", m.position+1, len(m.review.Items))
	}
	if m.skipped > 0 {
		s += fmt.Sprintf(" · skipped: %d", m.skipped)
	}
	return s
}
func (m QualityReviewModel) Result() QualityReviewResult {
	presented := m.position
	if m.position < len(m.review.Items) && m.stage != qualityReviewDone {
		presented++
	}
	if presented > len(m.review.Items) {
		presented = len(m.review.Items)
	}
	return QualityReviewResult{Presented: presented, Saved: m.saved, Quit: m.quit}
}
func (m QualityReviewModel) detailView() string {
	width := m.viewport.Width
	if width < 20 {
		width = 20
	}
	if m.stage == qualityReviewDone {
		return m.theme.headingStyle().Render("Review complete") + "\n\n" + m.theme.textStyle().Render("No unsaved statements remain.")
	}
	item := m.current()
	var b strings.Builder
	b.WriteString(m.theme.headingStyle().Render("Statement under review") + "\n")
	if len(item.Focus) == 0 {
		b.WriteString(m.theme.dimStyle().Render("No isolated statement is available. Skip this statement."))
	} else {
		for i, evidence := range item.Focus {
			if i > 0 {
				b.WriteString("\n")
			}
			if label := qualityTerminalText(evidence.Label); label != "" && len(item.Focus) > 1 {
				b.WriteString(m.theme.dimStyle().Render(label) + "\n")
			}
			b.WriteString(qualityWrap(m.theme, evidence.Text, width))
		}
	}
	b.WriteString("\n\n" + m.theme.headingStyle().Render("Filter decision") + "\n" + m.theme.textStyle().Bold(true).Render(orDash(qualityTerminalText(item.Selector))))
	if item.SelectorReason != "" {
		b.WriteString("\n" + qualityWrap(m.theme, item.SelectorReason, width))
	}
	b.WriteString("\n\n" + m.theme.headingStyle().Render("Advisory panel") + "\n")
	if item.PanelSummary != "" {
		b.WriteString(qualityWrap(m.theme, item.PanelSummary, width) + "\n")
	}
	shown := 0
	for _, judge := range item.Judges {
		if qualityReviewJudgeAvailable(judge) {
			shown++
			b.WriteString(m.judgeView(judge, width))
		}
	}
	if shown == 0 {
		b.WriteString(m.theme.dimStyle().Render("No available judge decisions."))
	}
	return b.String()
}
func qualityReviewJudgeAvailable(judge QualityReviewJudge) bool {
	switch strings.ToLower(strings.TrimSpace(judge.Position)) {
	case "agrees_with_filter", "disagrees_with_filter", "unsure":
		return true
	}
	return false
}
func (m QualityReviewModel) judgeView(judge QualityReviewJudge, width int) string {
	var b strings.Builder
	name := orDash(qualityTerminalText(judge.Name))
	position := qualityReviewJudgePosition(strings.ToLower(strings.TrimSpace(judge.Position)))
	b.WriteString(m.theme.textStyle().Bold(true).Render(truncate(name+": "+position, width)) + "\n")
	if why := strings.TrimSpace(judge.Why); why != "" {
		b.WriteString(qualityWrap(m.theme, "Why: "+why, width) + "\n")
	}
	return b.String()
}
func qualityReviewJudgePosition(position string) string {
	switch position {
	case "agrees_with_filter":
		return "AGREES WITH FILTER"
	case "disagrees_with_filter":
		return "DISAGREES WITH FILTER"
	case "unsure":
		return "UNSURE"
	}
	return ""
}
func qualityWrap(theme Theme, value string, width int) string {
	value = qualityTerminalText(value)
	if strings.TrimSpace(value) == "" {
		return theme.dimStyle().Render("—")
	}
	return lipgloss.NewStyle().Width(width).Foreground(theme.Text).Render(value)
}

// qualityTerminalText makes source text inert before Bubble Tea renders it.
func qualityTerminalText(value string) string {
	var out strings.Builder
	out.Grow(len(value))
	for _, r := range value {
		switch {
		case r == '\n':
			out.WriteRune(r)
		case r == '\t':
			out.WriteString("    ")
		case unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r):
			out.WriteRune('�')
		default:
			out.WriteRune(r)
		}
	}
	return out.String()
}
