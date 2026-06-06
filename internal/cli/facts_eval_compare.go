package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/spf13/cobra"
)

// metricComparison is a paired A/B comparison of one metric across the tasks
// both runs share, with a Holm-corrected significance verdict.
type metricComparison struct {
	Metric      string  `json:"metric"`
	N           int     `json:"n"`
	MeanA       float64 `json:"mean_a"`
	MeanB       float64 `json:"mean_b"`
	Delta       float64 `json:"delta"`
	T           float64 `json:"t"`
	P           float64 `json:"p"`
	PHolm       float64 `json:"p_holm_threshold"`
	Significant bool    `json:"significant"`
	CohenD      float64 `json:"cohen_d"`
}

// compareEvalSummaries computes a paired A/B comparison over the tasks both
// summaries share (matched by id), for each headline metric, Holm-corrected
// across the metric family at level alpha. A and B must be the same tasks under
// two retrieval configs (e.g. base vs --expand).
func compareEvalSummaries(a, b evalSummary, alpha float64) ([]metricComparison, int) {
	bByID := make(map[string]evalTaskResult, len(b.Results))
	for _, r := range b.Results {
		bByID[r.ID] = r
	}
	var ids []string
	for _, r := range a.Results {
		if _, ok := bByID[r.ID]; ok {
			ids = append(ids, r.ID)
		}
	}
	sort.Strings(ids)
	aByID := make(map[string]evalTaskResult, len(a.Results))
	for _, r := range a.Results {
		aByID[r.ID] = r
	}

	metrics := []struct {
		name string
		get  func(evalTaskResult) float64
	}{
		{"precision", func(r evalTaskResult) float64 { return r.Precision }},
		{"recall", func(r evalTaskResult) float64 { return r.Recall }},
		{"useful_per_1k", func(r evalTaskResult) float64 { return r.UsefulPer1k }},
		{"tokens", func(r evalTaskResult) float64 { return float64(r.Tokens) }},
	}

	comparisons := make([]metricComparison, 0, len(metrics))
	pvals := make([]float64, 0, len(metrics))
	for _, m := range metrics {
		av := make([]float64, len(ids))
		bv := make([]float64, len(ids))
		for i, id := range ids {
			av[i] = m.get(aByID[id])
			bv[i] = m.get(bByID[id])
		}
		st := pairedTTest(av, bv)
		comparisons = append(comparisons, metricComparison{
			Metric: m.name, N: st.N, MeanA: st.MeanA, MeanB: st.MeanB,
			Delta: st.Delta, T: st.T, P: st.P, CohenD: st.CohenD,
		})
		pvals = append(pvals, st.P)
	}
	reject := holmReject(pvals, alpha)
	for i := range comparisons {
		comparisons[i].Significant = reject[i]
	}
	return comparisons, len(ids)
}

func loadEvalSummary(path string) (evalSummary, error) {
	var s evalSummary
	data, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("parse eval summary %s: %w", path, err)
	}
	return s, nil
}

func newFactsEvalCompareCommand(opts Options) *cobra.Command {
	var (
		aPath   string
		bPath   string
		alpha   float64
		jsonOut bool
	)
	cmd := &cobra.Command{
		Use:   "eval-compare --a <A.json> --b <B.json>",
		Short: "Paired A/B comparison of two eval runs with a t-test and Holm correction",
		Long: `eval-compare reads two 'facts eval --json' summaries over the SAME tasks (two
retrieval configs, e.g. base vs --expand) and reports, per metric, the paired
mean delta, a two-sided Student-t p-value, Cohen's d, and a Holm-Bonferroni
family-wise significance verdict. Use it to report a lift honestly instead of
eyeballing two means.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if aPath == "" || bPath == "" {
				return fmt.Errorf("--a and --b are required")
			}
			a, err := loadEvalSummary(aPath)
			if err != nil {
				return err
			}
			b, err := loadEvalSummary(bPath)
			if err != nil {
				return err
			}
			comparisons, n := compareEvalSummaries(a, b, alpha)
			if n == 0 {
				return fmt.Errorf("the two runs share no task ids to compare")
			}
			if jsonOut {
				return writeJSON(cmd, map[string]any{"n": n, "alpha": alpha, "metrics": comparisons})
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "paired A/B over %d shared tasks (Holm-corrected at alpha=%.2f)\n", n, alpha)
			fmt.Fprintf(out, "%-14s %9s %9s %9s %8s %7s %8s %s\n", "metric", "A", "B", "delta", "t", "p", "cohen_d", "sig")
			for _, c := range comparisons {
				sig := ""
				if c.Significant {
					sig = "*"
				}
				fmt.Fprintf(out, "%-14s %9.3f %9.3f %+9.3f %8.2f %7.3f %8.2f %s\n",
					c.Metric, c.MeanA, c.MeanB, c.Delta, c.T, c.P, c.CohenD, sig)
			}
			if n < 12 {
				fmt.Fprintf(out, "note: n=%d is small — treat as directional; significance is underpowered.\n", n)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&aPath, "a", "", "Baseline eval summary JSON (facts eval --json)")
	cmd.Flags().StringVar(&bPath, "b", "", "Comparison eval summary JSON")
	cmd.Flags().Float64Var(&alpha, "alpha", 0.05, "Family-wise significance level for Holm correction")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit the comparison as JSON")
	return cmd
}
