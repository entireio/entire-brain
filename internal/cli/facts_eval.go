package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// evalTask is one held-out retrieval task. Task is the query an agent would
// issue; Relevant, when present, is the ground-truth set of fact ids that
// genuinely help the task (enables deterministic precision/recall). Without
// labels, --judge has an agent decide relevance per surfaced fact.
type evalTask struct {
	ID        string   `json:"id"`
	Task      string   `json:"task"`
	Branch    string   `json:"branch,omitempty"`
	QueryType string   `json:"query_type,omitempty"`
	K         int      `json:"k,omitempty"`
	Relevant  []string `json:"relevant,omitempty"`
}

// evalTaskResult is the per-task measurement. The headline metric is
// UsefulPer1k — relevant facts surfaced per 1,000 tokens spent — which captures
// the Appendix D agent constraint (value per token, not fact count).
type evalTaskResult struct {
	ID               string  `json:"id"`
	Task             string  `json:"task"`
	Surfaced         int     `json:"surfaced"`
	Tokens           int     `json:"tokens"`
	RelevantSurfaced int     `json:"relevant_surfaced"`
	Precision        float64 `json:"precision"`
	Recall           float64 `json:"recall,omitempty"`
	UsefulPer1k      float64 `json:"useful_per_1k"`
	Labeled          bool    `json:"labeled"`
}

type evalSummary struct {
	Tasks           int              `json:"tasks"`
	MeanTokens      float64          `json:"mean_tokens"`
	MeanPrecision   float64          `json:"mean_precision"`
	MeanUsefulPer1k float64          `json:"mean_useful_per_1k"`
	Results         []evalTaskResult `json:"results"`
}

// estimateTokens is a deterministic ~4-chars-per-token estimate over the text
// an agent would actually load (fact text plus its paths). It is a relative
// measure for comparing retrieval configurations, not an exact tokenizer.
func estimateTokens(facts []factRecord) int {
	chars := 0
	for _, f := range facts {
		chars += len(f.Text) + len(strings.Join(f.Paths, ",")) + 4 // small per-fact framing
	}
	return (chars + 3) / 4
}

// evalMetrics computes the per-task metrics given the surfaced facts and the set
// of relevant fact ids. totalRelevant is the size of the ground-truth relevant
// set (for recall); pass 0 when relevance came from a judge (recall undefined).
func evalMetrics(surfaced []factRecord, relevant map[string]struct{}, totalRelevant int) evalTaskResult {
	res := evalTaskResult{Surfaced: len(surfaced), Tokens: estimateTokens(surfaced)}
	for _, f := range surfaced {
		if _, ok := relevant[f.ID]; ok {
			res.RelevantSurfaced++
		}
	}
	if res.Surfaced > 0 {
		res.Precision = float64(res.RelevantSurfaced) / float64(res.Surfaced)
	}
	if totalRelevant > 0 {
		res.Recall = float64(res.RelevantSurfaced) / float64(totalRelevant)
	}
	if res.Tokens > 0 {
		res.UsefulPer1k = float64(res.RelevantSurfaced) / (float64(res.Tokens) / 1000.0)
	}
	return res
}

func summarizeEval(results []evalTaskResult) evalSummary {
	s := evalSummary{Tasks: len(results), Results: results}
	if len(results) == 0 {
		return s
	}
	var tok, prec, useful float64
	for _, r := range results {
		tok += float64(r.Tokens)
		prec += r.Precision
		useful += r.UsefulPer1k
	}
	n := float64(len(results))
	s.MeanTokens = tok / n
	s.MeanPrecision = prec / n
	s.MeanUsefulPer1k = useful / n
	return s
}

// --- agent judge ----------------------------------------------------------

// judgePrompt instructs the agent to mark which surfaced facts are relevant to
// a task. Like the other agent prompts it must not begin with a dash.
func judgePrompt() string {
	return `You assess whether retrieved repository facts are relevant and useful for a TASK.
On stdin you receive a TASK line followed by a numbered list of FACTS. For each
fact, decide if it would genuinely help an engineer or agent carry out the task.

Output exactly one line per fact, in fact-number order:

    <fact#> <yes|no>

Output only these lines. No prose.`
}

func judgeInput(task string, facts []factRecord) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "TASK: %s\n\nFACTS\n", task)
	for i, f := range facts {
		fmt.Fprintf(&b, "%d [%s] %s\n", i+1, strings.Join(f.Paths, ","), f.Text)
	}
	return []byte(b.String())
}

// parseJudgeOutput returns the set of surfaced-fact ids the agent marked
// relevant. An unparseable or missing line defaults to not-relevant, so a judge
// hiccup never inflates the usefulness metric.
func parseJudgeOutput(output string, facts []factRecord) map[string]struct{} {
	relevant := map[string]struct{}{}
	for _, raw := range strings.Split(output, "\n") {
		fields := strings.Fields(strings.TrimSpace(raw))
		if len(fields) < 2 {
			continue
		}
		n, err := strconv.Atoi(fields[0])
		if err != nil || n < 1 || n > len(facts) {
			continue
		}
		if strings.EqualFold(fields[1], "yes") {
			relevant[facts[n-1].ID] = struct{}{}
		}
	}
	return relevant
}

// --- command --------------------------------------------------------------

func newFactsEvalCommand(opts Options) *cobra.Command {
	var (
		tasksFile string
		branch    string
		k         int
		judge     bool
		agent     string
		jsonOut   bool
		run       distillAgentRunner
	)
	cmd := &cobra.Command{
		Use:   "eval --tasks <file>",
		Short: "Measure retrieval quality (tokens, precision, useful-facts-per-1k) over held-out tasks",
		Long: `Eval runs a held-out task set through fact retrieval and reports, per task and
in aggregate: facts surfaced, estimated tokens, precision, recall, and
useful-facts-per-1k-tokens (the headline agent metric).

The tasks file is a JSON array:
  [{"id":"t1","task":"how does X work","branch":"main","k":10,
    "relevant":["fact:abc","fact:def"]}]
With "relevant" ids, metrics are deterministic. Without them, pass --judge to
have the agent decide relevance per surfaced fact.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(tasksFile) == "" {
				return fmt.Errorf("--tasks <file> is required")
			}
			tasks, err := loadEvalTasks(tasksFile)
			if err != nil {
				return err
			}
			repoDir, brainDir, defaultBranch, err := resolveFactsTarget(cmd.Context(), opts, agentSurfaceTarget(opts, nil), branch)
			if err != nil {
				return err
			}
			var judgeArgs []string
			if judge {
				resolved := agent
				if resolved == "auto" {
					resolved = defaultRefreshAgent(cmd.Context(), opts.Runner, repoDir)
				}
				judgeArgs, err = distillAgentCommandArgs(resolved, nil, judgePrompt())
				if err != nil {
					return fmt.Errorf("judge agent: %w", err)
				}
				if run == nil {
					run = execDistillAgent
				}
			}
			results, err := runFactsEval(cmd.Context(), opts, brainDir, repoDir, defaultBranch, tasks, k, judge, run, judgeArgs)
			if err != nil {
				return err
			}
			summary := summarizeEval(results)
			if jsonOut {
				return writeJSON(cmd, summary)
			}
			printEvalSummary(cmd, summary)
			return nil
		},
	}
	cmd.Flags().StringVar(&tasksFile, "tasks", "", "Path to the held-out task set (JSON array)")
	cmd.Flags().StringVar(&branch, "branch", "", "Default branch for tasks that omit one (default: current branch)")
	cmd.Flags().IntVar(&k, "k", 10, "Facts to retrieve per task")
	cmd.Flags().BoolVar(&judge, "judge", false, "Use the agent to judge relevance when a task has no labels")
	cmd.Flags().StringVar(&agent, "agent", "auto", "Judge agent: auto, codex, claude-code, or command")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit the summary as JSON")
	return cmd
}

func loadEvalTasks(path string) ([]evalTask, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tasks []evalTask
	if err := json.Unmarshal(data, &tasks); err != nil {
		return nil, fmt.Errorf("parse tasks file: %w", err)
	}
	if len(tasks) == 0 {
		return nil, fmt.Errorf("tasks file has no tasks")
	}
	return tasks, nil
}

func runFactsEval(ctx context.Context, opts Options, brainDir, repoDir, defaultBranch string, tasks []evalTask, defaultK int, judge bool, run distillAgentRunner, judgeArgs []string) ([]evalTaskResult, error) {
	results := make([]evalTaskResult, 0, len(tasks))
	for _, task := range tasks {
		branch := task.Branch
		if branch == "" {
			branch = defaultBranch
		}
		k := task.K
		if k <= 0 {
			k = defaultK
		}
		facts, err := loadFacts(brainDir, branch)
		if err != nil {
			return nil, err
		}
		surfaced := rankFacts(facts, task.Task, k, false)

		var relevant map[string]struct{}
		totalRelevant := 0
		labeled := len(task.Relevant) > 0
		if labeled {
			relevant = make(map[string]struct{}, len(task.Relevant))
			for _, id := range task.Relevant {
				relevant[id] = struct{}{}
			}
			totalRelevant = len(relevant)
		} else if judge && len(surfaced) > 0 {
			out, judgeErr := run(ctx, repoDir, judgeArgs, judgeInput(task.Task, surfaced), defaultDistillTimeout)
			if judgeErr != nil {
				return nil, fmt.Errorf("judge task %s: %w", task.ID, judgeErr)
			}
			relevant = parseJudgeOutput(out, surfaced)
		} else {
			relevant = map[string]struct{}{}
		}

		res := evalMetrics(surfaced, relevant, totalRelevant)
		res.ID, res.Task, res.Labeled = task.ID, task.Task, labeled
		results = append(results, res)
	}
	return results, nil
}

func printEvalSummary(cmd *cobra.Command, s evalSummary) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "%-14s %8s %7s %7s %9s\n", "task", "tokens", "prec", "recall", "useful/1k")
	rows := append([]evalTaskResult(nil), s.Results...)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	for _, r := range rows {
		recall := "-"
		if r.Labeled {
			recall = fmt.Sprintf("%.2f", r.Recall)
		}
		fmt.Fprintf(out, "%-14s %8d %7.2f %7s %9.2f\n", truncateString(r.ID, 14), r.Tokens, r.Precision, recall, r.UsefulPer1k)
	}
	fmt.Fprintf(out, "%-14s %8.0f %7.2f %7s %9.2f\n", "MEAN", s.MeanTokens, s.MeanPrecision, "", s.MeanUsefulPer1k)
}
