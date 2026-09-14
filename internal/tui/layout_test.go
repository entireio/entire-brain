package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// This file is the geometry contract for the dashboard. Everything in it was
// written against a real terminal, driven through a pty, because none of these
// defects is visible from an exit code: the program started, accepted keys and
// quit cleanly through every one of them.

// TestStartTabLaysOutItsOwnColumns pins the crash behind `dash --tab sessions`.
//
// bubbles/table renders a row by indexing m.cols[i] for every cell i, and both
// SetRows and SetColumns re-render immediately. Opening straight onto a tab used
// to assign m.tab and call rebuildVisible while the table still held Home's
// three columns, so the first four-cell Sessions row panicked with
// "index out of range [3] with length 3" before the first frame — on any brain
// with at least one session.
func TestStartTabLaysOutItsOwnColumns(t *testing.T) {
	for _, tab := range allTabs {
		tab := tab
		t.Run(tab.String(), func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("opening on %v panicked: %v", tab, r)
				}
			}()
			m := newRunModel(sampleSnapshot(), themes["default"], nil, tab)
			updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
			_ = updated.(Model).View()
		})
	}
}

// TestColumnsNeverExceedTableWidth is the layout budget.
//
// bubbles/table pads each cell one column either side and never wraps a row
// itself, so a layout wider than the table spills past the pane border and the
// terminal wraps it. Flooring each flexible column at a minimum without
// re-checking the total made Facts overflow below ~123 columns, and Sessions
// and Semantic below ~150 — every ordinary terminal.
func TestColumnsNeverExceedTableWidth(t *testing.T) {
	for inner := 1; inner <= 160; inner++ {
		for _, tab := range allTabs {
			rendered := 0
			for _, col := range columnsFor(tab, inner) {
				if col.Width < 0 {
					t.Fatalf("tab %v inner %d: negative column width %d", tab, inner, col.Width)
				}
				if col.Width > 0 {
					rendered += col.Width + 2 // bubbles/table pads every cell
				}
			}
			if rendered > inner {
				t.Errorf("tab %v inner %d: columns render %d cells wide, table is %d", tab, inner, rendered, inner)
			}
		}
	}
}

// TestColumnCountIsStablePerTab guards the invariant layoutColumns must never
// break: a tab's rows carry a fixed number of cells, and renderRow indexes
// m.cols[i] for each of them, so shrinking a layout must zero widths and never
// drop columns.
func TestColumnCountIsStablePerTab(t *testing.T) {
	want := map[Tab]int{TabHome: 3, TabFacts: 3, TabSessions: 4, TabHistory: 2, TabSemantic: 3, TabSearch: 2}
	for tab, n := range want {
		for _, inner := range []int{0, 1, 5, 12, 20, 31, 50, 200} {
			if got := len(columnsFor(tab, inner)); got != n {
				t.Errorf("tab %v inner %d: %d columns, want %d", tab, inner, got, n)
			}
		}
	}
}

// TestViewNeverOverflowsTheTerminal pins the frame to the screen it is drawn on.
//
// Two separate leaks fed this. The footer was the only part of the frame View
// did not clip, and bubbles/help overshoots its own width limit whenever the
// "…" tail does not fit (shouldAddItem in v1.0.0) — at 80 columns that put a
// 113-column help line on screen, which wrapped and pushed the title off the
// top. The panes are also laid out from floors (never under 30 columns or 8
// rows), so a window smaller than those floors got a frame larger than itself.
func TestViewNeverOverflowsTheTerminal(t *testing.T) {
	sizes := [][2]int{{120, 30}, {100, 30}, {80, 24}, {80, 12}, {80, 8}, {80, 3}, {80, 1},
		{60, 20}, {40, 20}, {40, 10}, {30, 14}, {20, 24}, {10, 24}, {1, 1}, {200, 4}}
	for _, showAll := range []bool{false, true} {
		for _, size := range sizes {
			width, height := size[0], size[1]
			model := NewModel(sampleSnapshot(), themes["default"])
			var m tea.Model = model
			if showAll {
				m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("?")})
			}
			m, _ = m.Update(tea.WindowSizeMsg{Width: width, Height: height})
			for range allTabs {
				lines := strings.Split(m.View(), "\n")
				for i, line := range lines {
					if w := lipgloss.Width(line); w > width {
						t.Errorf("%dx%d help=%v: line %d is %d columns wide: %q", width, height, showAll, i, w, line)
					}
				}
				if len(lines) > height {
					t.Errorf("%dx%d help=%v: frame is %d rows tall", width, height, showAll, len(lines))
				}
				m, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
			}
		}
	}
}

// TestTabSwitchKeepsASelection pins the unset cursor.
//
// bubbles/table parks its cursor at -1 whenever it holds no rows — SetRows
// clamps to len(rows)-1, and SetCursor's own clamp(n, 0, len(rows)-1) collapses
// to -1 on an empty table too — and it never recovers when rows arrive. Since
// every tab switch clears the rows before swapping the columns, landing on a
// populated tab left the cursor at -1: no row highlighted, and a detail pane
// reading "No entries in this section." over a full list until a key was
// pressed.
func TestTabSwitchKeepsASelection(t *testing.T) {
	model := NewModel(sampleSnapshot(), themes["default"])
	var m tea.Model = model
	m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	for range allTabs {
		current := m.(Model)
		if current.snap.count(current.tab) == 0 {
			m, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
			continue
		}
		if got := current.selectedIndex(); got < 0 {
			t.Errorf("tab %v holds %d entries but nothing is selected (index %d)",
				current.tab, current.snap.count(current.tab), got)
		}
		if strings.Contains(renderDetail(current.theme, current.snap, current.tab, current.selectedIndex(), 60),
			"No entries in this section") {
			t.Errorf("tab %v holds %d entries but the detail pane says the section is empty", current.tab, current.snap.count(current.tab))
		}
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	}
}

// TestStartTabSelectsItsFirstRow is the same contract for --tab, which reaches
// the table by a different route (Run, not a key press).
func TestStartTabSelectsItsFirstRow(t *testing.T) {
	m := newRunModel(sampleSnapshot(), themes["default"], nil, TabSemantic)
	if got := m.selectedIndex(); got != 0 {
		t.Fatalf("opening on Semantic selected index %d, want 0", got)
	}
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 28})
	if got := updated.(Model).selectedIndex(); got != 0 {
		t.Fatalf("after the first resize the selection is %d, want 0", got)
	}
}

// TestUnreadableSourceIsNotShownAsPresent: a source the manifest declares but
// whose store could not be read must not render with the same glyph as one that
// loaded, and its row must carry the failure rather than the manifest's counts.
func TestUnreadableSourceIsNotShownAsPresent(t *testing.T) {
	broken := SourceHealth{Name: "Semantic", Present: true, Unreadable: true, Detail: "semantic unavailable: unexpected end of JSON input"}
	if broken.State() != "unreadable" {
		t.Fatalf("State() = %q, want unreadable", broken.State())
	}
	loaded := SourceHealth{Name: "Seed", Present: true, Detail: "1 entrypoints"}
	if sourceDot(broken) == sourceDot(loaded) {
		t.Errorf("an unreadable source draws the same glyph (%q) as one that loaded", sourceDot(broken))
	}
	if sourceDot(broken) == sourceDot(SourceHealth{Name: "Facts"}) {
		t.Errorf("an unreadable source draws the same glyph as an absent one")
	}

	snap := sampleSnapshot()
	snap.Home.Sources = []SourceHealth{broken, loaded}
	rows := rowsFor(snap, TabHome, []int{0, 1})
	if rows[0][0] == rows[1][0] {
		t.Errorf("Home rows do not distinguish unreadable from present: %q vs %q", rows[0][0], rows[1][0])
	}
}

// TestSupportsFullScreenRejectsADumbTerminal is the gate in front of the
// dashboard. On TERM=dumb or an unset TERM, `dash` used to start the
// full-screen program anyway and write \x1b[?1049h, rounded box-drawing and
// 256-colour SGR at a terminal that renders every byte of it as literal text.
// The line renderer in this package had always refused such a terminal; the
// dashboard now shares the same predicate so the two halves cannot disagree.
func TestSupportsFullScreenRejectsADumbTerminal(t *testing.T) {
	for _, tc := range []struct {
		term string
		want bool
	}{
		{"xterm-256color", true},
		{"screen", true},
		{"dumb", false},
		{"", false},
		{"  ", false},
	} {
		got := SupportsFullScreen(func(string) string { return tc.term })
		if got != tc.want {
			t.Errorf("SupportsFullScreen(TERM=%q) = %v, want %v", tc.term, got, tc.want)
		}
	}
	if SupportsFullScreen(nil) {
		t.Error("SupportsFullScreen(nil) must not claim a usable terminal")
	}
}

// wideGlyphSnapshot carries symbol names and fact text in scripts whose display
// width is not their rune count: CJK and emoji occupy two cells, Hebrew and
// Arabic one each but right-to-left, and combining marks none. go-runewidth is a
// direct dependency precisely so these measure correctly, and a table column
// mis-measured by one cell pushes every border on the row out of line.
func wideGlyphSnapshot() Snapshot {
	s := sampleSnapshot()
	s.Repo = "/work/プロジェクト"
	s.Branch = "機能/新しい"
	s.Semantic = []SemanticView{
		{Kind: "type", Name: "構造体テスト", QualifiedName: "パッケージ.構造体テスト", FilePath: "内部/テスト.go", StartLine: 4, Signature: "構造体テスト struct{ フィールド string }", Language: "Go"},
		{Kind: "func", Name: "🚀Rocket🛰", QualifiedName: "pkg.🚀Rocket🛰", FilePath: "launch.go", StartLine: 9, Signature: "func 🚀Rocket🛰(n int) int", Language: "Go"},
		{Kind: "func", Name: "שלוםעולם", QualifiedName: "pkg.שלוםעולם", FilePath: "rtl.go", StartLine: 15, Signature: "func שלוםעולם() string", Language: "Go"},
		{Kind: "func", Name: "مرحبابالعالم", QualifiedName: "pkg.مرحبابالعالم", FilePath: "rtl.go", StartLine: 20, Language: "Go"},
		{Kind: "const", Name: "été", QualifiedName: "pkg.été", FilePath: "combining.go", StartLine: 1, Language: "Go"},
	}
	s.Facts = []FactView{
		{ID: "fact:cjk", Kind: "決定", Text: "チェックポイントは SQLite に保存する。理由は移植性 🚀。", Status: "active"},
		{ID: "fact:rtl", Kind: "gotcha", Text: "الترميز يجب أن يكون UTF-8 دائمًا", Status: "active"},
	}
	s.History = []HistoryView{{ID: "history:1", Kind: "決定", Summary: "採用した RRF を検索の融合に 🚀", Path: "sessions/x.jsonl", Line: 40}}
	return s
}

// TestWideGlyphsDoNotBreakAlignment runs the geometry contract over content
// whose display width differs from its rune count.
func TestWideGlyphsDoNotBreakAlignment(t *testing.T) {
	for _, width := range []int{40, 60, 80, 100, 120} {
		model := NewModel(wideGlyphSnapshot(), themes["default"])
		var m tea.Model = model
		m, _ = m.Update(tea.WindowSizeMsg{Width: width, Height: 26})
		for range allTabs {
			frame := m.View()
			lines := strings.Split(frame, "\n")
			for i, line := range lines {
				if w := lipgloss.Width(line); w > width {
					t.Errorf("width %d tab %v: line %d measures %d cells: %q",
						width, m.(Model).tab, i, w, line)
				}
			}
			// Every row of the frame is one physical line: a mis-measured wide
			// glyph shows up as a row the terminal would have to wrap.
			if len(lines) > 26 {
				t.Errorf("width %d tab %v: frame is %d rows tall", width, m.(Model).tab, len(lines))
			}
			m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
			m, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
		}
	}
}

// TestTruncateCountsCellsNotRunes: the table's cell preview must cut on display
// width, or a CJK column overflows by one cell per wide glyph.
func TestTruncateCountsCellsNotRunes(t *testing.T) {
	for _, value := range []string{"構造体テストです", "🚀🚀🚀🚀🚀", "שלום עולם ושוב", "abcdefghij"} {
		for _, limit := range []int{1, 2, 3, 5, 8, 12} {
			got := truncate(value, limit)
			if w := lipgloss.Width(got); w > limit {
				t.Errorf("truncate(%q, %d) = %q, %d cells wide", value, limit, got, w)
			}
		}
	}
}
