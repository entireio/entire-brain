package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// docs/benchmarks.md is a page outsiders are invited to act on: it tells people
// evaluating Brain to stop trusting our numbers and run the commands
// themselves. A command on that page that does not exist, or a flag that was
// renamed, turns the invitation into an embarrassment at exactly the moment
// somebody took us up on it — and nothing else in the build would notice,
// because documentation does not compile.
//
// Writing this page was itself the proof: the first draft listed `facts eval`
// as a zero-configuration command (it requires --tasks) and invoked
// `eval-compare` with positional arguments (it takes --a and --b). Both were
// caught by running them, which is what this test now does every build.

func benchmarksDoc(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "benchmarks.md"))
	if err != nil {
		t.Fatalf("read docs/benchmarks.md: %v", err)
	}
	// Normalise line endings. Git checks the file out with CRLF on Windows, so
	// every check below that spans a line break — and every prefix match on a
	// line — would otherwise fail there and only there.
	return strings.ReplaceAll(string(data), "\r\n", "\n")
}

// documentedBrainCommands pulls every `entire brain ...` invocation out of the
// fenced shell blocks.
func documentedBrainCommands(doc string) [][]string {
	var commands [][]string
	inBlock := false
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(line, "```") {
			inBlock = strings.HasPrefix(line, "```sh") || strings.HasPrefix(line, "```bash")
			continue
		}
		if !inBlock {
			continue
		}
		line = strings.TrimSpace(line)
		// Drop trailing comments; they are prose, not arguments.
		if i := strings.Index(line, " #"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if !strings.HasPrefix(line, "entire brain ") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "entire brain "))
		if len(fields) > 0 {
			commands = append(commands, fields)
		}
	}
	return commands
}

func TestEveryCommandInTheBenchmarksPageExists(t *testing.T) {
	root := NewRootCommand(Options{Version: "test"})
	commands := documentedBrainCommands(benchmarksDoc(t))
	if len(commands) < 4 {
		t.Fatalf("only found %d documented commands; the extractor is probably broken", len(commands))
	}
	for _, args := range commands {
		// Resolve just the verb path, stopping at the first flag.
		var path []string
		for _, arg := range args {
			if strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "<") {
				break
			}
			path = append(path, arg)
		}
		if len(path) == 0 {
			continue
		}
		found, _, err := root.Find(path)
		if err != nil {
			t.Fatalf("docs/benchmarks.md documents `entire brain %s`, which does not exist: %v",
				strings.Join(path, " "), err)
		}
		// Find falls back to the nearest parent, so a bogus leaf resolves to its
		// group. Confirm the leaf itself is a real command.
		if found.Name() != path[len(path)-1] {
			t.Fatalf("docs/benchmarks.md documents `entire brain %s`, but %q is not a command (resolved to %q)",
				strings.Join(path, " "), path[len(path)-1], found.CommandPath())
		}
	}
}

func TestEveryFlagInTheBenchmarksPageExists(t *testing.T) {
	root := NewRootCommand(Options{Version: "test"})
	for _, args := range documentedBrainCommands(benchmarksDoc(t)) {
		var path []string
		var flags []string
		for _, arg := range args {
			switch {
			case strings.HasPrefix(arg, "--"):
				flags = append(flags, strings.TrimPrefix(strings.SplitN(arg, "=", 2)[0], "--"))
			case strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "<"):
				// placeholder or short flag; not checked here
			default:
				if len(flags) == 0 {
					path = append(path, arg)
				}
			}
		}
		if len(path) == 0 || len(flags) == 0 {
			continue
		}
		cmd, _, err := root.Find(path)
		if err != nil {
			continue // the command test above reports this
		}
		for _, name := range flags {
			if cmd.Flags().Lookup(name) == nil && cmd.PersistentFlags().Lookup(name) == nil && root.PersistentFlags().Lookup(name) == nil {
				t.Fatalf("docs/benchmarks.md passes --%s to `%s`, which has no such flag",
					name, cmd.CommandPath())
			}
		}
	}
}

// The page's credibility rests on saying what it cannot back up. These are the
// specific disclosures that make it honest rather than promotional, and any of
// them could be quietly dropped in an edit that made the page read better.
func TestBenchmarksPageKeepsItsDisclosures(t *testing.T) {
	doc := benchmarksDoc(t)
	for what, needle := range map[string]string{
		"that we have not run a head-to-head": "We have not run one",
		// Both halves: the machine-readable state AND the sentence saying it
		// blocks citation. The bare string survives in prose, so checking only
		// for it would pass a page that had quietly stopped explaining itself.
		"the claim policy state":                           "claim_policy: no_release_claim",
		"that the policy blocks citation":                  "have not passed the gate",
		"that the gate is enforced by a command":           "mise run release:evidence",
		"that we lose on scale":                            "does not\nscale to the largest monorepos",
		"the closed negatives section":                     "## Closed negatives",
		"that internal A/B is not a competitor comparison": "not comparisons with other products",
	} {
		if !strings.Contains(doc, needle) {
			t.Fatalf("docs/benchmarks.md no longer states %s (looked for %q)", what, needle)
		}
	}
	// A benchmarks page with no losses on it is a marketing page. Require
	// several, so removing one inconvenient row cannot pass.
	//
	// The section has to be bounded at the next heading, not run to the end of
	// the document: later sections also use bold bullets, and counting those
	// let three deletions here go unnoticed.
	section := doc[strings.Index(doc, "## Closed negatives"):]
	if end := strings.Index(section[len("## Closed negatives"):], "\n## "); end >= 0 {
		section = section[:len("## Closed negatives")+end]
	}
	negatives := regexp.MustCompile(`(?m)^- \*\*`).FindAllString(section, -1)
	if len(negatives) < 4 {
		t.Fatalf("only %d closed negatives are published; the section is meant to carry the losses", len(negatives))
	}
}

// A claim on this page must be traceable to something in the repository. The
// eval ledger is where the retrieval rows come from, and a page citing a ledger
// that no longer exists cites nothing.
func TestBenchmarksPageLinksResolve(t *testing.T) {
	doc := benchmarksDoc(t)
	links := regexp.MustCompile(`\]\(([a-zA-Z0-9_./-]+\.md)[^)]*\)`).FindAllStringSubmatch(doc, -1)
	if len(links) == 0 {
		t.Fatal("the page cites no sources")
	}
	for _, match := range links {
		target := filepath.Join("..", "..", "docs", match[1])
		if _, err := os.Stat(target); err != nil {
			t.Fatalf("docs/benchmarks.md links to %s, which does not exist", match[1])
		}
	}
}

func TestBenchmarkCommandsAreRunnableWithoutAnAgent(t *testing.T) {
	// The page tells people these two need no API key and no network. If either
	// grew a required agent flag, that sentence would become false and nobody
	// would find out until somebody offline tried it.
	root := NewRootCommand(Options{Version: "test"})
	for _, path := range [][]string{{"bench", "scale"}, {"bench", "semantic"}} {
		cmd, _, err := root.Find(path)
		if err != nil {
			t.Fatalf("`%s` does not exist: %v", strings.Join(path, " "), err)
		}
		var required []string
		cmd.Flags().VisitAll(func(f *pflag.Flag) {
			if _, ok := f.Annotations[cobra.BashCompOneRequiredFlag]; ok {
				required = append(required, f.Name)
			}
		})
		if len(required) > 0 {
			t.Fatalf("`%s` now requires %v, but the benchmarks page presents it as zero-configuration",
				cmd.CommandPath(), required)
		}
		if flag := cmd.Flags().Lookup("agent"); flag != nil {
			t.Fatalf("`%s` grew an --agent flag; the benchmarks page claims it needs no model",
				cmd.CommandPath())
		}
	}
}
