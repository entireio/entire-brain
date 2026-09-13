package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFactsEvalContextRequiresJSON(t *testing.T) {
	taskPath := filepath.Join(t.TempDir(), "tasks.json")
	if err := os.WriteFile(taskPath, []byte(`[{"id":"t","task":"payment"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := newFactsEvalCommand(Options{})
	cmd.SetArgs([]string{"--tasks", taskPath, "--include-context"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--include-context requires --json") {
		t.Fatalf("expected JSON requirement before repository access, got %v", err)
	}
}

func TestFactsEvalContextIsOptInAndMatchesScoredItems(t *testing.T) {
	brainDir := t.TempDir()
	fact := factRecord{ID: "fact:scope", Text: "Payment retries reuse a key only within the same attempt.", Paths: []string{"architecture.payment.scope"}, Branch: "main", Status: factStatusActive}
	if err := writeFacts(brainDir, "main", []factRecord{fact}); err != nil {
		t.Fatal(err)
	}
	tasks := []evalTask{{ID: "scope", Task: "payment retries key", Branch: "main", Relevant: []string{fact.ID}, LabelSource: evalLabelSourceHuman}}
	for _, include := range []bool{false, true} {
		results, err := runFactsEvalWithOptions(context.Background(), Options{}, brainDir, "/repo", "main", tasks, 4, false, nil, nil, loadJudgeCache(""), nil, nil, evalRetrieverFacts, factsEvalRunOptions{IncludeContext: include})
		if err != nil {
			t.Fatal(err)
		}
		if len(results) != 1 || results[0].RelevantSurfaced != 1 {
			t.Fatalf("unexpected retrieval result: %+v", results)
		}
		result := results[0]
		payload, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(payload), `"context"`) != include {
			t.Fatalf("context presence must match opt-in: %s", payload)
		}
		if include {
			if len(result.Context) != result.Surfaced || result.Context[0].ID != fact.ID || result.Context[0].Text != fact.Text {
				t.Fatalf("context must be the scored retrieval: %+v", result)
			}
			if estimateRetrievedTokens(result.Context) != result.Tokens {
				t.Fatal("context differs from measured token input")
			}
			if !strings.Contains(string(payload), `"id":"fact:scope"`) {
				t.Fatalf("context wire fields must use stable lowercase tags: %s", payload)
			}
		}
	}
}
