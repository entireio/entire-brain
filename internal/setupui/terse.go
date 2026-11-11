package setupui

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/entireio/entire-brain/internal/tui"
)

// terse.go is the DEFAULT shape of `entire brain setup`.
//
// The phase view in this package reports what the machine did: eight component
// names, absolute paths, provider error text, internal identifiers. That is the
// right report when something is wrong and the wrong one the other 95% of the
// time. A first run printed seventeen lines, two of them raw subprocess error
// dumps and one of them an internal ticket id, to say "it worked".
//
// The short form opens with an introduction, then answers three questions as
// each phase finishes: is it usable, what happens to my past sessions, and
// will it stay fresh. Details live behind `--verbose` and `doctor`.
//
// This is a pure function over a value: every state below is reachable in a
// test without a brain, a repo, or a terminal.

// DistillOutcome is what happened to the repo's past sessions.
type DistillOutcome int

const (
	// DistillNoSessions is a repo with nothing recorded yet. Not a problem,
	// and must not read like one: memories accrue as the person works.
	DistillNoSessions DistillOutcome = iota
	// DistillStarted means the backfill is running in the background.
	DistillStarted
	// DistillDeclined means the person said no at the prompt.
	DistillDeclined
	// DistillSkippedFlag means a flag decided, so no prompt was shown.
	DistillSkippedFlag
	// DistillNoAgent means sessions exist but nothing can read them.
	DistillNoAgent
	// DistillAlreadyDone means a previous run already distilled them.
	DistillAlreadyDone
	// DistillFailed means distillation was attempted and did not start. It
	// carries a reason, because "no sessions" and "it broke" look identical
	// from the outside and need opposite reactions.
	DistillFailed
	// DistillPaused is `brain stop`. Telling this reader to "run setup again"
	// is advice that cannot work: the next setup skips for the same reason.
	DistillPaused
	// DistillInFlight is a backfill already running from an earlier setup.
	// Calling that "skipped" contradicts what the machine is doing right now.
	DistillInFlight
	// DistillNoExecutable means this binary's own path would not resolve, so
	// there was nothing to spawn. Neither a decline nor a missing agent.
	DistillNoExecutable
)

// WatcherOutcome is what happened to the background watcher.
type WatcherOutcome int

const (
	// WatcherInstalled is the normal case.
	WatcherInstalled WatcherOutcome = iota
	// WatcherAlreadyRunning is a re-run finding a healthy watcher.
	WatcherAlreadyRunning
	// WatcherSkippedFlag is --no-daemon.
	WatcherSkippedFlag
	// WatcherUnavailable is a platform or permission problem. It carries a
	// reason, because this is the one watcher state a person must act on.
	WatcherUnavailable
)

// TerseReport is everything the short form needs and nothing it does not.
type TerseReport struct {
	// Elapsed is how long the instant phase took.
	Elapsed time.Duration
	// Queryable reports whether the brain can answer questions now. A build
	// can be degraded and still queryable, which is the common case and must
	// not be reported as failure.
	Queryable bool
	// Unavailable names capabilities that did NOT build, in words a person
	// recognises ("semantic search"), never component ids. Empty when the
	// build was complete.
	Unavailable []string
	// Remedy is the single command that fixes Unavailable, when one exists.
	Remedy string
	// UnnamedFailures counts components that failed and have no plain-words
	// label. They are deliberately not NAMED here -- an internal id tells a
	// reader nothing they can act on, and `doctor` lists every one by name --
	// but they must still be COUNTED, or a degraded build with only
	// unlabelled failures reads as a clean one.
	UnnamedFailures int

	Sessions  int
	Distilled int
	// Queued is how many sessions the just-started backfill will actually
	// distill (the pending pool capped by the spend budget). Zero means
	// uncapped or unknown; when it is smaller than the pending pool
	// (Sessions - Distilled), the distill line says both counts.
	Queued  int
	Distill DistillOutcome
	// DistillReason explains a DistillFailed outcome in one clause.
	DistillReason string

	Watcher       WatcherOutcome
	WatcherReason string

	// NoCommitsYet is a repository git has nothing in yet.
	//
	// Setup returns early on that path, so the later phases never ran: the
	// daemon was not inspected and no source was built. Rendering the normal
	// report from that half-filled value produced "Brain could not be built"
	// and "Watcher unavailable" -- two statements that are not measurements,
	// and neither of them the actual reason. The reason is one commit away
	// from being fixed and nothing is wrong with the machine.
	NoCommitsYet bool

	// UpToDate collapses the whole report to one line: a re-run that found
	// nothing to do. Repeating the full report teaches the reader to stop
	// reading it.
	UpToDate bool

	// BrainCmd is the command name the remedy and watch hints print.
	BrainCmd string

	// OfferConnect asks for one closing line about team sync.
	//
	// Hosted sync is off unless someone runs `connect`, and the short form is
	// now the default output, so without this the only place the feature is
	// announced is a block most people never see. It is deliberately NOT
	// shown on an up-to-date re-run: onboarding is a moment, and a line that
	// reappears every run is a nag rather than a discovery.
	OfferConnect bool

	// HostedError is this run's hosted connect/sync failure, verbatim. It
	// OWNS the closing line when set: printing the generic connect offer
	// right after connect-shaped work failed tells the reader to re-run the
	// thing that just failed, with the reason hidden.
	HostedError string
}

func (rep TerseReport) command() string {
	brainCmd := strings.TrimSpace(rep.BrainCmd)
	if brainCmd == "" {
		brainCmd = "entire brain"
	}
	return brainCmd
}

// RenderTerse writes a completed report. Setup uses the individual renderers
// below to report each result as soon as its phase finishes.
func RenderTerse(w io.Writer, r *Renderer, rep TerseReport) {
	brainCmd := rep.command()
	ok := terseMark(r, tui.MarkDone)
	skip := terseMark(r, tui.MarkSkipped)

	if rep.UpToDate {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "  %s %s\n", ok, rep.upToDateLine())
		return
	}

	// An empty repository is not a broken one, and the phases that would
	// have measured the rest never ran. Say the one true thing and the one
	// action, and claim nothing else.
	if rep.NoCommitsYet {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "  %s %s\n\n", skip, "Nothing to index yet — this repository has no commits")
		fmt.Fprintf(w, "Make a commit, then: %s setup\n", brainCmd)
		return
	}

	RenderTerseIntro(w, r)
	RenderTerseBuild(w, r, rep)
	RenderTerseDistill(w, r, rep)
	RenderTerseWatcher(w, r, rep)
	RenderTerseNext(w, rep)
}

// RenderTerseIntro introduces Brain before setup begins its work.
func RenderTerseIntro(w io.Writer, r *Renderer) {
	fmt.Fprintln(w)
	for _, line := range r.Wrap("Brain turns this repo's history and past AI sessions into memories your agents recall while they work.") {
		if strings.HasPrefix(line, "Brain ") {
			line = r.TUI().Bold(r.TUI().Paint(tui.CLIAccent, "Brain")) + strings.TrimPrefix(line, "Brain")
		}
		fmt.Fprintln(w, line)
	}
	fmt.Fprintln(w)
}

// RenderTerseBuild reports the completed build and any unavailable capabilities.
func RenderTerseBuild(w io.Writer, r *Renderer, rep TerseReport) {
	brainCmd := rep.command()
	skip := terseMark(r, tui.MarkSkipped)
	built := fmt.Sprintf("Brain built — queryable now (%s)", roundTerse(rep.Elapsed))
	if !rep.Queryable {
		built = "Brain could not be built"
	}
	fmt.Fprintf(w, "  %s %s\n", markFor(r, rep.Queryable), terseDetail(r, built))

	// A degraded build gets ONE line naming the capability in plain words and
	// the command that restores it -- not the provider's error text, which is
	// what `doctor` is for.
	if len(rep.Unavailable) > 0 {
		line := fmt.Sprintf("%s unavailable", strings.Join(rep.Unavailable, " and "))
		if rep.Remedy != "" {
			line += " — " + rep.Remedy
		}
		fmt.Fprintf(w, "  %s %s\n", skip, line)
	}
	if rep.UnnamedFailures > 0 {
		// Counted, not named, and pointed at the command that names them.
		fmt.Fprintf(w, "  %s %s did not build. Run `%s doctor` to see the list.\n",
			skip, plural(rep.UnnamedFailures, "other component"), brainCmd)
	}
}

// RenderTerseDistill reports the outcome of starting or skipping the backfill.
func RenderTerseDistill(w io.Writer, r *Renderer, rep TerseReport) {
	if line, mark := rep.distillLine(rep.command()); line != "" {
		fmt.Fprintf(w, "  %s %s\n", terseMark(r, mark), terseDetail(r, line))
	}
}

// RenderTerseWatcher reports the outcome of installing or skipping the watcher.
func RenderTerseWatcher(w io.Writer, r *Renderer, rep TerseReport) {
	if line, mark := rep.watcherLine(); line != "" {
		fmt.Fprintf(w, "  %s %s\n", terseMark(r, mark), terseDetail(r, line))
	}
}

// RenderTerseNext writes any action needed after the status lines.
func RenderTerseNext(w io.Writer, rep TerseReport) {
	if rep.UpToDate {
		return
	}
	brainCmd := rep.command()
	// A queryable brain ends on its status lines: the summary IS the output,
	// and the old "Ready. Try: ... brief" hint was one more thing to read
	// after the answer. The two exceptions each carry something the status
	// lines do not: the connect offer is the one place hosted sync gets
	// discovered on an unbound repo, and a failed build has to point at
	// doctor.
	switch {
	case rep.Queryable && rep.HostedError != "":
		fmt.Fprintln(w)
		fmt.Fprintf(w, "%s\n", rep.HostedError)
	case rep.Queryable && rep.OfferConnect:
		fmt.Fprintln(w)
		fmt.Fprintf(w, "Memories will stay local. To share them, run `%s connect` to sync.\n", brainCmd)
	case !rep.Queryable:
		fmt.Fprintln(w)
		fmt.Fprintf(w, "Run `%s doctor` for what went wrong.\n", brainCmd)
	}
}

func (rep TerseReport) upToDateLine() string {
	parts := []string{"Brain up to date"}
	switch {
	case rep.Distilled > 0:
		parts = append(parts, fmt.Sprintf("%s distilled", plural(rep.Distilled, "session")))
	case rep.Sessions > 0:
		parts = append(parts, fmt.Sprintf("%s indexed", plural(rep.Sessions, "session")))
	}
	line := strings.Join(parts, " — ")
	// Comma, not a third em dash: two dashes in one sentence stop separating
	// anything.
	if rep.Watcher == WatcherInstalled || rep.Watcher == WatcherAlreadyRunning {
		line += ", watcher running"
	}
	return line + fmt.Sprintf(" (%s)", roundTerse(rep.Elapsed))
}

func (rep TerseReport) distillLine(brainCmd string) (string, tui.Mark) {
	switch rep.Distill {
	case DistillNoSessions:
		return "No past sessions to distill — memories accrue as you work", tui.MarkDone
	case DistillStarted:
		pending := max(0, rep.Sessions-rep.Distilled)
		count := plural(pending, "session")
		if rep.Queued > 0 && rep.Queued < pending {
			count = fmt.Sprintf("%d of %d sessions", rep.Queued, pending)
		}
		return fmt.Sprintf("Distilling %s in the background (watch: %s status)",
			count, brainCmd), tui.MarkDone
	case DistillDeclined:
		return fmt.Sprintf("Distillation skipped — run `%s setup` again anytime", brainCmd), tui.MarkSkipped
	case DistillSkippedFlag:
		return fmt.Sprintf("Distillation skipped — run `%s setup` again anytime", brainCmd), tui.MarkSkipped
	case DistillNoAgent:
		// Named as a missing TOOL, not a failure of the repo: the sessions are
		// fine and nothing is lost by fixing this later.
		return fmt.Sprintf("%s found, but no agent CLI to distill them — install claude or codex, then re-run `%s setup`",
			plural(rep.Sessions, "session"), brainCmd), tui.MarkSkipped
	case DistillAlreadyDone:
		return fmt.Sprintf("%s already distilled", plural(rep.Distilled, "session")), tui.MarkDone
	case DistillFailed:
		line := "Distillation did not start"
		if reason := strings.TrimSpace(rep.DistillReason); reason != "" {
			line += " — " + reason
		}
		return line, tui.MarkFailed
	case DistillPaused:
		// The remedy is `start`, not `setup`. A re-run skips identically.
		return fmt.Sprintf("Background work is paused — run `%s start`, then `%s setup`", brainCmd, brainCmd), tui.MarkSkipped
	case DistillInFlight:
		return fmt.Sprintf("Distillation already running from an earlier run (watch: %s status)", brainCmd), tui.MarkDone
	case DistillNoExecutable:
		line := "Distillation could not start — this binary's own path would not resolve"
		if reason := strings.TrimSpace(rep.DistillReason); reason != "" {
			line += " (" + reason + ")"
		}
		return line, tui.MarkFailed
	}
	return "", tui.MarkDone
}

func (rep TerseReport) watcherLine() (string, tui.Mark) {
	switch rep.Watcher {
	case WatcherInstalled:
		return "Watcher installed — keeps the brain fresh; starts at login", tui.MarkDone
	case WatcherAlreadyRunning:
		return "Watcher already running — keeps the brain fresh", tui.MarkDone
	case WatcherSkippedFlag:
		return "Watcher not installed (--no-daemon) — the brain updates only when you re-run setup", tui.MarkSkipped
	case WatcherUnavailable:
		line := "Watcher unavailable — the brain updates only when you re-run setup"
		if reason := strings.TrimSpace(rep.WatcherReason); reason != "" {
			line += " (" + reason + ")"
		}
		return line, tui.MarkSkipped
	}
	return "", tui.MarkDone
}

// markFor goes through the Renderer so the non-UTF-8 and NO_COLOR fallbacks
// are the ones the rest of this package already agreed on.
func markFor(r *Renderer, ok bool) string {
	if ok {
		return terseMark(r, tui.MarkDone)
	}
	return terseMark(r, tui.MarkFailed)
}

// roundTerse renders a duration the way a person would say it.
func roundTerse(d time.Duration) string {
	switch {
	case d <= 0:
		return "0s"
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Round(time.Millisecond).Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%gs", d.Round(100*time.Millisecond).Seconds())
	default:
		return d.Round(time.Second).String()
	}
}

// Paint after composing text so styling cannot affect wrapping or wording.
func terseMark(r *Renderer, mark tui.Mark) string {
	color := tui.CLIMuted
	switch mark {
	case tui.MarkDone:
		color = tui.CLISuccess
	case tui.MarkFailed:
		color = tui.CLIError
	}
	return r.TUI().Paint(color, r.MarkGlyph(mark))
}

func terseDetail(r *Renderer, text string) string {
	for _, separator := range []string{" — ", " (watch:", " ("} {
		if i := strings.Index(text, separator); i >= 0 {
			return text[:i] + r.TUI().Paint(tui.CLIMuted, text[i:])
		}
	}
	return text
}
