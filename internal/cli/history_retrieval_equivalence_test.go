package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

// TestIncrementalHistoryMatchesFullRebuild protects the optimization boundary
// between the short-term overlay and the consolidated history projection. The
// overlay is allowed to change how records are found, but its effective truth
// and the user-visible conversation order/provenance must be the same as a
// subsequent full rebuild over the identical transcripts.
func TestIncrementalHistoryMatchesFullRebuild(t *testing.T) {
	brainDir, _, _ := shortTermFixture(t)
	buildShortTerm(t, brainDir)

	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := loadFreshHistory(brainDir, manifest.Sources.History)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh.overlay) == 0 {
		t.Fatal("fixture did not produce an incremental overlay")
	}
	incrementalRecords := canonicalHistoryRecords(t, fresh.reconciledRecords())
	incrementalResults := conversationRetrievalContract(t, brainDir)

	if _, err := writeBrainHistoryIndexAndSource(
		brainDir,
		time.Date(2026, 8, 7, 14, 0, 0, 0, time.UTC),
		nil,
	); err != nil {
		t.Fatal(err)
	}
	manifest, err = loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	consolidated, err := loadBrainHistoryIndex(brainDir, manifest.Sources.History)
	if err != nil {
		t.Fatal(err)
	}
	fullRecords := canonicalHistoryRecords(t, consolidated.Records)
	if !reflect.DeepEqual(incrementalRecords, fullRecords) {
		t.Fatalf("incremental truth differs from full rebuild\nincremental: %v\nfull: %v", incrementalRecords, fullRecords)
	}

	fullResults := conversationRetrievalContract(t, brainDir)
	if !reflect.DeepEqual(incrementalResults, fullResults) {
		t.Fatalf("conversation order or provenance changed after consolidation\nincremental: %#v\nfull: %#v", incrementalResults, fullResults)
	}
}

type conversationRetrievalObservation struct {
	Query     string
	ID        string
	Path      string
	Line      int
	EndLine   int
	Branch    string
	SessionID string
	Agent     string
	CreatedAt string
	Text      string
	Matched   []string
}

func conversationRetrievalContract(t testing.TB, brainDir string) []conversationRetrievalObservation {
	t.Helper()
	queries := []string{
		"flaky lock test windows",
		"corrupted vector store checksum",
		"profile exporter hot loop gzip",
	}
	var observed []conversationRetrievalObservation
	for _, query := range queries {
		results, err := retrieveConversation(brainDir, query, 10, modeLexical, retrievalOptions{})
		if err != nil {
			t.Fatalf("retrieve %q: %v", query, err)
		}
		if len(results) == 0 {
			t.Fatalf("retrieve %q returned no results", query)
		}
		for _, result := range results {
			observed = append(observed, conversationRetrievalObservation{
				Query: query, ID: result.ID, Path: result.Path, Line: result.Line,
				EndLine: result.EndLine, Branch: result.Branch, SessionID: result.SessionID,
				Agent: result.Agent, CreatedAt: result.CreatedAt, Text: result.Text,
				Matched: append([]string(nil), result.MatchedTerms...),
			})
		}
	}
	return observed
}

func canonicalHistoryRecords(t testing.TB, records []historyRecord) []string {
	t.Helper()
	canonical := make([]string, 0, len(records))
	for _, record := range records {
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		canonical = append(canonical, string(encoded))
	}
	sort.Strings(canonical)
	return canonical
}

// TestRawHistoryFallbackRetainsFallbackOnlyMatch names the safety property for
// future negative-result optimizations: an indexed miss is not proof of
// absence while raw exported history can still contain a valid match.
//
// Mutation proof: overlay agent_surface.go and replace
//
//	rawMatches, rawErr := rawHistoryMatcher(status.Brain.Path, task, nil, briefOpts.limit, rawProfile)
//
// with `_ = rawProfile` followed by
// `rawMatches, rawErr := []brainTextMatch(nil), error(nil)`. This test fails
// with "end-to-end brief lost fallback-only match"; the production file is
// never modified.
func TestRawHistoryFallbackRetainsFallbackOnlyMatch(t *testing.T) {
	const marker = "FALLBACK_ONLY_HISTORY_MARKER"
	fixture := newBrainBriefRawHistoryEndToEndFixture(t)
	storage, err := repoStoragePaths(context.Background(), fixture.opts.Runner, fixture.opts.Env, fixture.opts.Env.RepoRoot)
	if err != nil {
		t.Fatal(err)
	}
	brainDir := storage.BrainDir
	generatedAt := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	index := historyIndex{GeneratedAt: generatedAt, Records: []historyRecord{{
		ID: "indexed-unrelated", Kind: "decision", Path: "sessions/main/old.jsonl",
		Line: 1, Summary: "an unrelated indexed decision",
	}}}
	if got := rankHistoryRecordsScored(index, "history", marker, 8, 0); len(got) != 0 {
		t.Fatalf("fixture marker unexpectedly appeared in the index: %+v", got)
	}
	source := &historySourceManifest{
		GeneratedAt: generatedAt,
		IndexPath:   historyIndexPath,
		Records:     len(index.Records),
	}
	if err := publishHistoryComparisonIndex(brainDir, index, source); err != nil {
		t.Fatal(err)
	}

	// Add the marker only after the unrelated nonempty index was persisted.
	// The real brief orchestration must execute the raw-history fallback.
	fallbackPath := filepath.Join(brainDir, "notes", "fallback.md")
	if err := os.MkdirAll(filepath.Dir(fallbackPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fallbackPath, []byte("decision evidence "+marker+" remains available\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.task = marker
	var out bytes.Buffer
	if err := runBrainBriefRawHistoryEndToEndInto(&out, fixture, brainBriefRawHistoryMatchesObserved); err != nil {
		t.Fatal(err)
	}
	var report brainBriefJSONReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode brief: %v\n%s", err, out.String())
	}
	if !report.Status.Sources.History {
		t.Fatal("fixture did not expose the persisted history source")
	}
	if len(report.History.Matches) != 1 || report.History.Matches[0].Path != "notes/fallback.md" {
		t.Fatalf("end-to-end brief lost fallback-only match: %+v; warnings=%v; packet=%s", report.History.Matches, report.Warnings, out.String())
	}
}

var historyRetrievalEquivalenceSink []scoredHistoryRecord

func TestHistoryRetrievalEquivalentPaths(t *testing.T) {
	for _, size := range []int{12, 6000} {
		t.Run(fmt.Sprintf("records_%d", size), func(t *testing.T) {
			assertHistoryRetrievalEquivalentPaths(t, size)
		})
	}
}

func assertHistoryRetrievalEquivalentPaths(tb testing.TB, size int) (string, *historySourceManifest) {
	tb.Helper()
	brainDir, source := writeDirectHistoryFTSFixture(tb, historyRetrievalEquivalenceIndex(size))
	loaded, err := loadBrainHistoryIndex(brainDir, source)
	if err != nil {
		tb.Fatal(err)
	}
	cases := []struct {
		name         string
		kind         string
		query        string
		limit        int
		wantNonEmpty bool
	}{
		{name: "positive", kind: "history", query: "retrieval cache invalidation source provenance", limit: 10, wantNonEmpty: true},
		{name: "honest_empty", kind: "history", query: "xylophone zebra quokka", limit: 10},
		{name: "discriminating_top_k", kind: "decisions", query: "component_7 source provenance", limit: 3, wantNonEmpty: true},
		{name: "kind_filtered_empty", kind: "tool-paths", query: "retrieval cache invalidation source provenance", limit: 10},
	}
	for _, tc := range cases {
		want, ok := rankHistoryViaFTS(brainDir, loaded, tc.kind, tc.query, tc.limit)
		if !ok {
			tb.Fatalf("%s JSON-backed preflight unavailable", tc.name)
		}
		if tc.wantNonEmpty && len(want) == 0 {
			tb.Fatalf("%s JSON-backed preflight returned no results", tc.name)
		}
		if !tc.wantNonEmpty && len(want) != 0 {
			tb.Fatalf("%s must have no matches, got %d", tc.name, len(want))
		}
		if len(want) > tc.limit {
			tb.Fatalf("%s exceeded result limit: got %d, limit %d", tc.name, len(want), tc.limit)
		}
		got, used, rankErr := rankHistoryViaFreshFTS(brainDir, source, tc.kind, tc.query, tc.limit)
		if rankErr != nil || !used {
			tb.Fatalf("%s direct preflight: used=%v err=%v", tc.name, used, rankErr)
		}
		if !reflect.DeepEqual(got, want) {
			tb.Fatalf("%s direct payload differs from JSON-backed ranking\ndirect: %+v\nJSON: %+v", tc.name, got, want)
		}
	}
	return brainDir, source
}

func historyRetrievalEquivalenceIndex(size int) historyIndex {
	index := historyIndex{GeneratedAt: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)}
	for i := 0; i < size; i++ {
		index.Records = append(index.Records, historyRecord{
			ID: fmt.Sprintf("history:%08d", i), Kind: "decision", Branch: "main",
			Path:    fmt.Sprintf("sessions/main/20260807T%06dZ_%04d.jsonl", i%240000, i/20),
			Line:    i%20 + 1,
			Summary: fmt.Sprintf("Decision %d validates retrieval cache invalidation and source provenance for component_%d.", i, i%113),
			Terms:   []string{"retrieval_cache", "source_provenance", fmt.Sprintf("component_%d", i%113)},
		})
	}
	return index
}

// BenchmarkHistoryRetrievalEquivalentPaths compares the persisted direct
// payload accelerator with the JSON-load path. Correctness is checked before
// the timer so a faster but behaviorally different path cannot produce a
// publishable benchmark result.
func BenchmarkHistoryRetrievalEquivalentPaths(b *testing.B) {
	brainDir, source := assertHistoryRetrievalEquivalentPaths(b, 6000)
	const query = "retrieval cache invalidation source provenance"

	b.Run("json_load_then_rank", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			loaded, loadErr := loadBrainHistoryIndex(brainDir, source)
			if loadErr != nil {
				b.Fatal(loadErr)
			}
			results, ranked := rankHistoryViaFTS(brainDir, loaded, "history", query, 10)
			if !ranked {
				b.Fatal("JSON-backed FTS unavailable")
			}
			historyRetrievalEquivalenceSink = results
		}
	})
	b.Run("direct_payload", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			results, direct, rankErr := rankHistoryViaFreshFTS(brainDir, source, "history", query, 10)
			if rankErr != nil || !direct {
				b.Fatalf("direct FTS: used=%v err=%v", direct, rankErr)
			}
			historyRetrievalEquivalenceSink = results
		}
	})
}
