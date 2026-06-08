package cli

import (
	"os"
	"sort"
	"testing"
)

// TestHistoryRetrievalWithRequests measures whether indexing user prompts as
// "request" records improves the history layer as a retrieval corpus, and how
// history compares to distilled facts. Same embedder, same cosine retrieval, same
// tasks. Three corpora:
//
//	history+req : non-tool_call history records INCLUDING request (user prompt) records
//	history-req : the same, EXCLUDING request records (today's behavior)
//	facts       : the distilled facts
//
// A retrieved history record "covers" a relevant fact when it comes from the same
// session (record.Path == the fact's provenance transcript) — history's job is to
// surface the relevant session/context. Facts use direct id relevance. Metric:
// relevant facts covered per 1k tokens of returned material, plus recall. The
// with/without-request delta isolates the value of the user prompts.
// Env: HIST_EVAL_BRAIN, HIST_EVAL_TASKS, HIST_EVAL_BRANCH (main), HIST_EVAL_K (10).
func TestHistoryRetrievalWithRequests(t *testing.T) {
	brainDir := os.Getenv("HIST_EVAL_BRAIN")
	tasksPath := os.Getenv("HIST_EVAL_TASKS")
	if brainDir == "" || tasksPath == "" {
		t.Skip("set HIST_EVAL_BRAIN and HIST_EVAL_TASKS")
	}
	branch := envOrDefault("HIST_EVAL_BRANCH", "main")
	topK := envIntDefault("HIST_EVAL_K", 10)

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

	tasks, err := loadEvalTasks(tasksPath)
	if err != nil {
		t.Fatalf("load tasks: %v", err)
	}
	facts, err := loadFacts(brainDir, branch)
	if err != nil {
		t.Fatalf("load facts: %v", err)
	}
	factByID := make(map[string]factRecord, len(facts))
	for _, f := range facts {
		factByID[f.ID] = f
	}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil || manifest.Sources == nil || manifest.Sources.History == nil {
		t.Fatalf("history source: %v", err)
	}
	index, err := loadBrainHistoryIndex(brainDir, manifest.Sources.History)
	if err != nil {
		t.Fatalf("history index: %v", err)
	}

	// History corpus: non-tool_call records (context, not mechanics). Embed once;
	// the request flag lets a config include/exclude user-prompt records.
	type histVec struct {
		path      string
		isRequest bool
		tokens    int
		vec       []float32
	}
	var hist []histVec
	nReq := 0
	for _, r := range index.Records {
		if r.Kind == "tool_call" {
			continue
		}
		isReq := r.Kind == "request"
		if isReq {
			nReq++
		}
		hist = append(hist, histVec{r.Path, isReq, len(r.Summary) / 4, e.Embed(r.Summary)})
	}
	// Facts corpus.
	type factVec struct {
		id     string
		tokens int
		vec    []float32
	}
	var factVecs []factVec
	for _, f := range facts {
		if f.Status == factStatusActive {
			factVecs = append(factVecs, factVec{f.ID, len(f.Text) / 4, e.Embed(f.Text)})
		}
	}
	t.Logf("embedder=%s  history_records=%d (request=%d)  facts=%d", e.ID(), len(hist), nReq, len(factVecs))

	type agg struct{ useful, recall, tokens float64 }
	var withReq, noReq, factsAgg agg
	n := 0
	for _, task := range tasks {
		if len(task.Relevant) == 0 {
			continue
		}
		n++
		total := float64(len(task.Relevant))
		// transcript(session) -> relevant fact ids in that session.
		sessFacts := map[string][]string{}
		for _, id := range task.Relevant {
			if f, ok := factByID[id]; ok {
				for _, a := range f.Provenance {
					sessFacts[a.Transcript] = append(sessFacts[a.Transcript], id)
				}
			}
		}
		k := task.K
		if k <= 0 {
			k = topK
		}
		qv := embedQ(task.Task)

		// HIST_EVAL_HOLDOUT de-circularizes: the eval task IS this session's opening
		// prompt, so its own request record is a verbatim self-match. Holding out
		// request records from the task's relevant sessions measures the realistic
		// effect (do OTHER sessions' prompts help find this one) rather than the
		// trivial self-retrieval.
		holdout := envIntDefault("HIST_EVAL_HOLDOUT", 0) == 1
		histScore := func(includeReq bool) agg {
			idx := make([]int, 0, len(hist))
			for i := range hist {
				if hist[i].isRequest {
					if !includeReq {
						continue
					}
					if holdout && sessFacts[hist[i].path] != nil {
						continue // self-match: prompt from the task's own session
					}
				}
				idx = append(idx, i)
			}
			sort.Slice(idx, func(a, b int) bool {
				return cosineFloat32(qv, hist[idx[a]].vec) > cosineFloat32(qv, hist[idx[b]].vec)
			})
			covered := map[string]bool{}
			tok := 0
			for _, i := range idx[:min(k, len(idx))] {
				tok += hist[i].tokens
				for _, id := range sessFacts[hist[i].path] {
					covered[id] = true
				}
			}
			a := agg{recall: float64(len(covered)) / total, tokens: float64(tok)}
			if tok > 0 {
				a.useful = float64(len(covered)) / (float64(tok) / 1000)
			}
			return a
		}
		w := histScore(true)
		nr := histScore(false)
		withReq.useful += w.useful
		withReq.recall += w.recall
		withReq.tokens += w.tokens
		noReq.useful += nr.useful
		noReq.recall += nr.recall
		noReq.tokens += nr.tokens

		// Facts.
		fi := make([]int, len(factVecs))
		for i := range fi {
			fi[i] = i
		}
		sort.Slice(fi, func(a, b int) bool {
			return cosineFloat32(qv, factVecs[fi[a]].vec) > cosineFloat32(qv, factVecs[fi[b]].vec)
		})
		rel := map[string]bool{}
		for _, id := range task.Relevant {
			rel[id] = true
		}
		cov, tok := 0, 0
		for _, i := range fi[:min(k, len(fi))] {
			tok += factVecs[i].tokens
			if rel[factVecs[i].id] {
				cov++
			}
		}
		factsAgg.recall += float64(cov) / total
		factsAgg.tokens += float64(tok)
		if tok > 0 {
			factsAgg.useful += float64(cov) / (float64(tok) / 1000)
		}
	}
	nf := float64(n)
	t.Logf("=== %d tasks, k=%d, embedder=%s ===", n, topK, e.ID())
	t.Logf("history +req : useful/1k=%.3f  recall=%.3f  tok/result=%.0f", withReq.useful/nf, withReq.recall/nf, withReq.tokens/nf/float64(topK))
	t.Logf("history -req : useful/1k=%.3f  recall=%.3f  tok/result=%.0f", noReq.useful/nf, noReq.recall/nf, noReq.tokens/nf/float64(topK))
	t.Logf("facts        : useful/1k=%.3f  recall=%.3f  tok/result=%.0f", factsAgg.useful/nf, factsAgg.recall/nf, factsAgg.tokens/nf/float64(topK))
}
