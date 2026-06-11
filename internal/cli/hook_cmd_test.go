package cli

import (
	"strings"
	"testing"
	"time"
)

func hookTestFacts(now time.Time) []factRecord {
	return []factRecord{
		{ID: "f:neg", Kind: factKindClosedNegative, Paths: []string{"architecture.data.flow"},
			Text: "Relaxing the history term-coverage gate to 2-of-N was tried and failed: it admitted filler-word noise; BM25 is the fix.", Status: factStatusActive, UpdatedAt: now},
		{ID: "f:gotcha", Kind: factKindGotcha, Paths: []string{"constraints.invariants.general"},
			Text: "The distill cache fails silently when the facts directory is read-only; flushes retry on the next interval.", Status: factStatusActive, UpdatedAt: now},
		{ID: "f:conv", Kind: factKindConvention, Paths: []string{"workflow.testing.rules"},
			Text: "The history term-coverage gate is reviewed during retrieval syncs.", Status: factStatusActive, UpdatedAt: now},
		{ID: "f:file", Kind: factKindDecision, Paths: []string{"architecture.boundaries.rationale"},
			Text: "`history_fts.go` keeps the BM25 relevance cutoff relative to the top score, not absolute.", Status: factStatusActive, UpdatedAt: now},
	}
}

func TestHookMatchFailureKindsAndThreshold(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	facts := hookTestFacts(now)

	// A failure mentioning the coverage gate matches the closed-negative but
	// must NOT surface the equally-matching convention (wrong kind for a
	// failure moment).
	hits := hookMatchFailure(facts, "test failed: history term-coverage gate admitted weak matches")
	if len(hits) == 0 || hits[0].ID != "f:neg" {
		t.Fatalf("expected the closed-negative to match the failure, got %+v", hits)
	}
	for _, h := range hits {
		if h.ID == "f:conv" {
			t.Fatalf("convention kind must not surface on a failure: %+v", hits)
		}
	}

	// An unrelated failure stays silent — the precision bar holds.
	if hits := hookMatchFailure(facts, "compile error: undefined variable in renderer"); len(hits) != 0 {
		t.Fatalf("unrelated failure should match nothing, got %+v", hits)
	}
}

func TestHookPreEditLocusMatch(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	facts := hookTestFacts(now)
	// The pre-edit path: file + stem against fact locus (backticked file name).
	hits := factsRelevantToChange([]string{"internal/cli/history_fts.go", "history_fts"}, nil, facts, 8)
	if len(hits) != 1 || hits[0].ID != "f:file" {
		t.Fatalf("expected the file-anchored fact for history_fts.go, got %+v", hits)
	}
	if hits := factsRelevantToChange([]string{"internal/cli/seed.go", "seed"}, nil, facts, 8); len(hits) != 0 {
		t.Fatalf("unrelated file should surface nothing, got %+v", hits)
	}
}

func TestHookEmitSilentAndBudget(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	cmd := newHookCommand(Options{})

	// Empty hits: complete silence (the contract; even a "no facts" line is noise).
	out, err := execute(t, cmd, "post-failure")
	if err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("empty stdin should be silent, got %q (err %v)", out, err)
	}

	// Budget caps a multi-fact emission but always emits at least one.
	long := strings.Repeat("a very long fact sentence ", 20)
	hits := []factRecord{
		{ID: "1", Kind: factKindGotcha, Text: long, Status: factStatusActive, UpdatedAt: now},
		{ID: "2", Kind: factKindGotcha, Text: long, Status: factStatusActive, UpdatedAt: now},
		{ID: "3", Kind: factKindGotcha, Text: long, Status: factStatusActive, UpdatedAt: now},
	}
	c := newHookPostFailureCommand(Options{})
	var sb strings.Builder
	c.SetOut(&sb)
	if err := hookEmit(c, hits, 150, false); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(sb.String()), "\n")
	if len(lines) >= 3 {
		t.Fatalf("budget 150 should cap the emission below 3 long facts, got %d lines", len(lines))
	}
	if len(lines) < 1 || !strings.HasPrefix(lines[0], "[gotcha]") {
		t.Fatalf("at least one fact must be emitted with a kind prefix, got %q", sb.String())
	}
}

// TestHookNeverFailsOutsideARepo locks in the harness-safety contract: in a
// directory with no repo and no brain, both hooks exit 0 with no output —
// a hook must never break the harness it is wired into.
func TestHookNeverFailsOutsideARepo(t *testing.T) {
	opts := Options{Env: EntireEnv{RepoRoot: t.TempDir()}, Runner: ExecRunner{}, Now: time.Now}
	out, err := execute(t, newHookCommand(opts), "pre-edit", "--file", "x.go")
	if err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("pre-edit outside a repo must be silent success, got %q (err %v)", out, err)
	}
	c := newHookCommand(opts)
	c.SetIn(strings.NewReader("some failure text"))
	out, err = execute(t, c, "post-failure", "--command", "go test")
	if err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("post-failure outside a repo must be silent success, got %q (err %v)", out, err)
	}

	// Flag misuse still errors (hand-testing stays debuggable).
	if _, err := execute(t, newHookCommand(opts), "pre-edit"); err == nil {
		t.Fatal("pre-edit without --file should error")
	}
}
