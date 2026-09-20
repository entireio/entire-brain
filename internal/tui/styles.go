package tui

import (
	"net/url"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Theme is the color palette for the dashboard. Themes are selectable via the
// --theme flag (or ENTIRE_BRAIN_THEME); the default is a neutral dark scheme
// that works on any 256-color terminal.
type Theme struct {
	Name    string
	Border  lipgloss.Color // pane borders
	Accent  lipgloss.Color // titles, selected row, active tab
	Text    lipgloss.Color // primary text
	Dim     lipgloss.Color // secondary/muted text
	Good    lipgloss.Color // healthy / fresh / present
	OK      lipgloss.Color // partial / stale
	Bad     lipgloss.Color // missing / failed
	Flag    lipgloss.Color // warnings / superseded
	Link    lipgloss.Color // clickable links
	Heading lipgloss.Color // detail section headings
	// Alt is the second accent (magenta/purple family). It exists so a
	// line-oriented renderer can colour-code MORE than the four states the
	// dashboard needs — the backfill phase has to be distinguishable from the
	// daemon phase at a glance, and both are "in progress".
	Alt lipgloss.Color
}

var themes = map[string]Theme{
	"default": {
		Name:    "default",
		Border:  lipgloss.Color("244"),
		Accent:  lipgloss.Color("39"),
		Text:    lipgloss.Color("231"), // bright white
		Dim:     lipgloss.Color("250"), // light gray (still readable)
		Good:    lipgloss.Color("47"),
		OK:      lipgloss.Color("214"),
		Bad:     lipgloss.Color("203"),
		Flag:    lipgloss.Color("220"),
		Link:    lipgloss.Color("45"),
		Heading: lipgloss.Color("39"),
		Alt:     lipgloss.Color("170"),
	},
	"catppuccin": {
		Name:    "catppuccin",
		Border:  lipgloss.Color("#6c7086"),
		Accent:  lipgloss.Color("#89b4fa"),
		Text:    lipgloss.Color("#ffffff"), // white
		Dim:     lipgloss.Color("#bac2de"),
		Good:    lipgloss.Color("#a6e3a1"),
		OK:      lipgloss.Color("#f9e2af"),
		Bad:     lipgloss.Color("#f38ba8"),
		Flag:    lipgloss.Color("#fab387"),
		Link:    lipgloss.Color("#89dceb"),
		Heading: lipgloss.Color("#cba6f7"),
		Alt:     lipgloss.Color("#cba6f7"),
	},
	"gruvbox": {
		Name:    "gruvbox",
		Border:  lipgloss.Color("#928374"),
		Accent:  lipgloss.Color("#83a598"),
		Text:    lipgloss.Color("#fbf1c7"), // bright cream-white
		Dim:     lipgloss.Color("#d5c4a1"),
		Good:    lipgloss.Color("#b8bb26"),
		OK:      lipgloss.Color("#fabd2f"),
		Bad:     lipgloss.Color("#fb4934"),
		Flag:    lipgloss.Color("#fe8019"),
		Link:    lipgloss.Color("#8ec07c"),
		Heading: lipgloss.Color("#d3869b"),
		Alt:     lipgloss.Color("#d3869b"),
	},
	"tokyonight": {
		Name:    "tokyonight",
		Border:  lipgloss.Color("#565f89"),
		Accent:  lipgloss.Color("#7aa2f7"),
		Text:    lipgloss.Color("#ffffff"), // white
		Dim:     lipgloss.Color("#a9b1d6"),
		Good:    lipgloss.Color("#9ece6a"),
		OK:      lipgloss.Color("#e0af68"),
		Bad:     lipgloss.Color("#f7768e"),
		Flag:    lipgloss.Color("#ff9e64"),
		Link:    lipgloss.Color("#7dcfff"),
		Heading: lipgloss.Color("#bb9af7"),
		Alt:     lipgloss.Color("#bb9af7"),
	},
}

// ThemeByName resolves a theme by name (case-insensitive), falling back to the
// default scheme for an empty or unknown name. The bool reports whether the name
// matched a known theme.
func ThemeByName(name string) (Theme, bool) {
	if t, ok := themes[normalizeThemeName(name)]; ok {
		return t, true
	}
	return themes["default"], name == "" || normalizeThemeName(name) == "default"
}

// ThemeNames lists the available theme names (for help text / flag docs),
// derived from the themes map so it can never drift out of sync with it. The
// default scheme is listed first (it is the fallback); the rest are sorted.
func ThemeNames() []string {
	rest := make([]string, 0, len(themes))
	for name := range themes {
		if name != "default" {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	return append([]string{"default"}, rest...)
}

func normalizeThemeName(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z':
			out = append(out, r+('a'-'A'))
		case r == ' ' || r == '-' || r == '_':
			// fold separators so "tokyo night" == "tokyo-night" == "tokyonight"
		default:
			out = append(out, r)
		}
	}
	return string(out)
}

// healthColor maps a source/freshness state to the theme's good/ok/bad color.
// "fresh"/"present"/"ok" are good; "stale"/"partial"/"degraded" are ok; the rest
// (missing, failed, error) are bad.
func (t Theme) healthColor(state string) lipgloss.Color {
	switch normalizeThemeName(state) {
	case "fresh", "present", "ok", "healthy", "clean", "current", "true":
		return t.Good
	case "stale", "partial", "degraded", "warning", "pending":
		return t.OK
	default:
		return t.Bad
	}
}

func (t Theme) titleStyle() lipgloss.Style {
	return lipgloss.NewStyle().Bold(true).Foreground(t.Accent)
}

func (t Theme) headingStyle() lipgloss.Style {
	return lipgloss.NewStyle().Bold(true).Foreground(t.Heading)
}

func (t Theme) dimStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(t.Dim)
}

func (t Theme) textStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(t.Text)
}

func (t Theme) flagStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(t.Flag)
}

func (t Theme) linkStyle() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(t.Link).Underline(true)
}

// osc8 wraps label in an OSC 8 terminal hyperlink so terminals that support it
// (iTerm2, kitty, WezTerm, modern Terminal.app, VS Code) make it cmd/ctrl-click
// to open. Terminals that don't support it just show the label. The escape
// sequences are zero-width, so callers must not pass the result through a
// width-constrained lipgloss style (it would miscount and wrap).
func osc8(url, label string) string {
	if url == "" {
		return label
	}
	return "\x1b]8;;" + url + "\x1b\\" + label + "\x1b]8;;\x1b\\"
}

// fileURL encodes a local source path for OSC 8 links. Normalize Windows
// separators and drive prefixes before escaping reserved URL characters.
func fileURL(p string) string {
	if p == "" {
		return ""
	}
	u := strings.ReplaceAll(p, `\`, "/")
	if !strings.HasPrefix(u, "/") {
		u = "/" + u
	}
	return (&url.URL{Scheme: "file", Path: u}).String()
}

// paneStyle is a rounded-border pane in the theme's border color (accent when
// focused).
func (t Theme) paneStyle(focused bool) lipgloss.Style {
	color := t.Border
	if focused {
		color = t.Accent
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(color)
}
