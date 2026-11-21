package factmerge

import (
	"strings"
	"testing"
)

// TestFactKindsIsExactlyTheClosedSet pins the list against the set it renders.
//
// FactKinds exists because six hand-written copies of the kind vocabulary had
// drifted from validFactKinds and every one of them had dropped the same
// member. A second list beside the set only moves that failure here unless
// something holds the two together, so this asserts both directions: a kind
// added to the set and not the list is not renderable, and a kind in the list
// that the set rejects would be advertised on flag help and then refused by
// ValidateKindFlag -- the exact contradiction that started this.
func TestFactKindsIsExactlyTheClosedSet(t *testing.T) {
	t.Parallel()
	if len(validFactKinds) == 0 {
		t.Fatal("non-vacuity: the closed set is empty, so this test proves nothing")
	}
	listed := map[string]int{}
	for _, kind := range FactKinds {
		listed[kind]++
		if listed[kind] > 1 {
			t.Errorf("FactKinds repeats %q", kind)
		}
		if !ValidFactKind(kind) {
			t.Errorf("FactKinds offers %q, which ValidFactKind rejects", kind)
		}
	}
	for kind := range validFactKinds {
		if listed[kind] == 0 {
			t.Errorf("kind %q is accepted but never named: add it to FactKinds, or users cannot discover it", kind)
		}
	}
}

// TestFactKindListNamesEveryKind guards the rendered string itself, since that
// is what a user reads.
func TestFactKindListNamesEveryKind(t *testing.T) {
	t.Parallel()
	rendered := FactKindList()
	for kind := range validFactKinds {
		if !strings.Contains(rendered, kind) {
			t.Errorf("FactKindList() = %q, which omits the valid kind %q", rendered, kind)
		}
	}
	if got, want := strings.Count(rendered, "|")+1, len(validFactKinds); got != want {
		t.Errorf("FactKindList() renders %d kinds, want %d: %q", got, want, rendered)
	}
}
