package cli

import (
	"strings"
	"testing"
)

// The adoption gap was invisible in `memories status`: one served_total cannot
// distinguish a store an agent asks for from a store that delivers unprompted.
// Measured on main 2026-10-31, 525 of 539 serve events were `recall` and
// exactly one was `hook-pre-edit`, and seeing that took aggregating
// `memories vitality --json` by hand.
func TestVitalitySurfacesAreOrderedBusiestFirst(t *testing.T) {
	t.Parallel()

	got := formatVitalitySurfaces(map[string]int{
		"hook-pre-edit":     1,
		"recall":            525,
		"brief":             6,
		"mcp:brain_search":  7,
		"hook-post-failure": 0,
		"hook-session-end":  1,
	})
	// Counts descending; ties broken by name so the line is stable.
	const want = "recall=525 mcp:brain_search=7 brief=6 hook-pre-edit=1 hook-session-end=1 hook-post-failure=0"
	if got != want {
		t.Errorf("surface line =\n  %s\nwant\n  %s", got, want)
	}
}

// THE HAZARD THIS PINS: Go randomizes map iteration order. The whole point of
// this line is that a reader compares it between two runs, so an unsorted
// implementation would reshuffle every invocation and make the comparison
// impossible -- while looking perfectly correct in a single-run eyeball test.
func TestVitalitySurfaceLineIsStableAcrossCalls(t *testing.T) {
	t.Parallel()

	// Enough distinct keys at the SAME count that map order would show.
	surfaces := map[string]int{}
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l"} {
		surfaces[name] = 3
	}
	first := formatVitalitySurfaces(surfaces)
	for i := 0; i < 64; i++ {
		if again := formatVitalitySurfaces(surfaces); again != first {
			t.Fatalf("the line is not deterministic:\n  %s\n  %s", first, again)
		}
	}
	// And the tie-break is by name, so it is the readable order too.
	if !strings.HasPrefix(first, "a=3 b=3 c=3") {
		t.Errorf("ties are not ordered by name: %s", first)
	}
}

func TestVitalitySurfacesEmptyRendersNoLine(t *testing.T) {
	t.Parallel()

	for name, in := range map[string]map[string]int{
		"nil":   nil,
		"empty": {},
	} {
		if got := formatVitalitySurfaces(in); got != "" {
			t.Errorf("%s must render no line, got %q", name, got)
		}
	}
}
