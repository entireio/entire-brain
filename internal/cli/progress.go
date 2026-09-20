package cli

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/entireio/entire-brain/internal/tui"
)

// progress.go renders the running commentary of a long command. It has exactly
// two modes and they are not variations of each other:
//
//   - TERMINAL: ONE line, rewritten in place, carrying a spinner, an optional
//     determinate bar and the current label. Repaints are capped at
//     liveRepaintsPerSecond and the line is truncated to the terminal width.
//   - PLAIN (a pipe, a log, a hook, --json's io.Discard): whole lines, ASCII,
//     no colour, no control characters, byte-stable.
//
// Three bugs are designed out here rather than patched:
//
//  1. DOUBLE PRINTS. The old plain path printed on Begin, again on every Update
//     and again on Finish, so the overwhelmingly common `task.Update(summary);
//     task.Finish(nil)` pair emitted the summary twice — once bare and once with
//     " done". A plain task now buffers its current label and emits ONE line per
//     distinct label: when the label's meaning changes, the previous one is
//     flushed; Finish flushes the last one with its outcome suffix. One phase,
//     one line, always.
//
//  2. FIGHTING LIVE LINES. `setup` painted a spinner on stdout while the refresh
//     it called painted its own on stderr; both rewrote the same physical row,
//     so neither cleared the other and long lines wrapped into a visible
//     duplicate. Live lines are now owned by one process-wide stack: only the
//     innermost task paints, the outer one is cleared while it is suspended and
//     repainted when the inner one finishes.
//
//  3. FLOOD. The semantic stream reports every few hundred records. Repaints are
//     rate limited to liveRepaintsPerSecond, and plain-mode labels that differ
//     only in their counters are coalesced instead of emitted one per tick.

var (
	// progressCountPattern folds ": 12/100 metadata files" so two updates that
	// differ only in a counter are the same PHASE and can be coalesced.
	progressCountPattern = regexp.MustCompile(`: \d+/\d+ [^,]+`)
	// progressSessionCountPattern folds a trailing ", 3 sessions".
	progressSessionCountPattern = regexp.MustCompile(`, \d+ sessions?$`)
	// progressParenCountPattern folds "(40781 symbols, 131529 relations)" — the
	// semantic stream's shape, which is the one that flooded: it changes on
	// every callback and matched neither pattern above, so every single
	// repaint counted as a new phase.
	progressParenCountPattern = regexp.MustCompile(`\(\d[\d,]*\s[^)]*\)`)
	// progressBareCountPattern folds a bare ", 12 records embedded" style tail.
	progressBareCountPattern = regexp.MustCompile(`\b\d[\d,]*\b`)
)

const clearTerminalLine = "\r\033[2K"

// liveRepaintInterval caps in-place repaints at 10 per second. Anything faster
// is invisible to a human and visibly tears on a slow terminal.
const liveRepaintInterval = 100 * time.Millisecond

// plainCoalesceInterval is how long the plain path will keep coalescing updates
// that differ only in their counters before conceding that the phase is
// long-running and emitting a fresh progress line.
const plainCoalesceInterval = time.Second

// liveTerminal owns the single in-place line shared by every progress reporter
// in the process. Progress reporters do not write to the terminal directly;
// they ask this to paint for them, which is what keeps a nested refresh from
// stamping over the setup line that spawned it.
type liveTerminal struct {
	mu    sync.Mutex
	stack []*refreshProgressTask
	// writer is where the current live line physically sits — stdout for setup,
	// stderr for refresh. Clearing has to go to the writer that painted, not to
	// whichever one is asking now.
	writer  io.Writer
	painted bool
	lastAt  time.Time
	now     func() time.Time
}

func newLiveTerminal() *liveTerminal { return &liveTerminal{now: time.Now} }

// processLive is the one live line this process has. It is package state on
// purpose: the whole point is that independently constructed reporters writing
// to two different streams still coordinate.
var processLive = newLiveTerminal()

func (l *liveTerminal) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// push makes t the live task, suspending whatever was live before.
func (l *liveTerminal) push(t *refreshProgressTask) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.clearLocked()
	l.stack = append(l.stack, t)
	l.paintLocked(t, true)
}

// update repaints t, subject to the rate limit. A task that is not on top is
// suspended: its label is already recorded, and it will be repainted with the
// current text when it comes back to the top.
func (l *liveTerminal) update(t *refreshProgressTask, force bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.stack) == 0 || l.stack[len(l.stack)-1] != t {
		return
	}
	l.paintLocked(t, force)
}

// finish removes t, clears the live line, runs emit (which writes t's single
// permanent line), then restores whichever task was suspended underneath.
func (l *liveTerminal) finish(t *refreshProgressTask, emit func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, candidate := range l.stack {
		if candidate == t {
			l.stack = append(l.stack[:i], l.stack[i+1:]...)
			break
		}
	}
	l.clearLocked()
	if emit != nil {
		emit()
	}
	if len(l.stack) > 0 {
		l.paintLocked(l.stack[len(l.stack)-1], true)
	}
}

// emitAround clears the live line, writes a permanent line, and repaints. It is
// how a caller prints an ordinary line without shredding the spinner.
func (l *liveTerminal) emitAround(emit func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.clearLocked()
	if emit != nil {
		emit()
	}
	if len(l.stack) > 0 {
		l.paintLocked(l.stack[len(l.stack)-1], true)
	}
}

func (l *liveTerminal) clearLocked() {
	if !l.painted || l.writer == nil {
		return
	}
	fmt.Fprint(l.writer, clearTerminalLine)
	l.painted = false
}

func (l *liveTerminal) paintLocked(t *refreshProgressTask, force bool) {
	if t == nil || t.progress == nil || t.progress.out == nil {
		return
	}
	if !force && !l.lastAt.IsZero() && l.clock().Sub(l.lastAt) < liveRepaintInterval {
		return
	}
	if l.painted && l.writer != nil && l.writer != t.progress.out {
		fmt.Fprint(l.writer, clearTerminalLine)
	}
	fmt.Fprint(t.progress.out, clearTerminalLine+t.liveLine())
	l.writer = t.progress.out
	l.painted = true
	l.lastAt = l.clock()
}

type refreshProgress struct {
	out    io.Writer
	prefix string
	render *tui.Renderer
	live   *liveTerminal
	// phase colour-codes every line this reporter emits.
	phase tui.Phase
	mu    sync.Mutex
}

type refreshProgressTask struct {
	progress *refreshProgress
	mu       sync.Mutex
	label    string
	phase    tui.Phase
	// done/total drive the determinate bar. total <= 0 means indeterminate and
	// renders as a spinner with no bar, which is honest: a bar that cannot say
	// how full it is must not imply a fraction.
	done, total int
	// marks is the per-component tick row (instant phase), rendered beside the
	// spinner so a reader watches components land one by one.
	marks []tui.Mark
	tick  int
	// plainPending is the label the plain path has NOT emitted yet. It is the
	// mechanism that guarantees one line per phase: nothing is written until the
	// phase changes or the task ends.
	plainPending string
	plainHasPend bool
	plainKey     string
	plainPendAt  time.Time
	// finished makes Finish idempotent. Several call sites legitimately finish a
	// task on more than one path (an error branch and a shared cleanup, say),
	// and a second terminal line for the same task is exactly the duplicate this
	// renderer exists to eliminate.
	finished bool
	// plainLast is the last line the plain path actually wrote. Finish compares
	// against it so a task whose terminal state was already announced (by an
	// Event, say) does not restate it with a " done" suffix — that restatement
	// IS the double-print bug, just one call site further along.
	plainLast     string
	spinnerDone   chan struct{}
	spinnerHalted chan struct{}
}

func newRefreshProgress(out io.Writer) *refreshProgress {
	return newProgress(out, "refresh")
}

// newProgress builds a progress reporter whose plain-mode lines are prefixed
// with the given command name (for example "refresh" or "setup"). On a terminal
// it renders a single in-place spinner line instead.
func newProgress(out io.Writer, prefix string) *refreshProgress {
	return newProgressWithRenderer(out, prefix, tui.NewRenderer(out), processLive)
}

// newProgressWithRenderer is newProgress with the capability detection and the
// live-line owner injected. Rendering behaviour is a contract worth testing —
// TTY versus pipe versus NO_COLOR versus a non-UTF-8 locale are four different
// byte streams — and none of it is assertable if the only way to build a
// reporter is to hand it a real terminal.
func newProgressWithRenderer(out io.Writer, prefix string, render *tui.Renderer, live *liveTerminal) *refreshProgress {
	if live == nil {
		live = processLive
	}
	return &refreshProgress{out: out, prefix: prefix, render: render, live: live}
}

// withPhase returns a reporter that colour-codes its lines for a phase.
func (p *refreshProgress) withPhase(phase tui.Phase) *refreshProgress {
	if p == nil {
		return nil
	}
	return &refreshProgress{out: p.out, prefix: p.prefix, render: p.render, live: p.live, phase: phase}
}

func (p *refreshProgress) tty() bool {
	return p != nil && p.render != nil && p.render.Caps().TTY
}

func (p *refreshProgress) Step(label string) func(error) {
	task := p.Begin(label)
	return task.Finish
}

func (p *refreshProgress) Begin(label string) *refreshProgressTask {
	return p.BeginPhase(label, p.phase)
}

// BeginPhase starts a task rendered in a phase's colour.
func (p *refreshProgress) BeginPhase(label string, phase tui.Phase) *refreshProgressTask {
	if p == nil || p.out == nil {
		return &refreshProgressTask{}
	}
	task := &refreshProgressTask{
		progress:     p,
		label:        label,
		phase:        phase,
		plainPending: label,
		plainHasPend: true,
		plainKey:     progressStatusKey(label),
		plainPendAt:  time.Now(),
	}
	if p.tty() {
		task.spinnerDone = make(chan struct{})
		task.spinnerHalted = make(chan struct{})
		p.live.push(task)
		task.startSpinner()
		return task
	}
	// Plain mode announces the task the moment it starts, so a log reads in the
	// order things happened rather than in the order they ended. Nothing is
	// buffered yet: the deferral below only ever applies to LATER labels, which
	// is what stops the task's final label being printed twice.
	task.plainHasPend = false
	task.plainPending = ""
	task.plainLast = label
	p.writePlain(label)
	return task
}

// Skip reports a step that ran but had nothing to do.
func (p *refreshProgress) Skip(label string) {
	p.SkipPhase(label, tui.PhaseSkipped)
}

// SkipPhase is Skip in an explicit colour.
func (p *refreshProgress) SkipPhase(label string, phase tui.Phase) {
	if p == nil || p.out == nil {
		return
	}
	if p.tty() {
		p.live.emitAround(func() {
			fmt.Fprintf(p.out, "%s %s\n", p.render.Mark(tui.MarkSkipped), p.render.PhasePaint(phase, label+" skipped"))
		})
		return
	}
	p.writePlain(label + " skipped")
}

// printAroundLiveLine writes an ordinary line without shredding whatever
// in-place progress line is currently on screen: the live line is erased, the
// text is written, and the live line is repainted underneath it. Any code that
// prints to the same terminal a progress reporter is painting on must go
// through here, or its output is appended to the spinner row and then half
// erased by the next repaint.
func printAroundLiveLine(w io.Writer, format string, args ...any) {
	processLive.emitAround(func() { fmt.Fprintf(w, format, args...) })
}

// Note prints a plain informational line without disturbing the live line. It
// is for the one-off asides (a failure detail, a hint, a log path) that are not
// a step and must not look like one.
func (p *refreshProgress) Note(label string) { p.NotePhase(label, tui.PhaseNeutral) }

// NotePhase is Note in an explicit colour.
func (p *refreshProgress) NotePhase(label string, phase tui.Phase) {
	if p == nil || p.out == nil {
		return
	}
	if p.tty() {
		p.live.emitAround(func() {
			text := p.render.Dim(label)
			if phase != tui.PhaseNeutral {
				text = p.render.PhasePaint(phase, label)
			}
			fmt.Fprintf(p.out, "  %s\n", text)
		})
		return
	}
	p.writePlain(label)
}

// dash is an em dash where the destination can draw one and a plain "--" where
// it cannot. Callers building a sentence use it instead of writing "—" inline:
// a hard-coded em dash renders as a replacement box on a terminal whose locale
// is not UTF-8, which is where the stray glyph in the setup lines came from.
func (p *refreshProgress) dash() string {
	if p == nil || p.render == nil {
		return "--"
	}
	return p.render.Dash()
}

// writePlain emits one prefixed line. Every plain-mode line in this file goes
// through here, so the plain contract ("<prefix>: <text>\n", nothing else) is
// enforced in one place.
func (p *refreshProgress) writePlain(text string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprintf(p.out, "%s: %s\n", p.prefix, text)
}

// SetProgress attaches a determinate count to the task, which renders as a
// colour-ramped bar on a terminal and is folded into the label everywhere else.
// total <= 0 clears it back to indeterminate.
func (t *refreshProgressTask) SetProgress(done, total int) {
	if t == nil || t.progress == nil || t.progress.out == nil {
		return
	}
	t.mu.Lock()
	t.done, t.total = done, total
	t.mu.Unlock()
	if t.progress.tty() {
		t.progress.live.update(t, false)
	}
}

// SetMarks attaches a per-component tick row to the task.
func (t *refreshProgressTask) SetMarks(marks []tui.Mark) {
	if t == nil || t.progress == nil || t.progress.out == nil {
		return
	}
	t.mu.Lock()
	t.marks = append(t.marks[:0], marks...)
	t.mu.Unlock()
	if t.progress.tty() {
		t.progress.live.update(t, false)
	}
}

func (t *refreshProgressTask) Update(label string) {
	if t == nil || t.progress == nil || t.progress.out == nil {
		return
	}
	t.mu.Lock()
	t.label = label
	t.mu.Unlock()
	if t.progress.tty() {
		t.progress.live.update(t, false)
		return
	}
	t.plainUpdate(label)
}

// plainUpdate buffers the label and flushes the PREVIOUS one when the phase
// changes. Deferring by one is what removes the duplicate: the label that ends
// a task is emitted exactly once, by Finish, with its outcome suffix.
func (t *refreshProgressTask) plainUpdate(label string) {
	t.mu.Lock()
	key := progressStatusKey(label)
	var flush string
	switch {
	case !t.plainHasPend:
		t.plainPending, t.plainKey, t.plainPendAt, t.plainHasPend = label, key, time.Now(), true
	case key != t.plainKey:
		flush = t.plainPending
		t.plainPending, t.plainKey, t.plainPendAt = label, key, time.Now()
	case time.Since(t.plainPendAt) >= plainCoalesceInterval:
		// Same phase, but it has been running long enough that silence is worse
		// than a repeated line: emit where it has got to and keep going.
		flush = t.plainPending
		t.plainPending, t.plainPendAt = label, time.Now()
	default:
		t.plainPending = label
	}
	t.mu.Unlock()
	if flush != "" {
		t.progress.writePlain(flush)
		t.mu.Lock()
		t.plainLast = flush
		t.mu.Unlock()
	}
}

// Event reports a DISCRETE completion — one distilled session, one indexed
// branch — rather than a repaint of the same work. The distinction is load
// bearing: Update coalesces (a counter ticking inside one phase must not
// produce a line per tick), and coalescing a per-item event would silently
// delete the only per-session heartbeat a detached backfill log has. On a
// terminal both are the same in-place repaint; only the plain path differs.
func (t *refreshProgressTask) Event(label string) {
	if t == nil || t.progress == nil || t.progress.out == nil {
		return
	}
	t.mu.Lock()
	t.label = label
	pending, hadPending := t.plainPending, t.plainHasPend
	t.plainPending, t.plainKey, t.plainHasPend = "", "", false
	t.mu.Unlock()
	if t.progress.tty() {
		t.progress.live.update(t, false)
		return
	}
	// Flush whatever phase line was still buffered, then the event itself, so
	// the event never swallows the phase it happened inside.
	if hadPending && pending != "" && pending != label {
		t.progress.writePlain(pending)
		t.mu.Lock()
		t.plainLast = pending
		t.mu.Unlock()
	}
	t.progress.writePlain(label)
	t.mu.Lock()
	t.plainLast = label
	t.mu.Unlock()
}

func (t *refreshProgressTask) Finish(err error) {
	if t == nil || t.progress == nil || t.progress.out == nil {
		return
	}
	t.mu.Lock()
	if t.finished {
		t.mu.Unlock()
		return
	}
	t.finished = true
	t.mu.Unlock()
	t.stopSpinner()
	tty := t.progress.tty()
	t.mu.Lock()
	label := t.label
	// The plain path's buffered label is authoritative there: it is the one
	// thing that has NOT been emitted yet, and emitting it here — exactly once,
	// with the outcome suffix — is what makes one phase produce one line.
	if !tty && t.plainHasPend && t.plainPending != "" {
		label = t.plainPending
	}
	plainLast := t.plainLast
	t.plainHasPend = false
	t.mu.Unlock()

	mark, phase, suffix := tui.MarkDone, tui.PhaseDone, " done"
	if err != nil {
		mark, phase, suffix = tui.MarkFailed, tui.PhaseFailed, " failed"
	}
	if !tty {
		// A task whose last plain line already said exactly this needs no
		// second line to say it again with " done" bolted on. A FAILURE always
		// prints: silence is never an acceptable way to report one.
		if err == nil && label == plainLast {
			return
		}
		t.progress.writePlain(label + suffix)
		return
	}
	render := t.progress.render
	t.progress.live.finish(t, func() {
		line := label + suffix
		if err == nil && t.phase != tui.PhaseNeutral {
			phase = t.phase
		}
		// Budget this line exactly as liveLine budgets its own, and for the
		// same reason: Finish writes onto the row the in-place repaints own,
		// and clearTerminalLine erases exactly ONE row. A terminal line wider
		// than the terminal wraps onto a second row, the repaint that follows
		// lands on the second row, and every later erase clears only that one —
		// so the first row is stranded on screen for the rest of the run.
		// Seen at 130 cells on a 120-column terminal: "instant core ready in
		// 1m4.4s — built sessions, seed, docs, semantic, ...; the brain is
		// queryable now done".
		if width := render.Width(); width > 0 {
			used := utf8.RuneCountInString(render.MarkGlyph(mark)) + 1
			budget := max(0, width-1-used)
			if budget >= utf8.RuneCountInString(suffix) {
				line = render.TruncateTo(label, budget-utf8.RuneCountInString(suffix)) + suffix
			} else {
				line = render.TruncateTo(strings.TrimSpace(suffix), budget)
			}
		}
		fmt.Fprintf(t.progress.out, "%s %s\n", render.Mark(mark), render.PhasePaint(phase, line))
	})
}

func (t *refreshProgressTask) stopSpinner() {
	if t.spinnerDone == nil {
		return
	}
	close(t.spinnerDone)
	<-t.spinnerHalted
	t.spinnerDone = nil
}

func (t *refreshProgressTask) startSpinner() {
	go func() {
		defer close(t.spinnerHalted)
		ticker := time.NewTicker(liveRepaintInterval)
		defer ticker.Stop()
		for {
			select {
			case <-t.spinnerDone:
				return
			case <-ticker.C:
				t.mu.Lock()
				t.tick++
				t.mu.Unlock()
				t.progress.live.update(t, true)
			}
		}
	}()
}

// liveBarWidth is the cell width of the determinate bar on the live line. Wide
// enough to read a fraction from across a desk, narrow enough to leave the
// label room on an 80-column terminal.
const liveBarWidth = 16

// liveMinLabelWidth is the fewest label cells worth rendering. Below it the
// line says nothing, so the gauge is dropped to make room.
const liveMinLabelWidth = 20

// liveLine composes the in-place line: spinner, optional bar or tick row, then
// the label, truncated so it can never wrap. It is built as plain text first
// and coloured afterwards, because width has to be measured without counting
// escape bytes.
func (t *refreshProgressTask) liveLine() string {
	t.mu.Lock()
	label, phase, tick := t.label, t.phase, t.tick
	done, total := t.done, t.total
	marks := append([]tui.Mark(nil), t.marks...)
	t.mu.Unlock()

	render := t.progress.render
	spinner := render.Spinner(tick)
	var gauge, gaugePlain string
	switch {
	case total > 0:
		bar := render.Bar(done, total, liveBarWidth)
		ratio := fmt.Sprintf("%d/%d", done, total)
		gauge = bar + " " + render.Ratio(done, total)
		gaugePlain = strings.Repeat("#", liveBarWidth) + " " + ratio
	case len(marks) > 0:
		var painted, plain strings.Builder
		for _, mark := range marks {
			painted.WriteString(render.Mark(mark))
			plain.WriteString("x")
		}
		gauge, gaugePlain = painted.String(), plain.String()
	}

	// Budget: spinner + space [+ gauge + space] + label. Widths are counted in
	// RUNES, not bytes: a braille spinner frame is three bytes and one cell, and
	// charging it three would shrink the label for no reason.
	width := render.Width()
	used := utf8.RuneCountInString(spinner)
	partCount := 2 // spinner + label
	if gaugePlain != "" {
		used += utf8.RuneCountInString(gaugePlain)
		partCount++
	}
	// strings.Join inserts one cell between every pair of rendered parts.
	used += partCount - 1
	// A terminal too narrow to hold the gauge AND a readable label loses the
	// gauge, not the label: the label says what is happening, the gauge only
	// says how far along it is. Keeping both would overflow the row and wrap,
	// which is the failure mode this whole line-budget exists to prevent.
	if width > 0 && gaugePlain != "" && used+liveMinLabelWidth > width-1 {
		gauge, gaugePlain = "", ""
		used = utf8.RuneCountInString(spinner) + 1 // spinner + join space + label
	}
	limit := utf8.RuneCountInString(label)
	if width > 0 {
		limit = width - 1 - used
		if limit < 0 {
			limit = 0
		}
	}
	parts := []string{render.PhasePaint(phase, spinner)}
	if gauge != "" {
		parts = append(parts, gauge)
	}
	parts = append(parts, render.PhasePaint(phase, render.TruncateTo(label, limit)))
	return strings.Join(parts, " ")
}

// progressStatusKey folds a label down to its PHASE, discarding the counters
// that change on every callback. Two labels with the same key describe the same
// piece of work and are coalesced into one line.
func progressStatusKey(label string) string {
	label = progressCountPattern.ReplaceAllString(label, "")
	label = progressSessionCountPattern.ReplaceAllString(label, "")
	label = progressParenCountPattern.ReplaceAllString(label, "")
	// Anything numeric that survived the shaped patterns above is still a
	// counter: "history vectors: 412 new records embedded" is one phase, not
	// four hundred. Folding every number is deliberately aggressive — the cost
	// of over-folding is a line that updates less often, and the cost of
	// under-folding is the flood this exists to stop.
	return progressBareCountPattern.ReplaceAllString(label, "#")
}
