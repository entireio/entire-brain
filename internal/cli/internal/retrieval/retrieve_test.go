package retrieval

import (
	"testing"

	"github.com/entireio/entire-brain/internal/cli/internal/historystore"
)

func TestFilterHistoryRetrievalSelfEchoesRefillsWithEvidence(t *testing.T) {
	scored := []ScoredHistoryRecord{
		{Record: historystore.Record{Kind: "tool_call", Summary: `entire brain search '.github symbol was unexpectedly ignored' --json`}},
		{Record: historystore.Record{Kind: "code_fact", Summary: `t.Fatalf(".github symbol was unexpectedly ignored")`}},
		{Record: historystore.Record{Kind: "tool_call", Summary: `go test ./internal/cli -run TestSemantic`}},
	}
	got := filterHistoryRetrievalSelfEchoes(scored, ".github symbol was unexpectedly ignored")
	if len(got) != 2 || got[0].Record.Kind != "code_fact" || got[1].Record.Kind != "tool_call" {
		t.Fatalf("filtered history = %+v", got)
	}
}
