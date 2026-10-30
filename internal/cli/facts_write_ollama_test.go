package cli

// This root stayed behind when facts_write_cmd_test.go moved to
// internal/cli/command/facts: with no --agent-command Run override it reaches
// the real ollama distill runner, a cli-private seam injected through
// factsCommandConfig.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/command/facts"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
)

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

	tax := factstore.DefaultTaxonomy(time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC))
	paths, err := facts.ResolveRememberPaths(context.Background(), factsCommandConfig(Options{}), facts.RememberCommandOptions{Agent: "ollama", Model: wantModel}, t.TempDir(), "The user prefers tabs.", tax)
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
