package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDashFactViewsMapsFieldsAndSource(t *testing.T) {
	facts := []factRecord{
		{
			ID: "fact:aaa", Kind: "decision", Text: "Chose SQLite.", Paths: []string{"architecture.storage"},
			Status: "active", Origin: "distilled", RelatedIDs: []string{"fact:bbb"},
			Provenance: []factAnchor{{SessionID: "s1", Transcript: "sessions/x.jsonl", Line: 9, Verified: true}},
		},
		{ID: "fact:bbb", Kind: "gotcha", Text: "No transcript anchor here.", Status: "active"},
	}
	views := dashFactViews(facts, "/brain", 0)
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
	if got := dashFactViews(facts, "/brain", 1); len(got) != 1 {
		t.Errorf("limit 1 -> %d views", len(got))
	}
}

func TestDashSessionViewsMapsUsageAndSummary(t *testing.T) {
	sessions := []exportSession{{
		SessionID: "sess-1", Agent: "claude-code", Model: "opus", CreatedAt: time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC),
		FilesTouched: []string{"a.go"}, CheckpointsCount: 3, TranscriptPath: "sessions/x.jsonl",
		TokenUsage: &checkpointTokenUsage{InputTokens: 100, OutputTokens: 40},
		Summary:    &checkpointSummary{Intent: "add dash", Outcome: "shipped"},
	}}
	v := dashSessionViews(sessions, "/brain", 0)[0]
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

func TestDashSemanticViewsResolvesRepoSource(t *testing.T) {
	syms := []semanticRecord{{RecordType: "symbol", Kind: "func", Name: "Run", QualifiedName: "tui.Run",
		FilePath: "internal/tui/tui.go", StartLine: 80, EndLine: 90, Signature: "func Run()", Language: "go"}}
	v := dashSemanticViews(syms, "/work/acme")[0]
	if v.Name != "Run" || v.Language != "go" || v.StartLine != 80 {
		t.Errorf("semantic mapped wrong: %+v", v)
	}
	if v.Source != filepath.Join("/work/acme", "internal/tui/tui.go") || v.SourceLine != 80 {
		t.Errorf("semantic source = %q:%d", v.Source, v.SourceLine)
	}
}

func TestDashHomeViewSourcesAndNotBuilt(t *testing.T) {
	// An empty manifest (no sources, zero generated_at) flags "not built".
	report := brainStatusReport{Sources: brainStatusSources{}}
	empty := &exportManifest{}
	home := dashHomeView(report, empty, "main")
	if len(home.Sources) != 5 {
		t.Fatalf("want 5 source rows, got %d", len(home.Sources))
	}
	foundNotBuilt := false
	for _, w := range home.Warnings {
		if w == "brain not built yet — run `entire brain refresh`" {
			foundNotBuilt = true
		}
	}
	if !foundNotBuilt {
		t.Errorf("empty brain should warn it is not built; warnings=%v", home.Warnings)
	}

	// A built brain with sources present should not warn "not built" and should
	// carry per-source detail.
	built := brainStatusReport{
		Sources: brainStatusSources{Sessions: true, Semantic: true},
		Brain:   brainStatusBrain{GeneratedAt: "2026-06-10T09:00:00Z"},
	}
	manifest := &exportManifest{
		GeneratedAt: time.Now(),
		Sources: &brainSources{
			Sessions: &sessionSourceManifest{Sessions: make([]exportSession, 14), CheckpointsScanned: 30},
			Semantic: &semanticSourceManifest{Symbols: 55, Relations: 165, Files: 8},
		},
	}
	home = dashHomeView(built, manifest, "main")
	for _, w := range home.Warnings {
		if w == "brain not built yet — run `entire brain refresh`" {
			t.Errorf("built brain should not warn not-built")
		}
	}
	var semDetail string
	for _, s := range home.Sources {
		if s.Name == "Semantic" {
			semDetail = s.Detail
		}
	}
	if semDetail == "" || semDetail[:2] != "55" {
		t.Errorf("semantic source detail = %q, want it to start with the symbol count", semDetail)
	}
}

func TestDashLiveSummary(t *testing.T) {
	if got := dashLiveSummary(brainLiveState{Dirty: false}); got != "clean" {
		t.Errorf("clean tree summary = %q", got)
	}
	got := dashLiveSummary(brainLiveState{Dirty: true, Staged: []string{"a"}, Untracked: []string{"b", "c"}})
	if got != "1 staged · 2 untracked" {
		t.Errorf("dirty summary = %q, want \"1 staged · 2 untracked\"", got)
	}
}

func TestLoadSemanticSymbolsReadsSnapshot(t *testing.T) {
	dir := t.TempDir()
	rel := filepath.FromSlash("semantic/snapshots/gen1/snapshot.ndjson")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o755); err != nil {
		t.Fatal(err)
	}
	// Header line + two symbols + one relation; only symbols should come back.
	content := `{"schema_version":"1.0","provider":"entire-graph"}
{"record_type":"symbol","id":"s1","kind":"func","name":"Run","file_path":"a.go","start_line":1}
{"record_type":"relation","id":"r1","type":"calls","from_id":"s1","to_id":"s2"}
{"record_type":"symbol","id":"s2","kind":"type","name":"Model","file_path":"b.go","start_line":5}
`
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	source := &semanticSourceManifest{SnapshotPath: filepath.ToSlash(rel)}
	syms, err := loadSemanticSymbols(dir, source, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(syms) != 2 || syms[0].Name != "Run" || syms[1].Name != "Model" {
		t.Fatalf("symbols = %+v, want Run + Model only", syms)
	}
	// Limit caps the result.
	capped, err := loadSemanticSymbols(dir, source, 1)
	if err != nil || len(capped) != 1 {
		t.Fatalf("limit 1 -> %d symbols (err %v)", len(capped), err)
	}
	// A missing snapshot is not an error.
	if got, err := loadSemanticSymbols(dir, &semanticSourceManifest{SnapshotPath: "semantic/snapshots/missing/snapshot.ndjson"}, 0); err != nil || got != nil {
		t.Errorf("missing snapshot -> %v (err %v), want nil/nil", got, err)
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
