//go:build brain_harness

package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

// Developer-harness command surface (COR-2322): these commands exist only in
// brain_harness builds, so their discoverability contracts live here rather
// than in root_test.go.

// TestHarnessCommandsRemainRunnable keeps the harness help surfaces working
// and pins `bench scale`, the evidence behind the feature matrix's scale row
// (see feature_matrix_test.go).
func TestHarnessCommandsRemainRunnable(t *testing.T) {
	t.Parallel()
	root := NewRootCommand(Options{Version: "test"})
	if cmd, rest, err := root.Find([]string{"bench", "scale"}); err != nil || len(rest) != 0 || cmd.Name() != "scale" {
		t.Fatalf("missing bench scale: %v", err)
	}
	for _, args := range [][]string{{"bench", "--help"}, {"facts", "eval-gen", "--help"}, {"facts", "eval-compare", "--help"}} {
		out, err := execute(t, NewRootCommand(Options{Version: "test"}), args...)
		if err != nil || !strings.Contains(out, "Usage:") {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	missing := filepath.Join(t.TempDir(), "missing-tasks.json")
	out, err := execute(t, NewRootCommand(Options{Version: "test"}), "facts", "eval", "--tasks", missing)
	if err == nil || !strings.Contains(err.Error(), missing) || isUnknownCommandOrFlag(err.Error()) {
		t.Fatalf("eval should reach original task-file read: %v\n%s", err, out)
	}
}

func TestHarnessCommandsHiddenButDiscoverable(t *testing.T) {
	for _, args := range [][]string{{"--help-all"}, {"--help-all", "--help"}} {
		root := NewRootCommand(Options{Version: "test"})
		out, err := execute(t, root, args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		// All-help must list the harness paths without mutating dispatch
		// visibility.
		for _, path := range []string{"bench", "bench index", "bench scale", "memories eval", "memories eval-gen", "memories eval-compare", "history-eval"} {
			if !strings.Contains(out, "\n  "+path+" ") {
				t.Errorf("all-help missing %q", path)
			}
			c, _, err := root.Find(strings.Fields(path))
			// Bench children remain visible within the hidden family so
			// explicit `bench --help` can describe its subcommands.
			wantHidden := path != "bench index" && path != "bench scale"
			if err != nil || c.Hidden != wantHidden {
				t.Errorf("%q hidden should remain %v after all-help: %v", path, wantHidden, err)
			}
		}
	}
}

func TestBenchmarkHelpKeepsHiddenFamilyDiscoverable(t *testing.T) {
	root := NewRootCommand(Options{Version: "test"})
	out, err := execute(t, root, "bench", "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"entire-brain bench [command]", "Available Commands:", "\n  index ", "\n  scale "} {
		if !strings.Contains(out, want) {
			t.Errorf("bench help missing %q:\n%s", want, out)
		}
	}
	bench, _, err := root.Find([]string{"bench"})
	if err != nil || !bench.Hidden {
		t.Fatalf("benchmark family should remain hidden: %v", err)
	}
	for _, child := range bench.Commands() {
		if child.Hidden {
			t.Errorf("bench child %q should be visible in contextual help", child.Name())
		}
	}
	out, err = execute(t, NewRootCommand(Options{Version: "test"}), "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"bench", "bench index", "bench scale"} {
		if strings.Contains(out, "\n  "+path+" ") {
			t.Errorf("default root help should not list %q:\n%s", path, out)
		}
	}
}
