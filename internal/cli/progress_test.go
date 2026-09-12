package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ashtom/entire-brain/internal/tui"
	runewidth "github.com/mattn/go-runewidth"
)

// ttyCaps is a terminal that can do everything: colour, unicode, a known width.
func ttyCaps() tui.Caps { return tui.Caps{TTY: true, Color: true, Unicode: true, Width: 120} }

func newTestProgress(t *testing.T, out *bytes.Buffer, caps tui.Caps) *refreshProgress {
	t.Helper()
	theme, _ := tui.ThemeByName("default")
	return newProgressWithRenderer(out, "setup", tui.NewRendererWith(theme, caps), newLiveTerminal())
}

// visibleLines splits rendered output the way a terminal does: the in-place
// live line is repeatedly erased, so only what survives a "\r\033[2K" counts as
// a printed line.
func visibleLines(out string) []string {
	var lines []string
	for _, raw := range strings.Split(out, "\n") {
		// Everything before the last carriage-return erase on a physical row was
		// overwritten in place and never seen as a separate line.
		if idx := strings.LastIndex(raw, clearTerminalLine); idx >= 0 {
			raw = raw[idx+len(clearTerminalLine):]
		}
		if strings.TrimSpace(stripANSI(raw)) == "" {
			continue
		}
		lines = append(lines, stripANSI(raw))
	}
	return lines
}

// stripANSI removes SGR sequences so a test can assert on text.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			j := i + 1
			for j < len(s) && s[j] != 'm' && s[j] != 'K' && s[j] != 'J' {
				j++
			}
			i = j + 1
			continue
		}
		if s[i] == '\r' {
			i++
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// TestProgressPlainEmitsEachPhaseExactlyOnce is the regression test for the bug
// that started this: `task.Update(summary); task.Finish(nil)` printed the
// summary TWICE — once bare, once with " done" — for every phase of setup.
func TestProgressPlainEmitsEachPhaseExactlyOnce(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	p := newTestProgress(t, &buf, tui.PlainCaps())

	task := p.Begin("instant core (deterministic, no agent tokens)")
	task.Update("instant core ready in 700ms - built seed, docs, semantic; the brain is queryable now")
	task.Finish(nil)

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if got := strings.Count(buf.String(), "instant core ready in 700ms"); got != 1 {
		t.Fatalf("final phase line emitted %d times, want exactly 1:\n%s", got, buf.String())
	}
	assertNoDuplicateLines(t, lines)
}

// TestProgressPlainHasNoDuplicateLines walks a realistic setup transcript and
// asserts the whole plain stream is duplicate-free. This is the exact bug
// class, asserted at the level a reader experiences it: no line twice, ever.
func TestProgressPlainHasNoDuplicateLines(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	p := newTestProgress(t, &buf, tui.PlainCaps())

	instant := p.Begin("instant core (deterministic, no agent tokens)")
	instant.Update("instant core: export sessions")
	instant.Update("instant core: semantic index")
	instant.Update("instant core ready in 700ms - built seed, docs, semantic")
	instant.Finish(nil)

	p.Skip("fact backfill (--no-backfill)")

	daemon := p.Begin("background watcher")
	daemon.Update("background watcher installed (io.entire.brain-watch.a2d3fd66)")
	daemon.Finish(nil)

	p.Skip("session-end hook not wired")

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	assertNoDuplicateLines(t, lines)
	for _, want := range []string{
		"setup: instant core (deterministic, no agent tokens)",
		"setup: instant core ready in 700ms - built seed, docs, semantic done",
		"setup: fact backfill (--no-backfill) skipped",
		"setup: background watcher installed (io.entire.brain-watch.a2d3fd66) done",
	} {
		if !strings.Contains(buf.String(), want+"\n") {
			t.Errorf("plain output missing %q:\n%s", want, buf.String())
		}
	}
}

// assertNoDuplicateLines is the shape of the reported bug, asserted directly.
// It rejects two kinds of repeat:
//
//   - the same line twice, and
//   - the same line twice where the second copy only adds an outcome suffix,
//     which is exactly how "instant core ready in 700ms" appeared twice.
func assertNoDuplicateLines(t *testing.T, lines []string) {
	t.Helper()
	seen := map[string]int{}
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		seen[line]++
	}
	for line, count := range seen {
		if count > 1 {
			t.Errorf("line printed %d times (must be exactly once): %q", count, line)
		}
	}
	for line := range seen {
		for _, suffix := range []string{" done", " failed", " skipped"} {
			if seen[line+suffix] > 0 {
				t.Errorf("the same fact is printed twice, once bare and once with %q: %q", suffix, line)
			}
		}
	}
}

// TestProgressPlainIsByteStable pins the plain transcript byte for byte. A
// script or a CI log parsing this output must not have to cope with control
// characters, colour, or a spinner.
func TestProgressPlainIsByteStable(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	p := newTestProgress(t, &buf, tui.PlainCaps())

	task := p.Begin("instant core")
	task.SetProgress(300, 618)
	task.SetMarks([]tui.Mark{tui.MarkDone, tui.MarkFailed})
	task.Update("instant core ready")
	task.Finish(nil)
	p.Skip("fact backfill")
	p.Begin("background watcher").Finish(errors.New("launchctl: boom"))

	// A task announces itself when it starts and reports its outcome when it
	// ends. The two lines are never the same text: the outcome carries the
	// task's FINAL label. The one case where they would coincide — a task that
	// succeeds without ever updating its label — prints once, not twice.
	const want = "setup: instant core\n" +
		"setup: instant core ready done\n" +
		"setup: fact backfill skipped\n" +
		"setup: background watcher\n" +
		"setup: background watcher failed\n"
	if got := buf.String(); got != want {
		t.Fatalf("plain transcript\n got: %q\nwant: %q", got, want)
	}
}

// TestProgressPlainSilentSuccessPrintsOnce covers the degenerate task — begun
// and finished with no label change — which under the old renderer printed
// "<label>" and then "<label> done": the same fact, twice, one word apart.
func TestProgressPlainSilentSuccessPrintsOnce(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	p := newTestProgress(t, &buf, tui.PlainCaps())
	p.Step("branch overlays")(nil)
	if got := buf.String(); got != "setup: branch overlays\n" {
		t.Fatalf("silent success = %q, want one line", got)
	}
}

// TestProgressPlainCoalescesCounterFlood is the flood fix: the semantic stream
// reports "(N symbols, M relations)" hundreds of times a second, and the old
// key only folded "N/M unit" and ", N sessions", so every single callback
// counted as a new phase and got its own line.
func TestProgressPlainCoalescesCounterFlood(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	p := newTestProgress(t, &buf, tui.PlainCaps())

	task := p.Begin("semantic index")
	for i := 1; i <= 500; i++ {
		task.Update("semantic index: parsing sources (" + itoa(i*37) + " symbols, " + itoa(i*211) + " relations)")
	}
	task.Finish(nil)

	if got := strings.Count(buf.String(), "parsing sources"); got != 1 {
		t.Fatalf("500 counter-only updates emitted %d lines, want 1:\n%s", got, buf.String())
	}
	// The line that survives must carry the LATEST counts, not the first: a log
	// that stops at "37 symbols" is worse than no line at all.
	if !strings.Contains(buf.String(), "18500 symbols, 105500 relations") {
		t.Fatalf("coalesced line does not carry the latest counts:\n%s", buf.String())
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// TestProgressPlainStillReportsDistinctPhases guards the other direction: the
// coalescing must not swallow genuine phase changes.
func TestProgressPlainStillReportsDistinctPhases(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	p := newTestProgress(t, &buf, tui.PlainCaps())

	task := p.Begin("export sessions")
	task.Update("export sessions: detecting default branch")
	task.Update("export sessions: mapping checkpoint branches")
	task.Update("export sessions: reading checkpoint authors")
	task.Finish(nil)

	for _, want := range []string{"detecting default branch", "mapping checkpoint branches", "reading checkpoint authors done"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("distinct phase %q was swallowed:\n%s", want, buf.String())
		}
	}
}

// TestProgressTTYRendersOneLivePlusOneFinal locks the terminal contract: while
// a task runs there is exactly ONE in-place line, and when it ends there is
// exactly ONE permanent line.
func TestProgressTTYRendersOneLivePlusOneFinal(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	p := newTestProgress(t, &buf, ttyCaps())

	task := p.Begin("instant core")
	task.Update("instant core ready in 700ms")
	task.Finish(nil)

	out := buf.String()
	if n := strings.Count(out, "\n"); n != 1 {
		t.Fatalf("TTY task printed %d permanent lines, want 1:\n%q", n, out)
	}
	lines := visibleLines(out)
	if len(lines) != 1 || !strings.Contains(lines[0], "instant core ready in 700ms done") {
		t.Fatalf("visible TTY lines = %#v", lines)
	}
	assertNoDuplicateLines(t, lines)
}

// TestProgressTTYTruncatesToWidth is the other half of the "printed twice"
// report: a live line wider than the terminal WRAPS, the carriage-return erase
// only clears the last physical row, and the wrapped remainder stays on screen
// under the final line.
func TestProgressTTYTruncatesToWidth(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	caps := ttyCaps()
	caps.Width = 40
	p := newTestProgress(t, &buf, caps)

	task := p.Begin(strings.Repeat("very long label ", 20))
	task.SetProgress(3, 618)
	task.Update(strings.Repeat("still very long ", 20))
	task.Finish(nil)

	for _, painted := range strings.Split(buf.String(), clearTerminalLine) {
		// Only the in-place paints matter: a permanent line is allowed to wrap,
		// because nothing ever tries to erase it.
		if strings.Contains(painted, "\n") {
			continue
		}
		painted = stripANSI(painted)
		if painted == "" {
			continue
		}
		if width := len([]rune(painted)); width >= caps.Width {
			t.Fatalf("live paint is %d cells wide on a %d-column terminal (it will wrap): %q", width, caps.Width, painted)
		}
	}
}

func TestProgressGaugeBudgetIncludesJoinSpaces(t *testing.T) {
	t.Parallel()
	for width := 46; width <= 80; width++ {
		caps := ttyCaps()
		caps.Width = width
		var buf bytes.Buffer
		p := newTestProgress(t, &buf, caps)
		task := p.Begin(strings.Repeat("label ", 40))
		task.SetProgress(3, 618)
		line := stripANSI(task.liveLine())
		task.Finish(nil)
		if got := runewidth.StringWidth(line); got >= width {
			t.Fatalf("width %d: spinner, gauge, join spaces, and label consume %d cells: %q", width, got, line)
		}
	}
}

// TestProgressNestedTasksShareOneLiveLine reproduces the setup-calls-refresh
// collision: two reporters, two streams, one terminal row. Only the innermost
// task may paint, and the outer one must be erased while it is suspended.
func TestProgressNestedTasksShareOneLiveLine(t *testing.T) {
	t.Parallel()
	var outer, inner bytes.Buffer
	live := newLiveTerminal()
	theme, _ := tui.ThemeByName("default")
	render := tui.NewRendererWith(theme, ttyCaps())
	outerProgress := newProgressWithRenderer(&outer, "setup", render, live)
	innerProgress := newProgressWithRenderer(&inner, "refresh", render, live)

	outerTask := outerProgress.Begin("instant core")
	innerTask := innerProgress.Begin("export sessions")
	// While the inner task owns the line, the outer one must not paint.
	before := outer.Len()
	outerTask.Update("instant core: still working")
	if outer.Len() != before {
		t.Fatalf("suspended outer task painted while the inner task owned the line: %q", outer.String()[before:])
	}
	innerTask.Finish(nil)
	// Handing the line back must repaint the outer task, not leave a blank row.
	if outer.Len() == before {
		t.Fatal("outer task was not repainted after the inner task finished")
	}
	outerTask.Finish(nil)

	if n := strings.Count(inner.String(), "\n"); n != 1 {
		t.Errorf("inner task printed %d permanent lines, want 1", n)
	}
	if n := strings.Count(outer.String(), "\n"); n != 1 {
		t.Errorf("outer task printed %d permanent lines, want 1", n)
	}
}

// TestLiveTerminalRateLimitsRepaints pins the 10 frames-per-second cap that
// stops the semantic phase from tearing.
func TestLiveTerminalRateLimitsRepaints(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	live := newLiveTerminal()
	now := time.Unix(0, 0)
	live.now = func() time.Time { return now }
	theme, _ := tui.ThemeByName("default")
	p := newProgressWithRenderer(&buf, "setup", tui.NewRendererWith(theme, ttyCaps()), live)

	task := p.BeginPhase("semantic index", tui.PhaseInstant)
	task.stopSpinner() // the ticker owns its own clock; drive the rate limit directly
	paints := func() int { return strings.Count(buf.String(), clearTerminalLine) }
	after := paints()

	// Twenty updates inside one interval must produce no repaint at all.
	for i := 0; i < 20; i++ {
		now = now.Add(time.Millisecond)
		task.Update("semantic index: parsing sources")
	}
	if got := paints(); got != after {
		t.Fatalf("%d repaints inside one %v window, want 0", got-after, liveRepaintInterval)
	}
	// Crossing the interval must produce exactly one.
	now = now.Add(liveRepaintInterval)
	task.Update("semantic index: parsing sources")
	if got := paints() - after; got != 1 {
		t.Fatalf("crossing the rate-limit window produced %d repaints, want 1", got)
	}
}

// TestProgressNoColorProducesNoEscapes pins NO_COLOR: a terminal that asked for
// no colour still gets the in-place line, but not one SGR byte.
func TestProgressNoColorProducesNoEscapes(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	caps := ttyCaps()
	caps.Color = false
	p := newTestProgress(t, &buf, caps)

	task := p.BeginPhase("instant core", tui.PhaseInstant)
	task.SetProgress(300, 618)
	task.Finish(nil)
	p.SkipPhase("fact backfill", tui.PhaseBackfill)

	if strings.Contains(buf.String(), "\x1b[3") || strings.Contains(buf.String(), "\x1b[1m") {
		t.Fatalf("NO_COLOR output still carries SGR sequences: %q", buf.String())
	}
	if !strings.Contains(buf.String(), clearTerminalLine) {
		t.Fatal("NO_COLOR must still get the in-place line; only colour is off")
	}
}

// TestProgressASCIIFallbackHasNoNonASCII is the "□" fix: a terminal whose
// locale cannot promise UTF-8 must never be sent a braille spinner, a block
// bar, a check mark or an em dash.
func TestProgressASCIIFallbackHasNoNonASCII(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	caps := ttyCaps()
	caps.Unicode = false
	p := newTestProgress(t, &buf, caps)

	task := p.BeginPhase("instant core", tui.PhaseInstant)
	task.SetProgress(300, 618)
	task.SetMarks([]tui.Mark{tui.MarkDone, tui.MarkFailed, tui.MarkSkipped, tui.MarkPending})
	task.Update("instant core ready")
	task.Finish(nil)
	p.SkipPhase("fact backfill", tui.PhaseBackfill)
	p.Begin("background watcher").Finish(errors.New("boom"))

	for i, r := range buf.String() {
		if r > 127 {
			t.Fatalf("non-ASCII rune %q at byte %d in an ASCII-only terminal: %q", r, i, buf.String())
		}
	}
}

// TestProgressFinishIsIdempotent guards the duplicate that a double-finish
// would produce. Several call sites finish a task on more than one path.
func TestProgressFinishIsIdempotent(t *testing.T) {
	t.Parallel()
	var plain bytes.Buffer
	task := newTestProgress(t, &plain, tui.PlainCaps()).Begin("export sessions")
	task.Update("export sessions: 3 read")
	task.Finish(nil)
	task.Finish(nil)
	task.Finish(errors.New("late"))
	if got := strings.Count(plain.String(), "export sessions: 3 read done"); got != 1 {
		t.Fatalf("plain double-finish emitted %d terminal lines:\n%s", got, plain.String())
	}

	var tty bytes.Buffer
	ttyTask := newTestProgress(t, &tty, ttyCaps()).Begin("export sessions")
	ttyTask.Finish(nil)
	ttyTask.Finish(nil)
	if got := strings.Count(tty.String(), "\n"); got != 1 {
		t.Fatalf("TTY double-finish printed %d permanent lines:\n%q", got, tty.String())
	}
}

// TestProgressTTYNarrowTerminalDropsTheGaugeNotTheLabel pins the line budget on
// a terminal too narrow for both. The label says WHAT is happening; the gauge
// only says how far along. Keeping both would overflow the row and wrap, which
// is the failure mode the whole budget exists to prevent.
func TestProgressTTYNarrowTerminalDropsTheGaugeNotTheLabel(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	caps := ttyCaps()
	caps.Width = 30
	p := newTestProgress(t, &buf, caps)

	task := p.Begin("export sessions")
	task.SetProgress(300, 618)
	task.Finish(nil)

	for _, painted := range strings.Split(buf.String(), clearTerminalLine) {
		if strings.Contains(painted, "\n") {
			continue
		}
		plain := stripANSI(painted)
		if plain == "" {
			continue
		}
		if width := len([]rune(plain)); width >= caps.Width {
			t.Fatalf("live paint is %d cells on a %d-column terminal: %q", width, caps.Width, plain)
		}
		if strings.Contains(plain, "█") || strings.Contains(plain, "300/618") {
			t.Errorf("the gauge should have been dropped on a narrow terminal: %q", plain)
		}
		if !strings.Contains(plain, "export sessions") {
			t.Errorf("the label must survive: %q", plain)
		}
	}
}

// TestFinishTerminalLineFitsTheTerminal pins the width budget on the line
// Finish prints on a terminal.
//
// Finish writes onto the SAME single row the in-place repaints own, and
// clearTerminalLine ("\r\033[2K") erases exactly one row. A terminal line wider
// than the terminal wraps onto a second row; the repaint that liveTerminal.finish
// issues afterwards lands on that second row, and every later erase clears only
// that one — leaving the first row stranded on screen for the rest of the run.
// liveLine has budgeted its own width from the start; Finish did not, which is
// how a real setup produced a 130-cell line on a 120-column terminal:
//
//	✓ instant core ready in 1m4.4s — built sessions, seed, docs, semantic,
//	  patterns, entities, memory; the brain is queryable now done
func TestFinishTerminalLineFitsTheTerminal(t *testing.T) {
	t.Parallel()
	caps := ttyCaps()
	var buf bytes.Buffer
	p := newTestProgress(t, &buf, caps)
	// The real label that overflowed, reconstructed: a seven-component summary
	// is the ordinary outcome of a successful instant phase, not a pathological
	// input.
	task := p.Begin("instant core ready in 1m4.4s — built sessions, seed, docs, semantic, patterns, entities, memory; the brain is queryable now")
	task.Finish(nil)

	for _, line := range visibleLines(buf.String()) {
		if got := runewidth.StringWidth(line); got > caps.Width {
			t.Fatalf("terminal line is %d cells wide on a %d-column terminal, so it wraps and strands a row: %q",
				got, caps.Width, line)
		}
	}
}

// TestFinishTerminalLineKeepsShortLabelsWhole guards the fix from overshooting:
// a label that already fits must be printed verbatim, suffix and all. A budget
// that truncated everything would hide the outcome word the line exists to say.
func TestFinishTerminalLineKeepsShortLabelsWhole(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	p := newTestProgress(t, &buf, ttyCaps())
	p.Begin("workspace registration").Finish(nil)

	lines := visibleLines(buf.String())
	if len(lines) != 1 {
		t.Fatalf("expected exactly one terminal line, got %d: %q", len(lines), lines)
	}
	if !strings.Contains(lines[0], "workspace registration done") {
		t.Fatalf("terminal line lost its label or suffix: %q", lines[0])
	}
}

func TestProgressVeryNarrowLineAndFailureSuffix(t *testing.T) {
	for _, width := range []int{10, 20, 30} {
		var buf bytes.Buffer
		caps := ttyCaps()
		caps.Width = width
		p := newTestProgress(t, &buf, caps)
		task := p.Begin(strings.Repeat("long label ", 10))
		if got := runewidth.StringWidth(stripANSI(task.liveLine())); got >= width {
			t.Errorf("live width %d >= %d", got, width)
		}
		task.Finish(errors.New("failed"))
		if !strings.Contains(stripANSI(buf.String()), " failed") {
			t.Errorf("failure hidden at width %d: %s", width, stripANSI(buf.String()))
		}
	}
}
