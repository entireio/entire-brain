package dash

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"github.com/entireio/entire-brain/internal/cli/internal/retrieval"
	"github.com/entireio/entire-brain/internal/cli/internal/semstore"
)

func TestDashFactViewsMapsFieldsAndSource(t *testing.T) {
	facts := []factmerge.Record{
		{
			ID: "fact:aaa", Kind: "decision", Text: "Chose SQLite.", Paths: []string{"architecture.storage"},
			Status: "active", Origin: "distilled", RelatedIDs: []string{"fact:bbb"},
			Provenance: []factmerge.Anchor{{SessionID: "s1", Transcript: "sessions/x.jsonl", Line: 9, Verified: true}},
		},
		{ID: "fact:bbb", Kind: "gotcha", Text: "No transcript anchor here.", Status: "active"},
	}
	views := FactViews(facts, "/brain", 0)
	if len(views) != 2 {
		t.Fatalf("got %d views, want 2", len(views))
	}
	if views[0].Kind != "decision" || views[0].Text != "Chose SQLite." || len(views[0].Provenance) != 1 {
		t.Errorf("fact[0] mapped wrong: %+v", views[0])
	}
	if views[0].Source != filepath.Join("/brain", "sessions/x.jsonl") || views[0].SourceLine != 9 {
		t.Errorf("fact[0] source = %q:%d", views[0].Source, views[0].SourceLine)
	}
	if views[1].Source != "" {
		t.Errorf("fact[1] without an anchor should have no open target, got %q", views[1].Source)
	}
	// Limit caps the slice.
	if got := FactViews(facts, "/brain", 1); len(got) != 1 {
		t.Errorf("limit 1 -> %d views", len(got))
	}
}

func TestDashSessionViewsMapsUsageAndSummary(t *testing.T) {
	sessions := []bmanifest.Session{{
		SessionID: "sess-1", Agent: "claude-code", Model: "opus", CreatedAt: time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC),
		FilesTouched: []string{"a.go"}, CheckpointsCount: 3, TranscriptPath: "sessions/x.jsonl",
		TokenUsage: &bmanifest.CheckpointTokenUsage{InputTokens: 100, OutputTokens: 40},
		Summary:    &bmanifest.CheckpointSummary{Intent: "add dash", Outcome: "shipped"},
	}}
	v := SessionViews(sessions, "/brain", 0, brainstore.SessionReadGuard{})[0]
	if v.Agent != "claude-code" || v.Model != "opus" || v.InputTok != 100 || v.OutputTok != 40 {
		t.Errorf("session usage mapped wrong: %+v", v)
	}
	if v.Intent != "add dash" || v.Outcome != "shipped" || v.Checkpoints != 3 {
		t.Errorf("session summary mapped wrong: %+v", v)
	}
	if v.Created != "2026-06-09 10:00Z" || v.Source != filepath.Join("/brain", "sessions/x.jsonl") {
		t.Errorf("session created/source mapped wrong: %q / %q", v.Created, v.Source)
	}
}

func TestDashSessionViewsOmitExcludedMetadata(t *testing.T) {
	sessions := []bmanifest.Session{
		{SessionID: "private", TranscriptPath: "sessions/private.jsonl", Summary: &bmanifest.CheckpointSummary{Intent: "PRIVATE-DASH-CANARY"}},
		{SessionID: "public", TranscriptPath: "sessions/public.jsonl", Summary: &bmanifest.CheckpointSummary{Intent: "safe"}},
	}
	guard := brainstore.SessionReadGuard{IDs: map[string]brainstore.SessionTombstone{"private": {At: time.Now().UTC()}}}
	views := SessionViews(sessions, "/brain", 0, guard)
	if len(views) != 1 || views[0].ID != "public" || views[0].Intent != "safe" {
		t.Fatalf("dashboard exposed excluded session metadata: %+v", views)
	}
}

func TestDashSemanticViewsResolvesRepoSource(t *testing.T) {
	syms := []semstore.Record{{RecordType: "symbol", Kind: "func", Name: "Run", QualifiedName: "tui.Run",
		FilePath: "internal/tui/tui.go", StartLine: 80, EndLine: 90, Signature: "func Run()", Language: "go"}}
	v := dashSemanticViews(syms, "/work/acme")[0]
	if v.Name != "Run" || v.Language != "go" || v.StartLine != 80 {
		t.Errorf("semantic mapped wrong: %+v", v)
	}
	if v.Source != filepath.Join("/work/acme", "internal/tui/tui.go") || v.SourceLine != 80 {
		t.Errorf("semantic source = %q:%d", v.Source, v.SourceLine)
	}
}

func TestResolveDashTheme(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_THEME", "") // isolate from the host env
	// A known theme resolves without error.
	if th, err := resolveDashTheme("gruvbox"); err != nil || th.Name != "gruvbox" {
		t.Errorf("gruvbox -> %q (err %v)", th.Name, err)
	}
	// Empty falls back to default without error.
	if th, err := resolveDashTheme(""); err != nil || th.Name != "default" {
		t.Errorf("empty -> %q (err %v), want default/nil", th.Name, err)
	}
	// An unknown non-empty name is an error (a typo is surfaced, not swallowed).
	if _, err := resolveDashTheme("grubox"); err == nil {
		t.Errorf("unknown theme should error")
	}
	// The flag is empty but the env names a theme: the env is honored.
	t.Setenv("ENTIRE_BRAIN_THEME", "catppuccin")
	if th, err := resolveDashTheme(""); err != nil || th.Name != "catppuccin" {
		t.Errorf("env theme -> %q (err %v), want catppuccin/nil", th.Name, err)
	}
}

func TestAppendCapNote(t *testing.T) {
	if n := appendCapNote(nil, "facts", 1234, 500); len(n) != 1 || n[0] != "facts: showing 500 of 1234" {
		t.Errorf("cap note = %v", n)
	}
	if n := appendCapNote(nil, "facts", 10, 10); n != nil {
		t.Errorf("no cap should add no note, got %v", n)
	}
}

func TestBrainRelPathRejectsAbsolute(t *testing.T) {
	if got := containedPath("/brain", "/etc/passwd"); got != "" {
		t.Errorf("absolute path should not become an open target, got %q", got)
	}
	if got := containedPath("/brain", "sessions/x.jsonl"); got != filepath.Join("/brain", "sessions/x.jsonl") {
		t.Errorf("rel path join = %q", got)
	}
}

func TestBrainRelPathRejectsTraversal(t *testing.T) {
	// A "../"-laden anchor read from a brain file must never resolve to an open
	// target outside the brain (the `o` key hands the result to the OS opener).
	for _, rel := range []string{"../../etc/passwd", "..", "sessions/../../secret", "a/../../b"} {
		if got := containedPath("/brain", rel); got != "" {
			t.Errorf("containedPath(/brain, %q) = %q, want \"\" (escapes the brain)", rel, got)
		}
	}
	// A legitimate nested path that stays inside the brain is still allowed.
	if got := containedPath("/brain", "sessions/sub/x.jsonl"); got != filepath.Join("/brain", "sessions/sub/x.jsonl") {
		t.Errorf("in-brain nested path rejected: %q", got)
	}
}

func TestSearchResultFromUnifiedGatesOpenPath(t *testing.T) {
	// history/doc hits carry a real file path; fact hits carry taxonomy labels.
	hist := SearchResultFromUnified(retrieval.Result{Source: "history", ID: "h1", Path: "sessions/x.jsonl", Line: 4, Text: "t"}, "/brain")
	if hist.OpenPath != filepath.Join("/brain", "sessions/x.jsonl") || hist.OpenLine != 4 {
		t.Errorf("history result open target = %q:%d", hist.OpenPath, hist.OpenLine)
	}
	fact := SearchResultFromUnified(retrieval.Result{Source: "fact", ID: "fact:x", Path: "arch.storage.layout,perf.io.batching", Text: "t"}, "/brain")
	if fact.OpenPath != "" {
		t.Errorf("fact result must have no open target (Path is a taxonomy label), got %q", fact.OpenPath)
	}
}
