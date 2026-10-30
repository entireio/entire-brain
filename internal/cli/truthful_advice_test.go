package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/entireio/entire-brain/internal/agentsetup"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
)

// Three defects shared one shape: a string telling the reader something that is
// not true of the program printing it. Twelve of them named `entire brain
// index`, a command that has never existed in either spelling; two refusals
// offered only --worktree while .brainignore -- the hatch that actually fits,
// already loaded and honoured by the gate itself -- went unnamed anywhere; and
// the shipped agent guide told agents no MCP tool writes a fact while
// brain_remember sat registered in tools/list.
//
// None of it could fail a build, because a string does not compile. The guards
// below are the compiler these strings never had, and they follow the shape of
// benchmarks_doc_test.go, which does the same job for docs/benchmarks.md.

// ---------------------------------------------------------------------------
// Every command this source prints has to resolve against the command tree.
// ---------------------------------------------------------------------------

// printedCommand matches one backtick-quoted invocation of this binary in
// either of its two spellings. It is applied LINE BY LINE on purpose: a Go raw
// string literal is also backtick-delimited, and matching across lines would
// pull prose out of long `Long:` help text as if it were a command.
var printedCommand = regexp.MustCompile("`(?:entire brain|entire-brain) ([^`]*)`")

// commandToken is a verb: anything else -- a flag, a placeholder, a format
// verb, a path, an operand -- ends the command path.
var commandToken = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

type printedInvocation struct {
	file string
	line int
	text string
	path []string
}

func printedInvocations(t *testing.T) []printedInvocation {
	t.Helper()
	var found []printedInvocation
	// "." plus command/add: the add command moved there (COR-2268) and its
	// printed commands must stay under this guard rather than escape it by
	// moving.
	for _, dir := range []string{".", "command/add"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			if dir != "." {
				name = dir + "/" + name
			}
			data, err := os.ReadFile(name)
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			for number, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
				for _, match := range printedCommand.FindAllStringSubmatch(line, -1) {
					invocation := printedInvocation{file: name, line: number + 1, text: strings.TrimSpace(match[0])}
					for _, token := range strings.Fields(match[1]) {
						if !commandToken.MatchString(token) {
							break
						}
						invocation.path = append(invocation.path, token)
					}
					if len(invocation.path) == 0 {
						continue
					}
					found = append(found, invocation)
				}
			}
		}
	}
	return found
}

func TestEveryCommandPrintedInSourceResolves(t *testing.T) {
	t.Parallel()
	invocations := printedInvocations(t)
	// Non-vacuity. The extractor reads source rather than a fixture, so the
	// way it fails is by matching nothing and reporting success. Source
	// carried roughly ninety of these when the guard was written; a count that
	// has collapsed means the regex stopped working, not that the strings went
	// away.
	if len(invocations) < 30 {
		t.Fatalf("only %d printed commands were extracted; the extractor is probably broken", len(invocations))
	}
	root := NewRootCommand(Options{Version: "test"})
	for _, invocation := range invocations {
		found, _, err := root.Find(invocation.path)
		if err != nil {
			t.Errorf("%s:%d prints %s, which does not resolve: %v\n"+
				"every backtick-quoted command in this package must be runnable as printed; "+
				"to mention one that is not, write it without backticks",
				invocation.file, invocation.line, invocation.text, err)
			continue
		}
		// Find falls back to the nearest parent, so a verb that does not exist
		// resolves to its group and looks fine. `entire brain index` resolved
		// to the root this way for every one of the twelve strings that
		// printed it.
		if last := invocation.path[len(invocation.path)-1]; found.Name() != last {
			t.Errorf("%s:%d prints %s, but %q is not a command (it resolved to %q)",
				invocation.file, invocation.line, invocation.text, last, found.CommandPath())
		}
	}
}

// The agent guides are read by a machine that will run what they contain, so
// the same rule applies to the indented invocations in their prose.
func TestEveryCommandInTheAgentGuidesResolves(t *testing.T) {
	t.Parallel()
	root := NewRootCommand(Options{Version: "test"})
	checked := 0
	for name, guide := range agentsetup.AllGuides() {
		for number, line := range strings.Split(guide, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "entire brain ") {
				continue
			}
			var path []string
			for _, token := range strings.Fields(strings.TrimPrefix(line, "entire brain ")) {
				if !commandToken.MatchString(token) {
					break
				}
				path = append(path, token)
			}
			if len(path) == 0 {
				continue
			}
			checked++
			found, _, err := root.Find(path)
			if err != nil {
				t.Errorf("the %s guide (line %d) tells agents to run `entire brain %s`, which does not resolve: %v",
					name, number+1, strings.Join(path, " "), err)
				continue
			}
			if last := path[len(path)-1]; found.Name() != last {
				t.Errorf("the %s guide (line %d) tells agents to run `entire brain %s`, but %q is not a command (it resolved to %q)",
					name, number+1, strings.Join(path, " "), last, found.CommandPath())
			}
		}
	}
	if checked == 0 {
		t.Fatal("no guide commands were extracted; the extractor is probably broken")
	}
}

// ---------------------------------------------------------------------------
// Both dirty-tree refusals have to name the hatch that actually clears them.
// ---------------------------------------------------------------------------

// A user with roughly 250 dirty research files was blocked here for an
// afternoon. The refusal offered --worktree, which INDEXES those files rather
// than excluding them, and said nothing about .brainignore -- which the gate
// loads (loadBrainIgnore) and applies (worktreeDirtyWithIgnore) a few lines
// above the refusal itself. The answer was in the code that refused them.
func assertDirtyRefusalIsActionable(t *testing.T, what string, err error, wantPath string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s accepted a dirty worktree without --worktree", what)
	}
	message := err.Error()
	// The machine-readable prefix, by constant: setup keys its remedy off it.
	if !strings.Contains(message, brainstore.DirtyWorktreeCode+":") {
		t.Errorf("%s refusal lost the %s code: %q", what, brainstore.DirtyWorktreeCode, message)
	}
	for _, want := range []string{".brainignore", "--worktree"} {
		if !strings.Contains(message, want) {
			t.Errorf("%s refusal does not name %s, so the reader cannot act on it: %q", what, want, message)
		}
	}
	// "the worktree is dirty" is not actionable when the reader cannot see
	// which files are meant.
	if !strings.Contains(message, wantPath) {
		t.Errorf("%s refusal does not name the blocking path %q: %q", what, wantPath, message)
	}
}

func TestSeedDirtyRefusalNamesBrainignoreAndTheBlockingPaths(t *testing.T) {
	repoDir := seedFixtureRepo(t)
	runner := seedFixtureRunner(repoDir)
	runner.responses[fakeCommandKey("git", "status", "--porcelain", "--untracked-files=all")] = fakeCommandResponse{stdout: " M research/notes.md\n"}
	cmd := NewRootCommand(Options{Version: "test-version", Runner: runner})
	_, err := execute(t, cmd, "refresh", "seed", "--output", filepath.Join(t.TempDir(), "seed"), repoDir)
	assertDirtyRefusalIsActionable(t, "seed refresh", err, "research/notes.md")
}

// A tree dirty only in paths the reader already excluded is not dirty, so the
// refusal must not fire at all -- otherwise the advice it gives would be
// advice they had already taken.
func TestBrainignoredPathsDoNotTriggerTheDirtyRefusal(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	if err := os.WriteFile(filepath.Join(repoDir, ".brainignore"), []byte("research/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{stdout: " M research/notes.go\n"}

	err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"},
		Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{graphBinary: "entire"}, repoDir)
	if err != nil && strings.Contains(err.Error(), brainstore.DirtyWorktreeCode) {
		t.Fatalf("a worktree dirty only in .brainignore'd paths was refused: %v", err)
	}
}
