// Package setupui is the presentation layer for `entire brain setup`.
//
// It owns three things and nothing else:
//
//   - the SEVEN-PHASE MODEL (Detect, Build, Agents, Facts, Workspace, Watcher,
//     Ready). One live line per phase while it runs, one permanent line when it
//     settles. Sub-steps feed the live line and NEVER produce a line of their
//     own — the nine instant-core components used to render as a cryptic tick
//     run beside the spinner, again inside the completion sentence and a third
//     time in the summary, which is the single thing this package exists to
//     delete.
//   - the CAPABILITY BLOCK: what setup actually detected, rendered so that every
//     absent capability names a command the reader can run.
//   - the DISTILL CONSENT prompt, which defaults to No and can never block a
//     process that has no terminal.
//
// It does NOT own the spinner or the in-place repaints. Those live in
// internal/cli's progress reporter, which already coordinates a single
// process-wide live line across nested reporters; this package drives it
// through the Sink interface. Everything here is composed as plain text, width
// budgeted, and painted last, so the four destinations that matter — a colour
// terminal, NO_COLOR, a terminal whose locale is not UTF-8, and a pipe — are
// four byte streams off one model rather than four code paths.
package setupui

import "github.com/entireio/entire-brain/internal/tui"

// PhaseID identifies one of setup's seven phases. The order is the order they
// run in, and it is the order the summary reads in.
type PhaseID int

const (
	// PhaseDetect reports the repo, the agent, the graph provider and the
	// hosted binding. It spends nothing and it decides what the later phases
	// are even able to offer.
	PhaseDetect PhaseID = iota
	// PhaseBuild is the whole instant core behind ONE progress bar.
	PhaseBuild
	// PhaseAgents is agent instructions plus the pre-edit and session-end hooks.
	PhaseAgents
	// PhaseFacts asks before spending and only then distills.
	PhaseFacts
	// PhaseWorkspace is workspace registration.
	PhaseWorkspace
	// PhaseWatcher is the background daemon.
	PhaseWatcher
	// PhaseReady is the verdict and the next steps.
	PhaseReady
)

// Phases is every phase in run order.
var Phases = []PhaseID{PhaseDetect, PhaseBuild, PhaseAgents, PhaseFacts, PhaseWorkspace, PhaseWatcher, PhaseReady}

// TitleWidth is the width of the phase-name column, sized to the longest title
// ("Workspace"). Every row in the run — phase lines, continuation rows and the
// consent prompt — reserves exactly this many cells for it, which is what makes
// a scrolling setup read as one table instead of seven paragraphs.
const TitleWidth = 9

// titles must stay index-aligned with the PhaseID constants.
var titles = [...]string{"Detect", "Build", "Agents", "Facts", "Workspace", "Watcher", "Ready"}

// Title is the phase's column label.
func (p PhaseID) Title() string {
	if p < 0 || int(p) >= len(titles) {
		return ""
	}
	return titles[p]
}

func (p PhaseID) String() string { return p.Title() }

// Accent is the phase's colour family. There are four, not seven, on purpose:
// a rainbow is noise. Detect and Build are the free deterministic core, Facts
// is the only phase that can spend money, Agents/Workspace/Watcher are the
// wiring that outlives the run, and Ready is the verdict. A reader should be
// able to tell "this line could cost me" from across the room, and that is the
// only distinction colour is carrying.
func (p PhaseID) Accent() tui.Phase {
	switch p {
	case PhaseDetect, PhaseBuild:
		return tui.PhaseInstant
	case PhaseFacts:
		return tui.PhaseBackfill
	case PhaseAgents, PhaseWorkspace, PhaseWatcher:
		return tui.PhaseDaemon
	case PhaseReady:
		return tui.PhaseDone
	default:
		return tui.PhaseNeutral
	}
}

// State is how a phase ended.
type State int

const (
	// StateRunning is a phase that has not settled yet.
	StateRunning State = iota
	// StateOK is a phase that did its work.
	StateOK
	// StateSkipped is a phase that ran and had nothing to do, or was told not to.
	StateSkipped
	// StateFailed is a phase that was attempted and blew up.
	StateFailed
)

func (s State) String() string {
	switch s {
	case StateOK:
		return "ok"
	case StateSkipped:
		return "skipped"
	case StateFailed:
		return "failed"
	default:
		return "running"
	}
}

// Glyph is the state glyph in a row's first column.
func (s State) Glyph() Glyph {
	switch s {
	case StateOK:
		return GlyphOK
	case StateSkipped:
		return GlyphSkipped
	case StateFailed:
		return GlyphFailed
	default:
		return GlyphPending
	}
}
