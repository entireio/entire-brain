package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// history_eval.go is the history-layer retrieval eval — the measurement the
// Stage 1a findings left open ("tunable once a history eval lands"). It mirrors
// the facts eval (facts_eval.go / facts_eval_gen.go) and reuses its task,
// result, and summary machinery, so `facts eval-compare` works on history
// summaries unchanged. It exists to answer three questions the alignment plan
// gates on this eval:
//
//  1. The BM25 relevance cutoff and top-k (historyFTSRelevanceCutoff = 0.30 was
//     chosen by inspection) — sweep with --cutoff / --k.
//  2. Whether a semantic arm fused with the shipped BM25 lifts retrieval —
//     Model2Vec measured as noise on history prose, EmbeddingGemma separated
//     cleanly on the probe, but the quantitative lift was never measured. Run
//     --arm fused (optionally with ENTIRE_BRAIN_EMBEDDER=ollama) vs --arm bm25.
//  3. Whether history vectors are worth persisting in the vec0 store at all.
//
// Tasks are provenance-labeled, exactly like facts eval-gen: each captured
// session becomes a task (its opening user request), and the history records
// extracted FROM that session's transcript are the ground-truth relevant set.
// `request`-kind records are excluded from labels: the general ranking arm
// filters them out (they are the query restated), so counting them would cap
// recall on records the arm can never surface.

// --- task generation (history eval-gen) -------------------------------------

func newHistoryEvalGenCommand(opts Options) *cobra.Command {
	var (
		out        string
		limit      int
		branch     string
		minRecords int
		maxRecords int
	)
	cmd := &cobra.Command{
		Use:   "history-eval-gen",
		Short: "Generate a labeled history-retrieval task set from the brain's own sessions",
		Long: `history-eval-gen builds a provenance-labeled benchmark for the history layer:
each captured session becomes a task (its opening user request), and the history
records indexed from that session's transcript are the ground-truth relevant set.
Emits a tasks.json consumable by 'history-eval --tasks'.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, brainDir, _, err := resolveFactsTarget(cmd.Context(), opts, agentSurfaceTarget(opts, nil), branch)
			if err != nil {
				return err
			}
			manifest, err := loadBrainManifest(brainDir)
			if err != nil {
				return err
			}
			if manifest.Sources == nil || manifest.Sources.Sessions == nil {
				return fmt.Errorf("no exported sessions; run `entire brain refresh` first")
			}
			index, err := loadBrainHistoryIndex(brainDir, manifest.Sources.History)
			if err != nil {
				return err
			}
			tasks := generateHistoryEvalTasks(brainDir, manifest, index, branch, minRecords, maxRecords, limit)
			if len(tasks) == 0 {
				return fmt.Errorf("no sessions qualified as tasks (need >= %d indexed records and a recoverable opening request)", minRecords)
			}
			data, err := json.MarshalIndent(tasks, "", "  ")
			if err != nil {
				return err
			}
			data = append(data, '\n')
			if strings.TrimSpace(out) == "" {
				_, err = cmd.OutOrStdout().Write(data)
				return err
			}
			if err := os.WriteFile(out, data, 0o600); err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "wrote %d tasks to %s\n", len(tasks), out)
			return nil
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "Write tasks JSON to this file (default: stdout)")
	cmd.Flags().IntVar(&limit, "limit", 0, "Cap the number of tasks, sampled evenly across strata (0 = all)")
	cmd.Flags().StringVar(&branch, "branch", "", "Limit to sessions on one branch (default: all)")
	// History records are far denser per session than facts (~83 median on the
	// live brain vs the facts eval's 2-30 band): a session emits one record per
	// decision/validation/tool-path line, not one per durable insight. The
	// max-records cap therefore sits an order of magnitude above facts
	// eval-gen's max-facts.
	cmd.Flags().IntVar(&minRecords, "min-records", 2, "Minimum indexed records for a session to become a task")
	cmd.Flags().IntVar(&maxRecords, "max-records", 200, "Skip broad sessions with more records than this (diffuse targets); 0 = no cap")
	return cmd
}

// generateHistoryEvalTasks maps each qualifying session to one provenance-
// labeled task. A record is attributable to a session through its Path — the
// transcript file the record was extracted from — which equals the session's
// manifest TranscriptPath (both are slash-relative to the brain dir).
func generateHistoryEvalTasks(brainDir string, manifest *exportManifest, index historyIndex, branch string, minRecords, maxRecords, limit int) []evalTask {
	byPath := map[string][]string{} // transcript path -> rankable record ids
	seen := map[string]struct{}{}
	for _, r := range index.Records {
		if r.Kind == "request" {
			continue // never surfaced by the general arm; see file comment
		}
		if _, dup := seen[r.ID]; dup {
			continue
		}
		seen[r.ID] = struct{}{}
		byPath[r.Path] = append(byPath[r.Path], r.ID)
	}

	var tasks []evalTask
	for _, s := range manifest.Sources.Sessions.Sessions {
		if branch != "" && s.Branch != branch {
			continue
		}
		ids := byPath[s.TranscriptPath]
		if len(ids) < minRecords {
			continue
		}
		if maxRecords > 0 && len(ids) > maxRecords {
			continue // broad session: too many records to be a focused target
		}
		content, readErr := readBrainRelativeFile(brainDir, s.TranscriptPath)
		if readErr != nil {
			continue
		}
		request := firstUserRequest(content)
		if request == "" {
			continue
		}
		sorted := append([]string(nil), ids...)
		sort.Strings(sorted)
		tasks = append(tasks, evalTask{
			ID:        shortSessionID(s.SessionID),
			Task:      request,
			Branch:    s.Branch,
			QueryType: classifyHistoryQueryType(request),
			Relevant:  sorted,
		})
	}
	tasks = dedupeEvalTasks(tasks)
	sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })
	if limit > 0 && len(tasks) > limit {
		tasks = sampleAcrossStrata(tasks, limit)
	}
	return tasks
}

// classifyHistoryQueryType strata: code/howto/concept. The facts classifier's
// "convention" stratum keys off fact path scope, which has no history analog
// (record paths are transcript files, not code loci).
func classifyHistoryQueryType(task string) string {
	if hasStructuralCodeToken(task) {
		return queryTypeCode
	}
	lower := strings.ToLower(strings.TrimSpace(task))
	if hasAnyPrefix(lower, "where ", "how do i", "how should", "how can i", "add ", "implement ", "fix ", "wire ", "make ", "build ", "create ", "update ", "remove ") {
		return queryTypeHowto
	}
	return queryTypeConcept
}

// --- eval (history eval) -----------------------------------------------------

// historyEvalArm ranks the index for one query and returns the surfaced
// records, or an error when the arm's machinery is unavailable — an eval must
// fail loudly rather than silently measure a fallback arm.
type historyEvalArm func(query string, k int) ([]historyRecord, error)

func newHistoryEvalCommand(opts Options) *cobra.Command {
	var (
		tasksFile string
		branch    string
		k         int
		arm       string
		cutoff    float64
		jsonOut   bool
	)
	cmd := &cobra.Command{
		Use:   "history-eval --tasks <file>",
		Short: "Measure history-retrieval quality (tokens, precision, useful-records-per-1k) over held-out tasks",
		Long: `history-eval runs a labeled task set (from history-eval-gen) through one history
retrieval arm and reports the same metrics as 'facts eval' — surfaced records,
estimated tokens, precision, recall, useful-per-1k — so 'facts eval-compare'
works on the summaries unchanged.

Arms:
  substring  the pre-BM25 coverage-gate scorer (the 1a baseline)
  bm25       the shipped FTS5 ranker; --cutoff sweeps its relevance cutoff
  fused      bm25 + semantic (RRF k=60) over record summaries — the prospective
             arm the alignment plan wants measured. Uses the configured
             embedder (ENTIRE_BRAIN_EMBEDDER=ollama selects EmbeddingGemma);
             every index record is embedded once per run, so the first fused
             run over a large brain with a server-backed embedder is slow.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(tasksFile) == "" {
				return fmt.Errorf("--tasks <file> is required")
			}
			tasks, err := loadEvalTasks(tasksFile)
			if err != nil {
				return err
			}
			_, brainDir, _, err := resolveFactsTarget(cmd.Context(), opts, agentSurfaceTarget(opts, nil), branch)
			if err != nil {
				return err
			}
			manifest, err := loadBrainManifest(brainDir)
			if err != nil {
				return err
			}
			if manifest.Sources == nil {
				return fmt.Errorf("brain has no sources; run `entire brain refresh` first")
			}
			index, err := loadBrainHistoryIndex(brainDir, manifest.Sources.History)
			if err != nil {
				return err
			}
			armFn, err := selectHistoryEvalArm(arm, brainDir, index, cutoff)
			if err != nil {
				return err
			}
			results, err := runHistoryEval(tasks, k, armFn)
			if err != nil {
				return err
			}
			summary := summarizeEval(results)
			summary.Arm = arm
			if jsonOut {
				return writeJSON(cmd, summary)
			}
			printEvalSummary(cmd, summary)
			return nil
		},
	}
	cmd.Flags().StringVar(&tasksFile, "tasks", "", "Path to the held-out task set (JSON array, from history-eval-gen)")
	cmd.Flags().StringVar(&branch, "branch", "", "Brain branch context (default: current branch)")
	cmd.Flags().IntVar(&k, "k", 10, "Records to retrieve per task")
	cmd.Flags().StringVar(&arm, "arm", "bm25", "Retrieval arm: substring | bm25 | fused")
	cmd.Flags().Float64Var(&cutoff, "cutoff", historyFTSRelevanceCutoff, "BM25 relevance cutoff (fraction of the top score) for the bm25/fused arms")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit the summary as JSON")
	return cmd
}

func selectHistoryEvalArm(arm, brainDir string, index historyIndex, cutoff float64) (historyEvalArm, error) {
	switch arm {
	case "substring":
		return func(query string, k int) ([]historyRecord, error) {
			return rankHistoryRecords(index, "history", query, k), nil
		}, nil
	case "bm25":
		return func(query string, k int) ([]historyRecord, error) {
			// A query with no usable FTS terms (e.g. all stopwords: "fix") is a
			// deterministic honest empty — the same as the fused arm's lexical
			// side — not a harness failure. Only a real index fault errors.
			if historyFTSMatchExpr(query) == "" {
				return nil, nil
			}
			scored, ok := rankHistoryViaFTSCutoff(brainDir, index, "history", query, k, cutoff)
			if !ok {
				return nil, fmt.Errorf("bm25 arm unavailable for query %q (FTS index failed to build or query failed)", query)
			}
			return historyRecordsOf(scored), nil
		}, nil
	case "fused":
		e := defaultEmbedder()
		if e == nil {
			return nil, fmt.Errorf("--arm fused requested but the embedding backend is unavailable")
		}
		f := newHistoryFusedRanker(brainDir, index, e, cutoff)
		return f.rank, nil
	default:
		return nil, fmt.Errorf("unknown arm %q (substring | bm25 | fused)", arm)
	}
}

func historyRecordsOf(scored []scoredHistoryRecord) []historyRecord {
	out := make([]historyRecord, 0, len(scored))
	for _, s := range scored {
		out = append(out, s.Record)
	}
	return out
}

func runHistoryEval(tasks []evalTask, defaultK int, arm historyEvalArm) ([]evalTaskResult, error) {
	results := make([]evalTaskResult, 0, len(tasks))
	for _, task := range tasks {
		k := task.K
		if k <= 0 {
			k = defaultK
		}
		surfaced, err := arm(task.Task, k)
		if err != nil {
			return nil, fmt.Errorf("task %s: %w", task.ID, err)
		}
		relevant := make(map[string]struct{}, len(task.Relevant))
		for _, id := range task.Relevant {
			relevant[id] = struct{}{}
		}
		res := historyEvalMetrics(surfaced, relevant, len(relevant))
		res.ID, res.Task, res.QueryType, res.Labeled = task.ID, task.Task, task.QueryType, len(task.Relevant) > 0
		results = append(results, res)
	}
	return results, nil
}

// historyEvalMetrics mirrors evalMetrics over history records. The token
// estimate covers what an agent actually loads per surfaced record: the
// summary (already truncated to 700 chars at index time) plus its provenance
// path.
func historyEvalMetrics(surfaced []historyRecord, relevant map[string]struct{}, totalRelevant int) evalTaskResult {
	chars := 0
	for _, r := range surfaced {
		chars += len(r.Summary) + len(r.Path) + 4 // small per-record framing
	}
	res := evalTaskResult{Surfaced: len(surfaced), Tokens: (chars + 3) / 4}
	for _, r := range surfaced {
		if _, ok := relevant[r.ID]; ok {
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

// --- fused arm (BM25 + semantic RRF) ----------------------------------------

// historyFusedRanker is the prospective history semantic arm: RRF fusion of
// the shipped BM25 list with a cosine ranking of record summaries, mirroring
// rankFactsFused's structure. It is eval-only — whether it ships is exactly
// what the eval decides. Record vectors are embedded once and cached in memory
// for the run (keyed by record ID); the cache is deliberately not persisted so
// the eval never seeds a production store with an arm that may not ship.
type historyFusedRanker struct {
	brainDir string
	index    historyIndex
	e        Embedder
	cutoff   float64
	vecs     map[string][]float32
}

func newHistoryFusedRanker(brainDir string, index historyIndex, e Embedder, cutoff float64) *historyFusedRanker {
	return &historyFusedRanker{brainDir: brainDir, index: index, e: e, cutoff: cutoff, vecs: map[string][]float32{}}
}

func (f *historyFusedRanker) recordVector(r historyRecord) []float32 {
	if v, ok := f.vecs[r.ID]; ok {
		return v
	}
	v := f.e.Embed(r.Summary)
	if d := f.e.Dim(); d > 0 && len(v) == d {
		f.vecs[r.ID] = v // cache only full vectors so a transient failure retries
	}
	return v
}

func (f *historyFusedRanker) rank(query string, k int) ([]historyRecord, error) {
	// Lexical arm: the shipped BM25 list, over-fetched so fusion sees lexical
	// ranks beyond the final top-k. Degenerate queries (no usable terms) keep
	// an empty lexical list rather than failing: the semantic arm still ranks.
	lexRank := map[string]int{}
	if scored, ok := rankHistoryViaFTSCutoff(f.brainDir, f.index, "history", query, k*4, f.cutoff); ok {
		for i, s := range scored {
			lexRank[s.Record.ID] = i
		}
	}

	var qvec []float32
	if qe, ok := f.e.(queryEmbedder); ok {
		qvec = qe.EmbedQuery(query)
	} else {
		qvec = f.e.Embed(query)
	}
	if len(qvec) == 0 {
		return nil, fmt.Errorf("fused arm: query embedding unavailable")
	}

	// Candidates: every rankable record, deduped by normalized summary exactly
	// like both production scorers, so fusion never surfaces two copies of the
	// same templated line.
	type cand struct {
		rec historyRecord
		cos float64
	}
	cands := make([]cand, 0, len(f.index.Records))
	seen := map[string]struct{}{}
	for _, r := range f.index.Records {
		if r.Kind == "request" {
			continue
		}
		key := normalizeHistorySearchText(r.Summary)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		cands = append(cands, cand{rec: r, cos: cosineFloat32(qvec, f.recordVector(r))})
	}

	// Semantic ranks over the full candidate set (reachability is the point),
	// fused with the lexical ranks via RRF (k=60, as rankFactsFused).
	order := make([]int, len(cands))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return cands[order[a]].cos > cands[order[b]].cos })
	fusedScore := make([]float64, len(cands))
	for rank, idx := range order {
		fusedScore[idx] += 1.0 / (rrfK + float64(rank+1))
	}
	for i, c := range cands {
		if lr, ok := lexRank[c.rec.ID]; ok {
			fusedScore[i] += 1.0 / (rrfK + float64(lr+1))
		}
	}
	sort.SliceStable(order, func(a, b int) bool {
		ia, ib := order[a], order[b]
		if fusedScore[ia] != fusedScore[ib] {
			return fusedScore[ia] > fusedScore[ib]
		}
		return cands[ia].rec.ID < cands[ib].rec.ID // deterministic tiebreak
	})
	out := make([]historyRecord, 0, min(k, len(order)))
	for _, idx := range order {
		if fusedScore[idx] <= 0 {
			break
		}
		out = append(out, cands[idx].rec)
		if len(out) >= k {
			break
		}
	}
	return out, nil
}
