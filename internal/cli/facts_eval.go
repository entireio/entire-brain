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
	QueryType        string  `json:"query_type,omitempty"`
	Surfaced         int     `json:"surfaced"`
	Tokens           int     `json:"tokens"`
	RelevantSurfaced int     `json:"relevant_surfaced"`
	Precision        float64 `json:"precision"`
	Recall           float64 `json:"recall,omitempty"`
	UsefulPer1k      float64 `json:"useful_per_1k"`
	Labeled          bool    `json:"labeled"`
}

// evalStratum aggregates metrics for one query-type stratum.
type evalStratum struct {
	Tasks           int     `json:"tasks"`
	MeanTokens      float64 `json:"mean_tokens"`
	MeanPrecision   float64 `json:"mean_precision"`
	MeanUsefulPer1k float64 `json:"mean_useful_per_1k"`
}

type evalSummary struct {
	Arm             string                 `json:"arm,omitempty"`
	Tasks           int                    `json:"tasks"`
	MeanTokens      float64                `json:"mean_tokens"`
	MeanPrecision   float64                `json:"mean_precision"`
	MeanUsefulPer1k float64                `json:"mean_useful_per_1k"`
	ByStratum       map[string]evalStratum `json:"by_stratum,omitempty"`
	SurfacedByKind  map[string]int         `json:"surfaced_by_kind,omitempty"`
	Results         []evalTaskResult       `json:"results"`
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
	strata := map[string][]evalTaskResult{}
	for _, r := range results {
		tok += float64(r.Tokens)
		prec += r.Precision
		useful += r.UsefulPer1k
		if r.QueryType != "" {
			strata[r.QueryType] = append(strata[r.QueryType], r)
		}
	}
	n := float64(len(results))
	s.MeanTokens = tok / n
	s.MeanPrecision = prec / n
	s.MeanUsefulPer1k = useful / n
	if len(strata) > 0 {
		s.ByStratum = make(map[string]evalStratum, len(strata))
		for qt, rs := range strata {
			var t, p, u float64
			for _, r := range rs {
				t += float64(r.Tokens)
				p += r.Precision
				u += r.UsefulPer1k
			}
			m := float64(len(rs))
			s.ByStratum[qt] = evalStratum{Tasks: len(rs), MeanTokens: t / m, MeanPrecision: p / m, MeanUsefulPer1k: u / m}
		}
	}
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
		tasksFile    string
		branch       string
		k            int
		judge        bool
		semantic     bool
		arm          string
		expand       bool
		agent        string
		agentCommand []string
		judgeCache   string
		expandCache  string
		jsonOut      bool
		run          distillAgentRunner
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
			resolvedAgent := agent
			if (judge || expand) && resolvedAgent == "auto" {
				resolvedAgent = defaultRefreshAgent(cmd.Context(), opts.Runner, repoDir)
			}
			var judgeArgs []string
			if judge {
				judgeArgs, err = distillAgentCommandArgs(resolvedAgent, agentCommand, judgePrompt())
				if err != nil {
					return fmt.Errorf("judge agent: %w", err)
				}
				if run == nil {
					run = execDistillAgent
				}
			}
			var expander queryExpanderFunc
			expCache := loadExpansionCache(expandCache)
			if expand {
				expandArgs, expErr := distillAgentCommandArgs(resolvedAgent, agentCommand, queryExpansionPrompt())
				if expErr != nil {
					return fmt.Errorf("expand agent: %w", expErr)
				}
				expRun := run
				if expRun == nil {
					expRun = execDistillAgent
				}
				expander = func(query string) (string, error) {
					return expandQuery(cmd.Context(), expRun, expandArgs, repoDir, query, expCache)
				}
			}
			var rr *semanticReranker
			if semantic {
				rr = newSemanticReranker(defaultEmbedder())
				if rr == nil {
					return fmt.Errorf("--semantic requested but the embedding backend is unavailable")
				}
			}
			armFn, err := selectRetrievalArm(arm)
			if err != nil {
				return err
			}
			results, surfacedByKind, err := runFactsEval(cmd.Context(), opts, brainDir, repoDir, defaultBranch, tasks, k, judge, run, judgeArgs, loadJudgeCache(judgeCache), expander, rr, armFn)
			if err != nil {
				return err
			}
			if err := expCache.save(); err != nil {
				return fmt.Errorf("save expansion cache: %w", err)
			}
			summary := summarizeEval(results)
			if arm != "" {
				summary.Arm = arm
			}
			summary.SurfacedByKind = surfacedByKind
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
	cmd.Flags().StringVar(&agent, "agent", "auto", "Judge/expand agent: auto, codex, claude-code, or command")
	cmd.Flags().StringArrayVar(&agentCommand, "agent-command", nil, "Agent command argv for --agent command")
	cmd.Flags().StringVar(&judgeCache, "judge-cache", "", "Persist/reuse judge verdicts at this path so re-runs are deterministic and cheap")
	cmd.Flags().BoolVar(&semantic, "semantic", false, "Rerank with the local embedding backend (RRF fusion of lexical + semantic)")
	cmd.Flags().StringVar(&arm, "arm", "flat", "Retrieval structure to evaluate: flat | scoped (locus) | outline")
	cmd.Flags().BoolVar(&expand, "expand", false, "Expand each task query with agent-generated retrieval terms before recall")
	cmd.Flags().StringVar(&expandCache, "expand-cache", "", "Persist/reuse query expansions at this path")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit the summary as JSON")
	return cmd
}

// judgeCache persists agent relevance verdicts keyed by "<taskID>\x00<factID>"
// so judged evals are repeatable and only new (task, fact) pairs cost an agent
// call. It makes judge-based A/Bs deterministic across runs.
type judgeCache struct {
	path    string
	verdict map[string]bool
	dirty   bool
}

func judgeCacheKey(taskID, factID string) string { return taskID + "\x00" + factID }

func loadJudgeCache(path string) *judgeCache {
	c := &judgeCache{path: path, verdict: map[string]bool{}}
	if strings.TrimSpace(path) == "" {
		return c
	}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &c.verdict)
	}
	return c
}

func (c *judgeCache) get(taskID, factID string) (bool, bool) {
	if c == nil {
		return false, false
	}
	v, ok := c.verdict[judgeCacheKey(taskID, factID)]
	return v, ok
}

func (c *judgeCache) set(taskID, factID string, relevant bool) {
	if c == nil {
		return
	}
	c.verdict[judgeCacheKey(taskID, factID)] = relevant
	c.dirty = true
}

func (c *judgeCache) save() error {
	if c == nil || !c.dirty || strings.TrimSpace(c.path) == "" {
		return nil
	}
	data, err := json.MarshalIndent(c.verdict, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.path, append(data, '\n'), 0o600)
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

// queryExpanderFunc maps a task query to extra retrieval terms; nil disables
// expansion.
type queryExpanderFunc func(query string) (string, error)

func runFactsEval(ctx context.Context, opts Options, brainDir, repoDir, defaultBranch string, tasks []evalTask, defaultK int, judge bool, run distillAgentRunner, judgeArgs []string, cache *judgeCache, expander queryExpanderFunc, rr *semanticReranker, arm retrievalArm) ([]evalTaskResult, map[string]int, error) {
	if arm == nil {
		arm = flatArm
	}
	results := make([]evalTaskResult, 0, len(tasks))
	surfacedByKind := map[string]int{}
	factsByBranch := map[string][]factRecord{} // load each branch's facts once per run
	for _, task := range tasks {
		branch := task.Branch
		if branch == "" {
			branch = defaultBranch
		}
		k := task.K
		if k <= 0 {
			k = defaultK
		}
		facts, ok := factsByBranch[branch]
		if !ok {
			loaded, err := loadFacts(brainDir, branch)
			if err != nil {
				return nil, nil, err
			}
			facts = loaded
			factsByBranch[branch] = facts
		}
		query := task.Task
		if expander != nil {
			if exp, expErr := expander(task.Task); expErr != nil {
				return nil, nil, fmt.Errorf("expand task %s: %w", task.ID, expErr)
			} else {
				query = expandedQuery(task.Task, exp)
			}
		}
		surfaced := arm(facts, query, k, rr)
		for _, f := range surfaced {
			surfacedByKind[factKindOrInferred(f)]++
		}

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
			judged, jerr := judgeRelevance(ctx, run, repoDir, judgeArgs, task, surfaced, cache)
			if jerr != nil {
				return nil, nil, jerr
			}
			relevant = judged
		} else {
			relevant = map[string]struct{}{}
		}

		res := evalMetrics(surfaced, relevant, totalRelevant)
		res.ID, res.Task, res.QueryType, res.Labeled = task.ID, task.Task, task.QueryType, labeled
		results = append(results, res)
	}
	if err := cache.save(); err != nil {
		return nil, nil, fmt.Errorf("save judge cache: %w", err)
	}
	return results, surfacedByKind, nil
}

// judgeRelevance returns the set of surfaced-fact ids judged relevant for a
// task, consulting the cache first and only asking the agent about the
// uncached facts (then recording its verdicts). This makes judged evals cheap
// to re-run and deterministic across runs.
func judgeRelevance(ctx context.Context, run distillAgentRunner, repoDir string, judgeArgs []string, task evalTask, surfaced []factRecord, cache *judgeCache) (map[string]struct{}, error) {
	relevant := map[string]struct{}{}
	var uncached []factRecord
	for _, f := range surfaced {
		if v, ok := cache.get(task.ID, f.ID); ok {
			if v {
				relevant[f.ID] = struct{}{}
			}
			continue
		}
		uncached = append(uncached, f)
	}
	if len(uncached) == 0 {
		return relevant, nil
	}
	out, err := run(ctx, repoDir, judgeArgs, judgeInput(task.Task, uncached), defaultDistillTimeout)
	if err != nil {
		return nil, fmt.Errorf("judge task %s: %w", task.ID, err)
	}
	judged := parseJudgeOutput(out, uncached)
	for _, f := range uncached {
		_, isRel := judged[f.ID]
		cache.set(task.ID, f.ID, isRel)
		if isRel {
			relevant[f.ID] = struct{}{}
		}
	}
	return relevant, nil
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
	if len(s.ByStratum) > 0 {
		fmt.Fprintf(out, "\n%-14s %8s %7s %7s %9s\n", "stratum", "tokens", "prec", "tasks", "useful/1k")
		strata := make([]string, 0, len(s.ByStratum))
		for qt := range s.ByStratum {
			strata = append(strata, qt)
		}
		sort.Strings(strata)
		for _, qt := range strata {
			st := s.ByStratum[qt]
			fmt.Fprintf(out, "%-14s %8.0f %7.2f %7d %9.2f\n", qt, st.MeanTokens, st.MeanPrecision, st.Tasks, st.MeanUsefulPer1k)
		}
	}
}
