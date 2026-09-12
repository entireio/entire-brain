package cli

import (
	"strings"
	"testing"
)

// The README's own examples must be executable.
//
// Written 2026-09-11, scoring the whole command surface for release readiness.
// There are 34 top-level commands, and eleven were named in the README with no
// test behind them at all:
//
//	bench, inspect blame, inspect dead-code, inspect graph-schema,
//	inspect graph-ui, inspect ingest-traces, inspect trace-path,
//	facts map, facts promote, facts retract, facts eval-compare
//
// That gap is what makes "is it stable?" unanswerable. A documented command
// with no test is a promise nobody has checked, and the promise that matters is
// not deep behaviour — it is the first thing a new user does. They read the
// README, paste a line from it against a brain that does not exist yet, and
// find out from the result whether this project is finished.
//
// So every invocation below is COPIED FROM THE README, at the line noted, with
// only the angle-bracket placeholders filled in. That is the point: this is a
// contract test on the documentation, not an independent guess at how these
// commands are called. Writing the table by hand instead is how the first draft
// of this file "found" a bug that was mine — it invented `facts promote --to`,
// which does not exist, while README line 378 correctly documents `--from`.
//
// The contract is narrow on purpose:
//
//  1. THE README'S INVOCATION IS ACCEPTED. Not "unknown command", not "unknown
//     flag". A renamed flag or a subcommand moved under a different parent
//     silently turns a documented example into a dead end, and nothing else in
//     this repo would notice.
//
//  2. IT DOES NOT PANIC. A panic on an empty brain is the worst first
//     impression available, and most of these read stores, graphs and manifests
//     that are simply absent before the first refresh.
//
//  3. IT SAYS SOMETHING. Success or a real error, never silence. On an empty
//     brain most of these SHOULD fail; what they must not do is fail blankly.
//
// Deliberately NOT asserted: the shape of a successful result. These eleven
// span benchmarking, graph traversal and fact lifecycle, and pinning their
// output here would be a worse test than none — it would break on every honest
// change and teach people to delete it. Behaviour belongs in each command's own
// test. This is the floor beneath all of them.

// documentedCommand is one README example: the command path, the operands and
// flags that follow it, and the README line it was taken from.
type documentedCommand struct {
	path       []string
	args       []string
	readmeLine int
	note       string
}

func (dc documentedCommand) name() string { return strings.Join(dc.path, " ") }

func (dc documentedCommand) invocation() []string {
	return append(append([]string{}, dc.path...), dc.args...)
}

func documentedCommands() []documentedCommand {
	return []documentedCommand{
		// entire brain bench semantic .
		{path: []string{"bench"}, args: []string{"semantic", "."}, readmeLine: 419},

		// entire brain inspect graph-schema --json
		{path: []string{"inspect", "graph-schema"}, args: []string{"--json"}, readmeLine: 320},

		// entire brain inspect graph-ui semantic-graph.html
		{path: []string{"inspect", "graph-ui"}, args: []string{"semantic-graph.html"}, readmeLine: 321},

		// entire brain inspect trace-path "<caller>" "<callee>" --json
		{path: []string{"inspect", "trace-path"}, args: []string{"pkg.Caller", "pkg.Callee", "--json"},
			readmeLine: 322, note: "two symbols that are not indexed yet"},

		// entire brain inspect dead-code --json
		{path: []string{"inspect", "dead-code"}, args: []string{"--json"}, readmeLine: 323},

		// `brain_ingest_traces` / `inspect ingest-traces`
		{path: []string{"inspect", "ingest-traces"}, readmeLine: 342},

		// entire brain inspect blame <fact-id> --json
		{path: []string{"inspect", "blame"}, args: []string{"fact:missing", "--json"},
			readmeLine: 381, note: "a fact id that does not exist — the state before any distill"},

		// entire brain facts promote --from <branch> --strategy keep-both
		{path: []string{"facts", "promote"}, args: []string{"--from", "feature", "--strategy", "keep-both"},
			readmeLine: 378, note: "promoting from a branch that has no active facts"},

		// entire brain facts retract <fact-id>
		{path: []string{"facts", "retract"}, args: []string{"fact:missing"},
			readmeLine: 379, note: "retracting a fact that was never authored"},

		// entire brain facts eval-compare --a <before.json> --b <after.json>
		{path: []string{"facts", "eval-compare"}, args: []string{"--a", "missing-a.json", "--b", "missing-b.json"},
			readmeLine: 418, note: "the README's shape, with absent inputs"},

		// `facts map` — documented in the fact-lifecycle section.
		{path: []string{"facts", "map"}},
	}
}

func TestREADMEExamplesAreExecutable(t *testing.T) {
	for _, dc := range documentedCommands() {
		t.Run(dc.name(), func(t *testing.T) {
			f := newVerifyFixture(t)

			// execute() returns the error rather than exiting, so a panic in
			// the command surfaces here as a failed test rather than a dead
			// process.
			out, err := execute(t, NewRootCommand(f.opts), dc.invocation()...)

			if err != nil && isUnknownCommandOrFlag(err.Error()) {
				t.Fatalf("README line %d documents %q, which the binary rejects: %v\n%s",
					dc.readmeLine, strings.Join(dc.invocation(), " "), err, out)
			}
			if err == nil && strings.TrimSpace(out) == "" {
				t.Fatalf("%q succeeded silently — a user cannot tell whether it did anything", dc.name())
			}
			if err != nil && strings.TrimSpace(err.Error()) == "" {
				t.Fatalf("%q failed with an empty error message", dc.name())
			}
		})
	}
}

// TestDocumentedCommandsExplainThemselves covers the other half of the fresh
// clone: --help must work before anything is built, because it is what a user
// reaches for the moment a command fails. Help is served by cobra without
// touching the brain, so unlike the test above this one expects success every
// time.
func TestDocumentedCommandsExplainThemselves(t *testing.T) {
	for _, dc := range documentedCommands() {
		t.Run(dc.name(), func(t *testing.T) {
			f := newVerifyFixture(t)
			args := append(append([]string{}, dc.path...), "--help")

			out, err := execute(t, NewRootCommand(f.opts), args...)
			if err != nil {
				t.Fatalf("%s --help failed: %v\n%s", dc.name(), err, out)
			}
			if !strings.Contains(out, "Usage:") {
				t.Fatalf("%s --help printed no usage:\n%s", dc.name(), out)
			}
			// The first line is the one-line summary the grouped
			// `entire-brain --help` listing shows. A command with none is
			// invisible in the exact place a user goes looking for it.
			if strings.TrimSpace(strings.SplitN(out, "\n", 2)[0]) == "" {
				t.Fatalf("%s has no short description", dc.name())
			}
		})
	}
}

func isUnknownCommandOrFlag(msg string) bool {
	return strings.Contains(msg, "unknown command") ||
		strings.Contains(msg, "unknown flag") ||
		strings.Contains(msg, "unknown shorthand flag")
}
