package hook

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
)

// TestStemGateDropsOrdinaryWordsAndKeepsDistinctiveOnes pins the gate to the
// measured populations rather than to its own arithmetic: every stem below is
// one whose hits were actually graded on this store.
func TestStemGateDropsOrdinaryWordsAndKeepsDistinctiveOnes(t *testing.T) {
	t.Parallel()

	// A corpus the size of the one this gate was measured on (1,961 active
	// memories on `main`): "query" in 15 of 2,000 (0.75%) and "distill_cmd" in
	// 1 (0.05%), against a 0.50% gate. Corpus size matters to the fixture, not
	// just to the arithmetic -- a 200-memory store cannot distinguish them,
	// because one mention is already 0.5% of it. Neither count sits on the
	// threshold, so a fixture does not have to be re-tuned to keep passing.
	facts := make([]factmerge.Record, 0, 2000)
	for i := 0; i < 15; i++ {
		facts = append(facts, factmerge.Record{Text: "the query path ranks decisions first"})
	}
	facts = append(facts, factmerge.Record{Text: "distill_cmd caps transcripts at 256 MiB"})
	for len(facts) < 2000 {
		facts = append(facts, factmerge.Record{Text: "unrelated memory about release tagging"})
	}

	if hookStemIsDistinctive("query", facts) {
		t.Error(`"query" appears in 0.75% of memories: retrieving on it returns memories about the word, not about query.go`)
	}
	if !hookStemIsDistinctive("distill_cmd", facts) {
		t.Error(`"distill_cmd" appears in 0.05% of memories and is exactly the case the stem term was added for`)
	}
	if !hookStemIsDistinctive("graphdiff", facts) {
		t.Error("a stem no memory mentions must not be dropped: including it changes no result, and one measured rule beats two")
	}
	if hookStemIsDistinctive("", facts) {
		t.Error("an extensionless dotfile has no stem to query for")
	}
	if !hookStemIsDistinctive("query", nil) {
		t.Error("with no memories to measure against the gate must not fire")
	}
}

// TestStemGateCountsLocusNotJustText: locus is where the relevance engine
// looks for code identifiers, so a stem named only there is still a mention.
func TestStemGateCountsLocusNotJustText(t *testing.T) {
	t.Parallel()

	facts := make([]factmerge.Record, 0, 1000)
	for i := 0; i < 10; i++ {
		facts = append(facts, factmerge.Record{Text: "unrelated", Locus: []string{"internal/cli/state.go"}})
	}
	for len(facts) < 1000 {
		facts = append(facts, factmerge.Record{Text: "unrelated"})
	}
	if hookStemIsDistinctive("state", facts) {
		t.Error("10 of 1,000 memories carry the stem in locus only; counting text alone would have called it rare")
	}
}

// TestContainsWholeTokenRespectsIdentifierBoundaries is the half of the gate
// that decides WHAT counts as a mention. Substring matching is what made the
// stem term fuzzy in the first place.
func TestContainsWholeTokenRespectsIdentifierBoundaries(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		text, token string
		want        bool
		why         string
	}{
		{"the query path", "query", true, "a bare word is a mention"},
		{"see query.go for this", "query", true, `"." is not an identifier byte, so a filename is a mention`},
		{"queryFacts ranks rows", "query", false, "a longer identifier that merely contains the stem is not a mention"},
		{"a subquery is rewritten", "query", false, "nor is a suffix match"},
		{"query_cmd owns this", "query", false, `"_" is an identifier byte, so this names a different symbol`},
		{"query", "query", true, "the whole text may be the token"},
		{"ends with query", "query", true, "a token at the end of the text is a mention"},
		{"no match here", "query", false, "absent is absent"},
		{"anything", "", false, "an empty token matches nothing"},
	} {
		if got := containsWholeToken(tc.text, tc.token); got != tc.want {
			t.Errorf("containsWholeToken(%q, %q) = %v, want %v — %s", tc.text, tc.token, got, tc.want, tc.why)
		}
	}
}

// TestPreEditDropsAnOrdinaryStemFromTheQueryItRuns is the end-to-end half: it
// runs the real `pre-edit` command and inspects the TERMS the relevance engine
// was handed. The unit tests above can pass while the call site ignores the
// gate — that is where the original defect lived.
func TestPreEditDropsAnOrdinaryStemFromTheQueryItRuns(t *testing.T) {
	repoDir := t.TempDir()
	brainDir := t.TempDir()

	// "query" in 10 of 200 memories (5%, above the gate); "graphdiff" in none.
	facts := make([]factmerge.Record, 0, 200)
	for i := 0; i < 10; i++ {
		facts = append(facts, stemFixtureFact(i, "the query path ranks decisions first"))
	}
	for len(facts) < 200 {
		facts = append(facts, stemFixtureFact(len(facts), "unrelated memory about release tagging"))
	}
	if err := factstore.Write(brainDir, "main", facts); err != nil {
		t.Fatalf("write facts: %v", err)
	}

	for _, tc := range []struct {
		file      string
		wantTerms []string
	}{
		{"internal/entityindex/query.go", []string{"internal/entityindex/query.go"}},
		{"internal/entityindex/graphdiff.go", []string{"internal/entityindex/graphdiff.go", "graphdiff"}},
	} {
		t.Run(tc.file, func(t *testing.T) {
			var seen []string
			cfg := Config{
				Options: base.Options{},
				Deps: Deps{
					ResolveTarget: func(context.Context, base.Options, string) (string, string, string, error) {
						return repoDir, brainDir, "main", nil
					},
					FactsRelevantToChange: func(files []string, _ []factmerge.Record, _ int) []factmerge.Record {
						seen = append([]string(nil), files...)
						return nil
					},
					RecordServed: func(io.Writer, context.Context, base.Options, string, string, string, string, string, []factmerge.Record) {
					},
				},
			}
			cmd := newPreEditCommand(cfg)
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetIn(strings.NewReader(""))
			cmd.SetArgs([]string{"--file", tc.file})
			if err := cmd.ExecuteContext(context.Background()); err != nil {
				t.Fatalf("pre-edit: %v", err)
			}
			if strings.Join(seen, "\x00") != strings.Join(tc.wantTerms, "\x00") {
				t.Fatalf("the engine was asked for %q, want %q", seen, tc.wantTerms)
			}
		})
	}
}

func stemFixtureFact(i int, text string) factmerge.Record {
	return factmerge.Record{
		ID:     "fact:" + strings.Repeat("0", 3) + itoa(i),
		Paths:  []string{"architecture.retrieval.decision"},
		Text:   text,
		Branch: "main",
		Origin: "authored",
		Status: "active",
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}
