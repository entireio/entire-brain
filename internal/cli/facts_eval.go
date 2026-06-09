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
// retriever). Without labels, --judge has an agent decide relevance per
// surfaced item.
type evalTask struct {
	ID                   string   `json:"id"`
	Task                 string   `json:"task"`
	Branch               string   `json:"branch,omitempty"`
	QueryType            string   `json:"query_type,omitempty"`
	K                    int      `json:"k,omitempty"`
	Relevant             []string `json:"relevant,omitempty"`
	SourceSessionID      string   `json:"source_session_id,omitempty"`
	SourceTranscriptPath string   `json:"source_transcript_path,omitempty"`
}

// evalTaskResult is the per-task measurement. The headline metric is
// UsefulPer1k — relevant items surfaced per 1,000 tokens spent — which captures
// the Appendix D agent constraint (value per token, not item count).
type evalTaskResult struct {
	ID               string  `json:"id"`
	Task             string  `json:"task"`
	QueryType        string  `json:"query_type,omitempty"`
	Retriever        string  `json:"retriever,omitempty"`
	RelevanceSource  string  `json:"relevance_source,omitempty"`
	Surfaced         int     `json:"surfaced"`
	Tokens           int     `json:"tokens"`
	LatencyMS        int64   `json:"latency_ms"`
	RelevantSurfaced int     `json:"relevant_surfaced"`
	Precision        float64 `json:"precision"`
	Recall           float64 `json:"recall"`
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
	Retriever       string                 `json:"retriever,omitempty"`
	Tasks           int                    `json:"tasks"`
	MeanTokens      float64                `json:"mean_tokens"`
	MeanLatencyMS   float64                `json:"mean_latency_ms"`
	MeanPrecision   float64                `json:"mean_precision"`
	MeanUsefulPer1k float64                `json:"mean_useful_per_1k"`
	ByStratum       map[string]evalStratum `json:"by_stratum,omitempty"`
	Results         []evalTaskResult       `json:"results"`
}

const (
	evalRetrieverFacts       = "facts"
	evalRetrieverHistory     = "history"
	evalRetrieverQuery       = "query"
	evalRetrieverRawSessions = "raw-sessions"

	evalRelevanceExplicitLabel = "explicit_label"
	evalRelevanceSourceMatch   = "source_match"
	evalRelevanceJudge         = "judge"
	evalRelevanceNone          = "none"
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
	s := evalSummary{Tasks: len(results), Results: results}
	if len(results) == 0 {
		return s
	}
	s.Retriever = results[0].Retriever
	var tok, latency, prec, useful float64
	strata := map[string][]evalTaskResult{}
	for _, r := range results {
		tok += float64(r.Tokens)
		latency += float64(r.LatencyMS)
		prec += r.Precision
		useful += r.UsefulPer1k
		if r.QueryType != "" {
			strata[r.QueryType] = append(strata[r.QueryType], r)
		}
	}
	n := float64(len(results))
	s.MeanTokens = tok / n
	s.MeanLatencyMS = latency / n
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
		expand       bool
		agent        string
		model        string
		agentCommand []string
		judgeCache   string
		expandCache  string
		retriever    string
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
    "relevant":["fact:abc","fact:def"]}]
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
			if retriever == "" {
				retriever = evalRetrieverFacts
			}
			results, err := runFactsEvalWithOptions(cmd.Context(), opts, brainDir, repoDir, defaultBranch, tasks, k, judge, run, judgeArgs, loadJudgeCache(judgeCache), expander, rr, retriever, factsEvalRunOptions{JudgeSourceMatches: judgeSource})
			if err != nil {
				return err
			}
			if err := expCache.save(); err != nil {
				return fmt.Errorf("save expansion cache: %w", err)
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
	cmd.Flags().BoolVar(&judgeSource, "judge-source-matches", false, "With --judge, judge source-match proxy rows instead of counting transcript/session overlap as relevance")
	cmd.Flags().StringVar(&agent, "agent", "auto", "Judge/expand agent: auto, codex, claude-code, ollama, or command")
	cmd.Flags().StringVar(&model, "model", "", "Model for codex/claude-code/ollama judge and expand calls")
	cmd.Flags().StringArrayVar(&agentCommand, "agent-command", nil, "Agent command argv for --agent command")
	cmd.Flags().StringVar(&judgeCache, "judge-cache", "", "Persist/reuse judge verdicts at this path so re-runs are deterministic and cheap")
	cmd.Flags().BoolVar(&semantic, "semantic", false, "Rerank with the local embedding backend (RRF fusion of lexical + semantic)")
	cmd.Flags().BoolVar(&expand, "expand", false, "Expand each task query with agent-generated retrieval terms before retrieval")
	cmd.Flags().StringVar(&expandCache, "expand-cache", "", "Persist/reuse query expansions at this path")
	cmd.Flags().StringVar(&retriever, "retriever", evalRetrieverFacts, "Retrieval arm: facts, history, query, or raw-sessions")
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
		_ = json.Unmarshal(data, &c.verdict)
	}
	return c
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
	return tasks, nil
}

func validateEvalRetriever(retriever string) error {
	switch retriever {
	case evalRetrieverFacts, evalRetrieverHistory, evalRetrieverQuery, evalRetrieverRawSessions:
		return nil
	default:
		return fmt.Errorf("--retriever must be one of facts, history, query, raw-sessions")
	}
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
	return fmt.Sprintf("raw:%s:%d-%d", shortSessionID(session.SessionID), chunk.StartLine, chunk.EndLine)
}

func evalLabelsForRetriever(task evalTask, retriever string, surfaced []evalRetrievedItem) (map[string]struct{}, int, bool, string) {
	explicit := relevantIDsForRetriever(task.Relevant, retriever)
	if len(explicit) > 0 {
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

func evalItemMatchesTaskSource(task evalTask, item evalRetrievedItem) bool {
	sessionID := strings.TrimSpace(task.SourceSessionID)
	if sessionID != "" && strings.HasPrefix(item.ID, "raw:"+shortSessionID(sessionID)+":") {
		return true
	}
	transcript := filepath.ToSlash(strings.TrimSpace(task.SourceTranscriptPath))
	if transcript == "" {
		return false
	}
	path := filepath.ToSlash(strings.TrimSpace(item.Path))
	if idx := strings.LastIndex(path, ":"); idx > 0 {
		suffix := path[idx+1:]
		if _, err := strconv.Atoi(suffix); err == nil {
			path = path[:idx]
		}
	}
	return path == transcript
}

func taskHasEvalSourceAnchor(task evalTask) bool {
	return strings.TrimSpace(task.SourceSessionID) != "" || strings.TrimSpace(task.SourceTranscriptPath) != ""
}

// queryExpanderFunc maps a task query to extra retrieval terms; nil disables
// expansion.
type queryExpanderFunc func(query string) (string, error)

type factsEvalRunOptions struct {
	JudgeSourceMatches bool
}

func runFactsEval(ctx context.Context, opts Options, brainDir, repoDir, defaultBranch string, tasks []evalTask, defaultK int, judge bool, run distillAgentRunner, judgeArgs []string, cache *judgeCache, expander queryExpanderFunc, rr *semanticReranker, retriever string) ([]evalTaskResult, error) {
	return runFactsEvalWithOptions(ctx, opts, brainDir, repoDir, defaultBranch, tasks, defaultK, judge, run, judgeArgs, cache, expander, rr, retriever, factsEvalRunOptions{})
}

func runFactsEvalWithOptions(ctx context.Context, opts Options, brainDir, repoDir, defaultBranch string, tasks []evalTask, defaultK int, judge bool, run distillAgentRunner, judgeArgs []string, cache *judgeCache, expander queryExpanderFunc, rr *semanticReranker, retriever string, runOpts factsEvalRunOptions) ([]evalTaskResult, error) {
	if err := validateEvalRetriever(retriever); err != nil {
		return nil, err
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
		query := task.Task
		if expander != nil {
			if exp, expErr := expander(task.Task); expErr != nil {
				return nil, fmt.Errorf("expand task %s: %w", task.ID, expErr)
			} else {
				query = expandedQuery(task.Task, exp)
			}
		}
		started := time.Now()
		surfaced, err := retrieveEvalItems(brainDir, branch, query, k, retriever, facts, rr)
		if err != nil {
			return nil, fmt.Errorf("retrieve task %s: %w", task.ID, err)
		}
		latencyMS := time.Since(started).Milliseconds()

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
		res.RelevanceSource = relevanceSource
		res.LatencyMS = latencyMS
		results = append(results, res)
	}
	if err := cache.save(); err != nil {
		return nil, fmt.Errorf("save judge cache: %w", err)
	}
	return results, nil
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
	fmt.Fprintf(out, "%-14s %8s %8s %7s %7s %9s\n", "task", "tokens", "lat(ms)", "prec", "recall", "useful/1k")
	rows := append([]evalTaskResult(nil), s.Results...)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	for _, r := range rows {
		recall := "-"
		if r.Labeled {
			recall = fmt.Sprintf("%.2f", r.Recall)
		}
		fmt.Fprintf(out, "%-14s %8d %8d %7.2f %7s %9.2f\n", truncateString(r.ID, 14), r.Tokens, r.LatencyMS, r.Precision, recall, r.UsefulPer1k)
	}
	fmt.Fprintf(out, "%-14s %8.0f %8.0f %7.2f %7s %9.2f\n", "MEAN", s.MeanTokens, s.MeanLatencyMS, s.MeanPrecision, "", s.MeanUsefulPer1k)
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
