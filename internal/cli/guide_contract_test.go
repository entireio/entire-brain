package cli

import (
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// `guide` is the command set an agent is told to work from — it is the FIRST
// thing a coding agent reads about this tool, and every line in it is a promise
// that the line can be typed. Nothing checked that. The guide is a hand-written
// string literal, so a renamed flag or a subcommand moved under a different
// parent silently turns it into instructions for a tool that no longer exists,
// and an agent that follows it gets "unknown command" instead of an answer.
//
// This does not pin the guide's wording. It extracts every `entire brain …`
// invocation the guide prints, resolves the verbs against the real command tree
// and every long flag against that command's real flag set. Rewrite the guide
// however you like; you may not advertise something that is not there.

// guideInvocation matches an indented command line in the guide body.
var guideInvocation = regexp.MustCompile(`(?m)^\s+entire brain ([^\n#]+)`)

// guideCommandLines returns the guide's invocations, each already split into
// tokens with the trailing comment removed.
func guideCommandLines(t *testing.T) [][]string {
	t.Helper()
	out, err := execute(t, NewRootCommand(Options{Version: "test"}), "agent-guide")
	if err != nil {
		t.Fatalf("guide: %v", err)
	}
	var lines [][]string
	for _, match := range guideInvocation.FindAllStringSubmatch(out, -1) {
		if fields := strings.Fields(match[1]); len(fields) > 0 {
			lines = append(lines, fields)
		}
	}
	if len(lines) < 20 {
		t.Fatalf("only %d invocations found in the guide — the extraction is broken, not the guide", len(lines))
	}
	return lines
}

// guidePlaceholder reports whether a token is a documentation placeholder
// (`<query>`, `[repo]`, `"<task>"`, `<id>...`) rather than a real operand.
func guidePlaceholder(token string) bool {
	trimmed := strings.Trim(token, `"'`)
	return strings.HasPrefix(trimmed, "<") || strings.HasPrefix(trimmed, "[")
}

// guideResolvePath walks a guide invocation's leading tokens against the real
// command tree and returns the command it names, plus the tokens consumed. A
// token is only treated as a subcommand while the command reached so far HAS
// subcommands; once a leaf is reached the rest are operands
// (`inspect graph-ui semantic-graph.html`), not a misspelled verb. That
// distinction is the whole point: an operand must be ignored, a verb that does
// not exist must fail.
func guideResolvePath(t *testing.T, root *cobra.Command, tokens []string) (*cobra.Command, bool) {
	t.Helper()
	cmd := root
	consumed := 0
	for _, token := range tokens {
		if strings.HasPrefix(token, "-") || guidePlaceholder(token) {
			break
		}
		if consumed > 0 && !cmd.HasSubCommands() {
			break // a leaf command: everything left is an operand
		}
		next := guideSubcommand(cmd, token)
		if next == nil {
			t.Errorf("guide advertises `entire brain %s`, but %q is not a command under %q",
				strings.Join(tokens, " "), token, cmd.CommandPath())
			return nil, false
		}
		cmd, consumed = next, consumed+1
	}
	if consumed == 0 {
		t.Errorf("guide line %q names no command", strings.Join(tokens, " "))
		return nil, false
	}
	return cmd, true
}

// guideSubcommand finds a direct subcommand by name or alias.
func guideSubcommand(parent *cobra.Command, name string) *cobra.Command {
	for _, sub := range parent.Commands() {
		if sub.Name() == name {
			return sub
		}
		for _, alias := range sub.Aliases {
			if alias == name {
				return sub
			}
		}
	}
	return nil
}

func TestGuideOnlyAdvertisesCommandsThatExist(t *testing.T) {
	root := NewRootCommand(Options{Version: "test"})
	for _, tokens := range guideCommandLines(t) {
		guideResolvePath(t, root, tokens)
	}
}

func TestGuideOnlyAdvertisesFlagsThatExist(t *testing.T) {
	root := NewRootCommand(Options{Version: "test"})
	checked := 0
	for _, tokens := range guideCommandLines(t) {
		cmd, ok := guideResolvePath(t, root, tokens)
		if !ok {
			continue // the command test above already reports this
		}
		for _, token := range tokens {
			// `[--scope local|cross-cutting]` and `--path category.sub.type`
			// both appear; strip the documentation brackets and any value.
			name := strings.TrimLeft(token, "[")
			name = strings.TrimRight(name, "],")
			if !strings.HasPrefix(name, "--") {
				continue
			}
			name = strings.SplitN(strings.TrimPrefix(name, "--"), "=", 2)[0]
			if name == "" {
				continue
			}
			checked++
			if lookupGuideFlag(cmd, name) == nil {
				t.Errorf("guide advertises --%s on `%s`, which has no such flag", name, cmd.CommandPath())
			}
		}
	}
	if checked == 0 {
		t.Fatal("no flags extracted from the guide — the test is checking nothing")
	}
}

// lookupGuideFlag resolves a long flag on a command or any of its parents
// (persistent flags are inherited).
func lookupGuideFlag(cmd *cobra.Command, name string) any {
	if f := cmd.Flags().Lookup(name); f != nil {
		return f
	}
	for parent := cmd.Parent(); parent != nil; parent = parent.Parent() {
		if f := parent.PersistentFlags().Lookup(name); f != nil {
			return f
		}
	}
	return nil
}

// TestGuideRetrievalFlagClaimIsTrue pins the guide's one prose claim about
// flags — "search/vsearch/query take --json/--format json|cli/--limit/-n/
// --branch, get/multi-get take --json/--format json|cli/--branch" — to the
// commands themselves. Prose is where an advertised flag hides from a
// line-by-line extractor.
func TestGuideRetrievalFlagClaimIsTrue(t *testing.T) {
	root := NewRootCommand(Options{Version: "test"})
	for _, tc := range []struct {
		verb  string
		flags []string
	}{
		{"search", []string{"json", "format", "limit", "branch"}},
		{"vsearch", []string{"json", "format", "limit", "branch"}},
		{"query", []string{"json", "format", "limit", "branch"}},
		{"get", []string{"json", "format", "branch"}},
		{"multi-get", []string{"json", "format", "branch"}},
	} {
		cmd, _, err := root.Find([]string{tc.verb})
		if err != nil || cmd == root {
			t.Errorf("guide names `entire brain %s`, which does not resolve", tc.verb)
			continue
		}
		for _, name := range tc.flags {
			if lookupGuideFlag(cmd, name) == nil {
				t.Errorf("guide claims `%s` takes --%s; it does not", tc.verb, name)
			}
		}
	}
	// The guide also claims the short -n spelling for the three search verbs.
	for _, verb := range []string{"search", "vsearch", "query"} {
		cmd, _, err := root.Find([]string{verb})
		if err != nil {
			continue
		}
		if cmd.Flags().ShorthandLookup("n") == nil {
			t.Errorf("guide claims `%s` takes -n; it does not", verb)
		}
	}
}
