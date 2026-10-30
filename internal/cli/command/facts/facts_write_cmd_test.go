package facts

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
)

func TestParseClassifyOutput(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	tax := factstore.DefaultTaxonomy(now)

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
	tax := factstore.DefaultTaxonomy(now)
	// Explicit valid --path is used as-is (no agent call).
	paths, err := ResolveRememberPaths(context.Background(), Config{}, RememberCommandOptions{Path: "preferences.coding.style"}, "/repo", "some fact", tax)
	if err != nil || len(paths) != 1 || paths[0] != "preferences.coding.style" {
		t.Fatalf("explicit path: %v err=%v", paths, err)
	}
	// Invalid explicit --path errors.
	if _, err := ResolveRememberPaths(context.Background(), Config{}, RememberCommandOptions{Path: "not-a-path"}, "/repo", "f", tax); err == nil {
		t.Fatalf("expected error for invalid --path")
	}
	// Syntactically valid but under an unknown top-level category is rejected,
	// just like agent classification — no immediately-orphaned facts via --path.
	if _, err := ResolveRememberPaths(context.Background(), Config{}, RememberCommandOptions{Path: "nonsense.foo.bar"}, "/repo", "f", tax); err == nil {
		t.Fatalf("expected error for --path under unknown taxonomy category")
	}
	// A mix of a valid path and an unknown-category one rejects the whole --path
	// rather than silently dropping the unknown (it's deliberate user input, so a
	// typo must surface instead of storing under fewer paths than intended).
	if _, err := ResolveRememberPaths(context.Background(), Config{}, RememberCommandOptions{Path: "preferences.coding.style,nonsense.foo.bar"}, "/repo", "f", tax); err == nil {
		t.Fatalf("expected error when --path mixes valid and unknown-category paths")
	}
	// A new three-level path under a KNOWN top-level is still allowed (the
	// taxonomy permits inventing paths under existing categories).
	got, err := ResolveRememberPaths(context.Background(), Config{}, RememberCommandOptions{Path: "preferences.newarea.flavor"}, "/repo", "f", tax)
	if err != nil || len(got) != 1 || got[0] != "preferences.newarea.flavor" {
		t.Fatalf("new path under known category should be allowed: %v err=%v", got, err)
	}
}

func TestResolveRememberPathsAgentClassification(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	tax := factstore.DefaultTaxonomy(now)
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		if !strings.Contains(string(input), "prefers tabs") {
			t.Errorf("classifier should receive the fact text on stdin, got %q", input)
		}
		return "preferences.coding.style\n", nil
	}
	opts := RememberCommandOptions{Agent: "command", AgentCommand: []string{"fake"}, Run: run}
	paths, err := ResolveRememberPaths(context.Background(), Config{}, opts, "/repo", "The user prefers tabs.", tax)
	if err != nil || len(paths) != 1 || paths[0] != "preferences.coding.style" {
		t.Fatalf("agent classification: %v err=%v", paths, err)
	}
}

func TestResolveRememberPathsNoAgentRequiresPath(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	tax := factstore.DefaultTaxonomy(now)
	opts := RememberCommandOptions{Agent: "none"}
	if _, err := ResolveRememberPaths(context.Background(), Config{}, opts, "/repo", "f", tax); err == nil {
		t.Fatalf("expected error: no agent and no --path")
	}
}
