package cli

import (
	"fmt"
	"testing"
	"time"
)

type cachedRankBenchmarkEmbedder struct {
	dim   int
	query []float32
}

func (e *cachedRankBenchmarkEmbedder) Embed(string) []float32      { return nil }
func (e *cachedRankBenchmarkEmbedder) EmbedQuery(string) []float32 { return e.query }
func (e *cachedRankBenchmarkEmbedder) Dim() int                    { return e.dim }
func (e *cachedRankBenchmarkEmbedder) ID() string                  { return "cached-rank-benchmark" }

func BenchmarkRankFactsFusedCachedVectors(b *testing.B) {
	for _, count := range []int{44, 128, 512, 5_000} {
		b.Run(fmt.Sprintf("facts=%d", count), func(b *testing.B) {
			b.Setenv("ENTIRE_BRAIN_FACTS_BM25", "")
			const dim = 512 // the bundled Model2Vec model's embedding dimension
			query := benchmarkRankVector(17, dim)
			e := &cachedRankBenchmarkEmbedder{dim: dim, query: query}
			rr := newSemanticReranker(e)
			facts := make([]factRecord, count)
			base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			for i := range facts {
				id := fmt.Sprintf("fact:%08d", i)
				facts[i] = factRecord{
					ID:        id,
					Text:      fmt.Sprintf("component %d retry behavior and release policy", i),
					Status:    factStatusActive,
					UpdatedAt: base.Add(time.Duration(i) * time.Second),
				}
				rr.cache[id] = benchmarkRankVector(i+1, dim)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if got := rankFactsFused(facts, "retry release behavior", 6, false, rr); len(got) != 6 {
					b.Fatalf("result count = %d, want 6", len(got))
				}
			}
		})
	}
}

func benchmarkRankVector(seed, dim int) []float32 {
	v := make([]float32, dim)
	for i := range v {
		// Deterministic, non-zero, finite vectors with plentiful score ties and
		// near-ties exercise both ranking work and its stable tie semantics.
		v[i] = float32(((seed+11)*(i+17))%257-128) / 128
	}
	return v
}
