//go:build brain_harness

package cli

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
)

func TestRunFactsEvalWithExpander(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	p := normalizeFactPaths([]string{"architecture.data.flow"})
	// Fact text shares no words with the task, but does with the expansion.
	f := factRecord{ID: factRecordID("the mirror ref reconciliation", p), Paths: p, Text: "the mirror ref reconciliation", Branch: "main", Status: factStatusActive, UpdatedAt: now}
	if err := factstore.Write(brainDir, []factRecord{f}); err != nil {
		t.Fatal(err)
	}
	tasks := []evalTask{{ID: "t1", Task: "fix the sync bug", Branch: "main", Relevant: []string{f.ID}, LabelSource: evalLabelSourceHuman}}

	// Without expansion the query "fix the sync bug" doesn't match the fact.
	base, err := runFactsEval(context.Background(), Options{}, brainDir, "/repo", "main", tasks, 10, false, nil, nil, loadJudgeCache(""), nil, evalRetrieverFacts)
	if err != nil {
		t.Fatal(err)
	}
	if base[0].RelevantSurfaced != 0 {
		t.Fatalf("base query should not surface the fact, got %d", base[0].RelevantSurfaced)
	}

	// An expander that adds the fact's vocabulary lets recall find it.
	expander := func(query string) (string, error) {
		time.Sleep(20 * time.Millisecond)
		return "mirror ref reconciliation", nil
	}
	exp, err := runFactsEval(context.Background(), Options{}, brainDir, "/repo", "main", tasks, 10, false, nil, nil, loadJudgeCache(""), expander, evalRetrieverFacts)
	if err != nil {
		t.Fatal(err)
	}
	if exp[0].RelevantSurfaced != 1 {
		t.Fatalf("expansion should surface the fact, got %d", exp[0].RelevantSurfaced)
	}
	if exp[0].ExpansionLatencyMS <= 0 {
		t.Fatalf("expansion latency should be captured, got %+v", exp[0])
	}
	if exp[0].EndToEndLatencyMS < exp[0].ExpansionLatencyMS || exp[0].EndToEndLatencyMS < exp[0].LatencyMS {
		t.Fatalf("end-to-end latency should include expansion and retrieval, got %+v", exp[0])
	}
	data, err := json.Marshal(summarizeEval(exp))
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		MeanExpansionLatencyMS float64 `json:"mean_expansion_latency_ms"`
		MeanEndToEndLatencyMS  float64 `json:"mean_end_to_end_latency_ms"`
		Results                []struct {
			ExpansionLatencyMS int64 `json:"expansion_latency_ms"`
			EndToEndLatencyMS  int64 `json:"end_to_end_latency_ms"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.MeanExpansionLatencyMS <= 0 || payload.MeanEndToEndLatencyMS <= 0 || len(payload.Results) != 1 || payload.Results[0].ExpansionLatencyMS <= 0 || payload.Results[0].EndToEndLatencyMS <= 0 {
		t.Fatalf("summary JSON should include additive latency fields: %s", data)
	}
}
