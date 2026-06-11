package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// evalTask is one held-out retrieval task. Task is the query an agent would
// issue; Relevant, when present, is the labeled set of retrieved-item ids that
// genuinely help the task (enables deterministic precision/recall for that
// retriever). LabelSource records whether those labels are human/refined proof
// labels or provenance/silver hints. Without proof labels, --judge has an agent
// decide relevance per surfaced item. SourceLines records retained provenance
// anchor lines for source-match proxy checks; distilled facts currently inherit
// the source chunk's first line unless the source provided a finer line anchor.
type evalTask struct {
	ID                   string   `json:"id"`
	Task                 string   `json:"task"`
	Branch               string   `json:"branch,omitempty"`
	QueryType            string   `json:"query_type,omitempty"`
	K                    int      `json:"k,omitempty"`
	Relevant             []string `json:"relevant,omitempty"`
	LabelSource          string   `json:"label_source,omitempty"`
	SourceSessionID      string   `json:"source_session_id,omitempty"`
	SourceTranscriptPath string   `json:"source_transcript_path,omitempty"`
	SourceLines          []int    `json:"source_lines,omitempty"`
}

// evalTaskResult is the per-task measurement. The headline metric is
// UsefulPer1k — relevant items surfaced per 1,000 tokens spent — which captures
// the Appendix D agent constraint (value per token, not item count).
type evalTaskResult struct {
	ID                 string   `json:"id"`
	Task               string   `json:"task"`
	QueryType          string   `json:"query_type,omitempty"`
	Retriever          string   `json:"retriever,omitempty"`
	RetrievedIDs       []string `json:"retrieved_ids,omitempty"`
	RelevanceSource    string   `json:"relevance_source,omitempty"`
	Surfaced           int      `json:"surfaced"`
	Tokens             int      `json:"tokens"`
	LatencyMS          int64    `json:"latency_ms"`
	ExpansionLatencyMS int64    `json:"expansion_latency_ms"`
	EndToEndLatencyMS  int64    `json:"end_to_end_latency_ms"`
	RelevantSurfaced   int      `json:"relevant_surfaced"`
	Precision          float64  `json:"precision"`
	Recall             float64  `json:"recall"`
	UsefulPer1k        float64  `json:"useful_per_1k"`
	Labeled            bool     `json:"labeled"`
	LabelSource        string   `json:"label_source,omitempty"`
}

// evalStratum aggregates metrics for one query-type stratum.
type evalStratum struct {
	Tasks           int     `json:"tasks"`
	MeanTokens      float64 `json:"mean_tokens"`
	MeanPrecision   float64 `json:"mean_precision"`
	MeanUsefulPer1k float64 `json:"mean_useful_per_1k"`
}

type evalSummary struct {
	Retriever              string                 `json:"retriever,omitempty"`
	Arm                    string                 `json:"arm,omitempty"`
	RunConfig              *evalRunConfig         `json:"run_config,omitempty"`
	Tasks                  int                    `json:"tasks"`
	MeanTokens             float64                `json:"mean_tokens"`
	MeanLatencyMS          float64                `json:"mean_latency_ms"`
	MeanExpansionLatencyMS float64                `json:"mean_expansion_latency_ms"`
	MeanEndToEndLatencyMS  float64                `json:"mean_end_to_end_latency_ms"`
	MeanPrecision          float64                `json:"mean_precision"`
	MeanUsefulPer1k        float64                `json:"mean_useful_per_1k"`
	ByStratum              map[string]evalStratum `json:"by_stratum,omitempty"`
	SurfacedByKind         map[string]int         `json:"surfaced_by_kind,omitempty"`
	Results                []evalTaskResult       `json:"results"`
}

type evalRunConfig struct {
	TasksPath             string `json:"tasks_path,omitempty"`
	TasksSHA256           string `json:"tasks_sha256,omitempty"`
	BrainManifestSHA256   string `json:"brain_manifest_sha256,omitempty"`
	Branch                string `json:"branch,omitempty"`
	K                     int    `json:"k"`
	Retriever             string `json:"retriever"`
	Arm                   string `json:"arm,omitempty"`
	Judge                 bool   `json:"judge"`
	JudgeSourceMatches    bool   `json:"judge_source_matches,omitempty"`
	Expand                bool   `json:"expand"`
	Semantic              bool   `json:"semantic"`
	JudgeCachePath        string `json:"judge_cache_path,omitempty"`
	ExpansionCachePath    string `json:"expansion_cache_path,omitempty"`
	LabelPolicy           string `json:"label_policy"`
	IncludeIDs            bool   `json:"include_ids,omitempty"`
	RawSessionIDScheme    string `json:"raw_session_id_scheme"`
	TurnSigningLimitation string `json:"turn_signing_limitation,omitempty"`
}

const (
	evalRetrieverFacts       = "facts"
	evalRetrieverHistory     = "history"
	evalRetrieverQuery       = "query"
	evalRetrieverRawSessions = "raw-sessions"

	evalRelevanceExplicitLabel    = "explicit_label"
	evalRelevanceSourceMatch      = "source_match"
	evalRelevanceJudge            = "judge"
	evalRelevanceNone             = "none"
	evalRelevancePartialLabel     = "partial_explicit_label"
	evalRelevanceMixedLabelSource = "mixed_explicit_source_match"
	evalRelevanceSilverLabel      = "provenance_silver_label"
	evalRelevancePartialSilver    = "partial_provenance_silver_label"
	evalRelevanceMixedSilver      = "mixed_provenance_silver_source_match"

	evalLabelSourceHuman            = "human"
	evalLabelSourceProvenanceSilver = "provenance_silver"
	evalLabelSourceJudgeRefined     = "judge_refined"
)

type evalRetrievedItem struct {
	ID   string
	Text string
	Path string
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

func estimateRetrievedTokens(items []evalRetrievedItem) int {
	chars := 0
	for _, item := range items {
		chars += len(item.Text) + len(item.Path) + 4
	}
	return (chars + 3) / 4
}

// evalMetrics computes the per-task metrics given the surfaced facts and the set
// of relevant fact ids. totalRelevant is the size of the labeled relevant set
// for recall; pass 0 when relevance came from a judge or source-match proxy
// labels (recall undefined).
func evalMetrics(surfaced []factRecord, relevant map[string]struct{}, totalRelevant int) evalTaskResult {
	items := make([]evalRetrievedItem, len(surfaced))
	for i, f := range surfaced {
		items[i] = evalRetrievedItem{ID: f.ID, Text: f.Text, Path: strings.Join(f.Paths, ",")}
	}
	return evalItemMetrics(items, relevant, totalRelevant)
}

func evalItemMetrics(surfaced []evalRetrievedItem, relevant map[string]struct{}, totalRelevant int) evalTaskResult {
	res := evalTaskResult{Surfaced: len(surfaced), Tokens: estimateRetrievedTokens(surfaced)}
	for _, item := range surfaced {
		if _, ok := relevant[item.ID]; ok {
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
	return summarizeEvalWithConfig(results, nil)
}

func summarizeEvalWithConfig(results []evalTaskResult, config *evalRunConfig) evalSummary {
	s := evalSummary{Tasks: len(results), Results: results}
	if config != nil {
		copy := *config
		s.RunConfig = &copy
	}
	if len(results) == 0 {
		return s
	}
	s.Retriever = results[0].Retriever
	var tok, latency, expansionLatency, endToEndLatency, prec, useful float64
	strata := map[string][]evalTaskResult{}
	for _, r := range results {
		tok += float64(r.Tokens)
		latency += float64(r.LatencyMS)
		expansionLatency += float64(r.ExpansionLatencyMS)
		endToEndLatency += float64(evalEndToEndLatencyMS(r))
		prec += r.Precision
		useful += r.UsefulPer1k
		if r.QueryType != "" {
			strata[r.QueryType] = append(strata[r.QueryType], r)
		}
	}
	n := float64(len(results))
	s.MeanTokens = tok / n
	s.MeanLatencyMS = latency / n
	s.MeanExpansionLatencyMS = expansionLatency / n
	s.MeanEndToEndLatencyMS = endToEndLatency / n
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

func evalEndToEndLatencyMS(r evalTaskResult) int64 {
	if r.EndToEndLatencyMS > 0 {
		return r.EndToEndLatencyMS
	}
	return r.LatencyMS + r.ExpansionLatencyMS
}

func fileSHA256Hex(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func evalBrainManifestSHA256(brainDir string) string {
	return fileSHA256Hex(filepath.Join(brainDir, exportManifestFileName))
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

func judgeInputItems(task string, items []evalRetrievedItem) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "TASK: %s\n\nFACTS\n", task)
	for i, item := range items {
		fmt.Fprintf(&b, "%d [%s] %s\n", i+1, item.Path, item.Text)
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

func parseJudgeOutputItems(output string, items []evalRetrievedItem) map[string]struct{} {
	relevant := map[string]struct{}{}
	for _, raw := range strings.Split(output, "\n") {
		fields := strings.Fields(strings.TrimSpace(raw))
		if len(fields) < 2 {
			continue
		}
		n, err := strconv.Atoi(fields[0])
		if err != nil || n < 1 || n > len(items) {
			continue
		}
		if strings.EqualFold(fields[1], "yes") {
			relevant[items[n-1].ID] = struct{}{}
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
		judgeSource  bool
		semantic     bool
		arm          string
		expand       bool
		agent        string
		model        string
		agentCommand []string
		judgeCache   string
		expandCache  string
		retriever    string
		includeIDs   bool
		jsonOut      bool
		run          distillAgentRunner
	)
	cmd := &cobra.Command{
		Use:   "eval --tasks <file>",
		Short: "Measure retrieval quality (tokens, precision, useful-items-per-1k) over held-out tasks",
		Long: `Eval runs a held-out task set through a selected retriever and reports, per task and
	in aggregate: retrieved items, estimated tokens, precision, recall when labels
	exist for that retriever, and useful-items-per-1k-tokens (the headline agent
	metric).

The tasks file is a JSON array:
  [{"id":"t1","task":"how does X work","branch":"main","k":10,
    "relevant":["fact:abc","fact:def"],"label_source":"human"}]
			With retriever-specific "relevant" ids, metrics are deterministic. Generated
			source-session labels give source-match credit for history/preprocessed-session
			arms but do not define recall. Without labels, pass --judge to have the agent
			decide relevance per surfaced item. With source-session tasks, pass
			--judge --judge-source-matches to replace source-overlap proxy credit with
			judge-labeled relevance for history/preprocessed-session arms.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(tasksFile) == "" {
				return fmt.Errorf("--tasks <file> is required")
			}
			tasks, err := loadEvalTasks(tasksFile)
			if err != nil {
				return err
			}
			if judgeSource && !judge {
				return fmt.Errorf("--judge-source-matches requires --judge")
			}
			if retriever == "" {
				retriever = evalRetrieverFacts
			}
			if err := validateEvalRetriever(retriever); err != nil {
				return err
			}
			if err := validateEvalSemanticRetriever(semantic, retriever); err != nil {
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
				judgeArgs = injectAgentModel(judgeArgs, resolvedAgent, model)
				if run == nil {
					run = defaultDistillAgentRunner(resolvedAgent)
				}
			}
			var expander queryExpanderFunc
			expCache := loadExpansionCache(expandCache)
			if expand {
				expandArgs, expErr := distillAgentCommandArgs(resolvedAgent, agentCommand, queryExpansionPrompt())
				if expErr != nil {
					return fmt.Errorf("expand agent: %w", expErr)
				}
				expandArgs = injectAgentModel(expandArgs, resolvedAgent, model)
				expRun := run
				if expRun == nil {
					expRun = defaultDistillAgentRunner(resolvedAgent)
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
			cache := loadJudgeCache(judgeCache)
			if err := cache.validateLoaded(); err != nil {
				return err
			}
			runOpts := factsEvalRunOptions{
				JudgeSourceMatches: judgeSource,
				Arm:                armFn,
				SurfacedByKind:     map[string]int{},
				IncludeIDs:         includeIDs,
			}
			results, err := runFactsEvalWithOptions(cmd.Context(), opts, brainDir, repoDir, defaultBranch, tasks, k, judge, run, judgeArgs, cache, expander, rr, retriever, runOpts)
			if err != nil {
				return err
			}
			if err := expCache.save(); err != nil {
				return fmt.Errorf("save expansion cache: %w", err)
			}
			summary := summarizeEvalWithConfig(results, &evalRunConfig{
				TasksPath:             tasksFile,
				TasksSHA256:           fileSHA256Hex(tasksFile),
				BrainManifestSHA256:   evalBrainManifestSHA256(brainDir),
				Branch:                defaultBranch,
				K:                     k,
				Retriever:             retriever,
				Arm:                   arm,
				Judge:                 judge,
				JudgeSourceMatches:    judgeSource,
				Expand:                expand,
				Semantic:              semantic,
				JudgeCachePath:        judgeCache,
				ExpansionCachePath:    expandCache,
				LabelPolicy:           "human and judge_refined labels define precision/recall; provenance_silver is reported but not proof-labeled",
				IncludeIDs:            includeIDs,
				RawSessionIDScheme:    "raw:sha256(session_id)[:16]:start-end",
				TurnSigningLimitation: "turn-level cryptographic verification remains pending CLI-side turn signing",
			})
			summary.Arm = arm
			if len(runOpts.SurfacedByKind) > 0 {
				summary.SurfacedByKind = runOpts.SurfacedByKind
			}
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
	cmd.Flags().BoolVar(&judgeSource, "judge-source-matches", false, "With --judge, judge source-match proxy rows instead of counting transcript/session overlap as relevance")
	cmd.Flags().StringVar(&agent, "agent", "auto", "Judge/expand agent: auto, codex, claude-code, ollama, or command")
	cmd.Flags().StringVar(&model, "model", "", "Model for codex/claude-code/ollama judge and expand calls")
	cmd.Flags().StringArrayVar(&agentCommand, "agent-command", nil, "Agent command argv for --agent command")
	cmd.Flags().StringVar(&judgeCache, "judge-cache", "", "Persist/reuse judge verdicts at this path so re-runs are deterministic and cheap")
	cmd.Flags().BoolVar(&semantic, "semantic", false, "Rerank with the local embedding backend (RRF fusion of lexical + semantic)")
	cmd.Flags().StringVar(&arm, "arm", "flat", "Facts retrieval structure to evaluate: flat | scoped | scoped-floor | outline")
	cmd.Flags().BoolVar(&expand, "expand", false, "Expand each task query with agent-generated retrieval terms before retrieval")
	cmd.Flags().StringVar(&expandCache, "expand-cache", "", "Persist/reuse query expansions at this path")
	cmd.Flags().StringVar(&retriever, "retriever", evalRetrieverFacts, "Retrieval arm: facts, history, query, or raw-sessions")
	cmd.Flags().BoolVar(&includeIDs, "include-ids", false, "Include per-task retrieved item ids in JSON output for retained proof artifacts")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit the summary as JSON")
	return cmd
}

// judgeCache persists agent relevance verdicts keyed by a digest of the task,
// retriever, and surfaced item content. That keeps judged evals repeatable while
// avoiding stale verdict reuse when task wording or raw-session text changes.
type judgeCache struct {
	path    string
	verdict map[string]bool
	dirty   bool
	loadErr error
}

func judgeCacheKey(task evalTask, retriever string, item evalRetrievedItem) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		"v2",
		task.ID,
		task.Task,
		task.Branch,
		task.QueryType,
		retriever,
		item.ID,
		item.Path,
		item.Text,
	}, "\x00")))
	return task.ID + "\x00" + retriever + "\x00" + item.ID + "\x00sha256:" + hex.EncodeToString(sum[:])
}

func loadJudgeCache(path string) *judgeCache {
	c := &judgeCache{path: path, verdict: map[string]bool{}}
	if strings.TrimSpace(path) == "" {
		return c
	}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &c.verdict); err != nil {
			c.loadErr = fmt.Errorf("parse judge cache %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		c.loadErr = fmt.Errorf("read judge cache %s: %w", path, err)
	}
	return c
}

func (c *judgeCache) validateLoaded() error {
	if c == nil {
		return nil
	}
	return c.loadErr
}

func (c *judgeCache) get(task evalTask, retriever string, item evalRetrievedItem) (bool, bool) {
	if c == nil {
		return false, false
	}
	v, ok := c.verdict[judgeCacheKey(task, retriever, item)]
	return v, ok
}

func (c *judgeCache) set(task evalTask, retriever string, item evalRetrievedItem, relevant bool) {
	if c == nil {
		return
	}
	c.verdict[judgeCacheKey(task, retriever, item)] = relevant
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
	return writeFileAtomic(c.path, append(data, '\n'), 0o600)
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
	if err := validateEvalTasks(tasks); err != nil {
		return nil, err
	}
	return tasks, nil
}

func validateEvalTasks(tasks []evalTask) error {
	for i, task := range tasks {
		if err := validateEvalTask(task); err != nil {
			id := strings.TrimSpace(task.ID)
			if id == "" {
				id = fmt.Sprintf("#%d", i+1)
			}
			return fmt.Errorf("eval task %s: %w", id, err)
		}
	}
	return nil
}

func validateEvalTask(task evalTask) error {
	labelSource := strings.TrimSpace(task.LabelSource)
	switch labelSource {
	case "", evalLabelSourceHuman, evalLabelSourceJudgeRefined, evalLabelSourceProvenanceSilver:
	default:
		return fmt.Errorf("label_source must be one of %q, %q, or %q", evalLabelSourceHuman, evalLabelSourceJudgeRefined, evalLabelSourceProvenanceSilver)
	}
	if len(task.Relevant) > 0 && labelSource == "" {
		return fmt.Errorf("relevant labels require explicit label_source (%q, %q, or %q)", evalLabelSourceHuman, evalLabelSourceJudgeRefined, evalLabelSourceProvenanceSilver)
	}
	return nil
}

func validateEvalRetriever(retriever string) error {
	switch retriever {
	case evalRetrieverFacts, evalRetrieverHistory, evalRetrieverQuery, evalRetrieverRawSessions:
		return nil
	default:
		return fmt.Errorf("--retriever must be one of facts, history, query, raw-sessions")
	}
}

func validateEvalSemanticRetriever(semantic bool, retriever string) error {
	if semantic && retriever != evalRetrieverFacts {
		return fmt.Errorf("--semantic requires --retriever facts")
	}
	return nil
}

func retrieveEvalItems(brainDir, branch, query string, limit int, retriever string, facts []factRecord, rr *semanticReranker) ([]evalRetrievedItem, error) {
	switch retriever {
	case evalRetrieverFacts:
		return factsToEvalItems(rankFactsFused(facts, query, limit, false, rr)), nil
	case evalRetrieverHistory:
		manifest, err := loadBrainManifest(brainDir)
		if err != nil {
			return nil, err
		}
		if manifest.Sources == nil || manifest.Sources.History == nil {
			return nil, nil
		}
		index, err := loadBrainHistoryIndex(brainDir, manifest.Sources.History)
		if err != nil {
			return nil, err
		}
		index = filterHistoryIndexForEvalBranch(index, branch, manifest)
		return historyScoredToEvalItems(rankHistoryRecordsScored(index, "history", query, limit, 0)), nil
	case evalRetrieverQuery:
		results, err := retrieveEvalUnifiedLexical(brainDir, branch, query, limit, facts)
		if err != nil {
			return nil, err
		}
		return unifiedToEvalItems(results), nil
	case evalRetrieverRawSessions:
		return rankRawSessionChunks(brainDir, branch, query, limit)
	default:
		return nil, fmt.Errorf("unsupported retriever %q", retriever)
	}
}

func retrieveEvalUnifiedLexical(brainDir, branch, query string, limit int, facts []factRecord) ([]unifiedResult, error) {
	if limit <= 0 {
		limit = 10
	}
	var lists [][]unifiedResult
	active := make([]factRecord, 0, len(facts))
	for _, f := range facts {
		if f.Status == factStatusActive {
			active = append(active, f)
		}
	}
	if len(active) > 0 {
		lists = append(lists, factsToUnified(rankFacts(active, query, limit*2, false)))
	}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		return nil, err
	}
	if manifest.Sources != nil && manifest.Sources.History != nil {
		index, err := loadBrainHistoryIndex(brainDir, manifest.Sources.History)
		if err != nil {
			return nil, fmt.Errorf("load history index: %w", err)
		}
		index = filterHistoryIndexForEvalBranch(index, branch, manifest)
		if scored := rankHistoryRecordsScored(index, "history", query, limit*2, 0); len(scored) > 0 {
			lists = append(lists, historyToUnified(scored))
		}
	}
	docIdx, derr := loadDocIndex(brainDir)
	switch {
	case derr == nil && len(docIdx.Records) > 0:
		if scored := rankDocsLexical(docIdx, query, limit*2); len(scored) > 0 {
			lists = append(lists, docsToUnified(scored))
		}
	case derr != nil && !os.IsNotExist(derr):
		return nil, fmt.Errorf("load doc index: %w", derr)
	}
	return rrfMergeUnified(lists, limit), nil
}

func filterHistoryIndexForEvalBranch(index historyIndex, branch string, manifest *exportManifest) historyIndex {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return index
	}
	branchByPath := historyBranchByTranscriptPath(manifest)
	filtered := make([]historyRecord, 0, len(index.Records))
	for _, record := range index.Records {
		recordBranch := strings.TrimSpace(record.Branch)
		if recordBranch == "" {
			recordBranch = historyRecordBranchForPath(record.Path, branchByPath)
			record.Branch = recordBranch
		}
		if recordBranch == branch {
			filtered = append(filtered, record)
		}
	}
	index.Records = filtered
	return index
}

func factsToEvalItems(facts []factRecord) []evalRetrievedItem {
	items := make([]evalRetrievedItem, len(facts))
	for i, f := range facts {
		items[i] = evalRetrievedItem{ID: f.ID, Text: f.Text, Path: strings.Join(f.Paths, ",")}
	}
	return items
}

func historyScoredToEvalItems(scored []scoredHistoryRecord) []evalRetrievedItem {
	items := make([]evalRetrievedItem, len(scored))
	for i, s := range scored {
		items[i] = evalRetrievedItem{ID: s.Record.ID, Text: s.Record.Summary, Path: fmt.Sprintf("%s:%d", s.Record.Path, s.Record.Line)}
	}
	return items
}

func unifiedToEvalItems(results []unifiedResult) []evalRetrievedItem {
	items := make([]evalRetrievedItem, len(results))
	for i, r := range results {
		path := r.Path
		if r.Line > 0 {
			path = fmt.Sprintf("%s:%d", r.Path, r.Line)
		}
		items[i] = evalRetrievedItem{ID: r.ID, Text: r.Text, Path: path}
	}
	return items
}

type scoredEvalItem struct {
	Item  evalRetrievedItem
	Score int
	Order int
}

func rankRawSessionChunks(brainDir, branch, query string, limit int) ([]evalRetrievedItem, error) {
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		return nil, err
	}
	if manifest.Sources == nil || manifest.Sources.Sessions == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	var scored []scoredEvalItem
	order := 0
	for _, session := range manifest.Sources.Sessions.Sessions {
		sessionBranch := session.Branch
		if sessionBranch == "" {
			sessionBranch = manifest.DefaultBranch
		}
		if sessionBranch == "" {
			sessionBranch = distillDefaultBranch
		}
		if branch != "" && sessionBranch != branch {
			continue
		}
		content, err := readBrainRelativeFile(brainDir, session.TranscriptPath)
		if err != nil {
			return nil, fmt.Errorf("read raw session transcript %s: %w", session.TranscriptPath, err)
		}
		for _, chunk := range chunkLines(preprocessTranscriptForDistill(content), defaultDistillChunkSize, false) {
			record := historyRecord{
				ID:      rawSessionChunkID(session, chunk),
				Kind:    "raw-session",
				Path:    filepath.ToSlash(session.TranscriptPath),
				Line:    chunk.StartLine,
				Summary: chunk.Text,
			}
			score := historyRecordQueryScoreMin(record, query, 0)
			if score == 0 {
				continue
			}
			scored = append(scored, scoredEvalItem{
				Item:  evalRetrievedItem{ID: record.ID, Text: record.Summary, Path: fmt.Sprintf("%s:%d", record.Path, record.Line)},
				Score: score,
				Order: order,
			})
			order++
		}
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].Score != scored[j].Score {
			return scored[i].Score > scored[j].Score
		}
		return scored[i].Order < scored[j].Order
	})
	if len(scored) > limit {
		scored = scored[:limit]
	}
	items := make([]evalRetrievedItem, len(scored))
	for i, s := range scored {
		items[i] = s.Item
	}
	return items, nil
}

func rawSessionChunkID(session exportSession, chunk transcriptChunk) string {
	return fmt.Sprintf("raw:%s:%d-%d", rawSessionIDToken(session.SessionID), chunk.StartLine, chunk.EndLine)
}

func rawSessionIDToken(sessionID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(sessionID)))
	return hex.EncodeToString(sum[:8])
}

func evalLabelsForRetriever(task evalTask, retriever string, surfaced []evalRetrievedItem) (map[string]struct{}, int, bool, string) {
	explicit := relevantIDsForRetriever(task.Relevant, retriever)
	if len(explicit) > 0 {
		labelSource := normalizedEvalLabelSource(task)
		if labelSource == evalLabelSourceProvenanceSilver {
			if retriever == evalRetrieverQuery && !queryExplicitLabelsCoverSurfaced(task.Relevant, surfaced) {
				if taskHasEvalSourceAnchor(task) {
					mixed := copyStringSet(explicit)
					for _, item := range surfaced {
						if evalItemMatchesTaskSource(task, item) {
							mixed[item.ID] = struct{}{}
						}
					}
					return mixed, 0, false, evalRelevanceMixedSilver
				}
				return explicit, 0, false, evalRelevancePartialSilver
			}
			return explicit, 0, false, evalRelevanceSilverLabel
		}
		if retriever == evalRetrieverQuery && !queryExplicitLabelsCoverSurfaced(task.Relevant, surfaced) {
			if taskHasEvalSourceAnchor(task) {
				mixed := copyStringSet(explicit)
				for _, item := range surfaced {
					if evalItemMatchesTaskSource(task, item) {
						mixed[item.ID] = struct{}{}
					}
				}
				return mixed, 0, false, evalRelevanceMixedLabelSource
			}
			return explicit, 0, false, evalRelevancePartialLabel
		}
		return explicit, len(explicit), true, evalRelevanceExplicitLabel
	}
	if retriever == evalRetrieverFacts || retriever == evalRetrieverQuery {
		return map[string]struct{}{}, 0, false, evalRelevanceNone
	}
	sourceMatches := map[string]struct{}{}
	for _, item := range surfaced {
		if evalItemMatchesTaskSource(task, item) {
			sourceMatches[item.ID] = struct{}{}
		}
	}
	if taskHasEvalSourceAnchor(task) {
		return sourceMatches, 0, false, evalRelevanceSourceMatch
	}
	return sourceMatches, 0, false, evalRelevanceNone
}

func normalizedEvalLabelSource(task evalTask) string {
	return strings.TrimSpace(task.LabelSource)
}

func relevantIDsForRetriever(ids []string, retriever string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		switch retriever {
		case evalRetrieverFacts:
			if strings.HasPrefix(id, "fact:") || (!strings.Contains(id, ":") && !strings.HasPrefix(id, "raw:")) {
				out[id] = struct{}{}
			}
		case evalRetrieverHistory:
			if strings.HasPrefix(id, "history:") {
				out[id] = struct{}{}
			}
		case evalRetrieverRawSessions:
			if strings.HasPrefix(id, "raw:") {
				out[id] = struct{}{}
			}
		case evalRetrieverQuery:
			out[id] = struct{}{}
		}
	}
	return out
}

func copyStringSet(in map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for k := range in {
		out[k] = struct{}{}
	}
	return out
}

func queryExplicitLabelsCoverSurfaced(relevant []string, surfaced []evalRetrievedItem) bool {
	labelSources := map[string]struct{}{}
	for _, id := range relevant {
		source := evalIDSource(strings.TrimSpace(id))
		if source != "" {
			labelSources[source] = struct{}{}
		}
	}
	for _, item := range surfaced {
		source := evalIDSource(item.ID)
		if source == "" {
			continue
		}
		if _, ok := labelSources[source]; !ok {
			return false
		}
	}
	return true
}

func evalIDSource(id string) string {
	switch {
	case strings.HasPrefix(id, "fact:"):
		return "fact"
	case strings.HasPrefix(id, "history:"):
		return "history"
	case strings.HasPrefix(id, "doc:"):
		return "doc"
	case strings.HasPrefix(id, "raw:"):
		return "raw"
	case id != "" && !strings.Contains(id, ":"):
		return "fact"
	default:
		return ""
	}
}

func evalItemMatchesTaskSource(task evalTask, item evalRetrievedItem) bool {
	sessionID := strings.TrimSpace(task.SourceSessionID)
	transcript := filepath.ToSlash(strings.TrimSpace(task.SourceTranscriptPath))
	if transcript == "" {
		if sessionID == "" || !strings.HasPrefix(item.ID, "raw:"+rawSessionIDToken(sessionID)+":") {
			return false
		}
		return evalTaskSourceLinesOverlap(task, item)
	}
	path := filepath.ToSlash(strings.TrimSpace(item.Path))
	itemPath, _, _ := evalItemPathLineRange(path)
	if itemPath != transcript {
		return false
	}
	return evalTaskSourceLinesOverlap(task, item)
}

func taskHasEvalSourceAnchor(task evalTask) bool {
	return strings.TrimSpace(task.SourceSessionID) != "" || strings.TrimSpace(task.SourceTranscriptPath) != ""
}

func evalTaskSourceLinesOverlap(task evalTask, item evalRetrievedItem) bool {
	if len(task.SourceLines) == 0 {
		return true
	}
	start, end, ok := evalItemSourceLineRange(task, item)
	if !ok {
		return false
	}
	for _, line := range task.SourceLines {
		if line >= start && line <= end {
			return true
		}
	}
	return false
}

func evalItemSourceLineRange(task evalTask, item evalRetrievedItem) (int, int, bool) {
	if _, start, end, ok := rawSessionChunkRange(item.ID); ok {
		return start, end, true
	}
	if strings.TrimSpace(task.SourceTranscriptPath) != "" {
		path := filepath.ToSlash(strings.TrimSpace(item.Path))
		if _, line, ok := evalItemPathLineRange(path); ok {
			return line, line, true
		}
	}
	return 0, 0, false
}

func rawSessionChunkRange(id string) (string, int, int, bool) {
	parts := strings.Split(id, ":")
	if len(parts) != 3 || parts[0] != "raw" {
		return "", 0, 0, false
	}
	bounds := strings.Split(parts[2], "-")
	if len(bounds) != 2 {
		return "", 0, 0, false
	}
	start, err1 := strconv.Atoi(bounds[0])
	end, err2 := strconv.Atoi(bounds[1])
	if err1 != nil || err2 != nil || start <= 0 || end < start {
		return "", 0, 0, false
	}
	return parts[1], start, end, true
}

func evalItemPathLineRange(path string) (string, int, bool) {
	if idx := strings.LastIndex(path, ":"); idx > 0 {
		suffix := path[idx+1:]
		if line, err := strconv.Atoi(suffix); err == nil && line > 0 {
			return path[:idx], line, true
		}
	}
	return path, 0, false
}

// queryExpanderFunc maps a task query to extra retrieval terms; nil disables
// expansion.
type queryExpanderFunc func(query string) (string, error)

type factsEvalRunOptions struct {
	JudgeSourceMatches bool
	Arm                retrievalArm
	SurfacedByKind     map[string]int
	IncludeIDs         bool
}

func runFactsEval(ctx context.Context, opts Options, brainDir, repoDir, defaultBranch string, tasks []evalTask, defaultK int, judge bool, run distillAgentRunner, judgeArgs []string, cache *judgeCache, expander queryExpanderFunc, rr *semanticReranker, retriever string) ([]evalTaskResult, error) {
	return runFactsEvalWithOptions(ctx, opts, brainDir, repoDir, defaultBranch, tasks, defaultK, judge, run, judgeArgs, cache, expander, rr, retriever, factsEvalRunOptions{})
}

func runFactsEvalWithOptions(ctx context.Context, opts Options, brainDir, repoDir, defaultBranch string, tasks []evalTask, defaultK int, judge bool, run distillAgentRunner, judgeArgs []string, cache *judgeCache, expander queryExpanderFunc, rr *semanticReranker, retriever string, runOpts factsEvalRunOptions) ([]evalTaskResult, error) {
	if err := validateEvalTasks(tasks); err != nil {
		return nil, err
	}
	if err := validateEvalRetriever(retriever); err != nil {
		return nil, err
	}
	if err := validateEvalSemanticRetriever(rr != nil, retriever); err != nil {
		return nil, err
	}
	if runOpts.Arm == nil {
		runOpts.Arm = flatArm
	}
	results := make([]evalTaskResult, 0, len(tasks))
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
		var facts []factRecord
		if retriever == evalRetrieverFacts || retriever == evalRetrieverQuery {
			var ok bool
			facts, ok = factsByBranch[branch]
			if !ok {
				loaded, err := loadFacts(brainDir, branch)
				if err != nil {
					return nil, err
				}
				facts = loaded
				factsByBranch[branch] = facts
			}
		}
		taskStarted := time.Now()
		query := task.Task
		var expansionLatencyMS int64
		if expander != nil {
			expansionStarted := time.Now()
			if exp, expErr := expander(task.Task); expErr != nil {
				return nil, fmt.Errorf("expand task %s: %w", task.ID, expErr)
			} else {
				expansionLatencyMS = time.Since(expansionStarted).Milliseconds()
				query = expandedQuery(task.Task, exp)
			}
		}
		started := time.Now()
		var surfaced []evalRetrievedItem
		if retriever == evalRetrieverFacts {
			surfacedFacts := runOpts.Arm(facts, query, k, rr)
			if runOpts.SurfacedByKind != nil {
				for _, fact := range surfacedFacts {
					runOpts.SurfacedByKind[factKindOrInferred(fact)]++
				}
			}
			surfaced = factsToEvalItems(surfacedFacts)
		} else {
			var retrieveErr error
			surfaced, retrieveErr = retrieveEvalItems(brainDir, branch, query, k, retriever, facts, rr)
			if retrieveErr != nil {
				return nil, fmt.Errorf("retrieve task %s: %w", task.ID, retrieveErr)
			}
		}
		latencyMS := time.Since(started).Milliseconds()
		endToEndLatencyMS := time.Since(taskStarted).Milliseconds()

		relevant, totalRelevant, labeled, relevanceSource := evalLabelsForRetriever(task, retriever, surfaced)
		shouldJudge := judge && len(surfaced) > 0 && !labeled && relevanceSource != evalRelevanceSourceMatch
		if judge && len(surfaced) > 0 && !labeled && runOpts.JudgeSourceMatches && relevanceSource == evalRelevanceSourceMatch {
			shouldJudge = true
		}
		if shouldJudge {
			judged, jerr := judgeRelevanceItems(ctx, run, repoDir, judgeArgs, task, retriever, surfaced, cache)
			if jerr != nil {
				return nil, jerr
			}
			relevant = judged
			relevanceSource = evalRelevanceJudge
		}

		res := evalItemMetrics(surfaced, relevant, totalRelevant)
		res.ID, res.Task, res.QueryType, res.Labeled = task.ID, task.Task, task.QueryType, labeled
		res.Retriever = retriever
		if runOpts.IncludeIDs {
			res.RetrievedIDs = evalRetrievedIDs(surfaced)
		}
		res.RelevanceSource = relevanceSource
		res.LabelSource = normalizedEvalLabelSource(task)
		res.LatencyMS = latencyMS
		res.ExpansionLatencyMS = expansionLatencyMS
		res.EndToEndLatencyMS = endToEndLatencyMS
		results = append(results, res)
	}
	if err := cache.save(); err != nil {
		return nil, fmt.Errorf("save judge cache: %w", err)
	}
	return results, nil
}

func evalRetrievedIDs(items []evalRetrievedItem) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		if strings.TrimSpace(item.ID) == "" {
			continue
		}
		ids = append(ids, item.ID)
	}
	return ids
}

// judgeRelevance returns the set of surfaced-fact ids judged relevant for a
// task, consulting the cache first and only asking the agent about the
// uncached facts (then recording its verdicts). This makes judged evals cheap
// to re-run and deterministic across runs.
func judgeRelevance(ctx context.Context, run distillAgentRunner, repoDir string, judgeArgs []string, task evalTask, surfaced []factRecord, cache *judgeCache) (map[string]struct{}, error) {
	relevant := map[string]struct{}{}
	var uncached []factRecord
	for _, f := range surfaced {
		item := evalRetrievedItem{ID: f.ID, Text: f.Text, Path: strings.Join(f.Paths, ",")}
		if v, ok := cache.get(task, evalRetrieverFacts, item); ok {
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
		item := evalRetrievedItem{ID: f.ID, Text: f.Text, Path: strings.Join(f.Paths, ",")}
		_, isRel := judged[f.ID]
		cache.set(task, evalRetrieverFacts, item, isRel)
		if isRel {
			relevant[f.ID] = struct{}{}
		}
	}
	return relevant, nil
}

func judgeRelevanceItems(ctx context.Context, run distillAgentRunner, repoDir string, judgeArgs []string, task evalTask, retriever string, surfaced []evalRetrievedItem, cache *judgeCache) (map[string]struct{}, error) {
	relevant := map[string]struct{}{}
	var uncached []evalRetrievedItem
	for _, item := range surfaced {
		if v, ok := cache.get(task, retriever, item); ok {
			if v {
				relevant[item.ID] = struct{}{}
			}
			continue
		}
		uncached = append(uncached, item)
	}
	if len(uncached) == 0 {
		return relevant, nil
	}
	out, err := run(ctx, repoDir, judgeArgs, judgeInputItems(task.Task, uncached), defaultDistillTimeout)
	if err != nil {
		return nil, fmt.Errorf("judge task %s: %w", task.ID, err)
	}
	judged := parseJudgeOutputItems(out, uncached)
	for _, item := range uncached {
		_, isRel := judged[item.ID]
		cache.set(task, retriever, item, isRel)
		if isRel {
			relevant[item.ID] = struct{}{}
		}
	}
	return relevant, nil
}

func printEvalSummary(cmd *cobra.Command, s evalSummary) {
	out := cmd.OutOrStdout()
	if s.Retriever != "" {
		fmt.Fprintf(out, "retriever: %s\n", s.Retriever)
	}
	showLatencyBreakdown := evalHasLatencyBreakdown(s.Results)
	if showLatencyBreakdown {
		fmt.Fprintf(out, "%-14s %-24s %-13s %8s %8s %8s %8s %7s %7s %9s\n", "task", "relevance", "label", "tokens", "lat(ms)", "exp(ms)", "e2e(ms)", "prec", "recall", "useful/1k")
	} else {
		fmt.Fprintf(out, "%-14s %-24s %-13s %8s %8s %7s %7s %9s\n", "task", "relevance", "label", "tokens", "lat(ms)", "prec", "recall", "useful/1k")
	}
	rows := append([]evalTaskResult(nil), s.Results...)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	for _, r := range rows {
		recall := "-"
		if r.Labeled {
			recall = fmt.Sprintf("%.2f", r.Recall)
		}
		relevance := truncateString(valueOrUnset(r.RelevanceSource), 24)
		label := truncateString(valueOrUnset(r.LabelSource), 13)
		if showLatencyBreakdown {
			fmt.Fprintf(out, "%-14s %-24s %-13s %8d %8d %8d %8d %7.2f %7s %9.2f\n", truncateString(r.ID, 14), relevance, label, r.Tokens, r.LatencyMS, r.ExpansionLatencyMS, evalEndToEndLatencyMS(r), r.Precision, recall, r.UsefulPer1k)
		} else {
			fmt.Fprintf(out, "%-14s %-24s %-13s %8d %8d %7.2f %7s %9.2f\n", truncateString(r.ID, 14), relevance, label, r.Tokens, r.LatencyMS, r.Precision, recall, r.UsefulPer1k)
		}
	}
	if showLatencyBreakdown {
		fmt.Fprintf(out, "%-14s %-24s %-13s %8.0f %8.0f %8.0f %8.0f %7.2f %7s %9.2f\n", "MEAN", "", "", s.MeanTokens, s.MeanLatencyMS, s.MeanExpansionLatencyMS, s.MeanEndToEndLatencyMS, s.MeanPrecision, "", s.MeanUsefulPer1k)
	} else {
		fmt.Fprintf(out, "%-14s %-24s %-13s %8.0f %8.0f %7.2f %7s %9.2f\n", "MEAN", "", "", s.MeanTokens, s.MeanLatencyMS, s.MeanPrecision, "", s.MeanUsefulPer1k)
	}
	if note := evalSummaryRelevanceNote(s.Results); note != "" {
		fmt.Fprintln(out, note)
	}
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

func evalSummaryRelevanceNote(results []evalTaskResult) string {
	if len(results) == 0 {
		return ""
	}
	nonProof := map[string]int{}
	for _, r := range results {
		if evalRelevanceSourceIsProofLabel(r) {
			continue
		}
		source := strings.TrimSpace(r.RelevanceSource)
		if source == "" {
			source = evalRelevanceNone
		}
		nonProof[source]++
	}
	if len(nonProof) == 0 {
		return ""
	}
	sources := make([]string, 0, len(nonProof))
	for source, n := range nonProof {
		sources = append(sources, fmt.Sprintf("%s=%d", source, n))
	}
	sort.Strings(sources)
	return "note: non-proof relevance rows present (" + strings.Join(sources, ", ") + "); source-match, silver, judge, mixed, or none rows are not human/judge_refined recall proof."
}

func evalHasLatencyBreakdown(results []evalTaskResult) bool {
	for _, r := range results {
		if r.ExpansionLatencyMS != 0 {
			return true
		}
	}
	return false
}
