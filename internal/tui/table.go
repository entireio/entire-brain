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

// rowsFor builds the table rows for a tab, restricted to the given source
// indices (the filtered view). Cells are truncated to roughly their column
// width; the table re-truncates to the exact width when it renders.
func rowsFor(s Snapshot, tab Tab, indices []int) []table.Row {
	cols := columnsFor(tab, 80)
	rows := make([]table.Row, 0, len(indices))
	for _, i := range indices {
		switch tab {
		case TabHome:
			h := s.Home.Sources[i]
			dot := "○"
			if h.Present {
				dot = "●"
			}
			rows = append(rows, table.Row{dot, truncate(h.Name, cols[1].Width), truncate(h.Detail, cols[2].Width)})
		case TabFacts:
			f := s.Facts[i]
			rows = append(rows, table.Row{
				truncate(orDash(f.Kind), cols[0].Width),
				truncate(oneLine(f.Text), cols[1].Width),
				truncate(orDash(f.Status), cols[2].Width),
			})
		case TabSessions:
			v := s.Sessions[i]
			rows = append(rows, table.Row{
				truncate(v.Created, cols[0].Width),
				truncate(orDash(v.Agent), cols[1].Width),
				fmt.Sprintf("%d", len(v.Files)),
				truncate(shortID(v.ID), cols[3].Width),
			})
		case TabHistory:
			h := s.History[i]
			rows = append(rows, table.Row{
				truncate(orDash(h.Kind), cols[0].Width),
				truncate(oneLine(h.Summary), cols[1].Width),
			})
		case TabSemantic:
			v := s.Semantic[i]
			rows = append(rows, table.Row{
				truncate(orDash(v.Kind), cols[0].Width),
				truncate(orDash(v.Name), cols[1].Width),
				truncate(baseName(v.FilePath), cols[2].Width),
			})
		case TabSearch:
			r := s.Search[i]
			rows = append(rows, table.Row{
				truncate(orDash(r.Source), cols[0].Width),
				truncate(oneLine(r.Text), cols[1].Width),
			})
		}
	}
	return rows
}
