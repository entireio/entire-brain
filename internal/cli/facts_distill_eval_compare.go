package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/spf13/cobra"
)

type distillEvalComparison struct {
	A      string                   `json:"a"`
	B      string                   `json:"b"`
	Paired []metricComparison       `json:"paired"`
	Pooled []distillEvalPooledDelta `json:"pooled"`
}

// distillEvalPooledDelta is a whole-run delta with NO significance claim: the
// underlying metric has no defensible per-session pairing (precision is 0/0 on
// silent sessions).
type distillEvalPooledDelta struct {
	Metric string  `json:"metric"`
	A      float64 `json:"a"`
	B      float64 `json:"b"`
	Delta  float64 `json:"delta"`
	Note   string  `json:"note"`
}

const distillEvalPooledNote = "pooled (no paired test)"

func newDistillEvalCompareCommand(opts Options) *cobra.Command {
	var (
		alpha               float64
		allowCorpusMismatch bool
		allowJudgeMismatch  bool
	)
	cmd := &cobra.Command{
		Use:   "compare <A-summary.json> <B-summary.json>",
		Short: "Paired A/B comparison of two scored runs (Holm-corrected)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := loadDistillEvalSummaryFile(args[0])
			if err != nil {
				return err
			}
			b, err := loadDistillEvalSummaryFile(args[1])
			if err != nil {
				return err
			}
			if a.CorpusSHA256 != b.CorpusSHA256 && !allowCorpusMismatch {
				return fmt.Errorf("corpus fingerprints differ (A=%s, B=%s); rerun on the same corpus or pass --allow-corpus-mismatch", a.CorpusSHA256, b.CorpusSHA256)
			}
			if a.GoldenOnly != b.GoldenOnly && !allowCorpusMismatch {
				return fmt.Errorf("golden-only coverage differs (A=%t, B=%t); the runs scored different session sets — rerun with matching coverage or pass --allow-corpus-mismatch", a.GoldenOnly, b.GoldenOnly)
			}
			if a.JudgeConfig != "" && b.JudgeConfig != "" && a.JudgeConfig != b.JudgeConfig && !allowJudgeMismatch {
				return fmt.Errorf("judge configs differ (A=%s, B=%s); precision is only comparable under one judge — pass --allow-judge-mismatch to override", a.JudgeConfig, b.JudgeConfig)
			}

			result := distillEvalComparison{
				A: a.Variant.Name, B: b.Variant.Name,
				Paired: compareDistillEvalPaired(a, b, alpha),
				Pooled: compareDistillEvalPooled(a, b),
			}
			data, err := json.MarshalIndent(result, "", "  ")
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", data)
			return nil
		},
	}
	cmd.Flags().Float64Var(&alpha, "alpha", 0.05, "Family-wise significance level (Holm-corrected)")
	cmd.Flags().BoolVar(&allowCorpusMismatch, "allow-corpus-mismatch", false, "Compare despite different corpus fingerprints")
	cmd.Flags().BoolVar(&allowJudgeMismatch, "allow-judge-mismatch", false, "Compare despite different judge configs")
	return cmd
}

func loadDistillEvalSummaryFile(path string) (distillEvalSummary, error) {
	var s distillEvalSummary
	data, err := os.ReadFile(path)
	if err != nil {
		return s, fmt.Errorf("read summary %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("decode summary %s: %w", path, err)
	}
	return s, nil
}

// compareDistillEvalPaired pairs sessions by id across the shared set and runs
// the three volume/cost metrics through pairedTTest with Holm correction.
// Every metric here is lower-is-better — the eval's whole premise is that
// unnecessary facts, tokens, and calls are costs.
func compareDistillEvalPaired(a, b distillEvalSummary, alpha float64) []metricComparison {
	aByID := map[string]distillEvalSessionResult{}
	for _, s := range a.Sessions {
		aByID[s.SessionID] = s
	}
	var ids []string
	bByID := map[string]distillEvalSessionResult{}
	for _, s := range b.Sessions {
		bByID[s.SessionID] = s
		if _, ok := aByID[s.SessionID]; ok {
			ids = append(ids, s.SessionID)
		}
	}
	sort.Strings(ids)

	metrics := []struct {
		name string
		get  func(distillEvalSessionResult) float64
	}{
		{"facts", func(r distillEvalSessionResult) float64 { return float64(r.Facts) }},
		{"tokens", func(r distillEvalSessionResult) float64 { return float64(r.Tokens) }},
		{"agent_calls", func(r distillEvalSessionResult) float64 { return float64(r.AgentCalls) }},
	}
	comparisons := make([]metricComparison, 0, len(metrics))
	pvals := make([]float64, 0, len(metrics))
	for _, m := range metrics {
		av := make([]float64, 0, len(ids))
		bv := make([]float64, 0, len(ids))
		for _, id := range ids {
			av = append(av, m.get(aByID[id]))
			bv = append(bv, m.get(bByID[id]))
		}
		st := pairedTTest(av, bv)
		c := metricComparison{
			Metric: m.name, EvidenceBasis: evalMetricEvidenceOperational,
			N: st.N, MeanA: st.MeanA, MeanB: st.MeanB,
			Delta: st.Delta, T: st.T, P: st.P, CohenD: st.CohenD,
		}
		switch {
		case c.MeanA < c.MeanB:
			c.Winner = "a"
		case c.MeanB < c.MeanA:
			c.Winner = "b"
		default:
			c.Winner = "tie"
		}
		comparisons = append(comparisons, c)
		pvals = append(pvals, st.P)
	}
	reject, thresholds := holmRejectWithThresholds(pvals, alpha)
	for i := range comparisons {
		comparisons[i].Significant = reject[i]
		comparisons[i].PHolm = thresholds[i]
	}
	return comparisons
}

func compareDistillEvalPooled(a, b distillEvalSummary) []distillEvalPooledDelta {
	var out []distillEvalPooledDelta
	if a.Precision.Combined != nil && b.Precision.Combined != nil {
		out = append(out, distillEvalPooledDelta{
			Metric: "precision.combined", A: *a.Precision.Combined, B: *b.Precision.Combined,
			Delta: *b.Precision.Combined - *a.Precision.Combined, Note: distillEvalPooledNote,
		})
	}
	out = append(out,
		distillEvalPooledDelta{Metric: "cap_share", A: a.CapShare, B: b.CapShare, Delta: b.CapShare - a.CapShare, Note: distillEvalPooledNote},
		distillEvalPooledDelta{Metric: "redundancy.near_dup_rate", A: a.Redundancy.NearDupRate, B: b.Redundancy.NearDupRate, Delta: b.Redundancy.NearDupRate - a.Redundancy.NearDupRate, Note: distillEvalPooledNote},
	)
	return out
}
