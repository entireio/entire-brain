package cli

import (
	"bytes"
	"encoding/json"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	"github.com/entireio/entire-brain/internal/cli/internal/retrieval"
	"strings"
	"testing"
	"time"
)

func TestRetrievalRecencyPromotesCandidateBelowCutoff(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	brainDir := t.TempDir()
	facts := []factRecord{
		{ID: "fact:old", Text: "alpha beta retrieval", Status: factStatusActive, UpdatedAt: now.Add(-3 * 365 * 24 * time.Hour)},
		{ID: "fact:fresh", Text: "alpha retrieval", Status: factStatusActive, UpdatedAt: now},
	}
	if err := factstore.Write(brainDir, "main", facts); err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{false, true} {
		opts := retrievalOptions{Source: brainstore.RetrievalSourceFact, Recency: enabled, Now: now}
		full, err := retrieveUnifiedWithOptions("", brainDir, "main", "alpha beta", 2, retrieval.ModeLexical, opts)
		if err != nil {
			t.Fatal(err)
		}
		want := "fact:old"
		if enabled {
			want = "fact:fresh"
		}
		if len(full) != 2 || full[0].ID != want {
			t.Fatalf("recency=%v: unexpected full ranking: %+v", enabled, full)
		}
		page, err := retrieveUnifiedWithOptions("", brainDir, "main", "alpha beta", 1, retrieval.ModeLexical, opts)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) != 1 || page[0].ID != want || page[0].Score != full[0].Score {
			t.Fatalf("recency=%v: limit=1 must return the top weighted candidate %s, got %+v", enabled, want, page)
		}
	}
}

// A dated candidate can lose its place to an undated result because weighting
// worked. The absence of dates on the final page does not mean it was unavailable.
func TestRetrievalRecencyDemotesDatedCandidateOutsidePage(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	brainDir := t.TempDir()
	facts := []factRecord{
		{ID: "fact:old", Text: "alpha beta retrieval", Status: factStatusActive, UpdatedAt: now.Add(-3 * 365 * 24 * time.Hour)},
		{ID: "fact:undated", Text: "alpha retrieval", Status: factStatusActive},
	}
	if err := factstore.Write(brainDir, "main", facts); err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{false, true} {
		results, err := retrieveUnifiedWithOptions("", brainDir, "main", "alpha beta", 1, retrieval.ModeLexical, retrievalOptions{Source: brainstore.RetrievalSourceFact, Recency: enabled, Now: now})
		if err != nil {
			t.Fatal(err)
		}
		want := "fact:old"
		if enabled {
			want = "fact:undated"
		}
		if len(results) != 1 || results[0].ID != want {
			t.Fatalf("recency=%v results=%+v", enabled, results)
		}
		for _, caveat := range results[0].Caveats {
			if caveat.Kind == "recency_unavailable" {
				t.Fatal("weighting changed page membership; must not report unavailable")
			}
		}
	}
}

func TestRetrievalRecencyReportsUndatedResults(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		enabled    bool
		dated      bool
		empty      bool
		wantCaveat bool
	}{
		{name: "undated", enabled: true, wantCaveat: true},
		{name: "disabled"},
		{name: "mixed", enabled: true, dated: true},
		{name: "empty", enabled: true, empty: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			brainDir := t.TempDir()
			facts := []factRecord{
				{ID: "fact:a", Text: "alpha retrieval", Status: factStatusActive},
				{ID: "fact:b", Text: "alpha retrieval", Status: factStatusActive},
			}
			if tc.dated {
				facts[0].UpdatedAt = now
			}
			if tc.empty {
				facts = nil
			}
			if err := factstore.Write(brainDir, "main", facts); err != nil {
				t.Fatal(err)
			}
			results, err := retrieveUnifiedWithOptions("", brainDir, "main", "alpha", 10, retrieval.ModeLexical, retrievalOptions{Source: brainstore.RetrievalSourceFact, Recency: tc.enabled, Now: now})
			if err != nil {
				t.Fatal(err)
			}
			if len(results) != len(facts) {
				t.Fatalf("got %d results, want %d", len(results), len(facts))
			}
			for _, result := range results {
				found := false
				for _, caveat := range result.Caveats {
					if caveat.Kind == "recency_unavailable" {
						found = true
					}
				}
				if found != tc.wantCaveat {
					t.Fatalf("unexpected caveats: %+v", result.Caveats)
				}
			}
			if tc.wantCaveat {
				baseline, err := retrieveUnifiedWithOptions("", brainDir, "main", "alpha", 10, retrieval.ModeLexical, retrievalOptions{Source: brainstore.RetrievalSourceFact})
				if err != nil {
					t.Fatal(err)
				}
				for i := range results {
					if results[i].ID != baseline[i].ID || results[i].Score != baseline[i].Score {
						t.Fatal("undated scores/order changed")
					}
				}
				encoded, err := json.Marshal(retrieval.CompactResults(results, "alpha"))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Contains(encoded, []byte("recency_unavailable")) {
					t.Fatalf("JSON lost caveat: %s", encoded)
				}
				var out bytes.Buffer
				retrieval.PrintResultCaveats(&out, results[0])
				if !strings.Contains(out.String(), "none of the retrieved candidates has a usable timestamp") {
					t.Fatalf("text lost caveat: %s", out.String())
				}
			}
		})
	}
}
