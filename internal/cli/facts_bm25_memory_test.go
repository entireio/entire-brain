package cli

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"testing"
)

// Independent full-vocabulary reference for the pre-optimization BM25 formula.
// Keeping it in tests catches changes to token counts, length normalization,
// document frequency, and floating-point summation order.
func referenceBM25Scores(facts []factRecord, terms []string) []float64 {
	tokens := make([][]string, len(facts))
	df := map[string]int{}
	total := 0
	for i, fact := range facts {
		tokens[i] = factSearchTokens(fact)
		total += len(tokens[i])
		seen := map[string]bool{}
		for _, token := range tokens[i] {
			if !seen[token] {
				df[token]++
				seen[token] = true
			}
		}
	}
	scores := make([]float64, len(facts))
	for i, document := range tokens {
		if len(document) == 0 || len(terms) == 0 {
			continue
		}
		tf := map[string]int{}
		for _, token := range document {
			tf[token]++
		}
		for _, term := range terms {
			f := float64(tf[term])
			if f == 0 {
				continue
			}
			n := float64(len(facts))
			idf := math.Log((n-float64(df[term])+0.5)/(float64(df[term])+0.5) + 1)
			avgLen := float64(total) / n
			norm := 1 - bm25B + bm25B*float64(len(document))/avgLen
			scores[i] += idf * f * (bm25K1 + 1) / (f + bm25K1*norm)
		}
	}
	return scores
}

func TestBM25QueryStatisticsMatchFullVocabulary(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(7))
	vocabulary := []string{"retry", "checkpoint", "cache", "MySQL", "HTTPServer", "café", "Привет", "数据库", "東京"}
	facts := []factRecord{{}, {Text: "retry retry retry checkpoint", Paths: []string{"architecture.retry.cache"}}}
	for range 100 {
		words := make([]string, 1+rng.Intn(40))
		for i := range words {
			words[i] = vocabulary[rng.Intn(len(vocabulary))]
		}
		facts = append(facts, factRecord{Text: strings.Join(words, " "), Paths: []string{"architecture.local.cache"}})
	}
	queries := []string{"retry checkpoint", "retry retry cache", "MySQL HTTPServer", "café Привет", "数据库 東京", "architecture.local", "absent", "???", ""}
	for _, query := range queries {
		t.Run(query, func(t *testing.T) {
			t.Parallel()
			terms := factQueryTerms(query)
			corpus := newBM25CorpusContext(context.Background(), facts, terms)
			want := referenceBM25Scores(facts, terms)
			for i := range facts {
				if got := corpus.bm25Score(i); got != want[i] {
					t.Fatalf("document %d: score %.17g, full-vocabulary reference %.17g", i, got, want[i])
				}
			}
		})
	}
}

func BenchmarkBM25QueryStatistics(b *testing.B) {
	facts := make([]factRecord, 10000)
	for i := range facts {
		facts[i] = factRecord{Text: fmt.Sprintf("checkpoint retry policy component %d bounded backoff preserves provenance durable cursor successful write", i), Paths: []string{"architecture.checkpoint.retry"}}
	}
	for _, termCount := range []int{2, 1000} {
		terms := []string{"checkpoint", "retry"}
		for i := len(terms); i < termCount; i++ {
			terms = append(terms, fmt.Sprintf("absent%d", i))
		}
		b.Run(fmt.Sprintf("query_terms=%d", termCount), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				corpus := newBM25CorpusContext(context.Background(), facts, terms)
				if corpus.bm25Score(0) <= 0 {
					b.Fatal("matching fact lost")
				}
			}
		})
	}
}
