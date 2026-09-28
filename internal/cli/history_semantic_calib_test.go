package cli

import (
	"os"
	"sort"
	"testing"
)

// TestHistorySemanticCalibrationDiagnostic reports whether the embedder can separate
// relevant history decisions from noise — the gate for adding a semantic arm to
// history (today BM25-only). Model2Vec couldn't: on the live brain an unrelated
// query ("fake metadata") scored cosine 0.336, higher than every paraphrase
// positive (≤0.284). This re-runs the same probe with whatever embedder is
// selected (ENTIRE_BRAIN_EMBEDDER=ollama for EmbeddingGemma) and reports the
// separation: a semantic arm is viable only if the positives' cosine clears the
// negatives'. Env: CALIB_BRAIN, CALIB_BRANCH (main).
// This is an opt-in calibration diagnostic, not a fixed quality gate: expected
// separation varies with the selected external model and the supplied brain.
func TestHistorySemanticCalibrationDiagnostic(t *testing.T) {
	brainDir := os.Getenv("CALIB_BRAIN")
	if brainDir == "" {
		t.Skip("set CALIB_BRAIN to a built brain dir")
	}
	e := defaultEmbedder()
	if e == nil {
		t.Fatal("no embedder")
	}
	embedQ := func(s string) []float32 {
		if qe, ok := e.(queryEmbedder); ok {
			return qe.EmbedQuery(s)
		}
		return e.Embed(s)
	}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil || manifest.Sources == nil || manifest.Sources.History == nil {
		t.Fatalf("history source: %v", err)
	}
	index, err := loadBrainHistoryIndex(brainDir, manifest.Sources.History)
	if err != nil {
		t.Fatalf("history index: %v", err)
	}
	var summaries []string
	var vecs [][]float32
	for _, r := range index.Records {
		if r.Kind == "decision" {
			summaries = append(summaries, r.Summary)
			vecs = append(vecs, e.Embed(r.Summary))
		}
	}
	t.Logf("embedder=%s  decision_records=%d", e.ID(), len(summaries))

	// Positives are paraphrases of topics this brain decided on; negatives are
	// unrelated. (Tuned for the entire-brain brain — the retrieval/embedding
	// meta-domain.) maxCos = best match any decision gets for the query.
	maxCos := func(q string) (float64, string) {
		qv := embedQ(q)
		best, bestIdx := -1.0, 0
		for i := range vecs {
			if c := cosineFloat32(qv, vecs[i]); c > best {
				best, bestIdx = c, i
			}
		}
		s := summaries[bestIdx]
		if len(s) > 70 {
			s = s[:70]
		}
		return best, s
	}
	type probe struct {
		label string
		max   float64
	}
	var pos, neg []probe
	for _, q := range []string{
		"reasons we picked one embedding approach over another",
		"tradeoffs of shipping native binaries to many machines",
		"why did we abandon the pure Go constraint",
		"how does reconcile decide to supersede a fact",
		"what is the precision control for history ranking",
	} {
		m, hit := maxCos(q)
		pos = append(pos, probe{q, m})
		t.Logf("POS %.3f  %q -> %q", m, q, hit)
	}
	for _, q := range []string{
		"fake metadata",
		"banana pancake breakfast recipe",
		"weather forecast for tomorrow in Berlin",
		"how to tune a guitar",
	} {
		m, _ := maxCos(q)
		neg = append(neg, probe{q, m})
		t.Logf("NEG %.3f  %q", m, q)
	}
	sort.Slice(pos, func(i, j int) bool { return pos[i].max < pos[j].max })
	sort.Slice(neg, func(i, j int) bool { return neg[i].max > neg[j].max })
	worstPos, bestNeg := pos[0].max, neg[0].max
	t.Logf("=== %s: worst positive=%.3f (%q)  best negative=%.3f (%q)  separated=%v ===",
		e.ID(), worstPos, pos[0].label, bestNeg, neg[0].label, worstPos > bestNeg)
}
