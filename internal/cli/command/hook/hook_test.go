package hook

import (
	"testing"

	"github.com/entireio/entire-brain/factmerge"
)

func TestEstimateTokensMonotonic(t *testing.T) {
	few := []factmerge.Record{{Text: "short", Paths: []string{"a.b.c"}}}
	more := append([]factmerge.Record(nil), few...)
	more = append(more, factmerge.Record{Text: "a considerably longer fact statement here", Paths: []string{"a.b.c"}})
	if estimateTokens(more) <= estimateTokens(few) {
		t.Fatalf("more facts should estimate more tokens")
	}
	if estimateTokens(nil) != 0 {
		t.Fatalf("empty should be 0 tokens")
	}
}
