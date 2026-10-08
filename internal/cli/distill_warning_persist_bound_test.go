package cli

import (
	"strings"
	"testing"
)

// The display cap (50) must never reach disk — that was the bug. The persist
// bound exists for the opposite failure: a run where every chunk fails appends
// without limit to a file status reads on every call. One real distill run here
// produced 278 suppressed warnings, so the unbounded case is not hypothetical.
func TestPersistedWarningsAreBoundedButKeepTheExactTotal(t *testing.T) {
	t.Parallel()

	const produced = maxPersistedDistillWarnings + 357
	in := make([]string, produced)
	for i := range in {
		in[i] = "agent failed on session:" + itoaForTest(i)
	}

	got := distillWarningsForPersist(in)

	if len(got) > maxPersistedDistillWarnings+1 {
		t.Fatalf("persisted %d warnings; the bound is %d plus one overflow line",
			len(got), maxPersistedDistillWarnings)
	}
	overflow := got[len(got)-1]
	if !strings.Contains(overflow, itoaForTest(produced)) {
		t.Fatalf("the overflow line must state the exact total %d, got: %s", produced, overflow)
	}
	// The bug being fixed was a count that disagreed with reality. Bounding the
	// list is fine; misreporting how many there were is not.
	if strings.Contains(overflow, itoaForTest(maxPersistedDistillWarnings)) &&
		!strings.Contains(overflow, itoaForTest(produced)) {
		t.Fatal("overflow line reports the kept count as if it were the total")
	}
}

// A normal run must pass through untouched — no overflow line, no copy.
func TestPersistedWarningsUnderTheBoundAreUnchanged(t *testing.T) {
	t.Parallel()

	in := []string{"a", "b", "c"}
	got := distillWarningsForPersist(in)
	if len(got) != len(in) {
		t.Fatalf("a short list must pass through; got %d want %d", len(got), len(in))
	}
	for i := range in {
		if got[i] != in[i] {
			t.Fatalf("entry %d changed: %q -> %q", i, in[i], got[i])
		}
	}
}
