package cli

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestSeedRetrievalExperiment tests whether the embedder helps the seed layer —
// the last layer that doesn't use it. Seed is orientation content (agent-
// synthesized architecture/conventions/commands + copied repo docs), NOT session-
// derived, so it has no fact provenance. Relevance is therefore a deterministic,
// embedder-independent proxy: a retrieved seed chunk "covers" a relevant fact when
// a majority of that fact's content terms appear in the chunk. This measures
// whether seed content overlaps the session-fact topics at all, and whether a
// better embedder retrieves the overlapping chunks. Compared against facts (direct
// id relevance). Caveat: seed has no ground-truth labels; the fact-term proxy is a
// floor, and a near-zero result means "seed answers a different question than the
// session-fact eval", not "seed is useless".
// Env: SEED_EVAL_BRAIN, SEED_EVAL_TASKS, SEED_EVAL_BRANCH (main), SEED_EVAL_K (10),
// SEED_EVAL_BYTES (3600).
func TestSeedRetrievalExperiment(t *testing.T) {
	brainDir := os.Getenv("SEED_EVAL_BRAIN")
	tasksPath := os.Getenv("SEED_EVAL_TASKS")
	if brainDir == "" || tasksPath == "" {
		t.Skip("set SEED_EVAL_BRAIN and SEED_EVAL_TASKS")
	}
	branch := envOrDefault("SEED_EVAL_BRANCH", "main")
	topK := envIntDefault("SEED_EVAL_K", 10)
	chunkBytes := envIntDefault("SEED_EVAL_BYTES", 3600)

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
	// Precompute each fact's content terms (stopword-filtered) for the proxy.
	factTerms := make(map[string][]string, len(facts))
	for _, f := range facts {
		factTerms[f.ID] = historyQueryTerms(f.Text)
	}

	// Seed corpus: chunk every markdown file under brain/seed.
	type chunk struct {
		lower  string
		tokens int
		vec    []float32
	}
	var seedChunks []chunk
	seedDir := filepath.Join(brainDir, "seed")
	if err := filepath.WalkDir(seedDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(strings.ToLower(path), ".md") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, c := range chunkLines(string(data), chunkBytes, false) {
			seedChunks = append(seedChunks, chunk{strings.ToLower(c.Text), len(c.Text) / 4, e.Embed(c.Text)})
		}
		return nil
	}); err != nil {
		t.Fatalf("walk seed corpus: %v", err)
	}
	// Facts corpus for comparison.
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
	if len(seedChunks) == 0 || len(factVecs) == 0 {
		t.Fatalf("seed experiment needs non-empty seed chunks and active facts, got chunks=%d facts=%d", len(seedChunks), len(factVecs))
	}
	t.Logf("embedder=%s  seed_chunks=%d (<=%dB)  facts=%d", e.ID(), len(seedChunks), chunkBytes, len(factVecs))

	// chunkCoversFact: a majority of the fact's content terms appear in the chunk.
	chunkCoversFact := func(c chunk, factID string) bool {
		terms := factTerms[factID]
		if len(terms) == 0 {
			return false
		}
		hit := 0
		for _, term := range terms {
			if strings.Contains(c.lower, term) {
				hit++
			}
		}
		return hit*100 >= len(terms)*60
	}

	var seedUseful, seedRecall, seedTokens, factUseful, factRecall, factTokens float64
	n := 0
	for _, task := range tasks {
		if len(task.Relevant) == 0 {
			continue
		}
		n++
		total := float64(len(task.Relevant))
		qv := embedQ(task.Task)

		// Seed: top-k chunks -> relevant facts covered via the term proxy.
		si := make([]int, len(seedChunks))
		for i := range si {
			si[i] = i
		}
		sort.Slice(si, func(a, b int) bool {
			return cosineFloat32(qv, seedChunks[si[a]].vec) > cosineFloat32(qv, seedChunks[si[b]].vec)
		})
		covered := map[string]bool{}
		tok := 0
		for _, i := range si[:min(topK, len(si))] {
			tok += seedChunks[i].tokens
			for _, id := range task.Relevant {
				if chunkCoversFact(seedChunks[i], id) {
					covered[id] = true
				}
			}
		}
		seedRecall += float64(len(covered)) / total
		seedTokens += float64(tok)
		if tok > 0 {
			seedUseful += float64(len(covered)) / (float64(tok) / 1000)
		}

		// Facts: direct id relevance.
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
		cov, ftok := 0, 0
		for _, i := range fi[:min(topK, len(fi))] {
			ftok += factVecs[i].tokens
			if rel[factVecs[i].id] {
				cov++
			}
		}
		factRecall += float64(cov) / total
		factTokens += float64(ftok)
		if ftok > 0 {
			factUseful += float64(cov) / (float64(ftok) / 1000)
		}
	}
	if n == 0 {
		t.Fatal("seed experiment needs at least one eligible task")
	}
	nf := float64(n)
	t.Logf("=== %d tasks, k=%d, embedder=%s ===", n, topK, e.ID())
	t.Logf("seed  : useful/1k=%.3f  recall=%.3f  tok/result=%.0f", seedUseful/nf, seedRecall/nf, seedTokens/nf/float64(topK))
	t.Logf("facts : useful/1k=%.3f  recall=%.3f  tok/result=%.0f", factUseful/nf, factRecall/nf, factTokens/nf/float64(topK))
}
