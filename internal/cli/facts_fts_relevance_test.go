package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

const factsBM25PublicOracleQuery = "How should an Entire-managed Cursor hook be migrated when reinstalling changes its wrapper between sh and cmd.exe?"

func loadFactsBM25PublicSnapshot(t testing.TB) []factRecord {
	t.Helper()
	repoRoot, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(repoRoot, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(repoRoot)
		if parent == repoRoot {
			t.Fatal("locate repository root for public facts snapshot")
		}
		repoRoot = parent
	}
	path := filepath.Join(repoRoot, "benchmarks", "agent-brain", "confirmatory", "offline-relevance-fact-snapshot.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot struct {
		Facts []factRecord `json:"facts"`
	}
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot.Facts
}

func rankedFactsBM25IDs(scores map[string]float64) []string {
	ids := make([]string, 0, len(scores))
	for id := range scores {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if scores[ids[i]] != scores[ids[j]] {
			return scores[ids[i]] > scores[ids[j]]
		}
		return ids[i] < ids[j]
	})
	return ids
}

func factsBM25Rank(scores map[string]float64, id string) int {
	for i, candidate := range rankedFactsBM25IDs(scores) {
		if candidate == id {
			return i + 1
		}
	}
	return 0
}

// The public oracle and manual judgments are committed benchmark inputs. The
// additional phrasings below are explicitly synthetic regressions, not new
// judgments: they preserve the oracle's hook-wrapper intent while exercising
// identifier punctuation and the committed fact's cmd.exe locus.
func TestFactsBM25PublicOracleKeepsSolvingFactAboveHardDistractor(t *testing.T) {
	facts := loadFactsBM25PublicSnapshot(t)
	const target = "fact:30c6c2e4a031d91df16e4d27"
	const hardDistractor = "fact:32a34f711bb107845dc407c3"
	for _, query := range []string{
		factsBM25PublicOracleQuery,
		"managed Cursor hook wrapper migration",
		"cmd.exe sh wrapper reinstall",
	} {
		scores, ok := factsFTSScores(facts, query)
		if !ok {
			t.Fatalf("BM25 unavailable for %q", query)
		}
		targetRank := factsBM25Rank(scores, target)
		distractorRank := factsBM25Rank(scores, hardDistractor)
		if targetRank != 1 {
			t.Fatalf("query %q: solving fact rank=%d, want 1", query, targetRank)
		}
		if distractorRank > 0 && distractorRank <= targetRank {
			t.Fatalf("query %q: hard distractor rank=%d must remain below solving rank=%d", query, distractorRank, targetRank)
		}
	}
}

// These queries are exact locus values from the committed public snapshot, so
// the owning fact is an intrinsic address target rather than a new manual
// relevance judgment. Together they cover punctuation, path-like identifiers,
// and the top-10 boundary that the previous text+taxonomy-only index missed.
func TestFactsBM25PublicLocusScenarios(t *testing.T) {
	facts := loadFactsBM25PublicSnapshot(t)
	for _, scenario := range []struct {
		query   string
		target  string
		maxRank int
	}{
		{query: "git/config", target: "fact:11af406c1bcd1c4994261a6c", maxRank: 2},
		{query: "branch/push", target: "fact:86eeae1918bb7aa4eca19306", maxRank: 1},
		{query: "user/repository", target: "fact:9c39a262ba60f7ed28023b46", maxRank: 1},
		{query: "/api/v1/me/recap", target: "fact:b492d36505d004ee3f331cc1", maxRank: 1},
		{query: "entire/sessions", target: "fact:d939db93c090d7ab800329a0", maxRank: 10},
	} {
		scores, ok := factsFTSScores(facts, scenario.query)
		if !ok {
			t.Fatalf("BM25 unavailable for %q", scenario.query)
		}
		if rank := factsBM25Rank(scores, scenario.target); rank == 0 || rank > scenario.maxRank {
			t.Fatalf("query %q: owning locus fact rank=%d, want 1..%d", scenario.query, rank, scenario.maxRank)
		}
	}
}

// Locus values are explicit addresses attached to a fact. BM25 used to omit
// them even though the default hand-rolled lexical arm gives them a strong
// boost, making identifier-only queries silently miss under the BM25 opt-in.
func TestFactsBM25IndexesLocusIdentifiers(t *testing.T) {
	facts := []factRecord{
		{ID: "fact:target", Text: "Literal URL fetching avoids poisoning the named remote configuration.", Paths: []string{"constraints.git.behavior"}, Locus: []string{"remote.origin.*", "git/config"}},
		{ID: "fact:distractor", Text: "A checkpoint remote can use a repository origin.", Paths: []string{"constraints.remote.branching"}},
	}
	for _, query := range []string{"remote.origin.*", "git/config"} {
		scores, ok := factsFTSScores(facts, query)
		if !ok {
			t.Fatalf("BM25 unavailable for %q", query)
		}
		if factsBM25Rank(scores, "fact:target") == 0 {
			t.Fatalf("identifier-only locus query %q missed target: %v", query, rankedFactsBM25IDs(scores))
		}
	}
	if scores, ok := factsFTSScores(facts, "xylophone zebra quokka"); !ok || len(scores) != 0 {
		t.Fatalf("corpus-closed null must remain empty: ok=%v scores=%v", ok, scores)
	}
}

func TestRankFactsFusedBM25CarriesLocusHitIntoAggregation(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_FACTS_BM25", "1")
	facts := []factRecord{
		{ID: "fact:target", Text: "Literal URL fetching avoids poisoning the named source configuration.", Paths: []string{"constraints.git.behavior"}, Locus: []string{"remote.origin.*"}, Status: factStatusActive},
		{ID: "fact:distractor", Text: "A checkpoint remote can use a repository origin.", Paths: []string{"constraints.remote.branching"}, Status: factStatusActive},
	}
	// An empty embedder exercises the production RRF aggregation without adding
	// a semantic signal: any result here must have earned the BM25 lexical arm.
	got := rankFactsFused(facts, "remote.origin.*", 10, false, newSemanticReranker(emptyEmbedder{}))
	for _, fact := range got {
		if fact.ID == "fact:target" {
			return
		}
	}
	t.Fatalf("BM25 locus hit was lost before aggregation: %v", got)
}
