package setupui

import (
	"bytes"
	"github.com/entireio/entire-brain/internal/tui"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func testSelectRequest() SelectRequest {
	return SelectRequest{
		Title:   "Where should memories live?",
		Options: []SelectOption{{Label: "Hosted", Detail: "sync with your team"}, {Label: "Local", Detail: "this machine only"}},
		TTY:     true,
	}
}

func key(t tea.KeyType) tea.Msg { return tea.KeyMsg{Type: t} }

func runes(s string) tea.Msg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

func TestSelectModelMovesAndChooses(t *testing.T) {
	t.Parallel()
	m := newSelectModel(testSelectRequest(), rendererFor(tui.PlainCaps(), 60))
	next, _ := m.Update(key(tea.KeyDown))
	next, _ = next.(selectModel).Update(key(tea.KeyEnter))
	final := next.(selectModel)
	if !final.answered || final.cursor != 1 {
		t.Fatalf("down+enter must choose index 1, got answered=%v cursor=%d", final.answered, final.cursor)
	}
}

func TestSelectModelEscMeansUnanswered(t *testing.T) {
	t.Parallel()
	m := newSelectModel(testSelectRequest(), rendererFor(tui.PlainCaps(), 60))
	next, _ := m.Update(key(tea.KeyEsc))
	final := next.(selectModel)
	if final.answered || !final.done {
		t.Fatalf("esc must end the prompt unanswered, got %+v", final)
	}
}

func TestSelectModelViewListsOptionsThenErasesItself(t *testing.T) {
	t.Parallel()
	m := newSelectModel(testSelectRequest(), rendererFor(tui.PlainCaps(), 60))
	view := m.View()
	for _, want := range []string{"Hosted", "Local", "sync with your team"} {
		if !strings.Contains(view, want) {
			t.Errorf("view must list %q:\n%s", want, view)
		}
	}
	next, _ := m.Update(key(tea.KeyEnter))
	if got := next.(selectModel).View(); got != "" {
		t.Errorf("an answered prompt must erase itself, got %q", got)
	}
}

// The caller reports TTY capability explicitly (same contract as Consent).
// Without one, Select must not start a program, write a byte, or read a byte.
func TestSelectNoTTYNeverWritesEscapeCodes(t *testing.T) {
	t.Parallel()
	req := testSelectRequest()
	req.TTY = false
	req.Default = 1
	var out bytes.Buffer
	outcome := Select(req, strings.NewReader("0"), &out, rendererFor(tui.PlainCaps(), 60))
	if outcome.Answered || outcome.Index != 1 {
		t.Fatalf("no TTY must return the default unanswered, got %+v", outcome)
	}
	if out.Len() != 0 {
		t.Fatalf("no TTY must write nothing, got %q", out.String())
	}
}

func TestSelectModelShortcutRunePicksOption(t *testing.T) {
	t.Parallel()
	m := newSelectModel(testSelectRequest(), rendererFor(tui.PlainCaps(), 60))
	next, _ := m.Update(runes("2"))
	final := next.(selectModel)
	if !final.answered || final.cursor != 1 {
		t.Fatalf("typing an option number must choose it, got %+v", final)
	}
}

// Ctrl-C arrives as a key event under bubbletea's raw mode; it must mean
// "stop setup", not "dismiss the question and carry on installing".
func TestSelectModelCtrlCMeansInterrupted(t *testing.T) {
	t.Parallel()
	m := newSelectModel(testSelectRequest(), rendererFor(tui.PlainCaps(), 60))
	next, _ := m.Update(key(tea.KeyCtrlC))
	final := next.(selectModel)
	if final.answered || !final.done || !final.interrupted {
		t.Fatalf("ctrl-c must end the prompt interrupted, got %+v", final)
	}
	esc, _ := m.Update(key(tea.KeyEsc))
	if esc.(selectModel).interrupted {
		t.Fatal("esc is a dismissal, not an interrupt")
	}
}
