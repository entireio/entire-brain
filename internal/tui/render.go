package tui

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	xterm "github.com/charmbracelet/x/term"
	"github.com/mattn/go-isatty"
	"github.com/mattn/go-runewidth"
)

// render.go is the line-oriented half of this package. The dashboard above it
// (tui.go) owns a full-screen bubbletea program; the commands people actually
// run — `setup`, `status` — write ordinary lines to a stream that may be a
// terminal, a pipe, a CI log, a systemd journal or a hook whose stdout is
// contractually empty. They still deserve colour, marks, spinners and bars when
// the destination can render them, and byte-stable plain text when it cannot.
//
// Both halves share one palette (Theme), so a component that is Good-coloured in
// the dashboard is the same colour on a setup line. What they do NOT share is
// lipgloss's implicit renderer: lipgloss decides colour from a package-global
// profile bound to os.Stdout, which makes styled output untestable (it depends
// on how the TEST process was started) and wrong for a command writing to
// stderr. Caps below is an explicit, injectable answer to "what can THIS stream
// render", and every escape sequence in this file is gated on it.

// Caps is what a destination stream can render.
type Caps struct {
	// TTY reports whether the stream is a terminal. It gates every in-place
	// repaint: a pipe gets whole lines and nothing else.
	TTY bool
	// Color reports whether ANSI colour may be emitted. TTY implies nothing —
	// NO_COLOR, TERM=dumb and a non-terminal each turn it off independently.
	Color bool
	// Unicode reports whether non-ASCII glyphs will render. A terminal whose
	// locale is not UTF-8 draws them as replacement boxes, which is where the
	// stray "□" in the spinner and the em dashes came from.
	Unicode bool
	// Width is the usable line width; 0 means unknown (do not truncate).
	Width int
}

// PlainCaps is the byte-stable mode: whole lines, ASCII, no colour. Every
// non-terminal destination resolves to exactly this, which is what makes the
// plain path safe to assert on in tests and safe to pipe into a log.
func PlainCaps() Caps { return Caps{} }

// EnvLookup is os.LookupEnv's shape. The "found" half matters: NO_COLOR is
// honoured by PRESENCE, so NO_COLOR= (empty) must still disable colour, and a
// plain getenv cannot tell that from unset.
type EnvLookup func(string) (string, bool)

// DetectCaps resolves the capabilities of w from the process environment.
func DetectCaps(w io.Writer) Caps { return DetectCapsWithEnv(w, os.LookupEnv) }

// DetectCapsWithEnv is DetectCaps with the environment injected, so the
// TTY/NO_COLOR/locale matrix is testable without mutating process state.
func DetectCapsWithEnv(w io.Writer, lookup EnvLookup) Caps {
	if lookup == nil {
		lookup = func(string) (string, bool) { return "", false }
	}
	getenv := func(key string) string {
		value, _ := lookup(key)
		return value
	}
	file, ok := w.(*os.File)
	if !ok || file == nil {
		return PlainCaps()
	}
	fd := file.Fd()
	if !isatty.IsTerminal(fd) && !isatty.IsCygwinTerminal(fd) {
		return PlainCaps()
	}
	caps := Caps{TTY: true}
	term := strings.TrimSpace(getenv("TERM"))
	// NO_COLOR is honoured by PRESENCE, per no-color.org: an empty value still
	// means "no colour". TERM=dumb and an unset TERM describe a terminal that
	// cannot be trusted with escape sequences at all, so they disable the
	// in-place repaint too, not just the colour.
	if term == "" || term == "dumb" {
		return PlainCaps()
	}
	if _, noColor := lookup("NO_COLOR"); !noColor {
		caps.Color = true
	}
	caps.Unicode = localeIsUTF8(getenv)
	caps.Width = terminalWidth(fd, getenv)
	return caps
}

// terminalWidth resolves the usable line width for a terminal, in order of
// authority: the kernel, then COLUMNS, then a conservative 80.
//
// The fallback is not optional. On a terminal of UNKNOWN width the in-place
// line is not truncated, so a long label wraps — and the carriage-return erase
// only ever clears the last physical row, leaving the wrapped remainder on
// screen under the final line. That is precisely the "every phase prints twice"
// report. Guessing 80 can clip a label on a wide terminal; not guessing
// reintroduces the bug.
func terminalWidth(fd uintptr, getenv func(string) string) int {
	if width, _, err := xterm.GetSize(fd); err == nil && width > 0 {
		return width
	}
	if columns := strings.TrimSpace(getenv("COLUMNS")); columns != "" {
		width := 0
		for _, ch := range columns {
			if ch < '0' || ch > '9' {
				width = 0
				break
			}
			width = width*10 + int(ch-'0')
		}
		if width > 0 {
			return width
		}
	}
	return 80
}

// localeIsUTF8 reports whether the locale promises UTF-8 output. The check is
// deliberately conservative: an unset or non-UTF-8 locale falls back to ASCII
// glyphs rather than gambling that the font has a spinner in it.
func localeIsUTF8(getenv func(string) string) bool {
	for _, key := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		value := strings.TrimSpace(getenv(key))
		if value == "" {
			continue
		}
		lowered := strings.ToLower(value)
		return strings.Contains(lowered, "utf-8") || strings.Contains(lowered, "utf8")
	}
	// Windows Terminal and the VS Code terminal are UTF-8 without ever setting
	// a POSIX locale.
	return strings.TrimSpace(getenv("WT_SESSION")) != "" ||
		strings.EqualFold(strings.TrimSpace(getenv("TERM_PROGRAM")), "vscode")
}

// Mark is a per-outcome status glyph.
type Mark int

const (
	// MarkDone is a component that built.
	MarkDone Mark = iota
	// MarkFailed is a component that was attempted and blew up.
	MarkFailed
	// MarkSkipped is a component that was deliberately not attempted.
	MarkSkipped
	// MarkPending is a component that has not reported yet.
	MarkPending
)

// Renderer turns semantic pieces (a mark, a bar, a phase colour) into bytes
// appropriate for one destination stream.
type Renderer struct {
	theme Theme
	caps  Caps
}

// NewRenderer builds a renderer for w, detecting what w can render.
func NewRenderer(w io.Writer) *Renderer {
	theme, _ := ThemeByName(os.Getenv("ENTIRE_BRAIN_THEME"))
	return &Renderer{theme: theme, caps: DetectCaps(w)}
}

// NewRendererWith builds a renderer with an explicit theme and capabilities.
func NewRendererWith(theme Theme, caps Caps) *Renderer {
	return &Renderer{theme: theme, caps: caps}
}

// Caps reports what this renderer's destination can render.
func (r *Renderer) Caps() Caps {
	if r == nil {
		return PlainCaps()
	}
	return r.caps
}

// Theme reports the palette in use.
func (r *Renderer) Theme() Theme {
	if r == nil {
		return themes["default"]
	}
	return r.theme
}

// Paint wraps s in the SGR sequence for c, or returns s untouched when colour
// is off. It emits the escape itself rather than delegating to lipgloss because
// lipgloss resolves colour from a process-global profile bound to os.Stdout —
// which is neither the stream we are writing to nor stable under `go test`.
func (r *Renderer) Paint(c lipgloss.Color, s string) string {
	if r == nil || !r.caps.Color || s == "" {
		return s
	}
	seq := ansiForeground(c)
	if seq == "" {
		return s
	}
	return seq + s + "\x1b[0m"
}

// Bold renders s bold when the stream supports styling.
func (r *Renderer) Bold(s string) string {
	if r == nil || !r.caps.Color || s == "" {
		return s
	}
	return "\x1b[1m" + s + "\x1b[0m"
}

// Dim renders s in the muted palette colour.
func (r *Renderer) Dim(s string) string { return r.Paint(r.Theme().Dim, s) }

// spinnerFramesUnicode is the braille spinner; spinnerFramesASCII is the
// fallback for a terminal whose locale cannot promise UTF-8. The ASCII set is
// the classic four-frame bar, which is what the "-\|/" contract in the CLI
// expects.
var (
	spinnerFramesUnicode = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	spinnerFramesASCII   = []string{"-", "\\", "|", "/"}
)

// Spinner returns the frame for tick. It is pure: the caller owns the clock,
// so a test can assert frame N without sleeping.
func (r *Renderer) Spinner(tick int) string {
	frames := spinnerFramesASCII
	if r != nil && r.caps.Unicode {
		frames = spinnerFramesUnicode
	}
	if tick < 0 {
		tick = -tick
	}
	return frames[tick%len(frames)]
}

// SpinnerFrames reports the glyph set in use, for tests and for callers that
// need to size a column.
func (r *Renderer) SpinnerFrames() []string {
	if r != nil && r.caps.Unicode {
		return append([]string(nil), spinnerFramesUnicode...)
	}
	return append([]string(nil), spinnerFramesASCII...)
}

// Mark renders an outcome glyph, coloured by outcome.
func (r *Renderer) Mark(m Mark) string {
	glyph, color := r.markGlyph(m)
	return r.Paint(color, glyph)
}

func (r *Renderer) markGlyph(m Mark) (string, lipgloss.Color) {
	theme := r.Theme()
	unicode := r != nil && r.caps.Unicode
	switch m {
	case MarkDone:
		if unicode {
			return "✓", theme.Good
		}
		return "+", theme.Good
	case MarkFailed:
		if unicode {
			return "✗", theme.Bad
		}
		return "x", theme.Bad
	case MarkSkipped:
		if unicode {
			return "−", theme.Dim
		}
		return "-", theme.Dim
	default:
		if unicode {
			return "·", theme.Dim
		}
		return ".", theme.Dim
	}
}

// Dash is an em dash where the locale can draw one and a plain hyphen where it
// cannot. The stray "□" people saw mid-line was an em dash in a terminal whose
// locale was not UTF-8.
func (r *Renderer) Dash() string {
	if r != nil && r.caps.Unicode {
		return "—"
	}
	return "--"
}

// Bullet is the mid-dot separator where the locale can draw one and a pipe
// where it cannot. Same reasoning as Dash: a hard-coded "\u00b7" renders as a
// replacement box on a terminal whose locale is not UTF-8.
func (r *Renderer) Bullet() string {
	if r != nil && r.caps.Unicode {
		return "\u00b7"
	}
	return "|"
}

// Bar renders a determinate progress bar of the given cell width, coloured as a
// red -> amber -> green ramp ACROSS the bar: each filled cell takes the colour
// of its own position, so the bar visibly warms as it fills instead of flipping
// colour in one step. total <= 0 is indeterminate and renders as an empty
// track, because a bar that cannot say how full it is must not imply zero.
func (r *Renderer) Bar(done, total, width int) string {
	if width <= 0 {
		return ""
	}
	full, empty := "#", "."
	if r != nil && r.caps.Unicode {
		full, empty = "█", "░"
	}
	if total <= 0 {
		return r.Dim(strings.Repeat(empty, width))
	}
	if done < 0 {
		done = 0
	}
	if done > total {
		done = total
	}
	filled := done * width / total
	// Any real progress must light at least one cell, or a 1/618 export looks
	// identical to a 0/618 one for the first ninety seconds.
	if filled == 0 && done > 0 {
		filled = 1
	}
	var b strings.Builder
	for i := 0; i < filled; i++ {
		b.WriteString(r.Paint(r.rampColor(float64(i+1)/float64(width)), full))
	}
	if rest := width - filled; rest > 0 {
		b.WriteString(r.Dim(strings.Repeat(empty, rest)))
	}
	return b.String()
}

// rampColor maps a 0..1 position onto the palette's bad/ok/good ramp.
func (r *Renderer) rampColor(fraction float64) lipgloss.Color {
	theme := r.Theme()
	switch {
	case fraction < 0.34:
		return theme.Bad
	case fraction < 0.67:
		return theme.OK
	default:
		return theme.Good
	}
}

// Ratio renders "done/total" coloured by completeness, so a count reads the
// same way as the bar beside it.
func (r *Renderer) Ratio(done, total int) string {
	text := fmt.Sprintf("%d/%d", done, total)
	if total <= 0 {
		return r.Dim(text)
	}
	return r.Paint(r.rampColor(float64(done)/float64(total)), text)
}

// Truncate clips s to the renderer's line width, measured in display cells so a
// CJK or emoji-bearing label is not cut mid-glyph. A live line that WRAPS is
// the reason phase lines looked doubled: the carriage-return rewrite only ever
// clears the last physical row, so the wrapped first row stays on screen and
// the final line prints under it.
func (r *Renderer) Truncate(s string) string {
	if r == nil || r.caps.Width <= 0 {
		return s
	}
	// One cell of slack: writing into the last column makes some terminals wrap
	// eagerly, which is the very thing being avoided.
	return r.TruncateTo(s, r.caps.Width-1)
}

// TruncateTo clips s to limit display cells. It must be given PLAIN text: the
// measurement counts runes, so an already-coloured string would have its escape
// bytes charged as width. Callers that colour a line build it as plain text,
// truncate here, and colour the pieces afterwards.
func (r *Renderer) TruncateTo(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if runewidth.StringWidth(s) <= limit {
		return s
	}
	if limit <= 1 {
		return runewidth.Truncate(s, limit, "")
	}
	tail := "..."
	if r != nil && r.caps.Unicode {
		tail = "…"
	}
	return runewidth.Truncate(s, limit, tail)
}

// Width reports the usable line width, 0 when unknown.
func (r *Renderer) Width() int {
	if r == nil {
		return 0
	}
	return r.caps.Width
}

// Phase names the stage of onboarding a line belongs to. Colour-coding by phase
// is what makes a scrolling setup readable at a glance: the instant core, the
// background backfill and the daemon are three different kinds of work with
// three different cost profiles, and a reader should not have to parse the text
// to tell which one a line came from.
type Phase int

const (
	// PhaseNeutral is an uncategorised line.
	PhaseNeutral Phase = iota
	// PhaseInstant is the free, blocking, deterministic core.
	PhaseInstant
	// PhaseBackfill is the detached, token-spending fact backfill.
	PhaseBackfill
	// PhaseDaemon is workspace registration and the background watcher.
	PhaseDaemon
	// PhaseDone marks a completed step.
	PhaseDone
	// PhaseSkipped marks a step that was deliberately not run.
	PhaseSkipped
	// PhaseFailed marks a step that was attempted and failed.
	PhaseFailed
)

// PhaseColor resolves the palette entry for a phase.
func (r *Renderer) PhaseColor(p Phase) lipgloss.Color {
	theme := r.Theme()
	switch p {
	case PhaseInstant:
		return theme.Link
	case PhaseBackfill:
		return theme.Alt
	case PhaseDaemon:
		return theme.Accent
	case PhaseDone:
		return theme.Good
	case PhaseSkipped:
		return theme.Flag
	case PhaseFailed:
		return theme.Bad
	default:
		return theme.Text
	}
}

// PhasePaint colours s for a phase.
func (r *Renderer) PhasePaint(p Phase, s string) string {
	if p == PhaseNeutral {
		return s
	}
	return r.Paint(r.PhaseColor(p), s)
}

// ansiForeground converts a palette colour into an SGR foreground sequence.
// lipgloss.Color carries either an xterm-256 index ("214") or a hex triplet
// ("#f9e2af"); both themes in this package use both forms.
func ansiForeground(c lipgloss.Color) string {
	value := strings.TrimSpace(string(c))
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "#") {
		r, g, b, ok := parseHexColor(value)
		if !ok {
			return ""
		}
		return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", r, g, b)
	}
	index := 0
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			return ""
		}
		index = index*10 + int(ch-'0')
		if index > 255 {
			return ""
		}
	}
	return fmt.Sprintf("\x1b[38;5;%dm", index)
}

func parseHexColor(value string) (int, int, int, bool) {
	digits := value[1:]
	if len(digits) == 3 {
		expanded := make([]byte, 0, 6)
		for i := 0; i < 3; i++ {
			expanded = append(expanded, digits[i], digits[i])
		}
		digits = string(expanded)
	}
	if len(digits) != 6 {
		return 0, 0, 0, false
	}
	out := [3]int{}
	for i := 0; i < 3; i++ {
		hi, ok := hexNibble(digits[i*2])
		if !ok {
			return 0, 0, 0, false
		}
		lo, ok := hexNibble(digits[i*2+1])
		if !ok {
			return 0, 0, 0, false
		}
		out[i] = hi*16 + lo
	}
	return out[0], out[1], out[2], true
}

func hexNibble(b byte) (int, bool) {
	switch {
	case b >= '0' && b <= '9':
		return int(b - '0'), true
	case b >= 'a' && b <= 'f':
		return int(b-'a') + 10, true
	case b >= 'A' && b <= 'F':
		return int(b-'A') + 10, true
	}
	return 0, false
}
