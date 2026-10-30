package stats

import (
	"bytes"
	"os"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
)

func TestRenderBrainStatsHumanOutputIsCompleteAndSorted(t *testing.T) {
	report := Report{
		RepoKey: "gh/example/repo",
		Sessions: Sessions{
			Count: 3, Oldest: "2024-01-01T00:00:00Z", Latest: "2024-03-01T00:00:00Z",
			ByBranch: map[string]int{"zeta": 1, "alpha": 2}, ByAgent: map[string]int{"codex": 2, "claude": 1},
		},
		History: History{Records: 7, ScanCacheVersion: 4, FTSSchema: "v2", ByKind: map[string]int{"session": 3, "message": 4}},
		Conversation: &Conversation{
			Exchanges: 5, IncompleteExchanges: 1, RangeIncomplete: 2, IdentityDegraded: 1, ProjectionTruncated: 1,
			OldestCreatedAt: "2024-01-01T00:00:00Z", LatestCreatedAt: "2024-03-01T00:00:00Z",
			ByBranch: map[string]int{"zeta": 1, "alpha": 4}, ByAgent: map[string]int{"codex": 3, "claude": 2},
			VectorState: "current", VectorModelID: "model-a", Vectors: 5,
		},
		ShortTerm: &ShortTerm{Files: 2, Records: 6, Exchanges: 4, GeneratedAt: "2024-03-02T00:00:00Z", State: "partial", Truncated: true, FailedFiles: []string{"a", "b"}},
	}
	var out bytes.Buffer
	renderReport(&out, report)
	want := "repo: gh/example/repo\n" +
		"sessions: 3 (2024-01-01T00:00:00Z .. 2024-03-01T00:00:00Z)\n" +
		"  by branch: alpha=2 zeta=1\n" + "  by agent: claude=1 codex=2\n" +
		"history: 7 records (scan cache v4, fts schema v2)\n" + "  by kind: message=4 session=3\n" +
		"conversation: 5 exchanges (1 incomplete, 2 range-incomplete, 1 degraded-identity, 1 truncated-projection)\n" +
		"  sessions span 2024-01-01T00:00:00Z .. 2024-03-01T00:00:00Z\n" +
		"  by branch: alpha=4 zeta=1\n" + "  by agent: claude=2 codex=3\n" + "  vectors: current (5, model-a)\n" +
		"short-term memory: 6 records (4 exchanges) from 2 changed transcripts (built 2024-03-02T00:00:00Z); state partial; buffer full, consolidate with `" + base.BrainInvocation(os.LookupEnv, "refresh") + "`; 2 transcripts failed to scan\n"
	if out.String() != want {
		t.Fatalf("stats output mismatch:\n got:\n%s\nwant:\n%s", out.String(), want)
	}
}
