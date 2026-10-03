package cli

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// These retained example invocations must be accepted without panics and produce
// output or an actionable error on an empty brain. The historical line numbers
// identify their source; this test does not parse current documentation.

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

func TestUserAndAgentGuidesDoNotAdvertiseDeveloperIntrospection(t *testing.T) {
	for _, source := range []string{"../../README.md", "../../docs/getting-started.md", "../../docs/reference.md", "../../docs/semantic_agent_guide.md", "../../internal/agentsetup/brain-reference.md"} {
		data, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"graph-schema", "docs formats"} {
			if strings.Contains(string(data), path) {
				t.Errorf("%s advertises hidden developer path %q", source, path)
			}
		}
	}
	t.Chdir(t.TempDir()) // Outside a repository, preview standalone Brain guidance.
	guide, err := execute(t, NewRootCommand(Options{Version: "test"}), "agent-guide")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"facts eval", "facts reclassify", "bench semantic", "bench scale", "inspect graph-schema", "docs formats"} {
		if strings.Contains(guide, "entire brain "+path) {
			t.Errorf("generated agent guide advertises hidden developer path %q", path)
		}
	}
}

// TestDocumentedCommandsAreDiscoverable closes the gap between what the docs
// tell a reader to run and what `--help` will admit exists.
//
// `doctor` and `config` were both hidden. Meanwhile scripts/install.sh runs
// `entire brain config init`, runs `entire brain doctor`, and closes by telling
// the reader "re-run checks: entire brain doctor"; the README documents doctor's
// exit-code contract in its own section. A user who follows that advice once and
// later greps `--help` finds nothing, and concludes the command was removed.
//
// Every command named in a fenced block or inline code span of README.md,
// docs/getting-started.md, docs/operations.md or scripts/install.sh must appear
// in `--help`. Hiding one again is fine -- but then it has to stop being
// advertised, and this test says which document to edit.
func TestDocumentedCommandsAreDiscoverable(t *testing.T) {
	root := NewRootCommand(Options{Version: "test"})

	// Only the surfaces a new user is actually pointed at. Design documents and
	// plans describe futures, not the installed binary.
	sources := []string{
		"../../README.md",
		"../../docs/getting-started.md",
		"../../docs/operations.md",
		"../../scripts/install.sh",
	}

	// `entire brain <verb>` is the spelling every one of these documents uses,
	// because all four describe the plugin install.
	mention := regexp.MustCompile(`entire brain ([a-z][a-z-]*)`)

	visible := map[string]bool{}
	for _, c := range root.Commands() {
		if c.Hidden {
			continue
		}
		visible[c.Name()] = true
		for _, alias := range c.Aliases {
			visible[alias] = true
		}
	}

	// Deliberately hidden AND deliberately documented. Each is a developer or
	// machine surface whose reason for hiding is recorded at its registration
	// in root.go and still holds; the documents that name them address that
	// audience, not a new user. Adding to this list is how a decision to keep
	// something hidden gets written down -- a new hidden-but-documented command
	// fails this test until somebody does.
	hiddenOnPurpose := map[string]string{
		"review":           "machine contract for the host CLI's `entire review`, not a human verb",
		"history-eval":     "measurement harness for the retrieval layer; developer tooling",
		"history-eval-gen": "measurement harness for the retrieval layer; developer tooling",
		"bench":            "maintainer-only measurement harness, explicitly introduced as such in getting-started",
	}

	seen := map[string]string{}
	for _, source := range sources {
		data, err := os.ReadFile(source)
		if err != nil {
			t.Fatalf("read %s: %v", source, err)
		}
		for _, match := range mention.FindAllStringSubmatch(string(data), -1) {
			verb := match[1]
			if _, ok := seen[verb]; !ok {
				seen[verb] = source
			}
		}
	}
	if len(seen) == 0 {
		t.Fatal("no documented commands found -- the test is checking nothing")
	}

	for verb, source := range seen {
		// A word that is not a command at all is a prose false positive
		// ("entire brain from scratch"), not a discoverability failure.
		if cmd, _, err := root.Find([]string{verb}); err != nil || cmd == root {
			continue
		}
		if visible[verb] {
			if reason, ok := hiddenOnPurpose[verb]; ok {
				t.Errorf("`%s` is visible in --help but still listed as hidden on purpose (%s): drop it from hiddenOnPurpose", verb, reason)
			}
			continue
		}
		if _, ok := hiddenOnPurpose[verb]; ok {
			continue
		}
		t.Errorf("%s tells the reader to run `entire brain %s`, but it is hidden from --help: unhide it, or stop advertising it", source, verb)
	}
}
