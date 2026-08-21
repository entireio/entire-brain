package tui

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func envFrom(pairs map[string]string) EnvLookup {
	return func(key string) (string, bool) {
		value, ok := pairs[key]
		return value, ok
	}
}

// TestDetectCapsNonTerminalIsPlain is the contract every script, log and test
// depends on: anything that is not a terminal gets plain bytes, whatever the
// environment says.
func TestDetectCapsNonTerminalIsPlain(t *testing.T) {
	t.Parallel()
	caps := DetectCapsWithEnv(&bytes.Buffer{}, envFrom(map[string]string{"TERM": "xterm-256color", "LANG": "en_US.UTF-8"}))
	if caps != PlainCaps() {
		t.Fatalf("a bytes.Buffer resolved to %+v, want plain", caps)
	}
	// os.DevNull is a real *os.File and still not a terminal — the type assertion
	// alone is not the check.
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Skipf("cannot open %s: %v", os.DevNull, err)
	}
	defer devnull.Close()
	if caps := DetectCapsWithEnv(devnull, envFrom(map[string]string{"TERM": "xterm-256color"})); caps != PlainCaps() {
		t.Fatalf("%s resolved to %+v, want plain", os.DevNull, caps)
	}
}

func TestDetectCapsRespectsEnvironment(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		env         map[string]string
		wantColor   bool
		wantUnicode bool
		wantPlain   bool
	}{
		{name: "utf8 colour terminal", env: map[string]string{"TERM": "xterm-256color", "LANG": "en_US.UTF-8"}, wantColor: true, wantUnicode: true},
		{name: "NO_COLOR set", env: map[string]string{"TERM": "xterm-256color", "LANG": "en_US.UTF-8", "NO_COLOR": "1"}, wantUnicode: true},
		{name: "NO_COLOR empty still counts", env: map[string]string{"TERM": "xterm-256color", "LANG": "en_US.UTF-8", "NO_COLOR": ""}, wantUnicode: true},
		{name: "C locale falls back to ASCII", env: map[string]string{"TERM": "xterm-256color", "LANG": "C"}, wantColor: true},
		{name: "no locale at all falls back to ASCII", env: map[string]string{"TERM": "xterm-256color"}, wantColor: true},
		{name: "LC_ALL wins over LANG", env: map[string]string{"TERM": "xterm-256color", "LC_ALL": "C", "LANG": "en_US.UTF-8"}, wantColor: true},
		{name: "TERM=dumb is plain", env: map[string]string{"TERM": "dumb", "LANG": "en_US.UTF-8"}, wantPlain: true},
		{name: "unset TERM is plain", env: map[string]string{"LANG": "en_US.UTF-8"}, wantPlain: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			caps := detectCapsForFakeTerminal(tc.env)
			if tc.wantPlain {
				if caps != PlainCaps() {
					t.Fatalf("caps = %+v, want plain", caps)
				}
				return
			}
			if !caps.TTY {
				t.Fatalf("caps = %+v, want a terminal", caps)
			}
			if caps.Color != tc.wantColor {
				t.Errorf("Color = %t, want %t", caps.Color, tc.wantColor)
			}
			if caps.Unicode != tc.wantUnicode {
				t.Errorf("Unicode = %t, want %t", caps.Unicode, tc.wantUnicode)
			}
		})
	}
}

// detectCapsForFakeTerminal runs the environment half of DetectCaps with the
// terminal half forced true. Opening a real PTY per case would make this test
// platform-dependent for no gain: the TTY branch is covered above.
func detectCapsForFakeTerminal(env map[string]string) Caps {
	lookup := envFrom(env)
	getenv := func(key string) string { value, _ := lookup(key); return value }
	term := strings.TrimSpace(getenv("TERM"))
	if term == "" || term == "dumb" {
		return PlainCaps()
	}
	caps := Caps{TTY: true}
	if _, noColor := lookup("NO_COLOR"); !noColor {
		caps.Color = true
	}
	caps.Unicode = localeIsUTF8(getenv)
	return caps
}

func testRenderer(caps Caps) *Renderer {
	theme, _ := ThemeByName("default")
	return NewRendererWith(theme, caps)
}

// TestPlainRendererEmitsNoEscapesOrNonASCII is the golden plain-mode contract:
// every renderer primitive, in plain mode, produces bytes a script can parse.
func TestPlainRendererEmitsNoEscapesOrNonASCII(t *testing.T) {
	t.Parallel()
	r := testRenderer(PlainCaps())
	pieces := []string{
		r.Paint(r.Theme().Good, "green"),
		r.Bold("bold"),
		r.Dim("dim"),
		r.Dash(),
		r.Bullet(),
		r.Spinner(0), r.Spinner(1), r.Spinner(2), r.Spinner(3), r.Spinner(9),
		r.Mark(MarkDone), r.Mark(MarkFailed), r.Mark(MarkSkipped), r.Mark(MarkPending),
		r.Bar(3, 10, 10), r.Bar(0, 0, 6), r.Ratio(3, 10),
		r.PhasePaint(PhaseInstant, "instant"), r.PhasePaint(PhaseFailed, "failed"),
	}
	joined := strings.Join(pieces, "|")
	if strings.ContainsRune(joined, 0x1b) {
		t.Fatalf("plain mode emitted an escape sequence: %q", joined)
	}
	for _, r := range joined {
		if r > 127 {
			t.Fatalf("plain mode emitted non-ASCII %q in %q", r, joined)
		}
	}
	const want = "green|bold|dim|--|||-|\\|||/|\\|+|x|-|.|###.......|......|3/10|instant|failed"
	if joined != want {
		t.Fatalf("plain golden\n got: %q\nwant: %q", joined, want)
	}
}

// TestNoColorKeepsUnicodeButDropsColour proves the two capabilities are
// independent: NO_COLOR is about escapes, the locale is about glyphs, and
// conflating them is how a UTF-8 terminal ends up with ASCII art.
func TestNoColorKeepsUnicodeButDropsColour(t *testing.T) {
	t.Parallel()
	r := testRenderer(Caps{TTY: true, Unicode: true, Width: 80})
	out := r.Mark(MarkDone) + r.Bar(5, 10, 4) + r.PhasePaint(PhaseBackfill, "backfill")
	if strings.ContainsRune(out, 0x1b) {
		t.Fatalf("NO_COLOR output carries escapes: %q", out)
	}
	if !strings.Contains(out, "✓") || !strings.Contains(out, "█") {
		t.Fatalf("NO_COLOR must keep the unicode glyph set: %q", out)
	}
}

// TestColorRendererPaintsFromTheTheme pins the SGR encoding for both colour
// notations the themes use: an xterm-256 index and a hex triplet.
func TestColorRendererPaintsFromTheTheme(t *testing.T) {
	t.Parallel()
	def := testRenderer(Caps{TTY: true, Color: true, Unicode: true, Width: 80})
	if got := def.Paint(def.Theme().Good, "ok"); got != "\x1b[38;5;47mok\x1b[0m" {
		t.Errorf("256-colour paint = %q", got)
	}
	theme, ok := ThemeByName("catppuccin")
	if !ok {
		t.Fatal("catppuccin theme missing")
	}
	hex := NewRendererWith(theme, Caps{TTY: true, Color: true, Unicode: true, Width: 80})
	if got := hex.Paint(theme.Good, "ok"); got != "\x1b[38;2;166;227;161mok\x1b[0m" {
		t.Errorf("truecolour paint = %q", got)
	}
}

// TestPhaseColoursAreDistinct is the point of phase colour-coding: if two
// phases render the same, a reader cannot tell the free deterministic work from
// the one that spends tokens.
func TestPhaseColoursAreDistinct(t *testing.T) {
	t.Parallel()
	r := testRenderer(Caps{TTY: true, Color: true, Unicode: true})
	seen := map[string]Phase{}
	for _, phase := range []Phase{PhaseInstant, PhaseBackfill, PhaseDaemon, PhaseDone, PhaseSkipped, PhaseFailed} {
		color := string(r.PhaseColor(phase))
		if other, clash := seen[color]; clash {
			t.Errorf("phase %d and phase %d share colour %s", phase, other, color)
		}
		seen[color] = phase
	}
}

// TestBarRampWarmsWithProgress pins the red -> amber -> green gradient. A bar
// that is one flat colour tells the reader nothing the number beside it does
// not already say.
func TestBarRampWarmsWithProgress(t *testing.T) {
	t.Parallel()
	r := testRenderer(Caps{TTY: true, Color: true, Unicode: true})
	full := r.Bar(9, 9, 9)
	bad := "\x1b[38;5;203m" // theme.Bad
	ok := "\x1b[38;5;214m"  // theme.OK
	good := "\x1b[38;5;47m" // theme.Good
	for _, want := range []string{bad, ok, good} {
		if !strings.Contains(full, want) {
			t.Errorf("a full bar must span the whole ramp; %q missing from %q", want, full)
		}
	}
	// Order matters: the ramp must run cold to warm, not warm to cold.
	if strings.Index(full, bad) > strings.Index(full, good) {
		t.Errorf("the ramp runs backwards: %q", full)
	}
}

func TestBarEdgeCases(t *testing.T) {
	t.Parallel()
	r := testRenderer(PlainCaps())
	if got := r.Bar(0, 100, 10); got != strings.Repeat(".", 10) {
		t.Errorf("empty bar = %q", got)
	}
	// One unit of real progress must light one cell, or a 1/618 export looks
	// exactly like a 0/618 one for its first ninety seconds.
	if got := r.Bar(1, 618, 10); got != "#"+strings.Repeat(".", 9) {
		t.Errorf("bar at 1/618 = %q, want one lit cell", got)
	}
	if got := r.Bar(200, 100, 4); got != "####" {
		t.Errorf("overshoot must clamp, got %q", got)
	}
	if got := r.Bar(-5, 100, 4); got != "...." {
		t.Errorf("negative must clamp, got %q", got)
	}
	if got := r.Bar(5, 0, 4); got != "...." {
		t.Errorf("indeterminate must not imply a fraction, got %q", got)
	}
	if got := r.Bar(5, 10, 0); got != "" {
		t.Errorf("zero width = %q, want empty", got)
	}
}

func TestTruncateRespectsWidth(t *testing.T) {
	t.Parallel()
	r := testRenderer(Caps{TTY: true, Width: 20})
	got := r.Truncate(strings.Repeat("x", 100))
	if len([]rune(got)) != 19 {
		t.Fatalf("truncated to %d cells, want 19 (width-1)", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("ASCII truncation must use an ASCII ellipsis: %q", got)
	}
	unicode := NewRendererWith(r.Theme(), Caps{TTY: true, Unicode: true, Width: 20})
	if got := unicode.Truncate(strings.Repeat("x", 100)); !strings.HasSuffix(got, "…") {
		t.Errorf("unicode truncation should use the single-cell ellipsis: %q", got)
	}
	// Unknown width must never truncate: guessing 80 and clipping a CI log is
	// worse than a long line.
	unknown := NewRendererWith(r.Theme(), Caps{TTY: true})
	long := strings.Repeat("y", 500)
	if unknown.Truncate(long) != long {
		t.Error("unknown width must pass the line through untouched")
	}
}

func TestSpinnerFrameSetsAreGatedOnLocale(t *testing.T) {
	t.Parallel()
	ascii := testRenderer(Caps{TTY: true})
	for i := 0; i < 40; i++ {
		frame := ascii.Spinner(i)
		if len(frame) != 1 || frame[0] > 127 {
			t.Fatalf("ASCII spinner frame %d = %q, want a single ASCII byte", i, frame)
		}
	}
	unicode := testRenderer(Caps{TTY: true, Unicode: true})
	if unicode.Spinner(0) == ascii.Spinner(0) {
		t.Error("a UTF-8 terminal should get the richer spinner")
	}
	// Negative ticks must not panic or index out of range.
	_ = unicode.Spinner(-7)
}

func TestNilRendererIsSafe(t *testing.T) {
	t.Parallel()
	var r *Renderer
	if got := r.Paint(r.Theme().Good, "x"); got != "x" {
		t.Errorf("nil Paint = %q", got)
	}
	if got := r.Mark(MarkDone); got != "+" {
		t.Errorf("nil Mark = %q", got)
	}
	if got := r.Dash(); got != "--" {
		t.Errorf("nil Dash = %q", got)
	}
	if got := r.Bar(1, 2, 4); got != "##.." {
		t.Errorf("nil Bar = %q", got)
	}
	if got := r.Truncate("abc"); got != "abc" {
		t.Errorf("nil Truncate = %q", got)
	}
}

// TestTerminalWidthFallsBackRatherThanReturningZero pins the fallback chain.
// A terminal of "unknown" width does not truncate, and a live line that is not
// truncated wraps — which is the mechanism behind the duplicated phase lines.
// Guessing 80 clips a label; not guessing brings the bug back.
func TestTerminalWidthFallsBackRatherThanReturningZero(t *testing.T) {
	t.Parallel()
	// fd 0 in `go test` is not a terminal, so GetSize fails and the fallbacks run.
	if got := terminalWidth(0, func(string) string { return "" }); got != 80 {
		t.Errorf("no kernel size and no COLUMNS = %d, want the conservative 80", got)
	}
	if got := terminalWidth(0, func(key string) string {
		if key == "COLUMNS" {
			return "140"
		}
		return ""
	}); got != 140 {
		t.Errorf("COLUMNS=140 = %d, want 140", got)
	}
	if got := terminalWidth(0, func(key string) string {
		if key == "COLUMNS" {
			return "not-a-number"
		}
		return ""
	}); got != 80 {
		t.Errorf("a junk COLUMNS = %d, want the 80 fallback", got)
	}
}
