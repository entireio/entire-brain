package tui

import (
	"fmt"
	"sort"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/lipgloss"
)

// newBrainTable builds the left-pane table, themed and focused. It starts with
// placeholder columns so rows can be set before the first resize (which
// recomputes the real widths); the table panics rendering rows against zero
// columns otherwise.
func newBrainTable(th Theme) table.Model {
	t := table.New(table.WithFocused(true), table.WithColumns(columnsFor(TabHome, 36)))
	s := table.DefaultStyles()
	s.Header = s.Header.
		Foreground(th.Dim).
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(th.Border).
		BorderBottom(true).
		Bold(false)
	s.Selected = s.Selected.
		Foreground(lipgloss.Color("0")).
		Background(th.Accent).
		Bold(true)
	s.Cell = s.Cell.Foreground(th.Text)
	t.SetStyles(s)
	return t
}

// colSpec is one column's layout intent, resolved into a real width by
// layoutColumns. want is its comfortable width, min the narrowest width at
// which it still says something, and rank its drop order — the LOWEST rank
// gives up width first and is the first to disappear entirely, so the column
// carrying an entry's identity is the last thing a narrow pane takes away. At
// most one column is flex: it absorbs whatever is left over.
type colSpec struct {
	title string
	want  int
	min   int
	rank  int
	flex  bool
}

// layoutColumns fits a column layout into a table of inner display columns.
//
// bubbles/table pads every cell by one column on each side and never wraps a
// row itself: if the widths plus that padding exceed the table's width, the
// header and every row simply spill past the pane's right border and the
// TERMINAL wraps them, shredding the left pane. The previous layout floored
// each flexible column at a minimum without re-checking the total, so Facts
// overflowed below ~123 columns and Sessions and Semantic below ~150 — that is,
// on every ordinary terminal. This budgets instead of flooring, so the sum is
// never larger than the space there is.
//
// Width is surrendered in this order: fixed columns shrink want -> min lowest
// rank first, then drop min -> 0 lowest rank first, and only then does the flex
// column give up its own minimum. A zero-width column is skipped by both
// headersView and renderRow in bubbles/table, so dropping one keeps the header
// and the rows aligned. The column COUNT never changes: renderRow indexes
// m.cols[i] for every cell i of a row, so returning fewer columns than a tab's
// rows have cells would panic.
func layoutColumns(specs []colSpec, inner int) []table.Column {
	out := make([]table.Column, len(specs))
	for i, s := range specs {
		out[i] = table.Column{Title: s.title}
	}
	// bubbles/table's Cell/Header styles pad 1 cell either side of every column.
	budget := inner - 2*len(specs)
	if budget <= 0 {
		return out // nothing fits; every column renders empty rather than wrapped
	}

	flexIdx, used := -1, 0
	for i, s := range specs {
		if s.flex {
			flexIdx = i
			out[i].Width = s.min
			used += s.min
			continue
		}
		out[i].Width = s.want
		used += s.want
	}

	// Shrink fixed columns toward their minimums, cheapest column first.
	for _, i := range specOrderByRank(specs) {
		if used <= budget {
			break
		}
		give := out[i].Width - specs[i].min
		if give <= 0 {
			continue
		}
		if give > used-budget {
			give = used - budget
		}
		out[i].Width -= give
		used -= give
	}
	// Still over: drop fixed columns entirely, cheapest first.
	for _, i := range specOrderByRank(specs) {
		if used <= budget {
			break
		}
		used -= out[i].Width
		out[i].Width = 0
	}
	if flexIdx < 0 {
		return out
	}
	// Whatever is left over belongs to the flex column — and if the budget is so
	// small that even its minimum does not fit, it gives that up too.
	out[flexIdx].Width += budget - used
	if out[flexIdx].Width < 0 {
		out[flexIdx].Width = 0
	}
	return out
}

// specOrderByRank lists the fixed (non-flex) column indexes cheapest first:
// ascending rank, then right to left so a tie gives up the outermost column.
func specOrderByRank(specs []colSpec) []int {
	order := make([]int, 0, len(specs))
	for i, s := range specs {
		if !s.flex {
			order = append(order, i)
		}
	}
	sort.SliceStable(order, func(a, b int) bool {
		left, right := order[a], order[b]
		if specs[left].rank != specs[right].rank {
			return specs[left].rank < specs[right].rank
		}
		return left > right
	})
	return order
}

// columnsFor lays out the columns for a tab at a given inner table width. Column
// titles double as the per-tab header.
func columnsFor(tab Tab, inner int) []table.Column {
	switch tab {
	case TabHome:
		return layoutColumns([]colSpec{
			{title: "", want: 2, min: 2, rank: 3},
			{title: "Source", want: 14, min: 8, rank: 2},
			{title: "Detail", min: 8, flex: true},
		}, inner)
	case TabFacts:
		return layoutColumns([]colSpec{
			{title: "Kind", want: 11, min: 4, rank: 2},
			{title: "Fact", min: 12, flex: true},
			{title: "Status", want: 10, min: 5, rank: 1},
		}, inner)
	case TabSessions:
		return layoutColumns([]colSpec{
			{title: "When", want: 16, min: 10, rank: 3},
			{title: "Agent", want: 12, min: 6, rank: 2},
			{title: "Files", want: 5, min: 3, rank: 1},
			{title: "Session", min: 8, flex: true},
		}, inner)
	case TabHistory:
		return layoutColumns([]colSpec{
			{title: "Kind", want: 12, min: 6, rank: 1},
			{title: "Summary", min: 12, flex: true},
		}, inner)
	case TabSemantic:
		return layoutColumns([]colSpec{
			{title: "Kind", want: 10, min: 4, rank: 1},
			{title: "Name", min: 10, flex: true},
			{title: "File", want: 22, min: 8, rank: 2},
		}, inner)
	case TabSearch:
		return layoutColumns([]colSpec{
			{title: "Source", want: 9, min: 4, rank: 1},
			{title: "Result", min: 12, flex: true},
		}, inner)
	default:
		return layoutColumns([]colSpec{{title: "", min: 8, flex: true}}, inner)
	}
}

// maxCellPreview bounds a table cell's length before it is handed to the table.
// It is a performance guard against a pathologically long value (e.g. a
// multi-kilobyte fact text), not a layout limit: it is far wider than any
// terminal column, and the table truncates every cell to its exact column width
// on render (runewidth.Truncate in bubbles/table). So the visible width always
// tracks the live layout — never a fixed guess — while the table never has to
// process a huge string.
const maxCellPreview = 512

// rowsFor builds the table rows for a tab, restricted to the given source
// indices (the filtered view). Each cell is collapsed to one line and bounded to
// maxCellPreview; the table re-truncates to the exact column width when it
// renders.
func rowsFor(s Snapshot, tab Tab, indices []int) []table.Row {
	rows := make([]table.Row, 0, len(indices))
	for _, i := range indices {
		switch tab {
		case TabHome:
			h := s.Home.Sources[i]
			rows = append(rows, table.Row{sourceDot(h), cell(h.Name), cell(h.Detail)})
		case TabFacts:
			f := s.Facts[i]
			rows = append(rows, table.Row{cell(orDash(f.Kind)), cell(f.Text), cell(orDash(f.Status))})
		case TabSessions:
			v := s.Sessions[i]
			rows = append(rows, table.Row{
				cell(v.Created),
				cell(orDash(v.Agent)),
				fmt.Sprintf("%d", len(v.Files)),
				cell(shortID(v.ID)),
			})
		case TabHistory:
			h := s.History[i]
			rows = append(rows, table.Row{cell(orDash(h.Kind)), cell(h.Summary)})
		case TabSemantic:
			v := s.Semantic[i]
			rows = append(rows, table.Row{cell(orDash(v.Kind)), cell(orDash(v.Name)), cell(baseName(v.FilePath))})
		case TabSearch:
			r := s.Search[i]
			rows = append(rows, table.Row{cell(orDash(r.Source)), cell(r.Text)})
		}
	}
	return rows
}

// cell collapses a value to a single line and bounds it to maxCellPreview for
// the table; the table does the exact per-column truncation on render.
func cell(value string) string {
	return truncate(oneLine(value), maxCellPreview)
}
