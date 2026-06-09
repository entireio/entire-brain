package cli

import (
	"os"
	"sort"
	"strconv"
	"testing"
)

// TestChunkRetrievalVsDistill answers: does the agent-distillation step earn its
// cost, or would embedding the *stripped raw logs* (qmd-style, no agent) retrieve
// comparably? It compares two corpora under one identical mechanism — cosine
// retrieval with the same embedder over the same tasks — so only the unit differs:
//
//	A: stripped transcript chunks (preprocessTranscriptForDistill + chunkTranscript,
//	   NO agent). A chunk "covers" a relevant fact when the fact's provenance anchor
//	   (session + line) falls in the chunk's line range.
//	B: the distilled facts themselves (the agent's output). A retrieved fact is
//	   relevant when its id is in the task's labeled set.
//
// Metric (same for both): relevant facts delivered per 1k tokens of returned
// material — facts eval's useful/1k — plus recall. The gap quantifies the
// token-compression distillation provides. Env: CHUNK_EVAL_BRAIN, CHUNK_EVAL_TASKS,
// CHUNK_EVAL_BRANCH (main), CHUNK_EVAL_BYTES (distill default), CHUNK_EVAL_K (10).
func TestChunkRetrievalVsDistill(t *testing.T) {
	brainDir := os.Getenv("CHUNK_EVAL_BRAIN")
	tasksPath := os.Getenv("CHUNK_EVAL_TASKS")
	if brainDir == "" || tasksPath == "" {
		t.Skip("set CHUNK_EVAL_BRAIN and CHUNK_EVAL_TASKS")
	}
	branch := envOrDefault("CHUNK_EVAL_BRANCH", "main")
	chunkBytes := envIntDefault("CHUNK_EVAL_BYTES", defaultDistillChunkSize)
	topK := envIntDefault("CHUNK_EVAL_K", 10)

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

	// Corpus A: embed stripped transcript chunks (no agent).
	manifest, err := loadBrainManifest(brainDir)
	if err != nil || manifest.Sources == nil || manifest.Sources.Sessions == nil {
		t.Fatalf("manifest/sessions: %v", err)
	}
	type chunkVec struct {
		session    string
		start, end int
		tokens     int
		vec        []float32
	}
	var chunks []chunkVec
	for _, s := range manifest.Sources.Sessions.Sessions {
		content, rErr := readBrainRelativeFile(brainDir, s.TranscriptPath)
		if rErr != nil {
			continue
		}
		for _, c := range chunkTranscript(preprocessTranscriptForDistill(content), chunkBytes) {
			chunks = append(chunks, chunkVec{s.SessionID, c.StartLine, c.EndLine, len(c.Text) / 4, e.Embed(c.Text)})
		}
	}

	// Corpus B: embed the distilled facts (active only, matching the recall path).
	type factVec struct {
		id     string
		tokens int
		vec    []float32
	}
	var factVecs []factVec
	for _, f := range facts {
		if f.Status != factStatusActive {
			continue
		}
		factVecs = append(factVecs, factVec{f.ID, len(f.Text) / 4, e.Embed(f.Text)})
	}
	t.Logf("embedder=%s  chunks=%d (<=%dB)  active_facts=%d", e.ID(), len(chunks), chunkBytes, len(factVecs))

	var aUseful, aRecall, aTokens, bUseful, bRecall, bTokens float64
	n := 0
	for _, task := range tasks {
		if len(task.Relevant) == 0 {
			continue
		}
		n++
		relevant := make(map[string]bool, len(task.Relevant))
		for _, id := range task.Relevant {
			relevant[id] = true
		}
		k := task.K
		if k <= 0 {
			k = topK
		}
		qv := embedQ(task.Task)

		// A: top-k chunks -> which relevant facts' anchors do they cover.
		ca := make([]int, len(chunks))
		for i := range ca {
			ca[i] = i
		}
		sort.Slice(ca, func(x, y int) bool {
			return cosineFloat32(qv, chunks[ca[x]].vec) > cosineFloat32(qv, chunks[ca[y]].vec)
		})
		coveredA := map[string]bool{}
		tokA := 0
		for _, idx := range ca[:min(k, len(ca))] {
			c := chunks[idx]
			tokA += c.tokens
			for id := range relevant {
				f, ok := factByID[id]
				if !ok {
					continue
				}
				for _, a := range f.Provenance {
					if a.SessionID == c.session && a.Line >= c.start && a.Line <= c.end {
						coveredA[id] = true
					}
				}
			}
		}

		// B: top-k facts -> which are labeled relevant.
		cb := make([]int, len(factVecs))
		for i := range cb {
			cb[i] = i
		}
		sort.Slice(cb, func(x, y int) bool {
			return cosineFloat32(qv, factVecs[cb[x]].vec) > cosineFloat32(qv, factVecs[cb[y]].vec)
		})
		coveredB := 0
		tokB := 0
		for _, idx := range cb[:min(k, len(cb))] {
			tokB += factVecs[idx].tokens
			if relevant[factVecs[idx].id] {
				coveredB++
			}
		}

		total := float64(len(task.Relevant))
		aRecall += float64(len(coveredA)) / total
		bRecall += float64(coveredB) / total
		aTokens += float64(tokA)
		bTokens += float64(tokB)
		if tokA > 0 {
			aUseful += float64(len(coveredA)) / (float64(tokA) / 1000)
		}
		if tokB > 0 {
			bUseful += float64(coveredB) / (float64(tokB) / 1000)
		}
	}
	nf := float64(n)
	t.Logf("=== %d labeled tasks, k=%d, embedder=%s ===", n, topK, e.ID())
	t.Logf("A raw chunks   : useful/1k=%.3f  recall=%.3f  mean_tokens/result=%.0f", aUseful/nf, aRecall/nf, aTokens/nf/float64(topK))
	t.Logf("B distilled fac: useful/1k=%.3f  recall=%.3f  mean_tokens/result=%.0f", bUseful/nf, bRecall/nf, bTokens/nf/float64(topK))
}

func envOrDefault(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func envIntDefault(k string, d int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return d
}
