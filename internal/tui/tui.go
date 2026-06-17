// Package tui renders the interactive brain dashboard for entire-brain: a Home
// status/coverage view plus explorer tabs (Facts, Sessions, History, Semantic)
// that browse individual entries, with a left list and a right detail page,
// vim+arrow navigation, a fuzzy filter, a help bar, and selectable themes. Every
// page is deterministic and read-only — it only formats data the brain already
// stores, with no agent calls. The package does no file IO: the caller assembles
// a Snapshot and hands it in, and on a non-TTY the caller falls back to plain
// text so the dashboard is only constructed for a real terminal.
package tui

import (
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// SearchFunc runs a brain search for a query and returns ranked results. It is
// supplied by the caller (the cli layer) so the TUI itself stays IO-free; nil
// disables in-dashboard search.
type SearchFunc func(query string) ([]SearchResult, error)

// searchResultMsg carries the outcome of an async in-dashboard search back into
// Update.
type searchResultMsg struct {
	query   string
	results []SearchResult
	err     error
}

// Model is the dashboard model over a brain Snapshot.
type Model struct {
	snap     Snapshot
	theme    Theme
	searchFn SearchFunc

	keys   keyMap
	help   help.Model
	table  table.Model
	vp     viewport.Model
	filter textinput.Model
	search textinput.Model

	tab         Tab
	visible     []int // indices into the active tab's source slice
	focusDetail bool
	filtering   bool
	searching   bool

	width, height int
	leftTotal     int
	leftInner     int
	rightInner    int
	paneContentH  int
	tableCapacity int // data rows the table shows without scrolling
	detailWidth   int
	ready         bool
}

// NewModel builds a dashboard over the brain snapshot.
func NewModel(snap Snapshot, theme Theme) Model {
	ti := textinput.New()
	ti.Prompt = "filter: "
	ti.CharLimit = 64

	si := textinput.New()
	si.Prompt = "search: "
	si.CharLimit = 256

	m := Model{
		snap:   snap,
		theme:  theme,
		keys:   defaultKeys(),
		help:   help.New(),
		table:  newBrainTable(theme),
		vp:     viewport.New(60, 20),
		filter: ti,
		search: si,
		width:  100,
		height: 30,
		tab:    TabHome,
	}
	m.rebuildVisible()
	return m
}

// Run builds and runs the dashboard program against out (a real terminal),
// opening on startTab. search may be nil to disable in-dashboard search. It
// blocks until the user quits.
func Run(snap Snapshot, theme Theme, search SearchFunc, startTab Tab, out io.Writer) error {
	m := NewModel(snap, theme)
	m.searchFn = search
	if startTab != TabHome {
		m.tab = startTab
		m.rebuildVisible()
	}
	p := tea.NewProgram(m,
		tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithOutput(out))
	_, err := p.Run()
	return err
}

func (m Model) Init() tea.Cmd { return nil }

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ready = true
		m.resize()
		return m, nil
	case tea.MouseMsg:
		return m.updateMouse(msg)
	case searchResultMsg:
		m.searching = false
		m.search.Blur()
		m.snap.SearchQuery = msg.query
		if msg.err != nil {
			m.snap.Search = nil
			m.snap.SearchErr = msg.err.Error()
		} else {
			m.snap.Search = msg.results
			m.snap.SearchErr = ""
		}
		m.tab = TabSearch
		m.focusDetail = false
		// Clear any committed filter from the previous tab so it can't silently
		// hide search hits (which would also desync the Search (N) count).
		m.filter.SetValue("")
		m.table.SetRows(nil)
		m.table.SetColumns(columnsFor(m.tab, m.leftInner))
		m.table.SetCursor(0)
		m.rebuildVisible()
		return m, nil
	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC {
			return m, tea.Quit
		}
		if m.filtering {
			return m.updateFiltering(msg)
		}
		if m.searching {
			return m.updateSearching(msg)
		}
		switch {
		case key.Matches(msg, m.keys.Quit):
			return m, tea.Quit
		case key.Matches(msg, m.keys.Help):
			m.help.ShowAll = !m.help.ShowAll
			m.resize()
			return m, nil
		case key.Matches(msg, m.keys.Filter):
			m.filtering = true
			m.filter.Focus()
			return m, textinput.Blink
		case key.Matches(msg, m.keys.Search):
			if m.searchFn != nil {
				m.searching = true
				m.search.SetValue("")
				m.search.Focus()
				return m, textinput.Blink
			}
		case key.Matches(msg, m.keys.Open):
			return m, m.openSelected()
		case key.Matches(msg, m.keys.Section):
			m.switchTab(1)
			return m, nil
		case key.Matches(msg, m.keys.PrevTab):
			m.switchTab(-1)
			return m, nil
		}
		if m.focusDetail {
			if key.Matches(msg, m.keys.Back) {
				m.focusDetail = false
				return m, nil
			}
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg)
			return m, cmd
		}
		// list focus
		if key.Matches(msg, m.keys.Enter) {
			if len(m.visible) > 0 {
				m.focusDetail = true
			}
			return m, nil
		}
		prev := m.table.Cursor()
		var cmd tea.Cmd
		m.table, cmd = m.table.Update(msg)
		if m.table.Cursor() != prev {
			m.refreshDetail()
		}
		return m, cmd
	}
	return m, nil
}

// switchTab moves the active tab by delta (wrapping) and resets the view.
func (m *Model) switchTab(delta int) {
	n := len(allTabs)
	idx := (int(m.tab) + delta%n + n) % n
	m.tab = allTabs[idx]
	m.focusDetail = false
	m.filter.SetValue("")
	// Clear the rows before swapping in the new tab's columns: SetColumns
	// immediately re-renders the table against whatever rows it still holds, and
	// the previous tab's rows may have more cells than the new tab has columns
	// (e.g. Sessions has 4, History has 2) — which would panic. rebuildVisible
	// repopulates rows that match the new columns.
	m.table.SetRows(nil)
	m.table.SetColumns(columnsFor(m.tab, m.leftInner))
	m.table.SetCursor(0)
	m.rebuildVisible()
}

// updateMouse handles wheel scrolling (scrolls the detail page) and left-click
// (click a list row to open it, or click the detail pane to focus it). The chrome
// above the first list data row is: title(0) tabs(1) pane-top-border(2)
// header-text(3) header-bottom-border(4) first-row(5) — the bubbles/table header
// is two physical lines because of its bottom border. bubbles v1.0.0 exposes no
// scroll offset, so a click is only hit-tested when the whole list fits unscrolled
// (len <= tableCapacity); a scrolled list just focuses the page instead.
func (m Model) updateMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if m.filtering || m.searching {
		return m, nil
	}
	if msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown {
		m.focusDetail = true
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	}
	if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft {
		if msg.X >= m.leftTotal {
			m.focusDetail = true
			return m, nil
		}
		if row := msg.Y - 5; len(m.visible) <= m.tableCapacity && row >= 0 && row < len(m.visible) {
			m.table.SetCursor(row)
			m.refreshDetail()
		}
		m.focusDetail = true
	}
	return m, nil
}

func (m Model) updateFiltering(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.filtering = false
		m.filter.Blur()
		m.filter.SetValue("")
		m.rebuildVisible()
		return m, nil
	case "enter":
		m.filtering = false
		m.filter.Blur()
		return m, nil
	}
	var cmd tea.Cmd
	m.filter, cmd = m.filter.Update(msg)
	m.rebuildVisible()
	return m, cmd
}

func (m Model) updateSearching(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.searching = false
		m.search.Blur()
		return m, nil
	case "enter":
		query := strings.TrimSpace(m.search.Value())
		if query == "" {
			m.searching = false
			m.search.Blur()
			return m, nil
		}
		return m, m.runSearchCmd(query)
	}
	var cmd tea.Cmd
	m.search, cmd = m.search.Update(msg)
	return m, cmd
}

// runSearchCmd runs the search off the UI thread and reports back via
// searchResultMsg.
func (m Model) runSearchCmd(query string) tea.Cmd {
	fn := m.searchFn
	return func() tea.Msg {
		if fn == nil {
			return searchResultMsg{query: query, err: fmt.Errorf("search is unavailable")}
		}
		results, err := fn(query)
		return searchResultMsg{query: query, results: results, err: err}
	}
}

// resize recomputes the pane geometry from the terminal size and re-flows the
// table, viewport, and detail content.
func (m *Model) resize() {
	leftTotal := m.width / 3
	if leftTotal < 30 {
		leftTotal = 30
	}
	if leftTotal > 52 {
		leftTotal = 52
	}
	rightTotal := m.width - leftTotal
	m.leftTotal = leftTotal
	m.leftInner = max(leftTotal-2, 12)
	m.rightInner = max(rightTotal-2, 20)

	// Measure the footer's real height (the help line can wrap on a narrow
	// terminal) so the body never overruns it.
	m.help.Width = m.width
	footerH := lipgloss.Height(m.footerView())
	bodyTotalH := m.height - 2 /*title+tabs*/ - footerH
	if bodyTotalH < 8 {
		bodyTotalH = 8
	}
	m.paneContentH = bodyTotalH - 2 // pane borders

	tableHeight := max(m.paneContentH-1, 3)
	m.table.SetColumns(columnsFor(m.tab, m.leftInner))
	m.table.SetWidth(m.leftInner)
	m.table.SetHeight(tableHeight)
	// Data rows shown = table height minus the 2-line header (text + bottom
	// border). Used to decide whether a click can be safely hit-tested.
	m.tableCapacity = max(tableHeight-2, 0)

	m.vp.Width = m.rightInner
	m.vp.Height = m.paneContentH
	m.filter.Width = m.leftInner - len(m.filter.Prompt) - 1
	m.search.Width = max(m.width-len(m.search.Prompt)-1, 8)
	m.detailWidth = m.rightInner
	m.refreshDetail()
}

// selectedIndex returns the index into the active tab's source slice for the
// row under the cursor, or -1 when the list is empty.
func (m Model) selectedIndex() int {
	if len(m.visible) == 0 {
		return -1
	}
	cur := m.table.Cursor()
	if cur < 0 || cur >= len(m.visible) {
		return -1
	}
	return m.visible[cur]
}

// openSelected opens the selected entry's underlying source (a code file or a
// transcript) via the OS opener, without blocking the UI.
func (m Model) openSelected() tea.Cmd {
	idx := m.selectedIndex()
	if idx < 0 {
		return nil
	}
	target, _ := m.snap.openTarget(m.tab, idx)
	if target == "" {
		return nil
	}
	return openCmd(target)
}

// openCmd returns a command that opens target (a file path or URL) in the OS
// default handler without blocking. A failure is silent.
func openCmd(target string) tea.Cmd {
	return func() tea.Msg {
		var name string
		var args []string
		switch runtime.GOOS {
		case "darwin":
			name, args = "open", []string{target}
		case "windows":
			name, args = "rundll32", []string{"url.dll,FileProtocolHandler", target}
		default:
			name, args = "xdg-open", []string{target}
		}
		_ = exec.Command(name, args...).Start()
		return nil
	}
}

// rebuildVisible recomputes the filtered row indices for the active tab and
// refreshes the table + detail.
func (m *Model) rebuildVisible() {
	q := strings.ToLower(strings.TrimSpace(m.filter.Value()))
	m.visible = m.visible[:0]
	for i := 0; i < m.snap.count(m.tab); i++ {
		if q == "" || strings.Contains(m.snap.filterKey(m.tab, i), q) {
			m.visible = append(m.visible, i)
		}
	}
	m.table.SetRows(rowsFor(m.snap, m.tab, m.visible))
	if m.table.Cursor() >= len(m.visible) {
		m.table.SetCursor(max(0, len(m.visible)-1))
	}
	m.refreshDetail()
}

func (m *Model) refreshDetail() {
	idx := m.selectedIndex()
	// Home and Search render a page even with no row selected (the status page /
	// the search prompt); other tabs show a placeholder when empty.
	if idx < 0 && m.tab != TabHome && m.tab != TabSearch {
		m.vp.SetContent(m.theme.dimStyle().Render("No entries in this section."))
		m.vp.GotoTop()
		return
	}
	content := renderDetail(m.theme, m.snap, m.tab, idx, m.detailWidth)
	m.vp.SetContent(content)
	m.vp.GotoTop()
}

func (m Model) View() string {
	if !m.ready {
		return "loading…"
	}

	clip := lipgloss.NewStyle().MaxWidth(m.width)
	title := clip.Render(m.theme.titleStyle().Render("entire-brain") +
		m.theme.dimStyle().Render(fmt.Sprintf("  ·  %s  ·  branch %s",
			orDash(m.snap.Repo), orDash(m.snap.Branch))))

	var secondRow string
	switch {
	case m.searching:
		secondRow = clip.Render(m.search.View())
	case m.filtering:
		secondRow = clip.Render(m.filter.View())
	default:
		secondRow = clip.Render(m.renderTabs())
	}

	leftPane := m.theme.paneStyle(!m.focusDetail).
		Width(m.leftInner).Height(m.paneContentH).MaxHeight(m.paneContentH + 2).Render(m.table.View())
	rightPane := m.theme.paneStyle(m.focusDetail).
		Width(m.rightInner).Height(m.paneContentH).MaxHeight(m.paneContentH + 2).Render(m.vp.View())
	body := lipgloss.NewStyle().MaxWidth(m.width).Render(
		lipgloss.JoinHorizontal(lipgloss.Top, leftPane, rightPane))

	return strings.Join([]string{title, secondRow, body, m.footerView()}, "\n")
}

// footerView renders the help line (plus a scroll hint when the detail pane has
// more below the fold).
func (m Model) footerView() string {
	h := m.help.View(m.keys)
	if m.focusDetail && !m.vp.AtBottom() {
		h += m.theme.dimStyle().Render("   ↓ more")
	}
	return h
}

func (m Model) renderTabs() string {
	active := lipgloss.NewStyle().Background(m.theme.Accent).Foreground(lipgloss.Color("0")).Bold(true)
	inactive := m.theme.dimStyle()
	parts := make([]string, 0, len(allTabs))
	for _, t := range allTabs {
		label := fmt.Sprintf(" %s ", t)
		if t != TabHome {
			label = fmt.Sprintf(" %s (%d) ", t, m.snap.count(t))
		}
		if t == m.tab {
			parts = append(parts, active.Render(label))
		} else {
			parts = append(parts, inactive.Render(label))
		}
	}
	return strings.Join(parts, " ")
}

// truncate shortens value to at most max display columns, on rune boundaries,
// appending an ellipsis when it cuts. It is display-width aware (multi-byte runes
// and wide glyphs are measured correctly), so it never splits a UTF-8 rune.
func truncate(value string, max int) string {
	if max <= 0 {
		return ""
	}
	return runewidth.Truncate(value, max, "...")
}
