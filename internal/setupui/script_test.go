package setupui

import (
	"bytes"
	"errors"
	"strings"

	"github.com/entireio/entire-brain/internal/tui"
)

// script_test.go holds the one scripted run every rendering test asserts
// against. There is exactly one so that "the four destinations are four byte
// streams off ONE model" is a property of the tests too: if the pipe golden
// and the colour terminal disagreed about what happened, it would be because
// the model diverged, not because two scripts did.

// instantComponents is the real nine. They exist here to be counted and NEVER
// to be printed: nine sub-steps producing nine lines is the pytest-looking
// output this package was written to delete.
var instantComponents = []string{
	"session export", "seed baseline", "history index", "doc index",
	"semantic index", "pattern layer", "entity index", "memory coordinator",
	"fact store",
}

func sampleDetected() Detected {
	return Detected{
		Repo:     RepoFacts{Name: "acme-payments", Path: "/Users/dev/src/acme-payments", Commits: 13},
		Agent:    AgentFacts{Name: "claude-code", Model: "claude-haiku-5-5", Effort: "low"},
		Provider: ProviderFacts{Name: "entire-graph", Version: "v0.4.1", Semantic: Capability{OK: true}, Entities: Capability{OK: true}},
		Hosted:   HostedFacts{},
	}
}

func sampleSummary(failures []Failure) Summary {
	return Summary{
		RepoPath:  "/Users/dev/src/acme-payments",
		BrainPath: "/Users/dev/src/acme-payments/.entire/brain",
		Queryable: true,
		Failures:  failures,
		Next: []NextStep{
			{Command: "entire brain query \"why is the retry budget 3\"", Why: "ask it something"},
			{Command: "entire brain distill", Why: "turn 12 sessions into facts"},
		},
	}
}

// scriptedRun drives all seven phases exactly as setup would: nine sub-steps
// inside Build, three inside Agents, a watcher failure with a multi-line error
// underneath it, and a Facts phase that was declined.
func scriptedRun(sink Sink, rend *Renderer) *Run {
	run := NewRun(sink, rend)
	detected := sampleDetected()

	detect := run.Begin(PhaseDetect)
	for _, step := range []string{"git state", "agent CLI", "graph provider", "hosted binding"} {
		detect.Step(step)
	}
	detected.Report(detect, rend)

	build := run.Begin(PhaseBuild)
	for index, component := range instantComponents {
		build.Progress(index+1, len(instantComponents))
		build.Step(component)
	}
	build.OK("instant core ready in 340ms " + rend.Dash() + " " + plural(len(instantComponents), "component") + ", the brain is queryable now")

	agents := run.Begin(PhaseAgents)
	for _, step := range []string{"agent instructions", "pre-edit hook", "session-end hook"} {
		agents.Step(step)
	}
	bullet := " " + rend.Bullet() + " "
	agents.OK("instructions written" + bullet + "pre-edit hook already wired" + bullet + "session-end hook installed")

	facts := run.Begin(PhaseFacts)
	facts.Step("counting distillable sessions")
	facts.Skip("not distilled; the brain is queryable now, start later: entire brain distill")

	workspace := run.Begin(PhaseWorkspace)
	workspace.Step("registering repo")
	workspace.OK("joined workspace acme " + rend.Dash() + " 4 repos")

	watcher := run.Begin(PhaseWatcher)
	for _, step := range []string{"writing launchd plist", "bootstrapping service"} {
		watcher.Step(step)
	}
	watcher.Fail("background watcher not installed", errors.New(
		"launchctl bootstrap gui/501: Operation not permitted\n\tthe plist was written to /Users/dev/Library/LaunchAgents/io.entire.brain.plist\n\tbut the service could not be loaded\n\tsee log: /tmp/entire-brain-daemon.log"))

	ready := run.Begin(PhaseReady)
	sampleSummary(run.Failures()).Report(run, ready, rend)
	return run
}

// emptyRepoRun is the zero-commit shape: a repository where nothing is wrong
// and nothing can be indexed yet.
func emptyRepoRun(sink Sink, rend *Renderer) *Run {
	run := NewRun(sink, rend)
	detected := sampleDetected()
	detected.Repo = RepoFacts{Name: "fresh-repo", Path: "/Users/dev/src/fresh-repo", Commits: 0}

	detect := run.Begin(PhaseDetect)
	detect.Step("git state")
	detected.Report(detect, rend)

	build := run.Begin(PhaseBuild)
	build.Step("reading git history")
	build.Unmet(Precondition{
		Condition: "no commits yet, so there is nothing to index",
		Action:    "make one commit, then: entire brain setup",
	})

	ready := run.Begin(PhaseReady)
	Summary{
		RepoPath:  "/Users/dev/src/fresh-repo",
		BrainPath: "/Users/dev/src/fresh-repo/.entire/brain",
		Queryable: false,
	}.Report(run, ready, rend)
	return run
}

// modes are the four destinations that have to be correct. They are four
// capability sets, not four code paths.
type mode struct {
	name string
	caps tui.Caps
}

func renderModes(width int) []mode {
	return []mode{
		{"tty-color", tui.Caps{TTY: true, Color: true, Unicode: true, Width: width}},
		{"tty-nocolor", tui.Caps{TTY: true, Color: false, Unicode: true, Width: width}},
		{"tty-ascii", tui.Caps{TTY: true, Color: true, Unicode: false, Width: width}},
		{"pipe", tui.Caps{Width: width}},
	}
}

func rendererFor(caps tui.Caps, width int) *Renderer {
	theme, _ := tui.ThemeByName("default")
	return NewRendererWidth(tui.NewRendererWith(theme, caps), width)
}

// renderRun returns the permanent output a run produced, as a WriterSink would
// have written it.
func renderRun(build func(Sink, *Renderer) *Run, caps tui.Caps, width int) (string, *Run) {
	var buf bytes.Buffer
	rend := rendererFor(caps, width)
	run := build(NewWriterSink(&buf, rend), rend)
	return buf.String(), run
}

func outputLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
