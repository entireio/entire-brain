package cli

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestParseExpansion(t *testing.T) {
	if got := parseExpansion("# notes\nbrain path repo key plugin data dir storage\n"); got != "brain path repo key plugin data dir storage" {
		t.Fatalf("got %q", got)
	}
	if got := parseExpansion("\n\n  one   two  three \n"); got != "one two three" {
		t.Fatalf("whitespace collapse failed: %q", got)
	}
	if parseExpansion("") != "" {
		t.Fatalf("empty should be empty")
	}
}

func TestExpandedQuery(t *testing.T) {
	if got := expandedQuery("add path command", "brain repo key storage"); got != "add path command brain repo key storage" {
		t.Fatalf("got %q", got)
	}
	if got := expandedQuery("add path command", "  "); got != "add path command" {
		t.Fatalf("empty expansion should leave query unchanged, got %q", got)
	}
}

func TestExpandQueryCaches(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "exp.json")
	cache := loadExpansionCache(cachePath)
	calls := 0
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		calls++
		return "term one two three\n", nil
	}
	got, err := expandQuery(context.Background(), run, []string{"fake"}, "/repo", "a query", cache)
	if err != nil || got != "term one two three" {
		t.Fatalf("expandQuery: %q err=%v", got, err)
	}
	// Second call for the same query is served from cache (no agent call).
	if _, err := expandQuery(context.Background(), run, []string{"fake"}, "/repo", "a query", cache); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 agent call, cache should serve the rest, got %d", calls)
	}
}

func TestRunFactsEvalWithExpander(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	p := normalizeFactPaths([]string{"architecture.data.flow"})
	// Fact text shares no words with the task, but does with the expansion.
	f := factRecord{ID: factRecordID("the mirror ref reconciliation", p), Paths: p, Text: "the mirror ref reconciliation", Branch: "main", Status: factStatusActive, UpdatedAt: now}
	if err := writeFacts(brainDir, "main", []factRecord{f}); err != nil {
		t.Fatal(err)
	}
	tasks := []evalTask{{ID: "t1", Task: "fix the sync bug", Branch: "main", Relevant: []string{f.ID}}}

	// Without expansion the query "fix the sync bug" doesn't match the fact.
	base, _, err := runFactsEval(context.Background(), Options{}, brainDir, "/repo", "main", tasks, 10, false, nil, nil, loadJudgeCache(""), nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if base[0].RelevantSurfaced != 0 {
		t.Fatalf("base query should not surface the fact, got %d", base[0].RelevantSurfaced)
	}

	// An expander that adds the fact's vocabulary lets recall find it.
	expander := func(query string) (string, error) { return "mirror ref reconciliation", nil }
	exp, _, err := runFactsEval(context.Background(), Options{}, brainDir, "/repo", "main", tasks, 10, false, nil, nil, loadJudgeCache(""), expander, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if exp[0].RelevantSurfaced != 1 {
		t.Fatalf("expansion should surface the fact, got %d", exp[0].RelevantSurfaced)
	}
}
