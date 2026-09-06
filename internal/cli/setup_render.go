package cli

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/ashtom/entire-brain/internal/tui"
)

// setup_render.go is the presentation half of `setup`. It is deliberately
// separate from setup.go: the orchestration there decides WHAT happened, this
// decides how it looks, and the two must not be able to drift into each other.
//
// Everything here obeys the same three rules:
//
//   - The report is never re-derived. Every line is rendered from the
//     setupReport that --json would emit, so the text and the JSON can never
//     disagree about an outcome.
//   - Timings are render-only. They are NOT added to setupReport, because
//     `setup --json` is a published contract and a new field in it is a
//     breaking change for anything parsing it today.
//   - Nothing here decides whether it may use colour. The renderer already
//     resolved that from the destination stream.

// setupPhaseTiming is how long one phase took. Render-only; see above.
type setupPhaseTiming struct {
	Name    string
	Elapsed time.Duration
}

// setupTimings records per-phase durations in phase order.
type setupTimings struct {
	mu      sync.Mutex
	entries []setupPhaseTiming
}

func (t *setupTimings) record(name string, elapsed time.Duration) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries = append(t.entries, setupPhaseTiming{Name: name, Elapsed: elapsed})
}

func (t *setupTimings) get(name string) (time.Duration, bool) {
	if t == nil {
		return 0, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, entry := range t.entries {
		if entry.Name == name {
			return entry.Elapsed, true
		}
	}
	return 0, false
}

func (t *setupTimings) total() time.Duration {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var sum time.Duration
	for _, entry := range t.entries {
		sum += entry.Elapsed
	}
	return sum
}

// setupComponentObserver lets the instant phase report components AS THEY LAND
// rather than only in the slice it returns at the end. Without it the tick row
// could not exist: a phase that takes twenty seconds and then reveals six
// outcomes at once is exactly the experience this work is replacing.
//
// It is a struct rather than a plain callback because the observer has to be
// handed to resolveSetupSteps (which builds the instant closure) BEFORE the
// progress task it feeds exists.
type setupComponentObserver struct {
	mu sync.Mutex
	fn func(setupComponent)
}

func (o *setupComponentObserver) bind(fn func(setupComponent)) {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.fn = fn
	o.mu.Unlock()
}

func (o *setupComponentObserver) notify(component setupComponent) {
	if o == nil {
		return
	}
	o.mu.Lock()
	fn := o.fn
	o.mu.Unlock()
	if fn != nil {
		fn(component)
	}
}

// setupComponentMark maps a component outcome onto a status glyph.
func setupComponentMark(state string) tui.Mark {
	switch state {
	case "ok", "built":
		return tui.MarkDone
	case "failed":
		return tui.MarkFailed
	case "skipped":
		return tui.MarkSkipped
	default:
		return tui.MarkPending
	}
}

// setupPhaseMark maps a phase state onto a glyph.
func setupPhaseMark(state string) tui.Mark {
	switch state {
	case "ok":
		return tui.MarkDone
	case "degraded", "failed":
		return tui.MarkFailed
	case "skipped":
		return tui.MarkSkipped
	default:
		return tui.MarkPending
	}
}

// summaryBarWidth is narrower than the live bar: the summary is a static block
// where the numbers beside the bar carry the detail.
const summaryBarWidth = 14

// setupSummaryLabelWidth aligns the summary's left column.
const setupSummaryLabelWidth = 11

// renderSetupSummary prints the closing block: what this brain is, what each
// phase did, how long it took, and what to run next.
func renderSetupSummary(out io.Writer, render *tui.Renderer, report setupReport, timings *setupTimings, watchPlan setupWatchPlan, brainCmd string) {
	brainCmd = setupBrainCommand(brainCmd)
	headline := "Brain ready"
	headlinePhase := tui.PhaseDone
	if report.Instant.State == "degraded" {
		headline = "Brain ready (degraded)"
		headlinePhase = tui.PhaseSkipped
	}
	if total := timings.total(); total > 0 {
		headline += " in " + roundedDuration(total)
	}
	fmt.Fprintf(out, "\n%s\n", render.Bold(render.PhasePaint(headlinePhase, headline)))

	row := func(label, value, timing string) {
		line := fmt.Sprintf("  %-*s %s", setupSummaryLabelWidth, label, value)
		if timing != "" {
			line += "  " + render.Dim("("+timing+")")
		}
		fmt.Fprintln(out, line)
	}

	row("repo", fmt.Sprintf("%s  %s", report.Repo, render.Dim("("+report.RepoKey+")")), "")
	row("brain", render.Dim(report.BrainPath), "")

	_, failed := partitionSetupComponents(report.Instant.Components)
	if len(report.Instant.Components) > 0 {
		row("instant", setupComponentRow(render, report.Instant.Components), phaseTiming(timings, "instant"))
		for _, component := range failed {
			fmt.Fprintf(out, "  %-*s %s %s\n", setupSummaryLabelWidth, "",
				render.Mark(tui.MarkFailed),
				render.PhasePaint(tui.PhaseFailed, setupComponentLabel(component.Name)+": "+component.Detail))
			if hint := strings.TrimSpace(component.Hint); hint != "" {
				fmt.Fprintf(out, "  %-*s   %s\n", setupSummaryLabelWidth, "", render.Dim("hint: "+hint))
			}
		}
	}

	facts := report.Facts
	factsValue := fmt.Sprintf("%s %s %s",
		render.Bar(facts.Distilled, facts.Sessions, summaryBarWidth),
		render.Ratio(facts.Distilled, facts.Sessions),
		render.Dim("sessions distilled"))
	row("facts", factsValue, "")

	row("backfill", setupPhaseValue(render, report.Backfill, tui.PhaseBackfill, "running in the background"), phaseTiming(timings, "backfill"))
	workspaceValue := render.Mark(tui.MarkDone) + " " + render.PhasePaint(tui.PhaseDaemon, report.Workspace.Name)
	if !report.Workspace.Registered {
		workspaceValue = render.Mark(tui.MarkFailed) + " " + render.PhasePaint(tui.PhaseFailed, report.Workspace.Name+" (registration failed)")
	}
	row("workspace", workspaceValue, phaseTiming(timings, "workspace"))
	row("daemon", setupDaemonValue(render, report.Daemon, brainCmd), phaseTiming(timings, "daemon"))
	// Say plainly, at the moment of install, that this is a persistent service
	// and where its unit lives. "installed (io.entire.brain-watch.1a2b3c4d)"
	// does not tell a first-time reader that something now survives reboots.
	if report.Daemon.Installed && report.Daemon.UnitPath != "" {
		fmt.Fprintf(out, "  %-*s %s\n", setupSummaryLabelWidth, "",
			render.Dim("persistent service, starts again at every login: "+report.Daemon.UnitPath))
	}
	// What the ONE machine-wide watcher covers. Render-only: `setup --json` is a
	// published contract and a new field in it is a breaking change, so this is
	// derived here from the plan rather than added to setupReport.
	if report.Daemon.Installed {
		if coverage := describeWatchPlanCoverage(watchPlan); coverage != "" {
			fmt.Fprintf(out, "  %-*s %s\n", setupSummaryLabelWidth, "", render.Dim(coverage))
		}
	}

	for _, warning := range report.Warnings {
		fmt.Fprintf(out, "  %-*s %s %s\n", setupSummaryLabelWidth, "", render.Mark(tui.MarkFailed), render.PhasePaint(tui.PhaseSkipped, warning))
	}

	fmt.Fprintf(out, "\n%s\n", render.Bold("Next"))
	next := [][2]string{
		{brainCmd + " overview", "what this project is"},
		{brainCmd + ` brief "<task>"`, "task-shaped context"},
		{brainCmd + " status", "backfill progress and daemon health"},
	}
	// Name the removal command wherever the thing it removes was installed.
	// setup installs a persistent, restart-forever service by default, and
	// --uninstall-daemon appeared ONLY in `setup --help`: nothing in setup's
	// own output, this block, or `status` told a first-time user that a service
	// now exists on their machine or how to take it off. Surfacing it does not
	// change the default -- whether the default should be on is a product call,
	// not this line's.
	if report.Daemon.Installed {
		next = append(next, [2]string{brainCmd + " setup --uninstall-daemon", "stop and remove the background watcher"})
	}
	// Size the column to its widest entry rather than to a constant: the
	// conditional --uninstall-daemon row is longer than any fixed width chosen
	// for the three static ones, and a row that overflows its column loses the
	// alignment the block exists for.
	width := 28
	for _, entry := range next {
		if len(entry[0]) > width {
			width = len(entry[0])
		}
	}
	for _, next := range next {
		// Pad BEFORE colouring: a %-*s over an already-painted string counts
		// the escape bytes as width and the column collapses on a colour
		// terminal while looking fine in a pipe.
		fmt.Fprintf(out, "  %s %s\n",
			render.PhasePaint(tui.PhaseInstant, fmt.Sprintf("%-*s", width, next[0])),
			render.Dim(next[1]))
	}
}

// setupComponentRow renders the per-component tick row in build order.
func setupComponentRow(render *tui.Renderer, components []setupComponent) string {
	parts := make([]string, 0, len(components))
	for _, component := range components {
		mark := setupComponentMark(component.State)
		phase := tui.PhaseDone
		if component.failed() {
			phase = tui.PhaseFailed
		}
		parts = append(parts, render.Mark(mark)+" "+render.PhasePaint(phase, component.Name))
	}
	return strings.Join(parts, "  ")
}

func setupPhaseValue(render *tui.Renderer, phase setupPhase, color tui.Phase, okText string) string {
	mark := setupPhaseMark(phase.State)
	text := phase.Detail
	switch phase.State {
	case "ok":
		text = okText
		if phase.Detail != "" {
			text += " (" + phase.Detail + ")"
		}
	case "skipped":
		text = "skipped"
		if phase.Detail != "" {
			text = "skipped (" + phase.Detail + ")"
		}
		color = tui.PhaseSkipped
	case "failed":
		text = "failed"
		if phase.Detail != "" {
			text = "failed (" + phase.Detail + ")"
		}
		color = tui.PhaseFailed
	case "":
		return render.Dim("not run")
	}
	return render.Mark(mark) + " " + render.PhasePaint(color, text)
}

func setupDaemonValue(render *tui.Renderer, state daemonState, brainCmd string) string {
	text := describeDaemonState(state, brainCmd)
	switch {
	case state.Running:
		return render.Mark(tui.MarkDone) + " " + render.PhasePaint(tui.PhaseDaemon, text)
	case state.Installed:
		return render.Mark(tui.MarkSkipped) + " " + render.PhasePaint(tui.PhaseSkipped, text)
	default:
		return render.Mark(tui.MarkSkipped) + " " + render.PhasePaint(tui.PhaseSkipped, text)
	}
}

func phaseTiming(timings *setupTimings, name string) string {
	elapsed, ok := timings.get(name)
	if !ok {
		return ""
	}
	return roundedDuration(elapsed)
}

func roundedDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d < time.Second {
		return d.Round(10 * time.Millisecond).String()
	}
	return d.Round(100 * time.Millisecond).String()
}
