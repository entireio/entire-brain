package cli

import (
	"testing"
	"time"
)

func treeFact(id, path, text string, prov int, now time.Time) factRecord {
	return factRecord{
		ID:         id,
		Paths:      []string{path},
		Text:       text,
		Status:     factStatusActive,
		Provenance: make([]factAnchor, prov),
		UpdatedAt:  now,
	}
}

func TestBuildFactTreeGroupsAndOrders(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	facts := []factRecord{
		treeFact("a", "constraints.invariants.general", "inv 1", 1, now),
		treeFact("b", "constraints.invariants.general", "inv 2", 3, now), // more provenance
		treeFact("c", "constraints.security.general", "sec 1", 1, now),
		treeFact("d", "preferences.coding.style", "spaces not tabs", 1, now),
		{ID: "e", Paths: []string{"constraints.invariants.general"}, Text: "superseded", Status: factStatusSuperseded, UpdatedAt: now},
	}
	tree := buildFactTree(facts, false)

	// Root count excludes the superseded fact (4 active).
	if tree.Count != 4 {
		t.Fatalf("root count = %d, want 4 active", tree.Count)
	}
	// constraints (3) should sort before preferences (1).
	if tree.Children[0].Label != "constraints" || tree.Children[0].Count != 3 {
		t.Fatalf("top category wrong: %+v", tree.Children[0])
	}
	if tree.Children[1].Label != "preferences" {
		t.Fatalf("expected preferences second, got %s", tree.Children[1].Label)
	}
	// Within constraints, invariants.general (2) before security.general (1).
	inv := tree.Children[0].Children[0]
	if inv.Label != "constraints.invariants.general" || inv.Count != 2 {
		t.Fatalf("leaf order/count wrong: %+v", inv)
	}
	// Importance ordering: the 3-anchor fact ("inv 2") ranks first.
	if inv.Facts[0].ID != "b" {
		t.Fatalf("expected most-corroborated fact first, got %s", inv.Facts[0].ID)
	}
}

func TestBuildFactTreeIncludeAll(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	facts := []factRecord{
		treeFact("a", "project.tooling.stack", "active", 1, now),
		{ID: "b", Paths: []string{"project.tooling.stack"}, Text: "old", Status: factStatusSuperseded, UpdatedAt: now},
	}
	if got := buildFactTree(facts, false).Count; got != 1 {
		t.Fatalf("active-only count = %d, want 1", got)
	}
	if got := buildFactTree(facts, true).Count; got != 2 {
		t.Fatalf("includeAll count = %d, want 2", got)
	}
}

func TestDistinctFactCountAcrossCategories(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	// One fact carries two paths in DIFFERENT categories; node counts total path
	// occurrences (so it appears in both), but distinctFactCount must count it once.
	multi := factRecord{ID: "m", Paths: []string{"architecture.data.flow", "workflow.testing.rules"}, Status: factStatusActive, UpdatedAt: now}
	single := treeFact("s", "project.tooling.stack", "single", 1, now)
	tree := buildFactTree([]factRecord{multi, single}, false)

	// Occurrence roll-up double-counts the multi-path fact (3 = 2 occurrences + 1).
	if tree.Count != 3 {
		t.Fatalf("node Count should total path occurrences (3), got %d", tree.Count)
	}
	// Distinct count is the real number of facts (2).
	if got := distinctFactCount(tree); got != 2 {
		t.Fatalf("distinctFactCount = %d, want 2", got)
	}
}

func TestFilterTreeByPath(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	facts := []factRecord{
		treeFact("a", "constraints.invariants.general", "x", 1, now),
		treeFact("b", "constraints.security.general", "y", 1, now),
		treeFact("c", "preferences.coding.style", "z", 1, now),
	}
	tree := buildFactTree(facts, false)

	// Category prefix.
	c := filterTreeByPath(tree, "constraints")
	if c == nil || c.Count != 2 || len(c.Children) != 1 || c.Children[0].Label != "constraints" {
		t.Fatalf("category filter wrong: %+v", c)
	}
	// Sub-path prefix selects matching leaves only.
	s := filterTreeByPath(tree, "constraints.security")
	if s == nil || s.Count != 1 {
		t.Fatalf("sub-path filter wrong: %+v", s)
	}
	// Full path.
	f := filterTreeByPath(tree, "preferences.coding.style")
	if f == nil || f.Count != 1 {
		t.Fatalf("full-path filter wrong: %+v", f)
	}
	// No match.
	if filterTreeByPath(tree, "nonexistent") != nil {
		t.Fatalf("expected nil for no match")
	}
}
