package tui

import (
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// TestMain strips ANSI so substring assertions match the rendered text.
func TestMain(m *testing.M) {
	lipgloss.SetColorProfile(termenv.Ascii)
	os.Exit(m.Run())
}

func sampleSnapshot() Snapshot {
	return Snapshot{
		Repo:        "/work/acme",
		Branch:      "main",
		GeneratedAt: "2026-06-10T09:00:00Z",
		Home: HomeView{
			Sources: []SourceHealth{
				{Name: "Seed", Present: true, Detail: "3 entrypoints"},
				{Name: "Sessions", Present: true, Detail: "14 sessions"},
				{Name: "Semantic", Present: true, Detail: "55 symbols · 8 files"},
				{Name: "History", Present: false, Detail: ""},
				{Name: "Facts", Present: true, Detail: "12 facts across 1 branch(es)"},
			},
			Freshness:  "fresh",
			Axes:       []FreshnessAxis{{Name: "commit", State: "current"}},
			BlindSpots: []string{"internal/legacy: no symbols indexed"},
			Warnings:   []string{"semantic snapshot older than HEAD"},
			Live:       LiveState{Branch: "main", Head: "abcdef1234567890", Dirty: true, Summary: "2 staged · 1 untracked"},
		},
		Facts: []FactView{
			{
				ID: "fact:aaa111", Kind: "decision", Text: "The team chose SQLite for the local store.",
				Paths: []string{"architecture.storage"}, Status: "active", Origin: "distilled",
				Provenance: []Anchor{{SessionID: "sess-1", Transcript: "sessions/x.jsonl", Line: 12}},
				Source:     "/brain/sessions/x.jsonl", SourceLine: 12,
			},
			{ID: "fact:bbb222", Kind: "gotcha", Text: "Vector search needs the sqlite-vec extension loaded.", Status: "active"},
			{ID: "fact:ccc333", Kind: "convention", Text: "Commands return JSON under --json.", Status: "superseded"},
		},
		Sessions: []SessionView{
			{ID: "sess-1", Agent: "claude-code", Model: "opus", Created: "2026-06-09 10:00Z",
				Files: []string{"a.go", "b.go"}, InputTok: 100, OutputTok: 50, Intent: "add dash", Outcome: "shipped",
				Checkpoints: 4, Source: "/brain/sessions/x.jsonl"},
			{ID: "sess-2", Agent: "codex", Created: "2026-06-09 12:00Z"},
		},
		History: []HistoryView{
			{ID: "history:1", Kind: "decision", Summary: "Adopt RRF for hybrid search.", Path: "sessions/x.jsonl", Line: 40, Source: "/brain/sessions/x.jsonl", SourceLine: 40},
		},
		Semantic: []SemanticView{
			{Kind: "func", Name: "Run", QualifiedName: "tui.Run", FilePath: "internal/tui/tui.go", StartLine: 80, EndLine: 90,
				Signature: "func Run(snap Snapshot) error", Language: "go", Source: "/work/acme/internal/tui/tui.go", SourceLine: 80},
		},
		Notes: []string{"semantic symbols: showing 1 of 55"},
	}
}

func TestThemeByName(t *testing.T) {
	if th, ok := ThemeByName("catppuccin"); !ok || th.Name != "catppuccin" {
		t.Errorf("catppuccin not resolved: %+v ok=%v", th, ok)
	}
	if th, ok := ThemeByName("Tokyo Night"); !ok || th.Name != "tokyonight" {
		t.Errorf("separator-folded name not resolved: %+v ok=%v", th, ok)
	}
	if th, ok := ThemeByName("nonsense"); ok || th.Name != "default" {
		t.Errorf("unknown theme should fall back to default with ok=false, got %+v ok=%v", th, ok)
	}
}

func TestThemeNames(t *testing.T) {
	names := ThemeNames()
	if len(names) != len(themes) {
		t.Fatalf("ThemeNames() has %d entries but themes map has %d (they drifted)", len(names), len(themes))
	}
	if names[0] != "default" {
		t.Errorf("default should be listed first, got %q", names[0])
	}
	for _, n := range names {
		if _, ok := themes[n]; !ok {
			t.Errorf("ThemeNames() returned %q which is not in the themes map", n)
		}
	}
	// The trailing names (after default) are sorted.
	for i := 2; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Errorf("theme names after default should be sorted: %q before %q", names[i-1], names[i])
		}
	}
}

func TestParseTab(t *testing.T) {
	cases := map[string]Tab{"": TabHome, "home": TabHome, "facts": TabFacts, "Sessions": TabSessions, "HISTORY": TabHistory, "semantic": TabSemantic}
	for in, want := range cases {
		if got, _ := ParseTab(in); got != want {
			t.Errorf("ParseTab(%q) = %v, want %v", in, got, want)
		}
	}
	if _, ok := ParseTab("bogus"); ok {
		t.Errorf("ParseTab(bogus) should report ok=false")
	}
}

func TestRowsForPerTab(t *testing.T) {
	s := sampleSnapshot()
	// Facts: kind / fact preview / status — the superseded fact keeps its status.
	rows := rowsFor(s, TabFacts, []int{0, 1, 2})
	if len(rows) != 3 || !strings.Contains(rows[0][0], "decision") || rows[2][2] != "superseded" {
		t.Errorf("fact rows = %v", rows)
	}
	// Home: a present source shows the filled dot, an absent one the hollow dot.
	home := rowsFor(s, TabHome, []int{0, 1, 2, 3, 4})
	if home[0][0] != "●" || home[3][0] != "○" {
		t.Errorf("home dots = %q / %q, want ●/○", home[0][0], home[3][0])
	}
	// Semantic: the File column shows the base name, not the full path.
	sem := rowsFor(s, TabSemantic, []int{0})
	if sem[0][2] != "tui.go" {
		t.Errorf("semantic file cell = %q, want tui.go", sem[0][2])
	}
}

func TestRenderDetailPerTab(t *testing.T) {
	th, _ := ThemeByName("default")
	cases := []struct {
		tab  Tab
		idx  int
		want []string
	}{
		{TabHome, 0, []string{"Sources", "Semantic", "55 symbols", "Freshness", "Blind spots"}},
		{TabFacts, 0, []string{"Fact", "SQLite", "Provenance", "decision"}},
		{TabSessions, 0, []string{"Activity", "claude-code", "shipped", "checkpoints"}},
		{TabHistory, 0, []string{"Summary", "RRF"}},
		{TabSemantic, 0, []string{"Run", "Signature", "func Run"}},
	}
	s := sampleSnapshot()
	for _, c := range cases {
		out := renderDetail(th, s, c.tab, c.idx, 80)
		for _, w := range c.want {
			if !strings.Contains(out, w) {
				t.Errorf("renderDetail(%v) missing %q\n%s", c.tab, w, out)
			}
		}
	}
}

func TestViewSmoke(t *testing.T) {
	th, _ := ThemeByName("default")
	m := NewModel(sampleSnapshot(), th)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	view := updated.(Model).View()
	for _, want := range []string{"entire-brain", "/work/acme", "Home", "Facts (3)", "Semantic (1)"} {
		if !strings.Contains(view, want) {
			t.Errorf("View missing %q", want)
		}
	}
}

func TestClickSelectsCorrectRow(t *testing.T) {
	th, _ := ThemeByName("default")
	m := NewModel(sampleSnapshot(), th)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40}) // tall: all rows fit
	m = updated.(Model)
	// Move from Home to Facts (3 rows).
	u, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = u.(Model)

	// First data row renders at Y=5 (title 0, tabs 1, pane border 2, header text
	// 3, header bottom border 4, first row 5). Clicking it must select row 0.
	u, _ = m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 2, Y: 5})
	if got := u.(Model).table.Cursor(); got != 0 {
		t.Errorf("click at Y=5 selected row %d, want 0 (off-by-one regression)", got)
	}
	u, _ = m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 2, Y: 7})
	if got := u.(Model).table.Cursor(); got != 2 {
		t.Errorf("click at Y=7 selected row %d, want 2", got)
	}
}

func TestFilterNarrowsRows(t *testing.T) {
	th, _ := ThemeByName("default")
	m := NewModel(sampleSnapshot(), th)
	u, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = u.(Model)
	u, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab}) // -> Facts
	m = u.(Model)
	u, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	m = u.(Model)
	for _, r := range "vector" {
		u, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = u.(Model)
	}
	if len(m.visible) != 1 || m.visible[0] != 1 {
		t.Errorf("filter 'vector' -> visible %v, want [1]", m.visible)
	}
}

func TestTabCycleNoPanic(t *testing.T) {
	// Cycling every tab forward and backward must not panic: switching into a
	// tab with fewer columns than the current rows' cell count once crashed
	// because SetColumns re-rendered the stale wider rows.
	th, _ := ThemeByName("default")
	m := NewModel(sampleSnapshot(), th)
	u, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = u.(Model)
	for i := 0; i < len(allTabs)+1; i++ {
		u, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
		m = u.(Model)
		_ = m.View()
	}
	for i := 0; i < len(allTabs)+1; i++ {
		u, _ = m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
		m = u.(Model)
		_ = m.View()
	}
}

func TestRenderHomeSurvivesColorProfile(t *testing.T) {
	// Under a real (colored) profile the Home source lines carry ANSI escapes;
	// the width clamp must measure display width, not escape bytes, or the
	// per-source Detail text gets eaten.
	defer lipgloss.SetColorProfile(termenv.Ascii)
	lipgloss.SetColorProfile(termenv.ANSI256)
	th, _ := ThemeByName("default")
	out := renderHome(th, sampleSnapshot(), 50)
	for _, want := range []string{"55 symbols", "12 facts across 1 branch(es)"} {
		if !strings.Contains(out, want) {
			t.Errorf("renderHome dropped %q under a colored profile (ANSI-unaware truncation)", want)
		}
	}
}

func TestDetailRendersSourceLink(t *testing.T) {
	// An entry with a source renders a clickable OSC 8 hyperlink (the helper the
	// zero-width test guards is actually emitted in the render path).
	th, _ := ThemeByName("default")
	out := renderDetail(th, sampleSnapshot(), TabFacts, 0, 80)
	if !strings.Contains(out, "\x1b]8;;") || !strings.Contains(out, "open source") {
		t.Errorf("fact detail with a source should render an OSC 8 link; got:\n%s", out)
	}
	// An entry without a source renders no link.
	noSrc := renderDetail(th, sampleSnapshot(), TabFacts, 1, 80)
	if strings.Contains(noSrc, "\x1b]8;;") {
		t.Errorf("fact detail without a source should not render a link")
	}
}

func TestSearchPopulatesSearchTab(t *testing.T) {
	th, _ := ThemeByName("default")
	m := NewModel(sampleSnapshot(), th)
	m.searchFn = func(q string) ([]SearchResult, error) {
		return []SearchResult{{
			Source: "fact", ID: "fact:x", Text: "hit for " + q,
			Path: "sessions/x.jsonl", Line: 3, OpenPath: "/brain/sessions/x.jsonl", OpenLine: 3,
		}}, nil
	}
	u, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = u.(Model)

	u, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	m = u.(Model)
	if !m.searching {
		t.Fatal("`s` should enter search mode")
	}
	for _, r := range "rrf" {
		u, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = u.(Model)
	}
	u, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = u.(Model)
	if cmd == nil {
		t.Fatal("enter should return a search command")
	}
	u, _ = m.Update(cmd()) // run the async search and feed the result back
	m = u.(Model)

	if m.tab != TabSearch {
		t.Errorf("after a search the active tab = %v, want Search", m.tab)
	}
	if m.searching {
		t.Errorf("search mode should close after submitting")
	}
	if len(m.snap.Search) != 1 || m.snap.SearchQuery != "rrf" {
		t.Errorf("search results = %v, query = %q", m.snap.Search, m.snap.SearchQuery)
	}
	if len(m.visible) != 1 {
		t.Errorf("visible rows after search = %v, want 1", m.visible)
	}
	if view := m.View(); !strings.Contains(view, "Search (1)") {
		t.Errorf("tab bar should show Search (1)")
	}
	detail := renderDetail(th, m.snap, TabSearch, 0, 80)
	if !strings.Contains(detail, "hit for rrf") || !strings.Contains(detail, "Match") {
		t.Errorf("search detail missing the matched text:\n%s", detail)
	}
}

func TestSearchClearsStaleFilter(t *testing.T) {
	// A filter committed on a prior tab must not silently hide search results.
	th, _ := ThemeByName("default")
	m := NewModel(sampleSnapshot(), th)
	m.searchFn = func(q string) ([]SearchResult, error) {
		return []SearchResult{
			{Source: "history", ID: "h1", Text: "alpha result"},
			{Source: "doc", ID: "d1", Text: "beta result"},
		}, nil
	}
	u, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = u.(Model)
	// Commit a filter that matches none of the (future) search results.
	u, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	m = u.(Model)
	for _, r := range "zzznomatch" {
		u, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = u.(Model)
	}
	u, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // commit the filter (kept, not cleared)
	m = u.(Model)
	// Now search (type a query, then submit).
	u, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	m = u.(Model)
	for _, r := range "alpha" {
		u, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = u.(Model)
	}
	u, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = u.(Model)
	if cmd == nil {
		t.Fatal("enter with a query should return a search command")
	}
	u, _ = m.Update(cmd())
	m = u.(Model)
	if len(m.visible) != 2 {
		t.Errorf("stale filter hid search results: visible=%d, want 2 (Search count=%d)", len(m.visible), m.snap.count(TabSearch))
	}
	if m.filter.Value() != "" {
		t.Errorf("filter should be cleared after a search, got %q", m.filter.Value())
	}
}

func TestSearchExitsModeOnSubmitAndDropsStale(t *testing.T) {
	th, _ := ThemeByName("default")
	m := NewModel(sampleSnapshot(), th)
	m.searchFn = func(q string) ([]SearchResult, error) {
		return []SearchResult{{Source: "history", ID: "h", Text: "hit"}}, nil
	}
	u, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = u.(Model)
	u, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	m = u.(Model)
	for _, r := range "current" {
		u, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = u.(Model)
	}
	u, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = u.(Model)
	if m.searching {
		t.Error("search mode should exit immediately on submit (so repeated Enter can't enqueue searches)")
	}
	// A result for a superseded/older query must be dropped.
	u, _ = m.Update(searchResultMsg{query: "stale-other", results: []SearchResult{{Source: "doc", ID: "x", Text: "stale"}}})
	m = u.(Model)
	if len(m.snap.Search) != 0 {
		t.Errorf("stale result should be dropped, got %d results", len(m.snap.Search))
	}
	// The in-flight result applies.
	u, _ = m.Update(cmd())
	m = u.(Model)
	if len(m.snap.Search) != 1 || m.tab != TabSearch {
		t.Errorf("in-flight result should apply; search=%d tab=%v", len(m.snap.Search), m.tab)
	}
}

func TestSearchDisabledWhenNoFunc(t *testing.T) {
	th, _ := ThemeByName("default")
	m := NewModel(sampleSnapshot(), th) // no searchFn
	u, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = u.(Model)
	u, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	m = u.(Model)
	if m.searching {
		t.Errorf("`s` must be a no-op when no search function is wired")
	}
	// The empty Search tab shows a prompt rather than a crash.
	empty := renderDetail(th, m.snap, TabSearch, -1, 80)
	if !strings.Contains(empty, "search") {
		t.Errorf("empty Search tab should prompt to search:\n%s", empty)
	}
}

func TestHyperlinkIsZeroWidth(t *testing.T) {
	// OSC 8 hyperlinks must measure as just their visible label, or the viewport
	// and pane padding miscount and the right border misaligns.
	plain := lipgloss.Width("open ↗")
	linked := lipgloss.Width(osc8("file:///work/acme/internal/tui/tui.go", "open ↗"))
	if plain != linked {
		t.Errorf("hyperlink width = %d, want %d (label width); OSC 8 escapes must be zero-width", linked, plain)
	}
}

// TestDumpDashboard renders the dashboard to /tmp/brain-dashboard.txt (ANSI
// stripped) when BRAIN_DASH_DUMP is set, for a shareable static snapshot. It is
// a no-op in normal test runs.
func TestDumpDashboard(t *testing.T) {
	if os.Getenv("BRAIN_DASH_DUMP") == "" {
		t.Skip("set BRAIN_DASH_DUMP=1 to render a dashboard snapshot")
	}
	th, _ := ThemeByName("default")
	m := NewModel(sampleSnapshot(), th)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 110, Height: 32})
	if err := os.WriteFile("/tmp/brain-dashboard.txt", []byte(updated.(Model).View()), 0o644); err != nil {
		t.Fatal(err)
	}
}
