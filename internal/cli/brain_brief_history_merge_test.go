package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"github.com/spf13/cobra"
)

const briefHistoryMergeTask = "ValidateToken public history provenance ordering budget"

type briefHistoryJSON struct {
	History struct {
		Matches []brainTextMatch `json:"matches"`
	} `json:"history"`
}

func runBrainBriefHistoryMerge(t *testing.T, opts Options, limit int, matcher brainBriefRawHistoryMatcher) briefHistoryJSON {
	t.Helper()
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	briefOpts := brainBriefOptions{json: true, limit: limit, noSemantic: true}
	if err := runBrainBriefWithRawHistoryMatcher(context.Background(), cmd, opts, briefOpts, briefHistoryMergeTask, matcher); err != nil {
		t.Fatalf("brain brief: %v", err)
	}
	var report briefHistoryJSON
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("parse brief JSON: %v", err)
	}
	return report
}

func TestBrainBriefIndexedHistoryRanksBeforeRawTopUp(t *testing.T) {
	fixture := newBrainBriefOutputFixture(t)
	const limit = 50
	rawMatch := brainTextMatch{Path: "raw/fallback.jsonl", Line: 1, Excerpt: "raw ValidateToken fallback"}
	var gotExisting []brainTextMatch
	gotLimit := -1
	calls := 0
	matcher := func(brainDir, task string, existing []brainTextMatch, matcherLimit int, profile *brainBriefProfileRawHistory) ([]brainTextMatch, error) {
		calls++
		gotExisting = existing
		gotLimit = matcherLimit
		return []brainTextMatch{rawMatch}, nil
	}

	report := runBrainBriefHistoryMerge(t, fixture.opts, limit, matcher)

	matches := report.History.Matches
	if len(matches) < 2 {
		t.Fatalf("want indexed matches plus the raw top-up, got %d matches: %+v", len(matches), matches)
	}
	if calls != 1 {
		t.Fatalf("raw matcher calls = %d, want 1", calls)
	}
	indexed := len(matches) - 1
	if len(gotExisting) != indexed {
		t.Errorf("raw matcher existing = %d matches, want the %d indexed matches", len(gotExisting), indexed)
	}
	if want := limit - indexed; gotLimit != want {
		t.Errorf("raw matcher limit = %d, want remaining slots %d", gotLimit, want)
	}
	if last := matches[len(matches)-1]; last.Path != rawMatch.Path {
		t.Errorf("last match path = %q, want raw top-up %q (indexed results must fill first)", last.Path, rawMatch.Path)
	}
	for _, match := range matches[:indexed] {
		if match.Path == rawMatch.Path {
			t.Errorf("raw match ranked above an indexed match: %+v", matches)
		}
	}
}

func TestBrainBriefSkipsRawScanWhenIndexFillsLimit(t *testing.T) {
	fixture := newBrainBriefOutputFixture(t)
	calls := 0
	matcher := func(brainDir, task string, existing []brainTextMatch, matcherLimit int, profile *brainBriefProfileRawHistory) ([]brainTextMatch, error) {
		calls++
		return []brainTextMatch{{Path: "raw/fallback.jsonl", Line: 1, Excerpt: "raw ValidateToken fallback"}}, nil
	}

	report := runBrainBriefHistoryMerge(t, fixture.opts, 1, matcher)

	if got := len(report.History.Matches); got != 1 {
		t.Fatalf("want exactly 1 history match at limit 1, got %d", got)
	}
	if calls != 0 {
		t.Errorf("raw matcher calls = %d, want 0 when indexed history fills the limit", calls)
	}
	if path := report.History.Matches[0].Path; !strings.HasPrefix(path, "sessions/") {
		t.Errorf("match path = %q, want an indexed sessions match", path)
	}
}

func TestBrainBriefDuplicateIndexedKeysStillTopUpFromRaw(t *testing.T) {
	fixture := newBrainBriefOutputFixture(t)
	manifest, err := bmanifest.Load(fixture.brainDir)
	if err != nil {
		t.Fatal(err)
	}
	const rel = "sessions/main/overlay-dup.jsonl"
	const summary = "DUPKEY_CANARY identical ranked summary"
	overlay := shortTermIndex{
		ReconcilerVersion: historyShortTermReconcilerVersion,
		BaseGeneratedAt:   manifest.Sources.History.GeneratedAt,
		Files: map[string]shortTermFile{rel: {Records: []historyRecord{
			{ID: "dup-decision", Kind: "decision", Branch: "main", Path: rel, Line: 7,
				Summary: summary, Terms: []string{"dupkey", "canary"}},
			{ID: "dup-learning", Kind: "learning", Branch: "main", Path: rel, Line: 7,
				Summary: summary, Terms: []string{"dupkey", "canary"}},
		}}},
	}
	if err := saveHistoryShortTerm(fixture.brainDir, overlay); err != nil {
		t.Fatal(err)
	}
	rawMatch := brainTextMatch{Path: "raw/fallback.jsonl", Line: 1, Excerpt: "raw DUPKEY_CANARY fallback"}
	calls := 0
	matcher := func(brainDir, task string, existing []brainTextMatch, matcherLimit int, profile *brainBriefProfileRawHistory) ([]brainTextMatch, error) {
		calls++
		return []brainTextMatch{rawMatch}, nil
	}

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	briefOpts := brainBriefOptions{json: true, limit: 2, noSemantic: true}
	if err := runBrainBriefWithRawHistoryMatcher(context.Background(), cmd, fixture.opts, briefOpts, "DUPKEY_CANARY", matcher); err != nil {
		t.Fatalf("brain brief: %v", err)
	}
	var report briefHistoryJSON
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("parse brief JSON: %v", err)
	}

	matches := report.History.Matches
	if len(matches) != 2 {
		t.Fatalf("want 2 history matches (deduped indexed + raw top-up), got %d: %+v", len(matches), matches)
	}
	if calls != 1 {
		t.Errorf("raw matcher calls = %d, want 1: duplicate indexed keys must not consume the budget", calls)
	}
	if matches[0].Path != rel || matches[1].Path != rawMatch.Path {
		t.Errorf("want deduped indexed match then raw top-up, got %+v", matches)
	}
}

func TestBrainRawScanSkipsDerivedIndexes(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"docs/index.json", true},
		{"docs/index-fts.sqlite", true},
		{"entities/index.json", true},
		{"sessions/main/20260716T010203Z_0001.jsonl", false},
		{"README.md", false},
	}
	for _, tc := range cases {
		if got := brainRawScanSkipsDerived(tc.path); got != tc.want {
			t.Errorf("brainRawScanSkipsDerived(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}
