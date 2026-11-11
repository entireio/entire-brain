package setupui

import (
	"fmt"
	"io"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/entireio/entire-brain/internal/tui"
)

// select.go is the interactive single-choice prompt, on bubbletea so every
// interactive question in setup shares one stack and the CLI palette. It
// follows consent.go's two load-bearing properties: the caller reports TTY
// capability explicitly, and without one the prompt neither reads nor writes
// a byte — it returns the default, unanswered.

// SelectOption is one choice.
type SelectOption struct {
	Label  string
	Detail string
}

// SelectRequest is everything the prompt needs.
type SelectRequest struct {
	Title   string
	Options []SelectOption
	// Default is the index returned when no answer is possible (no TTY, a
	// dismissed prompt, a program error).
	Default int
	// TTY reports whether a human can answer; carried explicitly so every
	// path is reachable from a unit test.
	TTY bool
}

// SelectOutcome is the chosen index and whether a human actually chose it.
type SelectOutcome struct {
	Index    int
	Answered bool
}

// Select asks one single-choice question. The prompt erases itself once
// answered, like the consent block: a question is history, not output.
func Select(req SelectRequest, in io.Reader, out io.Writer, r *Renderer) SelectOutcome {
	if !req.TTY || len(req.Options) == 0 || in == nil || out == nil {
		return SelectOutcome{Index: req.Default}
	}
	program := tea.NewProgram(newSelectModel(req, r), tea.WithInput(in), tea.WithOutput(out))
	final, err := program.Run()
	if err != nil {
		return SelectOutcome{Index: req.Default}
	}
	m, ok := final.(selectModel)
	if !ok || !m.answered {
		return SelectOutcome{Index: req.Default}
	}
	return SelectOutcome{Index: m.cursor, Answered: true}
}

type selectModel struct {
	req      itemsRequest
	cursor   int
	answered bool
	done     bool
	r        *Renderer
}

// itemsRequest keeps the model independent of the outer request's TTY field.
type itemsRequest struct {
	title   string
	options []SelectOption
}

func newSelectModel(req SelectRequest, r *Renderer) selectModel {
	cursor := req.Default
	if cursor < 0 || cursor >= len(req.Options) {
		cursor = 0
	}
	return selectModel{req: itemsRequest{title: req.Title, options: req.Options}, cursor: cursor, r: r}
}

func (m selectModel) Init() tea.Cmd { return nil }

func (m selectModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch key.Type {
	case tea.KeyUp:
		if m.cursor > 0 {
			m.cursor--
		}
		return m, nil
	case tea.KeyDown:
		if m.cursor < len(m.req.options)-1 {
			m.cursor++
		}
		return m, nil
	case tea.KeyEnter:
		m.answered, m.done = true, true
		return m, tea.Quit
	case tea.KeyEsc, tea.KeyCtrlC:
		m.done = true
		return m, tea.Quit
	case tea.KeyRunes:
		switch string(key.Runes) {
		case "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "j":
			if m.cursor < len(m.req.options)-1 {
				m.cursor++
			}
		case "q":
			m.done = true
			return m, tea.Quit
		default:
			// A typed option number answers directly: "1" is the first option.
			for i := range m.req.options {
				if string(key.Runes) == fmt.Sprintf("%d", i+1) {
					m.cursor, m.answered, m.done = i, true, true
					return m, tea.Quit
				}
			}
		}
	}
	return m, nil
}

func (m selectModel) View() string {
	if m.done {
		// The answered prompt erases itself; the settled phase line is the
		// record, exactly like the consent block.
		return ""
	}
	t := m.r.TUI()
	var b strings.Builder
	b.WriteString("  " + t.Bold(t.Paint(tui.CLIAccent, m.req.title)) + "\n")
	for i, opt := range m.req.options {
		cursor := "  "
		label := opt.Label
		if i == m.cursor {
			cursor = t.Paint(tui.CLIAccent, "> ")
			label = t.Bold(label)
		}
		line := fmt.Sprintf("  %s%d) %s", cursor, i+1, label)
		if opt.Detail != "" {
			line += t.Paint(tui.CLIMuted, " — "+opt.Detail)
		}
		b.WriteString(line + "\n")
	}
	b.WriteString(t.Paint(tui.CLIMuted, "  ↑/↓ move · enter select · esc skip") + "\n")
	return b.String()
}
