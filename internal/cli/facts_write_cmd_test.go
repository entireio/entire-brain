package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	// A mix of a valid path and an unknown-category one rejects the whole --path
	// rather than silently dropping the unknown (it's deliberate user input, so a
	// typo must surface instead of storing under fewer paths than intended).
	if _, err := resolveRememberPaths(context.Background(), Options{}, rememberCommandOptions{path: "preferences.coding.style,nonsense.foo.bar"}, "/repo", "f", tax); err == nil {
		t.Fatalf("expected error when --path mixes valid and unknown-category paths")
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

func TestResolveRememberPathsOllamaUsesLoopbackRunner(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
	const wantModel = "llama3.2:test"
	type ollamaRequest struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
		Stream bool   `json:"stream"`
	}
	seen := make(chan ollamaRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/generate" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var req ollamaRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		seen <- req
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"response":"preferences.coding.style\n"}`))
	}))
	defer server.Close()
	t.Setenv("ENTIRE_BRAIN_OLLAMA_URL", server.URL+"/api/generate")

	tax := defaultFactTaxonomy(time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC))
	paths, err := resolveRememberPaths(context.Background(), Options{}, rememberCommandOptions{agent: "ollama", model: wantModel}, t.TempDir(), "The user prefers tabs.", tax)
	if err != nil {
		t.Fatalf("ollama classification: %v", err)
	}
	if len(paths) != 1 || paths[0] != "preferences.coding.style" {
		t.Fatalf("unexpected paths: %v", paths)
	}
	select {
	case req := <-seen:
		if req.Model != wantModel {
			t.Fatalf("ollama model = %q, want %q", req.Model, wantModel)
		}
		if req.Stream {
			t.Fatal("remember ollama classification must request non-streaming output")
		}
		if !strings.Contains(req.Prompt, "The user prefers tabs.") {
			t.Fatalf("fact text not sent to ollama prompt: %q", req.Prompt)
		}
	default:
		t.Fatal("ollama server did not receive classification request")
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
