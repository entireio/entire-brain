package setupui

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/entireio/entire-brain/internal/tui"
)

// Sink is the three-call contract between this package's model and whatever
// is physically painting. It is deliberately tiny:
//
//	Begin  open a transient line for a phase
//	Live   replace that transient line (a sub-step, a counter)
//	Settle close any transient line and write ONE permanent row
//
// The terminal implementation lives in internal/cli, which already owns the
// single process-wide live line, the spinner and the repaint rate limit.
// WriterSink below is the other implementation and it is the whole of what a
// pipe needs: Live is discarded, Settle is a line. That asymmetry is the
// entire difference between the two destinations, which is why "identical
// under a pipe, minus the repaints" can be asserted rather than reviewed.
//
// WIRING. The terminal implementation is about fifteen lines in package cli:
//
//	type setupSink struct {
//		progress *refreshProgress
//		render   *setupui.Renderer
//		task     *refreshProgressTask
//	}
//	func (s *setupSink) Begin(a tui.Phase, label string) { s.task = s.progress.BeginPhase(label, a) }
//	func (s *setupSink) Live(label string)               { s.task.Update(label) }
//	func (s *setupSink) Settle(line setupui.Line) {
//		if s.task != nil { s.task.Abandon(); s.task = nil }
//		s.progress.EmitLine(s.render.Line(line))
//	}
//
// It needs three purely additive hooks on the existing reporter, which this
// package deliberately does not add for itself:
//
//	func (p *refreshProgress) TTY() bool          // p.tty(), exported
//	func (p *refreshProgress) EmitLine(s string)  // Note without Note's formatting:
//	                                              // live.emitAround(fmt.Fprintln) on a
//	                                              // terminal, Fprintln otherwise
//	func (t *refreshProgressTask) Abandon()       // stopSpinner + live.finish(t, nil):
//	                                              // end the live line and write NOTHING,
//	                                              // so the caller composes the settled row
//
// Abandon is the one that cannot be worked around. Finish always writes
// label+" done"/" failed", which can express neither a skipped phase nor a
// composed row, and a settled line written any other way would be appended to
// the spinner's row and then half-erased by the next repaint.
type Sink interface {
	Begin(accent tui.Phase, label string)
	Live(label string)
	Settle(line Line)
}

// WriterSink writes permanent rows to w and discards transient ones. It has no
// spinner and no repaints by design, not by omission.
type WriterSink struct {
	mu sync.Mutex
	w  io.Writer
	r  *Renderer
}

// NewWriterSink builds the non-repainting sink.
func NewWriterSink(w io.Writer, r *Renderer) *WriterSink { return &WriterSink{w: w, r: r} }

// Begin opens nothing: there is no line to repaint.
func (s *WriterSink) Begin(tui.Phase, string) {}

// Live is discarded. A log is a record of what happened, not of what was
// momentarily on screen.
func (s *WriterSink) Live(string) {}

// Settle writes one row.
func (s *WriterSink) Settle(line Line) {
	if s == nil || s.w == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintln(s.w, s.r.Line(line))
}

// Run drives the seven phases and records every permanent row it emitted.
//
// The transcript is not debug scaffolding. "A sub-step never gets its own
// top-level line" is a property of the whole run, over every phase, and the
// only honest way to assert a property of the whole run is to hold the whole
// run.
type Run struct {
	mu    sync.Mutex
	sink  Sink
	rend  *Renderer
	lines []Line
	open  *PhaseRun
	// states records each phase's outcome for the verdict.
	states map[PhaseID]State
	// failures are the one-line reasons, in phase order.
	failures []Failure
}

// Failure is a phase that blew up, reduced to the one line it is allowed.
type Failure struct {
	Phase  PhaseID
	Reason string
}

// NewRun builds a run against a sink and a renderer.
func NewRun(sink Sink, rend *Renderer) *Run {
	return &Run{sink: sink, rend: rend, states: map[PhaseID]State{}}
}

// Renderer is the run's renderer, for callers composing their own fragments.
func (r *Run) Renderer() *Renderer { return r.rend }

// Lines is a copy of every permanent row emitted so far.
func (r *Run) Lines() []Line {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Line(nil), r.lines...)
}

// State reports how a phase ended, StateRunning if it has not.
func (r *Run) State(id PhaseID) State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.states[id]
}

// Failures lists the phases that failed, one line each.
func (r *Run) Failures() []Failure {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Failure(nil), r.failures...)
}

// emit records and paints one permanent row. Every row in this package goes
// through here; nothing else may call the sink's Settle.
func (r *Run) emit(line Line) {
	r.mu.Lock()
	r.lines = append(r.lines, line)
	sink := r.sink
	r.mu.Unlock()
	if sink != nil {
		sink.Settle(line)
	}
}

// Emit writes a row that belongs to no phase — the summary block's rows, a
// blank separator.
func (r *Run) Emit(line Line) { r.emit(line) }

// EmitAll writes a block of rows.
func (r *Run) EmitAll(lines []Line) {
	for _, line := range lines {
		r.emit(line)
	}
}

// Begin opens a phase. Opening a second phase while one is still open settles
// the first as skipped rather than silently interleaving two live lines; a
// caller that does this has a bug, and a missing line is a worse way to learn
// about it than a visible "skipped".
func (r *Run) Begin(id PhaseID) *PhaseRun {
	r.mu.Lock()
	previous := r.open
	r.mu.Unlock()
	if previous != nil {
		previous.Skip("not reported")
	}

	phase := &PhaseRun{run: r, id: id}
	r.mu.Lock()
	r.open = phase
	r.states[id] = StateRunning
	sink, rend := r.sink, r.rend
	r.mu.Unlock()
	if sink != nil {
		sink.Begin(id.Accent(), rend.LiveLabel(id.Title(), "starting", 0, 0))
	}
	return phase
}

// PhaseRun is one open phase. Its sub-step calls feed the transient line and
// cannot, structurally, reach Run.emit.
type PhaseRun struct {
	mu       sync.Mutex
	run      *Run
	id       PhaseID
	settled  bool
	steps    int
	label    string
	done     int
	total    int
	substeps []string
}

// ID is the phase this run belongs to.
func (p *PhaseRun) ID() PhaseID { return p.id }

// Steps is how many sub-steps reported. It exists so a settled summary can say
// "9 components" without the nine ever having been lines.
func (p *PhaseRun) Steps() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.steps
}

// Substeps is every sub-step label this phase saw, in order, for the settled
// sentence and for tests.
func (p *PhaseRun) Substeps() []string {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.substeps...)
}

// Step reports a sub-step. It repaints the phase's transient line and emits
// NOTHING permanent — not when it succeeds, not when there are nine of them,
// not ever. This is the whole anti-pytest invariant and it is enforced by this
// method having no path to Run.emit.
func (p *PhaseRun) Step(label string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.settled {
		p.mu.Unlock()
		return
	}
	p.steps++
	p.label = label
	p.substeps = append(p.substeps, label)
	done, total := p.done, p.total
	p.mu.Unlock()
	p.paint(label, done, total)
}

// Progress attaches a determinate count to the phase's transient line.
// total <= 0 is indeterminate.
func (p *PhaseRun) Progress(done, total int) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.settled {
		p.mu.Unlock()
		return
	}
	p.done, p.total = done, total
	label := p.label
	p.mu.Unlock()
	p.paint(label, done, total)
}

func (p *PhaseRun) paint(label string, done, total int) {
	p.run.mu.Lock()
	sink, rend := p.run.sink, p.run.rend
	p.run.mu.Unlock()
	if sink == nil {
		return
	}
	sink.Live(rend.LiveLabel(p.id.Title(), label, done, total))
}

// OK settles the phase. details become continuation rows under it — they are
// for facts the phase line cannot carry (what was detected, where it went),
// never for a replay of the sub-steps.
func (p *PhaseRun) OK(summary string, details ...string) {
	p.settle(StateOK, summary, details)
}

// Skip settles the phase as deliberately not done.
func (p *PhaseRun) Skip(reason string, details ...string) {
	p.settle(StateSkipped, reason, details)
}

// Fail settles the phase as failed and promotes EXACTLY ONE line beneath it.
// The error is flattened first: a multi-line stderr dump under a phase is the
// other half of the volume problem, and a reader who needs the dump has the
// log.
func (p *PhaseRun) Fail(summary string, err error) {
	reason := FlattenError(err)
	if reason == "" {
		reason = "no reason reported"
	}
	if p != nil {
		p.run.mu.Lock()
		p.run.failures = append(p.run.failures, Failure{Phase: p.id, Reason: reason})
		p.run.mu.Unlock()
	}
	p.settle(StateFailed, summary, []string{reason})
}

func (p *PhaseRun) settle(state State, summary string, details []string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.settled {
		p.mu.Unlock()
		return
	}
	p.settled = true
	p.mu.Unlock()

	p.run.mu.Lock()
	if p.run.open == p {
		p.run.open = nil
	}
	p.run.states[p.id] = state
	p.run.mu.Unlock()

	p.run.emit(Line{
		Title:  p.id.Title(),
		Text:   summary,
		Accent: p.id.Accent(),
		Glyph:  state.Glyph(),
	})
	// A failure is allowed one line and gets one line, whatever the caller
	// passed. Enforcing the cap here rather than trusting call sites is the
	// point: there are thirty-seven of them.
	if state == StateFailed && len(details) > 1 {
		details = details[:1]
	}
	for _, detail := range details {
		if strings.TrimSpace(detail) == "" {
			continue
		}
		p.run.emit(Line{Text: detail, Accent: p.id.Accent()})
	}
}

// Settled reports whether the phase has ended.
func (p *PhaseRun) Settled() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.settled
}

// flattenLimit caps a failure reason. Past this it is not a reason, it is a
// log, and it belongs in one.
const flattenLimit = 200

// FlattenError reduces an error to the single line a phase is allowed. Newlines
// become "; ", runs of whitespace collapse, and the result is clipped on a rune
// boundary.
func FlattenError(err error) string {
	if err == nil {
		return ""
	}
	return FlattenText(err.Error())
}

// FlattenText is FlattenError for a string that is already in hand.
func FlattenText(text string) string {
	fields := strings.FieldsFunc(text, func(r rune) bool { return r == '\n' || r == '\r' })
	parts := make([]string, 0, len(fields))
	for _, field := range fields {
		if collapsed := strings.Join(strings.Fields(field), " "); collapsed != "" {
			parts = append(parts, collapsed)
		}
	}
	out := strings.Join(parts, "; ")
	if utf8.RuneCountInString(out) > flattenLimit {
		runes := []rune(out)
		out = strings.TrimRight(string(runes[:flattenLimit-3]), " ;") + "..."
	}
	return out
}
