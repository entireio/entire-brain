//go:build brain_harness

package cli

// The harness examples the docs retain (COR-2322): these commands exist only
// in brain_harness builds.
func harnessDocumentedCommands() []documentedCommand {
	return []documentedCommand{
		// entire brain bench semantic .
		{path: []string{"bench"}, args: []string{"semantic", "."}, readmeLine: 419},
		// entire brain facts eval-compare --a <before.json> --b <after.json>
		{path: []string{"facts", "eval-compare"}, args: []string{"--a", "missing-a.json", "--b", "missing-b.json"},
			readmeLine: 418, note: "the README's shape, with absent inputs"},
	}
}
