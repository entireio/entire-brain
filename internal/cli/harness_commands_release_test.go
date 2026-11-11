//go:build !brain_harness

package cli

import (
	"strings"
	"testing"
)

// COR-2322 acceptance: builds without the brain_harness tag (the release
// configuration) must not carry the eval/bench developer harnesses.
func TestReleaseBuildOmitsHarnessCommands(t *testing.T) {
	root := NewRootCommand(Options{Version: "test"})
	out, err := execute(t, root, "--help-all")
	if err != nil {
		t.Fatalf("--help-all: %v\n%s", err, out)
	}
	for _, path := range []string{"bench", "memories eval", "memories eval-gen", "memories eval-compare", "history-eval", "history-eval-gen"} {
		if strings.Contains(out, "\n  "+path+" ") {
			t.Errorf("all-help lists harness command %q:\n%s", path, out)
		}
		c, _, err := root.Find(strings.Fields(path))
		if err == nil && c.CommandPath() == root.Name()+" "+path {
			t.Errorf("harness command %q is registered in a release build", path)
		}
	}
}
