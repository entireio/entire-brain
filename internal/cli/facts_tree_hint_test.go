package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/spf13/cobra"
)

func hintFact(id, path, kind string) factRecord {
	return factRecord{
		ID:        id,
		Paths:     []string{path},
		Kind:      kind,
		Text:      "text for " + id,
		Status:    factStatusActive,
		UpdatedAt: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC),
	}
}

// TestLeafNavigationHintAdvances pins the one property the pointer has to
// have: it must name a move the reader has not already made.
//
// The pointer used to be "drill with --path <leaf>" in every case, so drilling
// into a leaf ended in an instruction to run the command that produced the
// output. That is not a corner: this repository's own brain puts 67% of 3,950
// memories in 5 paths, so the buckets anyone opens are exactly the oversized
// ones where the only offered move was the identity.
//
// Each case therefore asserts what the hint does NOT say as well as what it
// does. Asserting only the presence of "--kind" would pass on a hint that
// offered both axes at once, which still tells a reader standing on the leaf to
// drill into it.
func TestLeafNavigationHintAdvances(t *testing.T) {
	t.Parallel()
	const path = "constraints.invariants.general"
	leaf := &factTreeNode{Label: path, Count: 5, Facts: []factRecord{
		hintFact("a", path, factmerge.KindGotcha),
		hintFact("b", path, factmerge.KindGotcha),
		hintFact("c", path, factmerge.KindInvariant),
		hintFact("d", path, factmerge.KindClosedNegative),
		hintFact("e", path, factmerge.KindClosedNegative),
	}}
	oneKind := &factTreeNode{Label: path, Count: 2, Facts: []factRecord{
		hintFact("a", path, factmerge.KindGotcha),
		hintFact("b", path, factmerge.KindGotcha),
	}}

	for _, tc := range []struct {
		name                 string
		leaf                 *factTreeNode
		rest                 int
		drilledPath, drilled string
		wantContains         []string
		wantOmits            []string
	}{
		{
			name: "nothing held back prints no pointer",
			leaf: leaf, rest: 0,
			wantOmits: []string{"--path", "--kind", "--leaves"},
		},
		{
			name: "not drilled yet: --path is the move",
			leaf: leaf, rest: 2,
			wantContains: []string{"+2 more", "--path " + path},
			wantOmits:    []string{"--kind"},
		},
		{
			name: "standing on the leaf: --kind is the move, not --path",
			leaf: leaf, rest: 2, drilledPath: path,
			// closed-negative is named here only because it is in the data;
			// before this change no surface ever offered it at all.
			wantContains: []string{"+2 more", "--kind", "gotcha (2)", "closed-negative (2)", "invariant (1)"},
			wantOmits:    []string{"--path"},
		},
		{
			name: "path and kind both spent: --leaves, and never --kind again",
			leaf: leaf, rest: 2, drilledPath: path, drilled: factmerge.KindGotcha,
			wantContains: []string{"+2 more", "--leaves"},
			wantOmits:    []string{"--path", "--kind"},
		},
		{
			name: "a leaf of one kind cannot be narrowed by kind",
			leaf: oneKind, rest: 1, drilledPath: path,
			wantContains: []string{"+1 more", "--leaves"},
			wantOmits:    []string{"--path", "--kind"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := leafNavigationHint(tc.leaf, tc.rest, tc.drilledPath, tc.drilled)
			if tc.rest == 0 && got != "" {
				t.Fatalf("hint = %q, want empty when nothing is held back", got)
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("hint = %q, want it to contain %q", got, want)
				}
			}
			for _, bad := range tc.wantOmits {
				if strings.Contains(got, bad) {
					t.Errorf("hint = %q, must not offer %q: the reader already used it", got, bad)
				}
			}
		})
	}
}

// TestRenderedTreeDoesNotTellYouToDrillWhereYouStand is the end-to-end form of
// the case above, through the renderer rather than the helper, so a future
// refactor that stops threading the drilled path into renderFactTree is caught
// even though leafNavigationHint still behaves.
func TestRenderedTreeDoesNotTellYouToDrillWhereYouStand(t *testing.T) {
	t.Parallel()
	const path = "constraints.invariants.general"
	facts := []factRecord{
		hintFact("a", path, factmerge.KindGotcha),
		hintFact("b", path, factmerge.KindInvariant),
		hintFact("c", path, factmerge.KindClosedNegative),
	}
	tree := buildFactTree(facts, false)
	cmd := &cobra.Command{}
	var out strings.Builder
	cmd.SetOut(&out)

	renderFactTree(cmd, tree, 3, 1, path, "")
	rendered := out.String()
	if !strings.Contains(rendered, "(+2 more") {
		t.Fatalf("non-vacuity: nothing was held back, so the pointer never rendered:\n%s", rendered)
	}
	if strings.Contains(rendered, "drill with --path") {
		t.Errorf("rendered tree tells a reader already at %s to drill into it:\n%s", path, rendered)
	}
	if !strings.Contains(rendered, "--kind") {
		t.Errorf("rendered tree offers no way forward from %s:\n%s", path, rendered)
	}
}

// TestEveryMemoryKindFlagNamesEveryKind is the class guard.
//
// The vocabulary was duplicated by hand in four places and all four omitted
// closed-negative, including `remember`, so an agent reading that help never
// authored one. Pinning those four call sites would only pin the four that
// were already found; this walks the whole command tree, so a fifth --kind
// flag added later with its own hand-written list fails here instead of
// shipping.
//
// "--kind" alone does not identify the flag: `inspect boundaries` takes
// route|tool|workflow under that name and `memory jobs` takes a job kind, and
// neither has anything to do with the memory vocabulary. A flag is the memory
// one when its help already names at least one memory kind. That test is
// circular on its own -- a help string rewritten to name none would be
// skipped rather than failed -- so the commands known to filter memories are
// also required to have been reached, which is what closes it.
func TestEveryMemoryKindFlagNamesEveryKind(t *testing.T) {
	t.Parallel()
	root := NewRootCommand(Options{Version: "test-version"})

	// The commands that pass --kind to factstore's memory-kind filter. A
	// command dropped from this set, or renamed, fails below rather than
	// quietly reducing the guard's reach.
	required := map[string]bool{
		"entire-brain memories tree": false,
		"entire-brain recall":        false,
		"entire-brain remember":      false,
	}

	namesAMemoryKind := func(usage string) bool {
		for _, kind := range factmerge.FactKinds {
			if strings.Contains(usage, kind) {
				return true
			}
		}
		return false
	}

	checked := 0
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		flag := cmd.Flags().Lookup("kind")
		if flag != nil && namesAMemoryKind(flag.Usage) {
			checked++
			if _, ok := required[cmd.CommandPath()]; ok {
				required[cmd.CommandPath()] = true
			}
			for _, kind := range factmerge.FactKinds {
				if !strings.Contains(flag.Usage, kind) {
					t.Errorf("%s --kind help omits the valid kind %q: %q",
						cmd.CommandPath(), kind, flag.Usage)
				}
			}
		}
		for _, child := range cmd.Commands() {
			walk(child)
		}
	}
	walk(root)

	for path, seen := range required {
		if !seen {
			t.Errorf("%s was not reached with a memory --kind flag; either it lost the flag or its help no longer names any kind, which would make this guard skip it silently", path)
		}
	}
	if checked == 0 {
		t.Fatal("non-vacuity: found no memory --kind flag in the command tree, so this proves nothing")
	}
	t.Logf("checked %d memory --kind flags", checked)
}
