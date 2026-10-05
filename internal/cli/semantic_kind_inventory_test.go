package cli

import (
	"bytes"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// seedSymbolKinds builds a minimal symbols table with the given kind:count
// shape, so the inventory can be checked against a known truth.
func seedSymbolKinds(t *testing.T, counts map[string]int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "semantic.db")
	db, err := sql.Open(sqliteDriverName, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE symbols (kind TEXT, name TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	for kind, n := range counts {
		for i := 0; i < n; i++ {
			if _, err := db.Exec(`INSERT INTO symbols (kind, name) VALUES (?, ?)`, kind, fmt.Sprintf("%s%d", kind, i)); err != nil {
				t.Fatalf("insert: %v", err)
			}
		}
	}
	return path
}

// total summed only the rows returned by LIMIT 6, so with more than six
// distinct kinds it under-reported -- a partial sum of the top groups, not the
// real total.
//
// This note exists to give an agent an accurate picture after an empty result.
// A too-low total risks the same false root cause the note was written to
// prevent, which is the mistake that prompted writing it in the first place.
func TestKindInventoryTotalCountsEveryKindNotJustTheTopSix(t *testing.T) {
	t.Parallel()

	// Nine kinds, one symbol each: six are displayed, all nine must be counted.
	counts := map[string]int{}
	for i := 0; i < 9; i++ {
		counts[fmt.Sprintf("kind%d", i)] = 1
	}
	path := seedSymbolKinds(t, counts)

	total, distinct, kinds := semanticIndexKindInventory(path)
	if total != 9 {
		t.Errorf("total = %d, want 9: the total must count every symbol, not only the displayed groups", total)
	}
	if distinct != 9 {
		t.Errorf("distinct kinds = %d, want 9", distinct)
	}
	if len(kinds) != 6 {
		t.Errorf("displayed kinds = %d, want 6 (the list stays bounded)", len(kinds))
	}
}

// With six or fewer kinds the numbers are unchanged, so the fix cannot be
// passing by inflating every count.
func TestKindInventoryIsExactBelowTheDisplayLimit(t *testing.T) {
	t.Parallel()

	path := seedSymbolKinds(t, map[string]int{"func": 5, "type": 3, "method": 2})
	total, distinct, kinds := semanticIndexKindInventory(path)
	if total != 10 {
		t.Errorf("total = %d, want 10", total)
	}
	if distinct != 3 || len(kinds) != 3 {
		t.Errorf("distinct=%d displayed=%d, want 3 and 3", distinct, len(kinds))
	}
}

// A truncated list must say it is truncated: six of nine kinds presented as the
// whole set is the same misdirection as an under-reported total.
func TestEmptySymbolNoteDisclosesATruncatedKindList(t *testing.T) {
	t.Parallel()

	counts := map[string]int{}
	for i := 0; i < 9; i++ {
		counts[fmt.Sprintf("kind%d", i)] = 1
	}
	path := seedSymbolKinds(t, counts)

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	printSemanticNoSymbolMatch(cmd, path, "someSymbol")

	got := out.String()
	if !strings.Contains(got, "holds 9 symbols") {
		t.Errorf("the note must report the true total:\n%s", got)
	}
	if !strings.Contains(got, "6 of 9 kinds") {
		t.Errorf("the note must say the kind list is partial:\n%s", got)
	}
}

// And with nothing truncated it must not add the parenthetical, or the note
// becomes noise on the common case.
func TestEmptySymbolNoteOmitsTheCountWhenNothingIsTruncated(t *testing.T) {
	t.Parallel()

	path := seedSymbolKinds(t, map[string]int{"func": 2, "type": 1})
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	printSemanticNoSymbolMatch(cmd, path, "someSymbol")

	if got := out.String(); strings.Contains(got, "of 2 kinds") {
		t.Errorf("nothing was truncated, so the note must not say it was:\n%s", got)
	}
}

func TestEmptySymbolNoteDoesNotClaimSnapshotIndexIsEmpty(t *testing.T) {
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	printSemanticNoSymbolMatch(cmd, "", "missingSymbol")
	if !strings.Contains(out.String(), "no symbols found") {
		t.Fatalf("missing no-match diagnostic: %s", out.String())
	}
	for _, unwanted := range []string{"holds no symbols at all", "refresh --semantic"} {
		if strings.Contains(out.String(), unwanted) {
			t.Errorf("snapshot-only search must not claim an empty inventory: %s", out.String())
		}
	}
}
