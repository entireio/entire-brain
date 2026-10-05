package cli

import (
	"strings"
	"testing"
)

// "no symbols found in the semantic index" meant four indistinguishable
// things: no such symbol, the index was never built, the command is broken,
// or — the one its own comment missed — the index holds no symbols OF THAT
// KIND. Go package-level consts and vars are not extracted at all, so a
// constant name produces the same line as a typo.
//
// That ambiguity cost an hour here: given the line for a symbol visible in the
// source, the conclusion drawn was "the search is broken", and a false root
// cause was pursued three steps deep. An inventory of what the index DOES hold
// settles it in one line.
func TestEmptySymbolSearchNamesWhatTheIndexHolds(t *testing.T) {
	t.Parallel()

	total, distinct, kinds := semanticIndexKindInventory("")
	if total != 0 || distinct != 0 || kinds != nil {
		t.Fatalf("an unreadable store must report an empty inventory, got %d / %d / %v", total, distinct, kinds)
	}
	// A missing file is the common case when the index was never built; it
	// must not error, because this runs only to explain an empty result.
	total, distinct, kinds = semanticIndexKindInventory(t.TempDir() + "/absent.sqlite")
	if total != 0 || distinct != 0 || kinds != nil {
		t.Fatalf("a missing store must report an empty inventory, got %d / %d / %v", total, distinct, kinds)
	}
}

// The note has to name the const/var gap explicitly. "0 results" plus a kind
// list a reader must interpret is weaker than saying the thing outright.
func TestEmptySymbolSearchNamesTheConstGapInWords(t *testing.T) {
	t.Parallel()

	src := readSourceForTest(t, "semantic.go")
	if !strings.Contains(src, "func printSemanticNoSymbolMatch") {
		t.Fatal("the symbol-search empty path no longer explains itself")
	}
	for _, want := range []string{"consts and vars are not extracted", "holds no symbols at all"} {
		if !strings.Contains(src, want) {
			t.Errorf("the note must state %q; a kind list alone leaves the reader to infer it", want)
		}
	}
	// And the empty path must actually call it, not just define it: the
	// recurring defect in this work is logic that exists but is never reached.
	if !strings.Contains(src, "printSemanticNoSymbolMatch(cmd, storePathForInventory, query)") {
		t.Error("the symbol-search empty branch does not call the explaining variant")
	}
}
