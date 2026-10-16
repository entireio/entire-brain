package setupui

import (
	"strings"

	"github.com/entireio/entire-brain/internal/tui"
)

// summary.go is the block that used to re-list every component a third time.
// It now shows only what the phase lines structurally could not: the two paths
// nobody can infer, the verdict, each failure's one line, and the commands to
// run next.

// NextStep is a command the reader can run, with why.
type NextStep struct {
	Command string
	Why     string
}

// Summary is the Ready phase's payload.
type Summary struct {
	// RepoPath and BrainPath are the two locations the phase lines never
	// carried. They are the whole reason this block still exists.
	RepoPath  string
	BrainPath string
	// Queryable decides the verdict and, upstream, the exit code: setup exits
	// 0 whenever the brain can answer a question.
	Queryable bool
	// Failures are the one-line reasons, already flattened.
	Failures []Failure
	// Next are the runnable follow-ups.
	Next []NextStep
}

// Verdict is the Ready phase's own line.
func (s Summary) Verdict(r *Renderer) string {
	if !s.Queryable {
		return "nothing usable was built " + r.Dash() + " the brain cannot answer yet"
	}
	if len(s.Failures) == 0 {
		return "the brain is queryable"
	}
	return "the brain is queryable " + r.Dash() + " " + plural(len(s.Failures), "phase") + " degraded"
}

// Lines are the continuation rows under the Ready phase.
func (s Summary) Lines(r *Renderer) []Line {
	rows := make([]Line, 0, 4+len(s.Failures)+len(s.Next))
	if s.RepoPath != "" {
		rows = append(rows, Line{Text: detailRow("repo", s.RepoPath), Accent: PhaseReady.Accent()})
	}
	if s.BrainPath != "" {
		rows = append(rows, Line{Text: detailRow("brain", s.BrainPath), Accent: PhaseReady.Accent()})
	}
	// A failure is restated here ONLY as its one line, and only because the
	// phase that failed has scrolled away by now.
	for _, failure := range s.Failures {
		rows = append(rows, Line{
			Text:   detailRow(strings.ToLower(failure.Phase.Title()), FlattenText(failure.Reason)),
			Accent: tui.PhaseFailed,
			Glyph:  GlyphFailed,
		})
	}
	for _, next := range s.Next {
		text := next.Command
		if next.Why != "" {
			text += "  " + r.Dash() + " " + next.Why
		}
		rows = append(rows, Line{Text: detailRow("next", text), Accent: PhaseReady.Accent()})
	}
	return rows
}

// Report settles the Ready phase with the verdict, then writes this block as
// its own rows. The rows are emitted through the run rather than folded into
// the phase's details because a failure row keeps its own glyph: a degraded
// phase restated without its mark reads as a note, and it is not one.
func (s Summary) Report(run *Run, phase *PhaseRun, r *Renderer) {
	if s.Queryable {
		phase.OK(s.Verdict(r))
	} else {
		phase.Skip(s.Verdict(r))
	}
	run.EmitAll(s.Lines(r))
}
