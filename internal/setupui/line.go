package setupui

import (
	"strings"
	"unicode"

	"github.com/mattn/go-runewidth"

	"github.com/entireio/entire-brain/internal/tui"
)

// Glyph is the state marker in a row's first column.
type Glyph int

const (
	// GlyphNone leaves the column blank. A running prompt and every
	// continuation row use it: they are not outcomes, and giving them an
	// outcome glyph is how a sub-step starts to look like a phase.
	GlyphNone Glyph = iota
	// GlyphOK is a phase that did its work.
	GlyphOK
	// GlyphSkipped is a phase that was deliberately not run.
	GlyphSkipped
	// GlyphFailed is a phase that was attempted and failed.
	GlyphFailed
	// GlyphPending is a phase that has not reported yet.
	GlyphPending
)

// mark maps a glyph onto the shared palette, so a tick on a setup line is the
// same tick the dashboard draws.
func (g Glyph) mark() (tui.Mark, bool) {
	switch g {
	case GlyphOK:
		return tui.MarkDone, true
	case GlyphSkipped:
		return tui.MarkSkipped, true
	case GlyphFailed:
		return tui.MarkFailed, true
	case GlyphPending:
		return tui.MarkPending, true
	default:
		return tui.MarkPending, false
	}
}

// Line is one permanent row of setup output. Every byte this package prints is
// a Line first and bytes second; that is what makes the transcript assertable
// and what makes "behaviour identical under a pipe, minus the repaints" a
// property rather than a hope.
type Line struct {
	// Title is the phase column. Empty means a continuation row, which is
	// indented to the same column as the text of the row above it.
	Title string
	// Text is the row's content.
	Text string
	// Accent colours Title and Text.
	Accent tui.Phase
	// Glyph is the state marker, coloured by state rather than by phase.
	Glyph Glyph
	// Blank makes the row an empty separator line. Text and Title are ignored.
	Blank bool
	// Flush drops the glyph and phase columns entirely, so the text starts at
	// column zero. The refusal block uses it: a message with no phase has no
	// business reserving eleven cells for one, least of all at forty columns.
	Flush bool
}

// fallbackWidth is the width assumed when nothing can report one: a pipe, a
// journal, a terminal with no winsize. Eighty is not a guess about the reader's
// terminal, it is the narrowest width anyone still uses, and a line that fits
// eighty fits everything. Assuming "unlimited" instead is what produced the
// 202-character summary row that wrapped into mush in any split pane.
const fallbackWidth = 80

// minWidth is the narrowest width worth budgeting against. Below it the prefix
// alone consumes the row and truncation has nothing left to decide.
const minWidth = 20

// Renderer paints Lines for one destination. It wraps tui.Renderer rather than
// replacing it: every glyph, colour, dash and spinner frame still comes from
// the shared palette, and every escape sequence is still gated on that
// renderer's capabilities.
type Renderer struct {
	r     *tui.Renderer
	width int
}

// NewRenderer builds a renderer whose width comes from the destination's
// detected capabilities, falling back to COLUMNS and then to 80.
func NewRenderer(r *tui.Renderer, lookup tui.EnvLookup) *Renderer {
	return &Renderer{r: r, width: ResolveWidth(capsOf(r), lookup)}
}

// NewRendererWidth builds a renderer with an explicit width, for tests and for
// callers that have already resolved one.
func NewRendererWidth(r *tui.Renderer, width int) *Renderer {
	if width < minWidth {
		width = minWidth
	}
	return &Renderer{r: r, width: width}
}

func capsOf(r *tui.Renderer) tui.Caps {
	if r == nil {
		return tui.PlainCaps()
	}
	return r.Caps()
}

// ResolveWidth answers "how many cells may a line use" in order of authority:
// what the terminal reported, then COLUMNS, then 80.
//
// COLUMNS is honoured even for a destination that is not a terminal. That is
// deliberate: piping setup into `head` is how this gets tested, and a width
// that only applies on a TTY is a width nobody can check.
func ResolveWidth(caps tui.Caps, lookup tui.EnvLookup) int {
	if caps.Width > 0 {
		return max(caps.Width, minWidth)
	}
	if lookup != nil {
		if value, ok := lookup("COLUMNS"); ok {
			if width := parsePositiveInt(strings.TrimSpace(value)); width > 0 {
				return max(width, minWidth)
			}
		}
	}
	return fallbackWidth
}

func parsePositiveInt(s string) int {
	if s == "" {
		return 0
	}
	n := 0
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return 0
		}
		n = n*10 + int(ch-'0')
		if n > 10000 {
			return 0
		}
	}
	return n
}

// Width is the cell budget every rendered row is held to.
func (r *Renderer) Width() int {
	if r == nil || r.width <= 0 {
		return fallbackWidth
	}
	return r.width
}

// Caps reports what the destination can render.
func (r *Renderer) Caps() tui.Caps {
	if r == nil {
		return tui.PlainCaps()
	}
	return capsOf(r.r)
}

// TUI exposes the underlying palette renderer for callers composing their own
// fragments (a mark inside a sentence, say).
func (r *Renderer) TUI() *tui.Renderer {
	if r == nil {
		return nil
	}
	return r.r
}

// Bullet is the mid-dot separator, or a pipe where the locale cannot draw one.
func (r *Renderer) Bullet() string {
	if r == nil {
		return "|"
	}
	return r.r.Bullet()
}

// Dash is an em dash, or "--" where the locale cannot draw one.
func (r *Renderer) Dash() string {
	if r == nil {
		return "--"
	}
	return r.r.Dash()
}

// MarkGlyph is an uncoloured outcome glyph, for embedding in a sentence.
func (r *Renderer) MarkGlyph(m tui.Mark) string {
	if r == nil || r.r == nil {
		return "?"
	}
	return r.r.MarkGlyph(m)
}

// prefixWidth is the gutter every row shares: glyph, space, title column,
// space.
const prefixWidth = 1 + 1 + TitleWidth + 1

// gutter is the continuation indent — the column Text starts in.
func gutter() string { return strings.Repeat(" ", prefixWidth) }

// Line renders one row: glyph, phase column, text, clamped to the width
// budget. Nothing it returns can exceed Width(); the budget is applied to the
// PLAIN text and the colour is added afterwards, so a clamp can never land
// inside an escape sequence and the escape bytes are never charged as width.
func (r *Renderer) Line(l Line) string {
	if l.Blank {
		return ""
	}
	width := r.Width()
	if l.Flush {
		text := r.Truncate(l.Text, width-1)
		if l.Accent != tui.PhaseNeutral && r != nil && r.r != nil {
			text = r.r.PhasePaint(l.Accent, text)
		}
		return strings.TrimRight(text, " ")
	}

	glyph := " "
	painted := " "
	if mark, ok := l.Glyph.mark(); ok {
		glyph = r.MarkGlyph(mark)
		painted = r.paintMark(mark)
	}

	title := l.Title
	if runewidth.StringWidth(title) > TitleWidth {
		title = r.hardTruncate(title, TitleWidth)
	}
	titlePad := strings.Repeat(" ", max(0, TitleWidth-runewidth.StringWidth(title)))

	// Budget against the PLAIN glyph, never the painted one: measuring the
	// coloured form charges its SGR bytes as display width and shrinks the row
	// for no reason. One further cell of slack, because writing into the final
	// column makes some terminals wrap eagerly — and a wrapped row is the
	// exact failure this budget exists to prevent.
	gutterWidth := runewidth.StringWidth(glyph) + 1 + TitleWidth + 1
	budget := width - gutterWidth - 1
	text := r.Truncate(l.Text, budget)

	paintedTitle := title
	paintedText := text
	if l.Accent != tui.PhaseNeutral && r != nil && r.r != nil {
		paintedTitle = r.r.PhasePaint(l.Accent, title)
		paintedText = r.r.PhasePaint(l.Accent, text)
	}

	row := painted + " " + paintedTitle + titlePad + " " + paintedText
	return strings.TrimRight(row, " ")
}

// paintMark colours the glyph by OUTCOME, not by phase. Colour never carries
// meaning on its own here — the glyph already says ok/skipped/failed, and the
// colour only makes it faster to find.
func (r *Renderer) paintMark(m tui.Mark) string {
	if r == nil || r.r == nil {
		return "?"
	}
	return r.r.Mark(m)
}

// Block renders rows joined by newlines, with no trailing newline.
func (r *Renderer) Block(lines []Line) string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, r.Line(l))
	}
	return strings.Join(out, "\n")
}

// Truncate clips s to limit display cells, preferring a word or separator
// boundary and appending an ellipsis. It is cell-measured, so a CJK path or a
// Cyrillic repo name is never cut mid-rune, and it is given PLAIN text, so it
// can never cut mid-escape.
func (r *Renderer) Truncate(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if runewidth.StringWidth(s) <= limit {
		return s
	}
	ellipsis := "..."
	if r.Caps().Unicode {
		ellipsis = "…"
	}
	ellipsisWidth := runewidth.StringWidth(ellipsis)
	if limit <= ellipsisWidth {
		return r.hardTruncate(s, limit)
	}
	keep := limit - ellipsisWidth

	// Walk once, in runes, remembering the last boundary that still fits. A
	// boundary is whitespace: the separators this package composes with — the
	// mid-dot, the em dash, "--" — are all space-delimited, so one rule covers
	// every one of them without a table of glyphs to keep in sync.
	best := -1
	wide := 0
	for index, ru := range s {
		if wide > keep {
			break
		}
		if unicode.IsSpace(ru) {
			best = index
		}
		wide += runewidth.RuneWidth(ru)
	}
	if best > 0 {
		head := strings.TrimRight(s[:best], " \t·|—-")
		headWidth := runewidth.StringWidth(head)
		// A boundary cut that throws away more than half the budget is not a
		// kindness, it is data loss: "repo /Users/dev/src/acme-payments" has
		// exactly one space in it, and honouring that boundary rendered the
		// whole row as "repo...". Below half, clip the token instead — a
		// clipped path is still a path.
		if head != "" && headWidth+ellipsisWidth <= limit && headWidth*2 >= keep {
			return head + ellipsis
		}
	}
	return r.hardTruncate(s, keep) + ellipsis
}

// Wrap re-flows prose to the row budget, returning one string per rendered
// row. Phase lines TRUNCATE because a fact loses nothing by being clipped;
// prose WRAPS because an explanation that is clipped has failed at the only
// job it had. Breaking is on whitespace, so a multi-byte word is never split,
// and a single word wider than the whole budget is clipped rune-safely rather
// than allowed to overflow.
func (r *Renderer) Wrap(text string) []string {
	return r.WrapWidth(text, r.Width()-prefixWidth-1)
}

// WrapFlush re-flows prose for a row that starts at column zero.
func (r *Renderer) WrapFlush(text string) []string {
	return r.WrapWidth(text, r.Width()-1)
}

// WrapWidth re-flows prose to an explicit cell budget.
func (r *Renderer) WrapWidth(text string, budget int) []string {
	if budget < 8 {
		budget = 8
	}
	words := strings.Fields(text)
	if len(words) == 0 {
		return nil
	}
	rows := make([]string, 0, 4)
	line := ""
	for _, word := range words {
		if runewidth.StringWidth(word) > budget {
			if line != "" {
				rows = append(rows, line)
				line = ""
			}
			rows = append(rows, r.hardTruncate(word, budget))
			continue
		}
		candidate := word
		if line != "" {
			candidate = line + " " + word
		}
		if runewidth.StringWidth(candidate) > budget {
			rows = append(rows, line)
			line = word
			continue
		}
		line = candidate
	}
	if line != "" {
		rows = append(rows, line)
	}
	return rows
}

// hardTruncate is the rune-safe last resort for a single token wider than the
// whole budget.
func (r *Renderer) hardTruncate(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	return runewidth.Truncate(s, limit, "")
}

// BarWidth sizes the determinate bar to the terminal rather than to a
// constant. A fixed 16-cell bar leaves nothing for the label at 40 columns and
// wastes two thirds of a 200-column row.
func (r *Renderer) BarWidth() int {
	width := r.Width()
	bar := width / 5
	if bar < 6 {
		bar = 6
	}
	if bar > 24 {
		bar = 24
	}
	// Never let the gauge crowd out the label: the label says what is
	// happening, the gauge only says how far along it is.
	if room := width - prefixWidth - 12; bar > room {
		bar = room
	}
	if bar < 0 {
		bar = 0
	}
	return bar
}

// LiveLabel composes the transient in-place line's text: the phase column, the
// current sub-step and, when the work is countable, a width-sized bar.
//
// It returns the LABEL only. The spinner frame and the repaint belong to the
// progress reporter in internal/cli, which already owns a single process-wide
// live line; a second spinner here would fight it for the same physical row.
// The budget reserves three cells for the spinner that reporter prepends.
func (r *Renderer) LiveLabel(title, step string, done, total int) string {
	width := r.Width()
	head := title + strings.Repeat(" ", max(0, TitleWidth-runewidth.StringWidth(title))) + " "
	var gauge string
	if total > 0 {
		if bar := r.BarWidth(); bar > 0 {
			gauge = r.barText(done, total, bar) + " " + ratio(done, total) + " "
		}
	}
	// The reporter prepends "<spinner> " (two cells) and keeps one cell of
	// slack of its own; three is the whole of what it takes from us.
	budget := width - 3 - runewidth.StringWidth(head) - runewidth.StringWidth(stripColor(gauge))
	if budget < 0 {
		budget = 0
	}
	return head + gauge + r.Truncate(step, budget)
}

func (r *Renderer) barText(done, total, width int) string {
	if r == nil || r.r == nil {
		return strings.Repeat("#", width)
	}
	return r.r.Bar(done, total, width)
}

func ratio(done, total int) string {
	return itoa(done) + "/" + itoa(total)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// stripColor removes SGR sequences so a composed fragment can be MEASURED.
// Painting happens last everywhere in this file, but Bar paints per cell, so
// the one gauge we assemble has to be measured through this.
func stripColor(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && !((s[j] >= 'a' && s[j] <= 'z') || (s[j] >= 'A' && s[j] <= 'Z')) {
				j++
			}
			if j < len(s) {
				j++
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// StripANSI removes SGR sequences from rendered output. Tests measure through
// it; so should anything that logs a rendered frame.
func StripANSI(s string) string { return stripColor(s) }
