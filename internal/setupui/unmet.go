package setupui

import (
	"strings"

	"github.com/entireio/entire-brain/internal/tui"
)

// unmet.go draws the line between "this is broken" and "this has nothing to
// work on yet". They are not the same event and they must not render the same.
//
// The measurement that forced this file: a repo with no commits produced 47
// lines and three red component failures (seed baseline, semantic index,
// entity index), while the neighbouring case — a directory that is not a git
// repo at all — produced 2 lines, named the one thing to do, and exited. Same
// class of problem, twenty-three times the output for the EASIER case, and the
// louder rendering was the one describing a repo where nothing is wrong.
//
// A phase whose inputs do not exist yet reports a Precondition: the condition
// once, the action once, a skipped glyph. It never reports a failure, it never
// enumerates the components that had nothing to read, and it never colours
// itself red. The distinction is carried by the GLYPH and the wording, so it
// survives NO_COLOR and a non-UTF-8 locale intact.

// Precondition is an input that does not exist yet.
type Precondition struct {
	// Condition is what is not there, stated once. "no commits yet, so there
	// is nothing to index".
	Condition string
	// Action is the one runnable thing that changes it.
	Action string
}

// Unmet settles a phase whose inputs do not exist. It emits exactly two rows:
// the condition and the action. It is NOT recorded as a failure, so it does
// not reach the summary's degraded list and it does not argue for a non-zero
// exit.
func (p *PhaseRun) Unmet(pre Precondition) {
	condition := strings.TrimSpace(pre.Condition)
	if condition == "" {
		condition = "nothing to do yet"
	}
	action := strings.TrimSpace(pre.Action)
	if action == "" {
		p.settle(StateSkipped, condition, nil)
		return
	}
	p.settle(StateSkipped, condition, []string{action})
}

// Blocked renders the refusal that setup gets RIGHT today and that the rest of
// this package is being shaped towards: what is wrong, why it matters, and the
// runnable ways out. Nothing else — no phases, no glyph run, no component
// list.
//
// Prose re-flows here rather than truncating. A phase line is a fact and loses
// nothing by being clipped; an explanation that is clipped has failed at the
// only job it had.
func Blocked(r *Renderer, condition, why string, actions []string) []Line {
	rows := make([]Line, 0, 6)
	for _, text := range r.WrapFlush(condition) {
		rows = append(rows, Line{Text: text, Accent: tui.PhaseFailed, Flush: true})
	}
	for _, text := range r.WrapFlush(why) {
		rows = append(rows, Line{Text: text, Flush: true})
	}
	for _, action := range actions {
		rows = append(rows, Line{Text: "run: " + action, Flush: true})
	}
	return rows
}
