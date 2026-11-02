package cli

// Caveat behavior for the retrieval surfaces; what remains of the former
// retrieval_guard_test.go after the privacy-layer and fact-review removals.

import (
	"fmt"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/internal/retrieval"
)

func TestCurrentCodeUnavailableCaveatsOnlyMemoryFacts(t *testing.T) {
	got := retrieval.AnnotateCurrentCodeUnavailable([]unifiedResult{
		{Source: "fact", ID: "fact:a"},
		{Source: "history", ID: "history:a"},
		{Source: "doc", ID: "doc:a"},
	})
	if !got[0].VerificationRequired || !hasRetrievalCaveat(got[0], retrieval.CaveatCurrentCodeUnavailable) {
		t.Fatalf("fact result was not caveated: %+v", got[0])
	}
	for i := 1; i < len(got); i++ {
		if got[i].VerificationRequired || len(got[i].Caveats) != 0 {
			t.Fatalf("non-fact result %d was incorrectly caveated: %+v", i, got[i])
		}
	}
}

func hasRetrievalCaveat(result unifiedResult, kind string) bool {
	for _, caveat := range result.Caveats {
		if caveat.Kind == kind {
			return true
		}
	}
	return false
}

func TestGetBatchCapsRequestSize(t *testing.T) {
	ids := make([]string, maxGetBatchIDs+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("doc:%03d", i)
	}
	if _, _, err := getUnifiedBatch("", t.TempDir(), "main", ids); err == nil {
		t.Fatal("oversized id batch must be rejected")
	}
	if _, _, err := getUnifiedBatch("", t.TempDir(), "main", ids[:maxGetBatchIDs]); err != nil {
		t.Fatalf("at-cap batch must be accepted: %v", err)
	}
}
