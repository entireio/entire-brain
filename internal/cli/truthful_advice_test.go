package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/agentsetup"
	"github.com/spf13/cobra"
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
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read internal/cli: %v", err)
	}
	var found []printedInvocation
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
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
	if !strings.Contains(message, dirtyWorktreeErrorCode+":") {
		t.Errorf("%s refusal lost the %s code: %q", what, dirtyWorktreeErrorCode, message)
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

func TestSemanticIndexDirtyRefusalNamesBrainignoreAndTheBlockingPaths(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{stdout: " M research/notes.go\n"}
	delete(runner.responses, fakeCommandKey("entire", "graph", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network"))

	err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"},
		Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{graphBinary: "entire"}, repoDir)
	assertDirtyRefusalIsActionable(t, "semantic index", err, "research/notes.go")
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
	if err != nil && strings.Contains(err.Error(), dirtyWorktreeErrorCode) {
		t.Fatalf("a worktree dirty only in .brainignore'd paths was refused: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The guide may not contradict the binary it ships with.
// ---------------------------------------------------------------------------

// mcp_remember_test.go already stops a TOOL DESCRIPTION from naming a tool that
// does not exist. Nothing stopped the GUIDE from denying one that does, and
// that is the direction this failed in: both generators told agents "No MCP
// tool writes a fact, so this is a CLI call; do not go looking for one" while
// brain_remember was registered, documented and tested. An agent that believed
// the guide would never call it.
var guideMCPDenials = []struct {
	tool    string
	deny    *regexp.Regexp
	reality string
}{
	{
		tool:    "brain_remember",
		deny:    regexp.MustCompile(`(?i)\bno MCP tool\b[^.]*\b(?:writes?|records?|stores?)\b`),
		reality: "brain_remember records a durable fact over MCP",
	},
	{
		tool:    "brain_remember",
		deny:    regexp.MustCompile(`(?i)\bdo not go looking for one\b`),
		reality: "brain_remember is exactly the tool this tells the reader not to look for",
	},
}

// reviewedGuideMCPClaims is every sentence in a shipped guide that makes a
// claim about the MCP surface, each one checked against tools/list by hand.
// A new or reworded claim fails here until somebody does the same, which is
// the review step that was missing when the false one was written.
var reviewedGuideMCPClaims = map[string]string{
	"Over MCP the same write is the brain_remember tool, which requires path.": "brain_remember is registered and its schema requires fact and path",
}

var guideSentenceSplit = regexp.MustCompile(`(?:\.|\n\n)`)

func registeredMCPTools(t *testing.T) map[string]bool {
	t.Helper()
	repoDir := seedFixtureRepo(t)
	opts := rememberTestOptions(t, repoDir)
	var out bytes.Buffer
	input := frameMCP(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	if err := runMCP((&cobra.Command{}).Context(), strings.NewReader(input), &out, opts); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	var result struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	raw, _ := json.Marshal(readMCPResponses(t, out.String())[0]["result"])
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	registered := map[string]bool{}
	for _, tool := range result.Tools {
		registered[tool.Name] = true
	}
	if len(registered) == 0 {
		t.Fatal("tools/list returned no tools; the guard would pass vacuously")
	}
	return registered
}

func TestGuideMCPClaimsMatchTheRegisteredToolList(t *testing.T) {
	registered := registeredMCPTools(t)
	namedTool := regexp.MustCompile(`\bbrain_[a-z_]+\b`)
	claims := 0
	for name, guide := range agentsetup.AllGuides() {
		for _, sentence := range guideSentenceSplit.Split(guide, -1) {
			sentence = strings.Join(strings.Fields(sentence), " ")
			if !strings.Contains(sentence, "MCP") {
				continue
			}
			claims++
			sentence += "."

			// A tool the guide names has to exist, the same promise
			// TestMCPToolDescriptionsOnlyNameToolsThatExist holds for
			// descriptions.
			for _, named := range namedTool.FindAllString(sentence, -1) {
				if named == "brain_cgo" || registered[named] {
					continue
				}
				t.Errorf("the %s guide names %q, which is not a registered MCP tool: %q", name, named, sentence)
			}

			// A tool that exists may not be denied.
			for _, denial := range guideMCPDenials {
				if registered[denial.tool] && denial.deny.MatchString(sentence) {
					t.Errorf("the %s guide denies a capability the binary ships -- %s: %q",
						name, denial.reality, sentence)
				}
			}

			if _, ok := reviewedGuideMCPClaims[sentence]; !ok {
				t.Errorf("the %s guide makes an MCP claim nobody has checked against tools/list: %q\n"+
					"add it to reviewedGuideMCPClaims once verified, or remove it", name, sentence)
			}
		}
	}
	if claims == 0 {
		t.Fatal("no MCP claims were extracted from any guide; the extractor is probably broken")
	}
}
