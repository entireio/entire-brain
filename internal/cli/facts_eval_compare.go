package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

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
	Winner      string  `json:"winner,omitempty"`
	Claim       string  `json:"claim"`
}

// compareEvalSummaries computes a paired A/B comparison over the tasks both
// summaries share (matched by id), for each headline metric, Holm-corrected
// across the metric family at level alpha. A and B must be the same tasks under
// two retrieval configs (e.g. base vs --expand).
func compareEvalSummaries(a, b evalSummary, alpha float64) ([]metricComparison, int, error) {
	return compareEvalSummariesWithOptions(a, b, alpha, false)
}

func compareEvalSummariesWithOptions(a, b evalSummary, alpha float64, allowProxyComparison bool) ([]metricComparison, int, error) {
	comparisons, n, _, _, err := compareEvalSummariesInternal(a, b, alpha, allowProxyComparison, false)
	return comparisons, n, err
}

type evalCompareOptions struct {
	AllowProxyComparison       bool
	AllowMissingTasks          bool
	AllowTaskHashMismatch      bool
	AllowBrainManifestMismatch bool
}

func compareEvalSummariesInternal(a, b evalSummary, alpha float64, allowProxyComparison, allowMissingTasks bool) ([]metricComparison, int, []string, []string, error) {
	return compareEvalSummariesInternalWithOptions(a, b, alpha, evalCompareOptions{AllowProxyComparison: allowProxyComparison, AllowMissingTasks: allowMissingTasks})
}

func compareEvalSummariesInternalWithOptions(a, b evalSummary, alpha float64, opts evalCompareOptions) ([]metricComparison, int, []string, []string, error) {
	if err := validateEvalSummaryTaskHashes(a, b, opts.AllowTaskHashMismatch); err != nil {
		return nil, 0, nil, nil, err
	}
	if err := validateEvalSummaryBrainManifestHashes(a, b, opts.AllowBrainManifestMismatch); err != nil {
		return nil, 0, nil, nil, err
	}
	aByID, err := evalResultsByID(a.Results, "A")
	if err != nil {
		return nil, 0, nil, nil, err
	}
	bByID, err := evalResultsByID(b.Results, "B")
	if err != nil {
		return nil, 0, nil, nil, err
	}
	missingFromA, missingFromB := missingEvalTaskIDs(aByID, bByID)
	if !opts.AllowMissingTasks && (len(missingFromA) > 0 || len(missingFromB) > 0) {
		return nil, 0, missingFromA, missingFromB, fmt.Errorf("eval task id sets differ: %d missing from A, %d missing from B (pass --allow-missing-tasks to compare shared ids only)", len(missingFromA), len(missingFromB))
	}
	var ids []string
	for _, r := range a.Results {
		if other, ok := bByID[r.ID]; ok {
			if err := validateEvalPair(r, other); err != nil {
				return nil, 0, missingFromA, missingFromB, err
			}
			ids = append(ids, r.ID)
		}
	}
	sort.Strings(ids)

	metrics := []struct {
		name              string
		get               func(evalTaskResult) float64
		include           func(evalTaskResult, evalTaskResult) bool
		requiresSameTruth bool
	}{
		{"precision", func(r evalTaskResult) float64 { return r.Precision }, nil, true},
		{"recall", func(r evalTaskResult) float64 { return r.Recall }, func(a, b evalTaskResult) bool { return a.Labeled && b.Labeled }, true},
		{"useful_per_1k", func(r evalTaskResult) float64 { return r.UsefulPer1k }, nil, true},
		{"tokens", func(r evalTaskResult) float64 { return float64(r.Tokens) }, nil, false},
		{"latency_ms", func(r evalTaskResult) float64 { return float64(r.LatencyMS) }, nil, false},
	}

	comparisons := make([]metricComparison, 0, len(metrics))
	pvals := make([]float64, 0, len(metrics))
	for _, m := range metrics {
		var av, bv []float64
		for _, id := range ids {
			ar, br := aByID[id], bByID[id]
			if m.include != nil && !m.include(ar, br) {
				continue
			}
			if m.requiresSameTruth && !opts.AllowProxyComparison && !evalRelevanceSourcesComparable(ar, br) {
				return nil, 0, missingFromA, missingFromB, fmt.Errorf("task %q compares %s with non-proof or different relevance sources (A=%s label_source=%s labeled=%t, B=%s label_source=%s labeled=%t); rerun with human/judge_refined labels or pass --allow-proxy-comparison", id, m.name, valueOrUnset(ar.RelevanceSource), valueOrUnset(ar.LabelSource), ar.Labeled, valueOrUnset(br.RelevanceSource), valueOrUnset(br.LabelSource), br.Labeled)
			}
			av = append(av, m.get(ar))
			bv = append(bv, m.get(br))
		}
		st := pairedTTest(av, bv)
		comparisons = append(comparisons, metricComparison{
			Metric: m.name, N: st.N, MeanA: st.MeanA, MeanB: st.MeanB,
			Delta: st.Delta, T: st.T, P: st.P, CohenD: st.CohenD,
		})
		pvals = append(pvals, st.P)
	}
	reject, thresholds := holmRejectWithThresholds(pvals, alpha)
	for i := range comparisons {
		comparisons[i].Significant = reject[i]
		comparisons[i].PHolm = thresholds[i]
		comparisons[i].Winner = evalMetricWinner(comparisons[i])
		comparisons[i].Claim = evalMetricClaim(comparisons[i])
	}
	return comparisons, len(ids), missingFromA, missingFromB, nil
}

func validateEvalSummaryTaskHashes(a, b evalSummary, allowMismatch bool) error {
	if allowMismatch {
		return nil
	}
	aHash := evalSummaryTasksSHA256(a)
	bHash := evalSummaryTasksSHA256(b)
	if aHash == "" || bHash == "" || aHash == bHash {
		return nil
	}
	return fmt.Errorf("eval run_config.tasks_sha256 values differ (A=%s, B=%s); rerun over the same tasks file or pass --allow-task-hash-mismatch", aHash, bHash)
}

func validateEvalSummaryBrainManifestHashes(a, b evalSummary, allowMismatch bool) error {
	if allowMismatch {
		return nil
	}
	aHash := evalSummaryBrainManifestSHA256(a)
	bHash := evalSummaryBrainManifestSHA256(b)
	if aHash == "" || bHash == "" || aHash == bHash {
		return nil
	}
	return fmt.Errorf("eval run_config.brain_manifest_sha256 values differ (A=%s, B=%s); rerun against the same brain manifest or pass --allow-brain-manifest-mismatch", aHash, bHash)
}

func evalSummaryTasksSHA256(s evalSummary) string {
	if s.RunConfig == nil {
		return ""
	}
	return strings.TrimSpace(s.RunConfig.TasksSHA256)
}

func evalSummaryBrainManifestSHA256(s evalSummary) string {
	if s.RunConfig == nil {
		return ""
	}
	return strings.TrimSpace(s.RunConfig.BrainManifestSHA256)
}

func missingEvalTaskIDs(aByID, bByID map[string]evalTaskResult) ([]string, []string) {
	var missingFromA, missingFromB []string
	for id := range bByID {
		if _, ok := aByID[id]; !ok {
			missingFromA = append(missingFromA, id)
		}
	}
	for id := range aByID {
		if _, ok := bByID[id]; !ok {
			missingFromB = append(missingFromB, id)
		}
	}
	sort.Strings(missingFromA)
	sort.Strings(missingFromB)
	return missingFromA, missingFromB
}

func evalRelevanceSourcesComparable(a, b evalTaskResult) bool {
	if a.RelevanceSource == "" || b.RelevanceSource == "" {
		return false
	}
	if a.Labeled != b.Labeled || a.RelevanceSource != b.RelevanceSource {
		return false
	}
	return evalRelevanceSourceIsProofLabel(a) && evalRelevanceSourceIsProofLabel(b)
}

func evalRelevanceSourceIsProofLabel(r evalTaskResult) bool {
	if !r.Labeled || r.RelevanceSource != evalRelevanceExplicitLabel {
		return false
	}
	switch strings.TrimSpace(r.LabelSource) {
	case evalLabelSourceHuman, evalLabelSourceJudgeRefined:
		return true
	default:
		return false
	}
}

func evalResultsByID(results []evalTaskResult, label string) (map[string]evalTaskResult, error) {
	byID := make(map[string]evalTaskResult, len(results))
	for _, r := range results {
		if r.ID == "" {
			return nil, fmt.Errorf("%s eval summary contains a result with empty id", label)
		}
		if _, exists := byID[r.ID]; exists {
			return nil, fmt.Errorf("%s eval summary contains duplicate task id %q", label, r.ID)
		}
		byID[r.ID] = r
	}
	return byID, nil
}

func validateEvalPair(a, b evalTaskResult) error {
	if a.Task != "" && b.Task != "" && a.Task != b.Task {
		return fmt.Errorf("task %q differs between eval summaries", a.ID)
	}
	if a.QueryType != "" && b.QueryType != "" && a.QueryType != b.QueryType {
		return fmt.Errorf("task %q query_type differs between eval summaries", a.ID)
	}
	return nil
}

func evalMetricWinner(c metricComparison) string {
	switch c.Metric {
	case "tokens", "latency_ms":
		if c.MeanA < c.MeanB {
			return "a"
		}
		if c.MeanB < c.MeanA {
			return "b"
		}
	default:
		if c.MeanA > c.MeanB {
			return "a"
		}
		if c.MeanB > c.MeanA {
			return "b"
		}
	}
	return "tie"
}

func evalMetricClaim(c metricComparison) string {
	if !c.Significant {
		return "directional only; not significant after Holm correction"
	}
	switch c.Winner {
	case "a":
		return "A wins significantly"
	case "b":
		return "B wins significantly"
	default:
		return "no winner"
	}
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
		aPath                      string
		bPath                      string
		alpha                      float64
		jsonOut                    bool
		allowProxyComparison       bool
		allowMissingTasks          bool
		allowTaskHashMismatch      bool
		allowBrainManifestMismatch bool
	)
	cmd := &cobra.Command{
		Use:   "eval-compare --a <A.json> --b <B.json>",
		Short: "Paired A/B comparison of two eval runs with a t-test and Holm correction",
		Long: `eval-compare reads two 'facts eval --json' summaries over the SAME tasks (two
retrieval configs, e.g. base vs --expand) and reports, per metric, the paired
mean delta, a two-sided Student-t p-value, Cohen's d, and a Holm-Bonferroni
family-wise significance verdict. Relevance metrics require comparable
human/judge_refined proof labels unless --allow-proxy-comparison is explicit.
Task IDs, non-empty task-file hashes, and non-empty brain manifest hashes must
match exactly unless the corresponding override is explicit. Use it to report a
lift honestly instead of eyeballing two means.`,
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
			compareOpts := evalCompareOptions{
				AllowProxyComparison:       allowProxyComparison,
				AllowMissingTasks:          allowMissingTasks,
				AllowTaskHashMismatch:      allowTaskHashMismatch,
				AllowBrainManifestMismatch: allowBrainManifestMismatch,
			}
			comparisons, n, missingFromA, missingFromB, err := compareEvalSummariesInternalWithOptions(a, b, alpha, compareOpts)
			if err != nil {
				return err
			}
			if n == 0 {
				return fmt.Errorf("the two runs share no task ids to compare")
			}
			if jsonOut {
				return writeJSON(cmd, map[string]any{"n": n, "alpha": alpha, "a_retriever": a.Retriever, "b_retriever": b.Retriever, "a_tasks_sha256": evalSummaryTasksSHA256(a), "b_tasks_sha256": evalSummaryTasksSHA256(b), "a_brain_manifest_sha256": evalSummaryBrainManifestSHA256(a), "b_brain_manifest_sha256": evalSummaryBrainManifestSHA256(b), "allow_proxy_comparison": allowProxyComparison, "allow_missing_tasks": allowMissingTasks, "allow_task_hash_mismatch": allowTaskHashMismatch, "allow_brain_manifest_mismatch": allowBrainManifestMismatch, "missing_from_a": missingFromA, "missing_from_b": missingFromB, "metrics": comparisons})
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "paired A/B over %d shared tasks (Holm-corrected at alpha=%.2f)\n", n, alpha)
			if a.Retriever != "" || b.Retriever != "" {
				fmt.Fprintf(out, "A=%s B=%s\n", valueOrUnset(a.Retriever), valueOrUnset(b.Retriever))
			}
			if allowProxyComparison {
				fmt.Fprintln(out, "warning: relevance metrics may mix explicit labels, source-match proxies, and/or judge labels")
			}
			if allowMissingTasks && (len(missingFromA) > 0 || len(missingFromB) > 0) {
				fmt.Fprintf(out, "warning: comparing shared ids only; missing_from_a=%d missing_from_b=%d\n", len(missingFromA), len(missingFromB))
			}
			if allowTaskHashMismatch {
				fmt.Fprintln(out, "warning: task file hashes differ or were explicitly ignored; label-set equality is not guaranteed")
			}
			if allowBrainManifestMismatch {
				fmt.Fprintln(out, "warning: brain manifest hashes differ or were explicitly ignored; paired run state is not identical")
			}
			fmt.Fprintf(out, "%-14s %9s %9s %9s %8s %7s %8s %-6s %s\n", "metric", "A", "B", "delta", "t", "p", "cohen_d", "winner", "claim")
			for _, c := range comparisons {
				fmt.Fprintf(out, "%-14s %9.3f %9.3f %+9.3f %8.2f %7.3f %8.2f %-6s %s\n",
					c.Metric, c.MeanA, c.MeanB, c.Delta, c.T, c.P, c.CohenD, c.Winner, c.Claim)
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
	cmd.Flags().BoolVar(&allowProxyComparison, "allow-proxy-comparison", false, "Allow relevance metrics to compare runs with different relevance sources")
	cmd.Flags().BoolVar(&allowMissingTasks, "allow-missing-tasks", false, "Compare only shared task ids when eval runs have missing tasks")
	cmd.Flags().BoolVar(&allowTaskHashMismatch, "allow-task-hash-mismatch", false, "Allow comparison when non-empty eval run_config.tasks_sha256 values differ")
	cmd.Flags().BoolVar(&allowBrainManifestMismatch, "allow-brain-manifest-mismatch", false, "Allow comparison when non-empty eval run_config.brain_manifest_sha256 values differ")
	return cmd
}
