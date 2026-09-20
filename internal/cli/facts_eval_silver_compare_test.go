package cli

import (
	"context"
	"testing"
	"time"
)

func TestActualSilverEvalResultsCompareWithoutProofClaims(t *testing.T) {
	d := t.TempDir()
	paths := normalizeFactPaths([]string{"project.tooling.stack"})
	f := factRecord{ID: factRecordID("The project uses Go modules", paths), Paths: paths, Text: "The project uses Go modules", Branch: "main", Status: factStatusActive, UpdatedAt: time.Now()}
	if err := writeFacts(d, "main", []factRecord{f}); err != nil {
		t.Fatal(err)
	}
	tasks := []evalTask{{ID: "silver", Task: "Go modules", Branch: "main", Relevant: []string{f.ID}, LabelSource: evalLabelSourceProvenanceSilver}}
	result, err := runFactsEval(context.Background(), Options{}, d, "/repo", "main", tasks, 10, false, nil, nil, loadJudgeCache(""), nil, nil, evalRetrieverFacts)
	if err != nil {
		t.Fatal(err)
	}
	metrics, _, err := compareEvalSummaries(summarizeEval(result), summarizeEval(result), .05)
	if err != nil {
		t.Fatal(err)
	}
	for _, metric := range metrics {
		if metric.EvidenceBasis == evalMetricEvidenceProofLabels {
			t.Fatalf("silver comparison claimed proof: %+v", metric)
		}
	}
	for _, source := range []string{evalRelevancePartialSilver, evalRelevanceMixedSilver} {
		partial := result[0]
		partial.RelevanceSource = source
		if evalRelevanceSourcesComparable(partial, partial) {
			t.Fatalf("result-dependent labels considered fixed: %s", source)
		}
	}
}
