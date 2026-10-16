package tui

import (
	"strings"
	"testing"
)

// paintedBar reproduces the shape that exposed the bug: a bar coloured one
// cell at a time, which is what Renderer.Bar emits.
func paintedBar(cells int) string {
	var b strings.Builder
	for range cells {
		b.WriteString("\x1b[38;5;203m█\x1b[0m")
	}
	return b.String()
}

// hasPartialEscape reports an ESC that never reaches a CSI final byte. A
// terminal handed one waits for the rest and swallows what follows.
func hasPartialEscape(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != 0x1b {
			continue
		}
		j := i + 1
		for j < len(s) && !isANSIFinalByte(s[j]) {
			j++
		}
		if j >= len(s) {
			return true
		}
	}
	return false
}

func TestTruncateToNeverCutsInsideAnEscape(t *testing.T) {
	t.Parallel()
	r := NewRendererWith(Theme{}, Caps{Unicode: true, Color: true, Width: 100})
	bar := paintedBar(16)
	// Non-vacuity: the input really must be long enough to force a cut, and
	// really must contain escapes.
	if ansiVisibleWidth(bar) != 16 {
		t.Fatalf("fixture: want 16 visible cells, got %d", ansiVisibleWidth(bar))
	}
	if !strings.Contains(bar, "\x1b[") {
		t.Fatal("fixture: no escapes, test would prove nothing")
	}
	for limit := 1; limit <= 15; limit++ {
		got := r.TruncateTo(bar, limit)
		if hasPartialEscape(got) {
			t.Fatalf("limit %d cut inside an escape: %q", limit, got)
		}
		if w := ansiVisibleWidth(got); w > limit {
			t.Fatalf("limit %d: visible width %d exceeds it: %q", limit, w, got)
		}
	}
}

func TestTruncateToChargesEscapesNothing(t *testing.T) {
	t.Parallel()
	r := NewRendererWith(Theme{}, Caps{Unicode: true, Color: true, Width: 100})
	// Ten visible cells wrapped in colour. It FITS a limit of 10, so it must
	// come back whole — byte-counting would see ~19 and truncate it.
	painted := "\x1b[38;5;45m" + strings.Repeat("x", 10) + "\x1b[0m"
	if got := r.TruncateTo(painted, 10); got != painted {
		t.Fatalf("a string that fits must be returned unchanged.\n got: %q\nwant: %q", got, painted)
	}
}

func TestTruncateToClosesAColourItCutsThrough(t *testing.T) {
	t.Parallel()
	r := NewRendererWith(Theme{}, Caps{Unicode: true, Color: true, Width: 100})
	// Colour opened, never closed before the cut: the result must reset, or
	// the colour bleeds into every line after it.
	got := r.TruncateTo("\x1b[38;5;45m"+strings.Repeat("x", 40), 10)
	if !strings.HasSuffix(got, "\x1b[0m") {
		t.Fatalf("a cut through an open colour must reset it, got %q", got)
	}
}

func TestTruncateToLeavesPlainTextAlone(t *testing.T) {
	t.Parallel()
	// Regression guard for the far larger set of callers that pass no colour.
	r := NewRendererWith(Theme{}, Caps{Unicode: true, Width: 100})
	if got := r.TruncateTo("hello", 10); got != "hello" {
		t.Fatalf("short plain text must pass through, got %q", got)
	}
	got := r.TruncateTo(strings.Repeat("a", 40), 10)
	if len([]rune(got)) != 10 || !strings.HasSuffix(got, "…") {
		t.Fatalf("plain truncation must still fit and mark the cut, got %q (%d runes)", got, len([]rune(got)))
	}
}
