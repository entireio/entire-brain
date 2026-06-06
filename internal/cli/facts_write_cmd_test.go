package cli

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestParseClassifyOutput(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	tax := defaultFactTaxonomy(now)

	// Clean single path.
	if got := parseClassifyOutput("preferences.coding.style\n", tax); len(got) != 1 || got[0] != "preferences.coding.style" {
		t.Fatalf("single path: got %v", got)
	}
	// Two comma-separated paths.
	got := parseClassifyOutput("architecture.data.flow, project.tooling.stack", tax)
	if len(got) != 2 {
		t.Fatalf("two paths: got %v", got)
	}
	// Unknown top-level dropped -> empty.
	if got := parseClassifyOutput("nonsense.foo.bar", tax); len(got) != 0 {
		t.Fatalf("unknown top-level should be dropped: %v", got)
	}
	// Skips a prose preamble line, finds the path on a later line.
	if got := parseClassifyOutput("Here is the path:\nworkflow.testing.rules", tax); len(got) != 1 || got[0] != "workflow.testing.rules" {
		t.Fatalf("should skip prose and find path: %v", got)
	}
}

func TestResolveRememberPathsExplicit(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	tax := defaultFactTaxonomy(now)
	// Explicit valid --path is used as-is (no agent call).
	paths, err := resolveRememberPaths(context.Background(), Options{}, rememberCommandOptions{path: "preferences.coding.style"}, "/repo", "some fact", tax)
	if err != nil || len(paths) != 1 || paths[0] != "preferences.coding.style" {
		t.Fatalf("explicit path: %v err=%v", paths, err)
	}
	// Invalid explicit --path errors.
	if _, err := resolveRememberPaths(context.Background(), Options{}, rememberCommandOptions{path: "not-a-path"}, "/repo", "f", tax); err == nil {
		t.Fatalf("expected error for invalid --path")
	}
	// Syntactically valid but under an unknown top-level category is rejected,
	// just like agent classification — no immediately-orphaned facts via --path.
	if _, err := resolveRememberPaths(context.Background(), Options{}, rememberCommandOptions{path: "nonsense.foo.bar"}, "/repo", "f", tax); err == nil {
		t.Fatalf("expected error for --path under unknown taxonomy category")
	}
	// A new three-level path under a KNOWN top-level is still allowed (the
	// taxonomy permits inventing paths under existing categories).
	got, err := resolveRememberPaths(context.Background(), Options{}, rememberCommandOptions{path: "preferences.newarea.flavor"}, "/repo", "f", tax)
	if err != nil || len(got) != 1 || got[0] != "preferences.newarea.flavor" {
		t.Fatalf("new path under known category should be allowed: %v err=%v", got, err)
	}
}

func TestResolveRememberPathsAgentClassification(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	tax := defaultFactTaxonomy(now)
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		if !strings.Contains(string(input), "prefers tabs") {
			t.Errorf("classifier should receive the fact text on stdin, got %q", input)
		}
		return "preferences.coding.style\n", nil
	}
	opts := rememberCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: run}
	paths, err := resolveRememberPaths(context.Background(), Options{}, opts, "/repo", "The user prefers tabs.", tax)
	if err != nil || len(paths) != 1 || paths[0] != "preferences.coding.style" {
		t.Fatalf("agent classification: %v err=%v", paths, err)
	}
}

func TestResolveRememberPathsNoAgentRequiresPath(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	tax := defaultFactTaxonomy(now)
	opts := rememberCommandOptions{agent: "none"}
	if _, err := resolveRememberPaths(context.Background(), Options{}, opts, "/repo", "f", tax); err == nil {
		t.Fatalf("expected error: no agent and no --path")
	}
}
