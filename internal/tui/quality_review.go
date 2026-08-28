package tui

// This file deliberately contains only presentation and interaction state. The
// command layer owns loading the sealed packet, translating it to these view
// models, and persisting an answer through QualityReviewSaveFunc.

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// QualityReview is the IO-free data needed to review a bounded queue. Advisory
// information is display-only: it is intentionally not represented in
// QualityReviewDecision and can therefore never become a default decision.
type QualityReview struct {
	Title    string
	Items    []QualityReviewItem
	Progress QualityReviewProgress
}

// QualityReviewItem is one candidate in the human-review queue.
type QualityReviewItem struct {
	ID        string
	Candidate string
	Stratum   string
	Evidence  []QualityReviewEvidence
	Judges    []QualityReviewJudge
	Sources   []QualityReviewSource
}

// QualityReviewEvidence is a readable, redacted evidence fragment.
type QualityReviewEvidence struct {
	Label string
	Kind  string
	Text  string
}

// QualityReviewJudge is one advisory judge's result. State is rendered as a
// word as well as a color, so it remains understandable without color.
type QualityReviewJudge struct {
	Name      string
	Model     string
	State     string
	Scores    []QualityReviewScore
	Rationale string
}

// QualityReviewScore is a compact, human-readable advisory assessment. Summary
// is preferred over machine payloads; RawScore-like data is intentionally not
// part of this public presentation model.
type QualityReviewScore struct {
	Dimension string
	Label     string
	Summary   string
	Rationale string
}

// QualityReviewSource identifies local source context without requiring the
// TUI to open or read it.
type QualityReviewSource struct {
	Label  string
	Path   string
	Lines  string
	Detail string
}

// QualityReviewProgress describes work already saved before the TUI opens.
// Total normally includes completed, queued, and skipped items in the chosen
// batch. Queue is a human-facing description such as "calibration".
type QualityReviewProgress struct {
	Reviewer        string
	Queue           string
	Total           int
	AlreadyReviewed int
	Remaining       int
}

// QualityReviewAnswer is an explicitly human-entered answer. It has no
// advisory-derived default; every field in a saved decision must be set by the
// reviewer using y, n, or u.
type QualityReviewAnswer string

const (
	QualityReviewYes     QualityReviewAnswer = "yes"
	QualityReviewNo      QualityReviewAnswer = "no"
	QualityReviewUnclear QualityReviewAnswer = "unclear"
)

// QualityReviewDecision is the sole payload supplied to the persistence
// callback. A non-empty Rationale is optional.
type QualityReviewDecision struct {
	Admission QualityReviewAnswer
	Authority QualityReviewAnswer
	Safety    QualityReviewAnswer
	Rationale string
}

// QualityReviewSaveFunc persists one complete human decision. It is called
// only after the explicit confirmation step. The primitive signature keeps the
// adapter at the CLI boundary small and avoids leaking presentation structs
// into its private persistence types.
type QualityReviewSaveFunc func(itemID, admission, authority, safety, rationale string) error

// QualityReviewResult lets the caller preserve its normal completion summary.
// Saved excludes pre-existing records; Presented counts queue items reached by
// the session, including skips.
type QualityReviewResult struct {
	Presented int
	Saved     int
	Quit      bool
}

// RunQualityReview runs the full-screen adjudication UI. It does no file IO:
// the caller supplies terminal streams and a save callback. Nil streams or a
// nil callback are rejected rather than leaving an unsaveable decision screen.
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
	qualityReviewStart qualityReviewStage = iota
	qualityReviewAdmission
	qualityReviewAuthority
	qualityReviewSafety
	qualityReviewRationaleChoice
	qualityReviewRationaleEdit
	qualityReviewConfirm
	qualityReviewDone
)

// QualityReviewModel is exported so callers and tests can drive it without a
// terminal. Construct it with NewQualityReviewModel.
type QualityReviewModel struct {
	review QualityReview
	theme  Theme
	save   QualityReviewSaveFunc

	stage    qualityReviewStage
	position int
	skipped  int
	saved    int
	quit     bool
	decision QualityReviewDecision
	err      error
	help     bool

	viewport      viewport.Model
	rationale     textinput.Model
	width, height int
	ready         bool
}

// NewQualityReviewModel creates an IO-free Bubble Tea model. Items are copied
// at the slice boundary so a caller can safely reuse its queue slice.
func NewQualityReviewModel(review QualityReview, theme Theme, save QualityReviewSaveFunc) QualityReviewModel {
	items := append([]QualityReviewItem(nil), review.Items...)
	review.Items = items
	ti := textinput.New()
	ti.Prompt = "rationale: "
	ti.CharLimit = 8192
	m := QualityReviewModel{
		review:    review,
		theme:     theme,
		save:      save,
		viewport:  viewport.New(70, 20),
		rationale: ti,
		width:     100,
		height:    32,
	}
	if len(items) == 0 {
		m.stage = qualityReviewDone
	}
	m.resize()
	m.refreshDetail()
	return m
}

func (m QualityReviewModel) Init() tea.Cmd { return nil }

// Update implements the review state machine. q, ctrl+c, and x skip/quit
// without calling save; a decision is never persisted until confirm+y.
func (m QualityReviewModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height, m.ready = msg.Width, msg.Height, true
		m.resize()
		m.refreshDetail()
		return m, nil
	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC {
			m.quit = true
			return m, tea.Quit
		}
		// A rationale is ordinary user text: printable q and x must stay
		// typeable here. Esc cancels the edit and Ctrl-C is the only quit key
		// while the text editor has focus.
		if m.stage == qualityReviewRationaleEdit {
			return m.updateRationale(msg)
		}
		if msg.String() == "q" {
			m.quit = true
			return m, tea.Quit
		}
		if msg.String() == "?" {
			m.help = !m.help
			return m, nil
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
		if isQualityScroll(msg) {
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd
		}
		return m.updateDecision(msg)
	}
	return m, nil
}

func (m QualityReviewModel) updateRationale(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Type == tea.KeyEsc {
		m.rationale.Blur()
		m.stage = qualityReviewRationaleChoice
		return m, nil
	}
	if msg.Type == tea.KeyEnter {
		m.decision.Rationale = strings.TrimSpace(m.rationale.Value())
		m.rationale.Blur()
		m.stage = qualityReviewConfirm
		m.refreshDetail()
		m.viewport.GotoBottom()
		return m, nil
	}
	var cmd tea.Cmd
	m.rationale, cmd = m.rationale.Update(msg)
	m.refreshDetail()
	return m, cmd
}

func (m QualityReviewModel) updateDecision(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	switch m.stage {
	case qualityReviewStart:
		if msg.Type == tea.KeyEnter || key == "d" {
			m.stage = qualityReviewAdmission
		}
	case qualityReviewAdmission:
		if answer, ok := qualityAnswerForKey(key); ok {
			m.decision.Admission, m.stage = answer, qualityReviewAuthority
		}
	case qualityReviewAuthority:
		if answer, ok := qualityAnswerForKey(key); ok {
			m.decision.Authority, m.stage = answer, qualityReviewSafety
		}
	case qualityReviewSafety:
		if answer, ok := qualityAnswerForKey(key); ok {
			m.decision.Safety, m.stage = answer, qualityReviewRationaleChoice
		}
	case qualityReviewRationaleChoice:
		switch {
		case key == "e":
			m.stage = qualityReviewRationaleEdit
			m.rationale.SetValue(m.decision.Rationale)
			m.rationale.Focus()
			m.refreshDetail()
			m.viewport.GotoBottom()
			return m, textinput.Blink
		case key == "s" || msg.Type == tea.KeyEnter:
			m.decision.Rationale = ""
			m.stage = qualityReviewConfirm
		}
	case qualityReviewConfirm:
		switch {
		case key == "y":
			if m.save == nil {
				m.err = errors.New("quality review: save callback is unavailable")
			} else if err := m.save(m.current().ID, string(m.decision.Admission), string(m.decision.Authority), string(m.decision.Safety), m.decision.Rationale); err != nil {
				m.err = err
			} else {
				m.err = nil
				m.advanceSaved()
			}
		case key == "n" || msg.Type == tea.KeyEsc:
			m.stage = qualityReviewRationaleChoice
		}
	}
	m.refreshDetail()
	// Evidence should not jump away as the reviewer answers the three proof
	// questions. Only the final confirmation lives at the bottom of the detail.
	if m.stage == qualityReviewConfirm {
		m.viewport.GotoBottom()
	}
	return m, nil
}

func qualityAnswerForKey(key string) (QualityReviewAnswer, bool) {
	switch key {
	case "y":
		return QualityReviewYes, true
	case "n":
		return QualityReviewNo, true
	case "u":
		return QualityReviewUnclear, true
	default:
		return "", false
	}
}

func (m *QualityReviewModel) advanceSaved() {
	m.saved++
	m.position++
	m.resetDecision()
}

func (m *QualityReviewModel) advanceSkipped() {
	m.skipped++
	m.position++
	m.resetDecision()
}

func (m *QualityReviewModel) resetDecision() {
	m.decision = QualityReviewDecision{}
	m.rationale.SetValue("")
	m.viewport.GotoTop()
	if m.position >= len(m.review.Items) {
		m.stage = qualityReviewDone
	} else {
		m.stage = qualityReviewStart
	}
	m.refreshDetail()
}

func (m QualityReviewModel) current() QualityReviewItem {
	if m.position >= 0 && m.position < len(m.review.Items) {
		return m.review.Items[m.position]
	}
	return QualityReviewItem{}
}

func isQualityScroll(msg tea.KeyMsg) bool {
	key := msg.String()
	if key != "up" && key != "k" && key != "down" && key != "j" && key != "pgup" && key != "pgdown" && key != "home" && key != "end" {
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
	// Two-line header, stage line (which may wrap), queue/detail chrome, footer.
	contentH := m.height - 9
	if contentH < 3 {
		contentH = 3
	}
	if m.width >= 90 {
		m.viewport.Width, m.viewport.Height = m.width-38, contentH
	} else {
		// A one-row queue wastes scarce height in a narrow terminal. The progress
		// line already identifies the current item, so give the detail the space.
		m.viewport.Width, m.viewport.Height = m.width-4, contentH
		if m.viewport.Height < 3 {
			m.viewport.Height = 3
		}
	}
	m.rationale.Width = m.viewport.Width - len(m.rationale.Prompt)
	if m.rationale.Width < 8 {
		m.rationale.Width = 8
	}
}

func (m *QualityReviewModel) refreshDetail() {
	m.viewport.SetContent(m.detailView())
}

func (m QualityReviewModel) View() string {
	if !m.ready {
		return "Loading quality review…"
	}
	title := m.review.Title
	if title == "" {
		title = "Quality review"
	}
	header := m.theme.titleStyle().Render(qualityTerminalText(title)) + "\n" + lipgloss.NewStyle().Width(m.width).Render(m.theme.dimStyle().Render(m.progressText()))
	stageText := m.theme.headingStyle().Render("Decision: "+m.stageName()) + "  " + m.theme.dimStyle().Render(m.stagePrompt()+m.choiceSummary())
	stage := lipgloss.NewStyle().Width(m.width).Render(stageText)
	var body string
	if m.width >= 90 {
		left := m.theme.paneStyle(false).Width(32).Height(m.viewport.Height).Render(m.queueView(30))
		right := m.theme.paneStyle(true).Width(m.viewport.Width).Height(m.viewport.Height).Render(m.viewport.View())
		body = lipgloss.JoinHorizontal(lipgloss.Top, left, " ", right)
	} else {
		body = m.theme.paneStyle(true).Width(m.width - 2).Height(m.viewport.Height).Render(m.viewport.View())
	}
	footerText := "↑/k ↓/j scroll  ·  d start  ·  x skip (unsaved)  ·  q quit (unsaved)  ·  ? help"
	if m.stage == qualityReviewRationaleEdit {
		footerText = "type rationale  ·  Enter continue  ·  Esc cancel edit  ·  Ctrl-C quit (unsaved)"
	}
	footer := m.theme.dimStyle().Render(footerText)
	if m.help {
		footer += "\n" + m.theme.dimStyle().Render("Flow: d/Enter → admission y/n/u → authority y/n/u → safety y/n/u → rationale → confirm y saves. e edits a rationale; s/Enter skips it.")
	}
	if m.err != nil {
		footer = m.theme.flagStyle().Render("SAVE ERROR: "+qualityTerminalText(m.err.Error())+" — correct or cancel; this item remains unsaved.") + "\n" + footer
	}
	return header + "\n" + stage + "\n" + body + "\n" + footer
}

func (m QualityReviewModel) progressText() string {
	total := m.review.Progress.Total
	if total < m.review.Progress.AlreadyReviewed+len(m.review.Items) {
		total = m.review.Progress.AlreadyReviewed + len(m.review.Items)
	}
	s := fmt.Sprintf("Saved %d/%d", m.review.Progress.AlreadyReviewed+m.saved, total)
	if m.review.Progress.Reviewer != "" {
		s += " · reviewer: " + qualityTerminalText(m.review.Progress.Reviewer)
	}
	if m.review.Progress.Queue != "" {
		s += " · queue: " + qualityTerminalText(m.review.Progress.Queue)
	}
	if len(m.review.Items) > 0 {
		current := m.position + 1
		if current > len(m.review.Items) {
			current = len(m.review.Items)
		}
		s += fmt.Sprintf(" · item: %d/%d", current, len(m.review.Items))
	}
	if m.skipped > 0 {
		s += fmt.Sprintf(" · skipped: %d", m.skipped)
	}
	return s
}

// Result returns the state accumulated by a driven model. It is useful for
// tests and is what RunQualityReview returns after the program exits.
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

func (m QualityReviewModel) queueView(width int) string {
	var b strings.Builder
	b.WriteString(m.theme.headingStyle().Render("Queue"))
	if len(m.review.Items) == 0 {
		return b.String() + "\n" + m.theme.dimStyle().Render("No items to review.")
	}
	start, end := 0, len(m.review.Items)
	if m.width < 90 {
		start = m.position
		end = start + 1
		if end > len(m.review.Items) {
			end = len(m.review.Items)
		}
	} else {
		// Keep the queue within its pane instead of letting a large batch grow
		// past the terminal. The selected row stays visible as work advances.
		visible := m.viewport.Height - 1
		if visible < 1 {
			visible = 1
		}
		if end > visible {
			start = m.position - visible/2
			if start < 0 {
				start = 0
			}
			end = start + visible
			if end > len(m.review.Items) {
				end = len(m.review.Items)
				start = end - visible
			}
		}
	}
	for i := start; i < end; i++ {
		item := m.review.Items[i]
		marker := "  "
		if i < m.position {
			marker = "✓ "
		}
		if i == m.position && m.stage != qualityReviewDone {
			marker = "> "
		}
		label := qualityTerminalText(item.ID)
		if item.Stratum != "" {
			label += " · " + qualityTerminalText(item.Stratum)
		}
		line := truncate(marker+label, width)
		if i == m.position && m.stage != qualityReviewDone {
			b.WriteString("\n" + m.theme.titleStyle().Render(line))
		} else {
			b.WriteString("\n" + m.theme.dimStyle().Render(line))
		}
	}
	return b.String()
}

func (m QualityReviewModel) detailView() string {
	width := m.viewport.Width
	if width < 20 {
		width = 20
	}
	if m.stage == qualityReviewDone {
		return m.theme.headingStyle().Render("Queue complete") + "\n\n" + m.theme.textStyle().Render("No unsaved decision remains. Press Enter or Esc to exit.")
	}
	item := m.current()
	var b strings.Builder
	b.WriteString(m.theme.titleStyle().Render(orDash(qualityTerminalText(item.ID))))
	if item.Stratum != "" {
		b.WriteString("\n" + m.theme.dimStyle().Render("stratum: "+qualityTerminalText(item.Stratum)))
	}
	b.WriteString("\n\n")
	if item.Candidate != "" {
		b.WriteString(m.theme.headingStyle().Render("Candidate"))
		b.WriteString("\n")
		b.WriteString(qualityWrap(m.theme, item.Candidate, width))
		b.WriteString("\n\n")
	}
	b.WriteString(m.theme.headingStyle().Render("Evidence"))
	b.WriteString("\n")
	if len(item.Evidence) == 0 {
		b.WriteString(m.theme.dimStyle().Render("No evidence supplied."))
	} else {
		for _, evidence := range item.Evidence {
			label := orDash(qualityTerminalText(evidence.Label))
			if evidence.Kind != "" {
				label += " (" + qualityTerminalText(evidence.Kind) + ")"
			}
			b.WriteString(m.theme.textStyle().Render("• "+label) + "\n")
			b.WriteString(qualityWrap(m.theme, evidence.Text, width))
			b.WriteString("\n")
		}
	}
	b.WriteString("\n" + m.theme.headingStyle().Render("Advisory judge summaries") + "\n")
	if len(item.Judges) == 0 {
		b.WriteString(m.theme.dimStyle().Render("No advisory judge result."))
	} else {
		for _, judge := range item.Judges {
			b.WriteString(m.judgeView(judge, width))
		}
	}
	b.WriteString("\n" + m.theme.headingStyle().Render("Source context") + "\n")
	if len(item.Sources) == 0 {
		b.WriteString(m.theme.dimStyle().Render("No local source context supplied."))
	} else {
		for _, source := range item.Sources {
			line := "• " + orDash(qualityTerminalText(source.Label))
			if source.Path != "" {
				line += ": " + qualityTerminalText(source.Path)
			}
			if source.Lines != "" {
				line += " " + qualityTerminalText(source.Lines)
			}
			b.WriteString(m.theme.textStyle().Render(truncate(line, width)) + "\n")
			if source.Detail != "" {
				b.WriteString(qualityWrap(m.theme, source.Detail, width) + "\n")
			}
		}
	}
	b.WriteString("\n" + m.decisionView())
	return b.String()
}

func (m QualityReviewModel) judgeView(judge QualityReviewJudge, width int) string {
	var b strings.Builder
	name := orDash(qualityTerminalText(judge.Name))
	if judge.Model != "" {
		name += " · " + qualityTerminalText(judge.Model)
	}
	state := strings.ToUpper(orDash(qualityTerminalText(judge.State)))
	b.WriteString(m.theme.textStyle().Render(name) + "  " + m.theme.dimStyle().Render("["+state+"]") + "\n")
	if len(judge.Scores) == 0 {
		b.WriteString(m.theme.dimStyle().Render("  no score supplied") + "\n")
	}
	for _, score := range judge.Scores {
		line := fmt.Sprintf("  %-12s %-10s %s", truncate(orDash(qualityTerminalText(score.Dimension)), 12), strings.ToUpper(orDash(qualityTerminalText(score.Label))), qualityTerminalText(score.Summary))
		b.WriteString(m.theme.textStyle().Render(truncate(line, width)) + "\n")
		if score.Rationale != "" {
			b.WriteString(qualityWrap(m.theme, "    rationale: "+score.Rationale, width) + "\n")
		}
	}
	if judge.Rationale != "" {
		b.WriteString(qualityWrap(m.theme, "  assessment: "+judge.Rationale, width) + "\n")
	}
	return b.String()
}

func (m QualityReviewModel) decisionView() string {
	if m.stage == qualityReviewRationaleEdit {
		return m.theme.headingStyle().Render("Optional rationale") + "\n" + m.rationale.View() + "\nEnter confirms this text; Esc returns without changing it."
	}
	if m.stage != qualityReviewConfirm {
		return m.theme.headingStyle().Render("Your decision") + "\n" + m.theme.textStyle().Render(m.stagePrompt())
	}
	return m.theme.headingStyle().Render("Confirm human decision") + "\n" +
		m.theme.textStyle().Render("admission: "+string(m.decision.Admission)+" · authority: "+string(m.decision.Authority)+" · safety: "+string(m.decision.Safety)) + "\n" +
		m.theme.dimStyle().Render("rationale: "+orDash(m.decision.Rationale)) + "\n" +
		m.theme.textStyle().Render("Press y to save this complete decision, or n/Esc to edit it. This is the only save action.")
}

func (m QualityReviewModel) stageName() string {
	switch m.stage {
	case qualityReviewStart:
		return "Start decision"
	case qualityReviewAdmission:
		return "Admission"
	case qualityReviewAuthority:
		return "Authority"
	case qualityReviewSafety:
		return "Safety"
	case qualityReviewRationaleChoice, qualityReviewRationaleEdit:
		return "Rationale"
	case qualityReviewConfirm:
		return "Confirm and save"
	default:
		return "Complete"
	}
}

func (m QualityReviewModel) stagePrompt() string {
	switch m.stage {
	case qualityReviewStart:
		return "Enter begins · x skips unsaved."
	case qualityReviewAdmission:
		return "Admit as durable knowledge? y yes · n no · u unclear"
	case qualityReviewAuthority:
		return "Adequate human/corroborated authority? y yes · n no · u unclear"
	case qualityReviewSafety:
		return "Safe from injection/pasted/one-off/review text? y yes · n no · u unclear"
	case qualityReviewRationaleChoice:
		return "Optional rationale: [e]dit, or [s]kip / Enter to continue."
	case qualityReviewRationaleEdit:
		return ""
	case qualityReviewConfirm:
		return "Review the explicit human answers below."
	default:
		return "Press Enter or Esc to exit."
	}
}

func (m QualityReviewModel) choiceSummary() string {
	var choices []string
	if m.decision.Admission != "" {
		choices = append(choices, "admission="+string(m.decision.Admission))
	}
	if m.decision.Authority != "" {
		choices = append(choices, "authority="+string(m.decision.Authority))
	}
	if m.decision.Safety != "" {
		choices = append(choices, "safety="+string(m.decision.Safety))
	}
	if len(choices) == 0 {
		return ""
	}
	return " · " + strings.Join(choices, " · ")
}

func qualityWrap(theme Theme, value string, width int) string {
	value = qualityTerminalText(value)
	if strings.TrimSpace(value) == "" {
		return theme.dimStyle().Render("  —")
	}
	return lipgloss.NewStyle().Width(width).Foreground(theme.Text).Render(value)
}

// qualityTerminalText removes terminal control sequences from redacted source
// text and provider rationales before they reach Bubble Tea. Newlines remain
// meaningful; tabs become spaces; every other control/bidi formatting rune is
// made visibly inert so evidence cannot rewrite the screen or clipboard.
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
