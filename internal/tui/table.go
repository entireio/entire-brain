package tui

import (
	"fmt"

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

// flexWidth budgets a flexible column: the inner table width minus the fixed
// columns and bubbles/table's 2-cell-per-column padding, floored at min.
func flexWidth(inner, fixedSum, numCols, min int) int {
	w := inner - fixedSum - 2*numCols
	if w < min {
		return min
	}
	return w
}

// columnsFor lays out the columns for a tab at a given inner table width. Column
// titles double as the per-tab header.
func columnsFor(tab Tab, inner int) []table.Column {
	switch tab {
	case TabHome:
		name := 14
		return []table.Column{
			{Title: "", Width: 2},
			{Title: "Source", Width: name},
			{Title: "Detail", Width: flexWidth(inner, 2+name, 3, 8)},
		}
	case TabFacts:
		kind, status := 11, 10
		return []table.Column{
			{Title: "Kind", Width: kind},
			{Title: "Fact", Width: flexWidth(inner, kind+status, 3, 12)},
			{Title: "Status", Width: status},
		}
	case TabSessions:
		date, agent, files := 16, 12, 5
		return []table.Column{
			{Title: "When", Width: date},
			{Title: "Agent", Width: agent},
			{Title: "Files", Width: files},
			{Title: "Session", Width: flexWidth(inner, date+agent+files, 4, 8)},
		}
	case TabHistory:
		kind := 12
		return []table.Column{
			{Title: "Kind", Width: kind},
			{Title: "Summary", Width: flexWidth(inner, kind, 2, 12)},
		}
	case TabSemantic:
		kind, file := 10, 22
		return []table.Column{
			{Title: "Kind", Width: kind},
			{Title: "Name", Width: flexWidth(inner, kind+file, 3, 10)},
			{Title: "File", Width: file},
		}
	case TabSearch:
		source := 9
		return []table.Column{
			{Title: "Source", Width: source},
			{Title: "Result", Width: flexWidth(inner, source, 2, 12)},
		}
	default:
		return []table.Column{{Title: "", Width: max(inner-2, 8)}}
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
			dot := "○"
			if h.Present {
				dot = "●"
			}
			rows = append(rows, table.Row{dot, cell(h.Name), cell(h.Detail)})
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
