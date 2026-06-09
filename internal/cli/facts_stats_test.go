package cli

import (
	"math"
	"testing"
)

func approx(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestStudentTTwoSidedP(t *testing.T) {
	// Reference values (scipy stats.t.sf*2):
	//   t=2.0, df=10  -> 0.073388
	//   t=2.228, df=10 -> ~0.05
	//   t=0          -> 1.0
	//   t=3.169, df=10 -> ~0.01
	cases := []struct {
		t, df, want, tol float64
	}{
		{2.0, 10, 0.073388, 1e-4},
		{2.228, 10, 0.05, 2e-3},
		{0, 10, 1.0, 1e-9},
		{3.169, 10, 0.01, 2e-3},
		{1.0, 1, 0.5, 1e-3}, // Cauchy: t=1,df=1 -> 0.5
	}
	for _, c := range cases {
		if got := studentTTwoSidedP(c.t, c.df); !approx(got, c.want, c.tol) {
			t.Errorf("studentTTwoSidedP(%v, %v) = %v, want ~%v", c.t, c.df, got, c.want)
		}
	}
}

func TestIncompleteBetaSymmetry(t *testing.T) {
	// I_x(a,b) = 1 - I_{1-x}(b,a)
	if got := incompleteBeta(2, 3, 0.4); !approx(got, 1-incompleteBeta(3, 2, 0.6), 1e-9) {
		t.Errorf("incompleteBeta symmetry broken: %v", got)
	}
	if incompleteBeta(2, 2, 0.5) < 0.49 || incompleteBeta(2, 2, 0.5) > 0.51 {
		t.Errorf("I_0.5(2,2) should be 0.5, got %v", incompleteBeta(2, 2, 0.5))
	}
}

func TestPairedTTest(t *testing.T) {
	// B is consistently ~0.1 higher than A across 5 tasks.
	a := []float64{0.1, 0.2, 0.15, 0.05, 0.3}
	b := []float64{0.2, 0.31, 0.24, 0.16, 0.41}
	st := pairedTTest(a, b)
	if st.N != 5 {
		t.Fatalf("n = %d", st.N)
	}
	if st.Delta < 0.09 || st.Delta > 0.13 {
		t.Errorf("delta = %v, want ~0.1", st.Delta)
	}
	if st.T <= 0 {
		t.Errorf("t should be positive for a positive shift, got %v", st.T)
	}
	if st.P > 0.05 {
		t.Errorf("a consistent ~0.1 shift should be significant, p=%v", st.P)
	}
	if st.CohenD <= 0 {
		t.Errorf("cohen_d should be positive, got %v", st.CohenD)
	}

	// Identical series: no effect.
	id := pairedTTest(a, a)
	if id.Delta != 0 || id.P != 1 {
		t.Errorf("identical series should give delta=0, p=1, got %+v", id)
	}

	// Constant non-zero shift: perfectly reproducible -> p=0.
	c := pairedTTest([]float64{1, 2, 3}, []float64{2, 3, 4})
	if c.P != 0 || c.Delta != 1 {
		t.Errorf("constant +1 shift should give delta=1, p=0, got %+v", c)
	}
}

func TestHolmReject(t *testing.T) {
	// p = [0.01, 0.04, 0.03], alpha 0.05, n=3.
	// sorted: 0.01 vs 0.05/3=0.0167 -> reject; 0.03 vs 0.05/2=0.025 -> fail -> stop.
	got := holmReject([]float64{0.01, 0.04, 0.03}, 0.05)
	if !got[0] || got[1] || got[2] {
		t.Fatalf("Holm rejections wrong: %v", got)
	}
	// All tiny: all reject.
	all := holmReject([]float64{0.001, 0.002, 0.003}, 0.05)
	for i, r := range all {
		if !r {
			t.Errorf("expected reject at %d", i)
		}
	}
}

func TestCompareEvalSummaries(t *testing.T) {
	a := evalSummary{Results: []evalTaskResult{
		{ID: "t1", Task: "one", Recall: 0.1, UsefulPer1k: 1, Tokens: 100, Labeled: true},
		{ID: "t2", Task: "two", Recall: 0.2, UsefulPer1k: 2, Tokens: 100, Labeled: true},
		{ID: "t3", Task: "three", Recall: 0.15, UsefulPer1k: 1.5, Tokens: 100, Labeled: true},
		{ID: "only-a", Task: "only", Recall: 9, UsefulPer1k: 9, Tokens: 9, Labeled: true},
	}}
	b := evalSummary{Results: []evalTaskResult{
		{ID: "t1", Task: "one", Recall: 0.2, UsefulPer1k: 2, Tokens: 100, Labeled: true},
		{ID: "t2", Task: "two", Recall: 0.3, UsefulPer1k: 3, Tokens: 100, Labeled: true},
		{ID: "t3", Task: "three", Recall: 0.25, UsefulPer1k: 2.5, Tokens: 100, Labeled: true},
	}}
	comps, n, err := compareEvalSummaries(a, b, 0.05)
	if err != nil {
		t.Fatalf("compareEvalSummaries: %v", err)
	}
	if n != 3 {
		t.Fatalf("should compare only the 3 shared tasks, got n=%d", n)
	}
	byMetric := map[string]metricComparison{}
	for _, c := range comps {
		byMetric[c.Metric] = c
	}
	if r := byMetric["recall"]; r.Delta < 0.09 || r.Delta > 0.11 {
		t.Errorf("recall delta = %v, want ~0.1", r.Delta)
	}
	// Tokens identical -> delta 0.
	if tk := byMetric["tokens"]; tk.Delta != 0 {
		t.Errorf("tokens delta should be 0, got %v", tk.Delta)
	}
	if byMetric["recall"].PHolm == 0 {
		t.Errorf("recall Holm threshold should be populated: %+v", byMetric["recall"])
	}
}

func TestCompareEvalSummariesSkipsUndefinedRecallAndRejectsBadPairs(t *testing.T) {
	a := evalSummary{Results: []evalTaskResult{
		{ID: "t1", Task: "same", UsefulPer1k: 1, Tokens: 100},
		{ID: "dup", Task: "x"},
		{ID: "dup", Task: "x"},
	}}
	b := evalSummary{Results: []evalTaskResult{{ID: "t1", Task: "same", UsefulPer1k: 2, Tokens: 120}}}
	if _, _, err := compareEvalSummaries(a, b, 0.05); err == nil {
		t.Fatal("duplicate task ids should be rejected")
	}

	a = evalSummary{Results: []evalTaskResult{{ID: "t1", Task: "old"}}}
	b = evalSummary{Results: []evalTaskResult{{ID: "t1", Task: "new"}}}
	if _, _, err := compareEvalSummaries(a, b, 0.05); err == nil {
		t.Fatal("mismatched task text should be rejected")
	}

	a = evalSummary{Results: []evalTaskResult{{ID: "t1", Task: "same", UsefulPer1k: 1, Tokens: 100}}}
	b = evalSummary{Results: []evalTaskResult{{ID: "t1", Task: "same", UsefulPer1k: 2, Tokens: 120}}}
	comps, _, err := compareEvalSummaries(a, b, 0.05)
	if err != nil {
		t.Fatalf("compare unlabeled: %v", err)
	}
	for _, c := range comps {
		if c.Metric == "recall" && c.N != 0 {
			t.Fatalf("undefined recall should be skipped, got %+v", c)
		}
	}
}

func TestCompareEvalSummariesRejectsMixedRelevanceSources(t *testing.T) {
	a := evalSummary{Retriever: evalRetrieverFacts, Results: []evalTaskResult{{
		ID: "t1", Task: "same", Precision: 1, UsefulPer1k: 10, Tokens: 100,
		Labeled: true, RelevanceSource: evalRelevanceExplicitLabel,
	}}}
	b := evalSummary{Retriever: evalRetrieverRawSessions, Results: []evalTaskResult{{
		ID: "t1", Task: "same", Precision: 1, UsefulPer1k: 10, Tokens: 120,
		Labeled: false, RelevanceSource: evalRelevanceSourceMatch,
	}}}
	if _, _, err := compareEvalSummaries(a, b, 0.05); err == nil {
		t.Fatal("mixed explicit labels and source-match proxy relevance should be rejected by default")
	}
	comps, n, err := compareEvalSummariesWithOptions(a, b, 0.05, true)
	if err != nil {
		t.Fatalf("allow proxy comparison: %v", err)
	}
	if n != 1 || len(comps) == 0 {
		t.Fatalf("unexpected allowed comparison n=%d comps=%+v", n, comps)
	}
}
