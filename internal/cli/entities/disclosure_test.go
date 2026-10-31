package entities

import (
	"strings"
	"testing"

	"github.com/entireio/entire-brain/internal/entityindex"
)

// testConfig supplies the one dep these warnings need: how to spell a brain
// command for the reader.
func testConfig() Config {
	return Config{Deps: Deps{BrainInvocation: func(args string) string { return "entire brain " + args }}}
}

// The per-commit delta cap is a DIFFERENT cap from the index-cache one.
// `show --summary` printed delta.Entities verbatim, so a capped commit rendered
// a list shaped exactly like a complete one and a human had no way to know the
// answer was clipped. Measured on this repository: one commit stored 2,000 of
// 14,839 changed entities.
//
// The two truncated cases must not collapse into one message. EntityCount
// exists only on documents written since it was added, so a truncated document
// without one cannot name a total and must not imply it.
func TestStoredDeltaTruncationWarningSeparatesKnownFromUnrecordedTotals(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		delta    entityindex.Delta
		silent   bool
		contains []string
	}{
		{
			name:   "complete document says nothing",
			delta:  entityindex.Delta{Entities: make([]entityindex.EntityDelta, 12)},
			silent: true,
		},
		{
			name: "capped with a recorded total names both numbers",
			delta: entityindex.Delta{
				Truncated:   true,
				EntityCount: 14839,
				Entities:    make([]entityindex.EntityDelta, 2000),
			},
			contains: []string{"2000 of 14839", "NOT evidence"},
		},
		{
			name: "capped without a recorded total refuses to imply one",
			delta: entityindex.Delta{
				Truncated: true,
				Entities:  make([]entityindex.EntityDelta, 2000),
			},
			contains: []string{"true total was not recorded", "backfill --full", "NOT evidence"},
		},
		{
			// EntityCount == stored carries no information beyond the list
			// itself, so it must take the unrecorded branch rather than claim
			// "2000 of 2000", which reads as complete.
			name: "a total not exceeding the stored count is treated as unrecorded",
			delta: entityindex.Delta{
				Truncated:   true,
				EntityCount: 2000,
				Entities:    make([]entityindex.EntityDelta, 2000),
			},
			contains: []string{"true total was not recorded"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := storedDeltaTruncationWarning(tc.delta, testConfig())
			if tc.silent {
				if got != "" {
					t.Fatalf("storedDeltaTruncationWarning = %q, want empty", got)
				}
				return
			}
			for _, want := range tc.contains {
				if !strings.Contains(got, want) {
					t.Fatalf("storedDeltaTruncationWarning = %q, want it to contain %q", got, want)
				}
			}
			// A capped document must never claim the number it stored is the
			// number that changed.
			if strings.Contains(got, "complete") {
				t.Fatalf("truncation warning claims completeness: %q", got)
			}
		})
	}
}

// The remedy a diagnostic names has to work in the state that produced the
// diagnostic. This one used to name only `entities migrate`, and that cost a
// real session: migrate refuses on any `graph diff` warning, so on a repository
// carrying one large unparseable file it cannot run at all — the advice pointed
// at the one route that was closed.
func TestOtherParserRevisionWarningNamesBothRemedies(t *testing.T) {
	t.Parallel()

	got := otherParserRevisionWarning(testConfig())
	for _, want := range []string{"entities migrate", "entities backfill"} {
		if !strings.Contains(got, want) {
			t.Errorf("the remedy omits %q, leaving a reader with no route when the other is closed: %q", want, got)
		}
	}
}

// An incomplete input yields silence, never a confident negative. The
// reachability walk is capped, and HEAD missing from a capped walk is as likely
// to be beyond the cap as genuinely off-branch — reporting it either way would
// make the warning untrustworthy in the direction that matters.
func TestBranchExcludesHeadWarningStaysSilentWhenItCannotTell(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	for name, tc := range map[string]struct {
		branch, head string
		truncated    bool
	}{
		"a truncated walk cannot tell": {"main", "abc1234", true},
		"no branch filter":             {"", "abc1234", false},
		"no resolved head":             {"main", "", false},
		"blank branch":                 {"   ", "abc1234", false},
		"blank head":                   {"main", "  ", false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := branchExcludesHeadWarning(tc.branch, tc.head, tc.truncated, cfg); got != "" {
				t.Errorf("%s must be silent, got %q", name, got)
			}
		})
	}

	// And when it CAN tell, it must say so and name the fix. This is the trap
	// that cost the most time: `--branch main` filters to the LOCAL ref, which
	// can trail its remote while the checkout sits ahead of it, so every answer
	// was correct about that branch and silently blind to the commits actually
	// being worked on.
	got := branchExcludesHeadWarning("main", "abc1234", false, cfg)
	for _, want := range []string{"abc1234", "main", "NOT your checked-out commits", "entities backfill"} {
		if !strings.Contains(got, want) {
			t.Errorf("the warning omits %q: %q", want, got)
		}
	}
}
