package tui

import (
	"fmt"
	"path"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// renderDetail renders the detail page for the selected entry on a tab. width is
// the content width inside the detail viewport. All content is deterministic —
// it only formats data already present in the snapshot.
func renderDetail(th Theme, s Snapshot, tab Tab, idx, width int) string {
	if width < 20 {
		width = 20
	}
	switch tab {
	case TabHome:
		return renderHome(th, s, width)
	case TabFacts:
		if idx >= 0 && idx < len(s.Facts) {
			return renderFact(th, s.Facts[idx], width)
		}
	case TabSessions:
		if idx >= 0 && idx < len(s.Sessions) {
			return renderSession(th, s.Sessions[idx], width)
		}
	case TabHistory:
		if idx >= 0 && idx < len(s.History) {
			return renderHistory(th, s.History[idx], width)
		}
	case TabSemantic:
		if idx >= 0 && idx < len(s.Semantic) {
			return renderSemantic(th, s.Semantic[idx], width)
		}
	case TabSearch:
		if idx >= 0 && idx < len(s.Search) {
			return renderSearchResult(th, s.Search[idx], width)
		}
		return renderSearchEmpty(th, s)
	}
	return th.dimStyle().Render("Nothing to show in this section.")
}

// renderSearchEmpty is the Search tab's detail when there are no results: a
// prompt to search, or the last query's "no results"/error state.
func renderSearchEmpty(th Theme, s Snapshot) string {
	var b strings.Builder
	b.WriteString(sectionHeader(th, "Search"))
	b.WriteString("\n")
	if s.SearchErr != "" {
		b.WriteString("  " + th.flagStyle().Render("⚠ "+s.SearchErr) + "\n\n")
	} else if s.SearchQuery != "" {
		b.WriteString("  " + th.dimStyle().Render(fmt.Sprintf("no results for %q", s.SearchQuery)) + "\n\n")
	}
	b.WriteString(th.textStyle().Render("  Press ") + th.titleStyle().Render("s") +
		th.textStyle().Render(" to search facts, history, and docs."))
	b.WriteString("\n")
	return b.String()
}

func renderSearchResult(th Theme, r SearchResult, width int) string {
	wrap := lipgloss.NewStyle().Width(width).Foreground(th.Text)
	var b strings.Builder

	b.WriteString(th.titleStyle().Render(orDash(r.Source)))
	if r.Score > 0 {
		b.WriteString(th.dimStyle().Render(fmt.Sprintf("  score %.3f", r.Score)))
	}
	b.WriteString("\n")
	b.WriteString(th.dimStyle().Render(r.ID))
	b.WriteString("\n")
	if r.Path != "" {
		ref := r.Path
		if r.Line > 0 {
			ref += fmt.Sprintf(":%d", r.Line)
		}
		b.WriteString(th.dimStyle().Render(truncate(ref, width)))
		b.WriteString("\n")
	}
	b.WriteString("\n")

	if r.Heading != "" {
		b.WriteString(th.headingStyle().Render(truncate(r.Heading, width)))
		b.WriteString("\n")
	}
	b.WriteString(sectionHeader(th, "Match"))
	b.WriteString("\n")
	b.WriteString(wrap.Render(th.textStyle().Render(r.Text)))
	b.WriteString("\n\n")

	writeOpenHint(&b, th, r.OpenPath)
	return b.String()
}

func renderHome(th Theme, s Snapshot, width int) string {
	wrap := lipgloss.NewStyle().Width(width).Foreground(th.Text)
	var b strings.Builder

	b.WriteString(th.titleStyle().Render(orDash(s.Repo)))
	b.WriteString("\n")
	meta := "branch " + orDash(s.Branch)
	if s.GeneratedAt != "" {
		meta += " · built " + s.GeneratedAt
	}
	b.WriteString(th.dimStyle().Render(truncate(meta, width)))
	b.WriteString("\n\n")

	b.WriteString(sectionHeader(th, "Sources"))
	b.WriteString("\n")
	for _, src := range s.Home.Sources {
		dot := "○"
		color := th.Bad
		if src.Present {
			dot, color = "●", th.Good
		}
		line := lipgloss.NewStyle().Foreground(color).Render("  "+dot+" ") +
			th.textStyle().Render(fmt.Sprintf("%-9s", src.Name))
		if src.Detail != "" {
			line += "  " + th.dimStyle().Render(src.Detail)
		}
		// MaxWidth measures display width and ignores ANSI escapes; truncate()
		// (runewidth) would count the escape bytes and eat the visible text.
		b.WriteString(lipgloss.NewStyle().MaxWidth(width).Render(line))
		b.WriteString("\n")
	}
	b.WriteString("\n")

	if s.Home.Freshness != "" || len(s.Home.Axes) > 0 {
		b.WriteString(sectionHeader(th, "Freshness"))
		b.WriteString("\n")
		if s.Home.Freshness != "" {
			b.WriteString("  " + lipgloss.NewStyle().Foreground(th.healthColor(s.Home.Freshness)).Render(s.Home.Freshness) + "\n")
		}
		for _, ax := range s.Home.Axes {
			line := "    " + ax.Name + ": " + ax.State
			if ax.Detail != "" {
				line += " (" + ax.Detail + ")"
			}
			b.WriteString(th.dimStyle().Render(truncate(line, width)) + "\n")
		}
		b.WriteString("\n")
	}

	if s.Home.Live.Branch != "" || s.Home.Live.Head != "" || s.Home.Live.Summary != "" {
		b.WriteString(sectionHeader(th, "Working tree"))
		b.WriteString("\n")
		state := "clean"
		color := th.Good
		if s.Home.Live.Dirty {
			state, color = orDash(s.Home.Live.Summary), th.OK
		}
		b.WriteString("  " + th.dimStyle().Render("branch: ") + th.textStyle().Render(orDash(s.Home.Live.Branch)) + "\n")
		if s.Home.Live.Head != "" {
			b.WriteString("  " + th.dimStyle().Render("head: ") + th.textStyle().Render(shortID(s.Home.Live.Head)) + "\n")
		}
		b.WriteString("  " + th.dimStyle().Render("status: ") + lipgloss.NewStyle().Foreground(color).Render(state) + "\n\n")
	}

	if len(s.Home.BlindSpots) > 0 {
		b.WriteString(sectionHeader(th, "Blind spots"))
		b.WriteString("\n")
		for _, bs := range s.Home.BlindSpots {
			b.WriteString(wrap.Render("  " + th.flagStyle().Render("• "+bs)))
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	writeWarnings(&b, th, wrap, s.Home.Warnings)
	writeNotes(&b, th, s.Notes)
	return b.String()
}

func renderFact(th Theme, f FactView, width int) string {
	wrap := lipgloss.NewStyle().Width(width).Foreground(th.Text)
	var b strings.Builder

	head := orDash(f.Kind)
	if f.Status != "" {
		head += "  " + statusBadge(th, f.Status)
	}
	b.WriteString(th.titleStyle().Render(head))
	b.WriteString("\n")
	if len(f.Paths) > 0 {
		b.WriteString(th.dimStyle().Render(truncate(strings.Join(f.Paths, " · "), width)))
		b.WriteString("\n")
	}
	b.WriteString(th.dimStyle().Render(f.ID))
	b.WriteString("\n\n")

	b.WriteString(sectionHeader(th, "Fact"))
	b.WriteString("\n")
	b.WriteString(wrap.Render(th.textStyle().Render(f.Text)))
	b.WriteString("\n\n")

	b.WriteString(sectionHeader(th, "Details"))
	b.WriteString("\n")
	metric(&b, th, "origin", orDash(f.Origin))
	if f.Confidence != "" {
		metric(&b, th, "confidence", f.Confidence)
	}
	if len(f.Locus) > 0 {
		metric(&b, th, "locus", strings.Join(f.Locus, ", "))
	}
	if len(f.Related) > 0 {
		metric(&b, th, "related", strings.Join(f.Related, ", "))
	}
	b.WriteString("\n")

	if len(f.Provenance) > 0 {
		b.WriteString(sectionHeader(th, "Provenance"))
		b.WriteString("\n")
		for _, a := range f.Provenance {
			line := "  ↳ "
			if a.SessionID != "" {
				line += "session " + shortID(a.SessionID)
			}
			if a.Commit != "" {
				line += " · " + shortID(a.Commit)
			}
			if a.Transcript != "" {
				ref := a.Transcript
				if a.Line > 0 {
					ref += fmt.Sprintf(":%d", a.Line)
				}
				line += " · " + ref
			}
			if a.Verified {
				line += " ✓"
			}
			b.WriteString(th.dimStyle().Render(truncate(line, width)))
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	writeOpenHint(&b, th, f.Source)
	return b.String()
}

func renderSession(th Theme, v SessionView, width int) string {
	wrap := lipgloss.NewStyle().Width(width).Foreground(th.Text)
	var b strings.Builder

	b.WriteString(th.titleStyle().Render(orDash(v.Agent)))
	if v.Model != "" {
		b.WriteString(th.dimStyle().Render("  (" + v.Model + ")"))
	}
	b.WriteString("\n")
	meta := orDash(v.Created)
	if v.Branch != "" {
		meta += " · " + v.Branch
	}
	if v.Kind != "" {
		meta += " · " + v.Kind
	}
	b.WriteString(th.dimStyle().Render(truncate(meta, width)))
	b.WriteString("\n")
	b.WriteString(th.dimStyle().Render(truncate(v.ID, width)))
	b.WriteString("\n\n")

	if v.Intent != "" || v.Outcome != "" {
		b.WriteString(sectionHeader(th, "Summary"))
		b.WriteString("\n")
		if v.Intent != "" {
			b.WriteString(wrap.Render(th.dimStyle().Render("intent: ") + th.textStyle().Render(v.Intent)))
			b.WriteString("\n")
		}
		if v.Outcome != "" {
			b.WriteString(wrap.Render(th.dimStyle().Render("outcome: ") + th.textStyle().Render(v.Outcome)))
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	b.WriteString(sectionHeader(th, "Activity"))
	b.WriteString("\n")
	metric(&b, th, "checkpoints", fmt.Sprintf("%d", v.Checkpoints))
	metric(&b, th, "files touched", fmt.Sprintf("%d", len(v.Files)))
	if v.InputTok > 0 || v.OutputTok > 0 {
		metric(&b, th, "tokens", fmt.Sprintf("%d in / %d out", v.InputTok, v.OutputTok))
	}
	b.WriteString("\n")

	if len(v.Files) > 0 {
		b.WriteString(sectionHeader(th, "Files"))
		b.WriteString("\n")
		for _, f := range v.Files {
			b.WriteString(th.dimStyle().Render(truncate("  "+f, width)))
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	writeOpenHint(&b, th, v.Source)
	return b.String()
}

func renderHistory(th Theme, h HistoryView, width int) string {
	wrap := lipgloss.NewStyle().Width(width).Foreground(th.Text)
	var b strings.Builder

	b.WriteString(th.titleStyle().Render(orDash(h.Kind)))
	b.WriteString("\n")
	if h.Branch != "" {
		b.WriteString(th.dimStyle().Render("branch " + h.Branch))
		b.WriteString("\n")
	}
	ref := h.Path
	if h.Line > 0 {
		ref += fmt.Sprintf(":%d", h.Line)
	}
	b.WriteString(th.dimStyle().Render(truncate(ref, width)))
	b.WriteString("\n\n")

	b.WriteString(sectionHeader(th, "Summary"))
	b.WriteString("\n")
	b.WriteString(wrap.Render(th.textStyle().Render(orDash(h.Summary))))
	b.WriteString("\n\n")

	writeOpenHint(&b, th, h.Source)
	return b.String()
}

func renderSemantic(th Theme, v SemanticView, width int) string {
	wrap := lipgloss.NewStyle().Width(width).Foreground(th.Text)
	var b strings.Builder

	b.WriteString(th.titleStyle().Render(orDash(v.Name)))
	b.WriteString(th.dimStyle().Render("  " + orDash(v.Kind)))
	b.WriteString("\n")
	if v.QualifiedName != "" && v.QualifiedName != v.Name {
		b.WriteString(th.dimStyle().Render(truncate(v.QualifiedName, width)))
		b.WriteString("\n")
	}
	loc := v.FilePath
	if v.StartLine > 0 {
		loc += fmt.Sprintf(":%d", v.StartLine)
	}
	b.WriteString(th.dimStyle().Render(truncate(loc, width)))
	b.WriteString("\n\n")

	b.WriteString(sectionHeader(th, "Details"))
	b.WriteString("\n")
	if v.Language != "" {
		metric(&b, th, "language", v.Language)
	}
	if v.StartLine > 0 {
		span := fmt.Sprintf("%d", v.StartLine)
		if v.EndLine > v.StartLine {
			span = fmt.Sprintf("%d–%d", v.StartLine, v.EndLine)
		}
		metric(&b, th, "lines", span)
	}
	b.WriteString("\n")

	if v.Signature != "" {
		b.WriteString(sectionHeader(th, "Signature"))
		b.WriteString("\n")
		b.WriteString(wrap.Render(th.textStyle().Render(v.Signature)))
		b.WriteString("\n\n")
	}

	writeOpenHint(&b, th, v.Source)
	return b.String()
}

// --- small render helpers --------------------------------------------------

func sectionHeader(th Theme, title string) string {
	return th.headingStyle().Render("▌ " + title)
}

func metric(b *strings.Builder, th Theme, label, value string) {
	b.WriteString("  " + th.dimStyle().Render(label+": ") + th.textStyle().Render(value) + "\n")
}

func statusBadge(th Theme, status string) string {
	style := th.textStyle()
	switch status {
	case "active":
		style = lipgloss.NewStyle().Foreground(th.Good)
	case "superseded", "retracted":
		style = th.flagStyle()
	}
	return style.Render(status)
}

func writeWarnings(b *strings.Builder, th Theme, wrap lipgloss.Style, warnings []string) {
	if len(warnings) == 0 {
		return
	}
	b.WriteString(sectionHeader(th, "Warnings"))
	b.WriteString("\n")
	for _, w := range warnings {
		b.WriteString(wrap.Render("  " + th.flagStyle().Render("⚠ "+w)))
		b.WriteString("\n")
	}
	b.WriteString("\n")
}

func writeNotes(b *strings.Builder, th Theme, notes []string) {
	for _, n := range notes {
		b.WriteString(th.dimStyle().Render(truncate("· "+n, 200)))
		b.WriteString("\n")
	}
}

func writeOpenHint(b *strings.Builder, th Theme, source string) {
	if source == "" {
		return
	}
	// Render the source as an OSC 8 hyperlink (cmd/ctrl-click in supporting
	// terminals) and note the `o` keybinding, which is the reliable opener when
	// mouse capture keeps the terminal from delivering clicks to the link. The
	// link is written raw — OSC 8 escapes are zero-width, so it must not pass
	// through a width-constrained style.
	link := osc8(fileURL(source), th.linkStyle().Render("↗ open source"))
	b.WriteString(link + th.dimStyle().Render("   (press o)"))
	b.WriteString("\n")
}

// orDash returns value or an em dash when empty.
func orDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "—"
	}
	return value
}

// oneLine collapses internal newlines/whitespace runs to single spaces for a
// table-cell preview.
func oneLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// shortID trims a long id/hash to its first 12 characters for display.
func shortID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}

// baseName returns the final path segment of a slash path (the file name).
func baseName(p string) string {
	if p == "" {
		return "—"
	}
	return path.Base(p)
}
