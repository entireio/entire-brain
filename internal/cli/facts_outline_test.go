package cli

import (
	"context"
	"strings"
	"testing"
	"time"
)

func outlineTestFacts() []factRecord {
	return []factRecord{
		{ID: "f1", Status: factStatusActive, Paths: []string{"architecture.data.flow"}, Locus: []string{"internal/cli/facts.go", "factrecord"}, Text: "factRecord stores the durable fact."},
		{ID: "f2", Status: factStatusActive, Paths: []string{"architecture.data.flow"}, Locus: []string{"internal/cli/distill.go"}, Text: "Distill parses agent output."},
		{ID: "f3", Status: factStatusActive, Paths: []string{"architecture.data.flow"}, Locus: []string{"internal/db/store.go"}, Text: "The store is a SQLite index."},
		{ID: "f4", Status: factStatusActive, Paths: []string{"preferences.coding.style"}, Text: "The user prefers tabs."},                          // cross-cutting → root
		{ID: "f5", Status: factStatusActive, Paths: []string{"project.tooling.stack"}, Text: "The project is written in Go."},                      // no locus → root
		{ID: "f6", Status: factStatusSuperseded, Paths: []string{"architecture.data.flow"}, Locus: []string{"internal/cli/facts.go"}, Text: "old"}, // excluded
	}
}

func TestBuildFactOutlineTree(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	outline := buildFactOutlineTree("main", outlineTestFacts(), now)

	for _, key := range []string{"", "internal", "internal/cli", "internal/db"} {
		if _, ok := outline.Nodes[key]; !ok {
			t.Fatalf("expected node %q to exist", key)
		}
	}
	if got := outline.Nodes["internal/cli"].LeafFactIDs; len(got) != 2 || got[0] != "f1" || got[1] != "f2" {
		t.Fatalf("internal/cli leaves wrong: %v", got)
	}
	if got := outline.Nodes["internal/db"].LeafFactIDs; len(got) != 1 || got[0] != "f3" {
		t.Fatalf("internal/db leaves wrong: %v", got)
	}
	// Cross-cutting (f4) and no-locus (f5) facts home at the root; the superseded
	// f6 is excluded.
	if got := outline.Nodes[""].LeafFactIDs; len(got) != 2 || got[0] != "f4" || got[1] != "f5" {
		t.Fatalf("root leaves wrong: %v", got)
	}
	// internal is an intermediate node with no direct leaves but two children.
	if got := outline.Nodes["internal"].Children; len(got) != 2 || got[0] != "internal/cli" || got[1] != "internal/db" {
		t.Fatalf("internal children wrong: %v", got)
	}
	if got := subtreeFactCount(outline, ""); got != 5 {
		t.Fatalf("root subtree count = %d, want 5 (active only)", got)
	}
	if got := subtreeFactCount(outline, "internal"); got != 3 {
		t.Fatalf("internal subtree count = %d, want 3", got)
	}
}

func TestOutlineFingerprintChangesWithLeaf(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	a := buildFactOutlineTree("main", outlineTestFacts(), now)
	facts := outlineTestFacts()
	facts = append(facts, factRecord{ID: "f7", Status: factStatusActive, Paths: []string{"architecture.data.flow"}, Locus: []string{"internal/cli/embed.go"}, Text: "Embeds facts."})
	b := buildFactOutlineTree("main", facts, now)
	// Adding a leaf under internal/cli must change that node's fingerprint AND the
	// root's (it propagates up), but not the unrelated internal/db node.
	if a.Nodes["internal/cli"].Fingerprint == b.Nodes["internal/cli"].Fingerprint {
		t.Fatalf("internal/cli fingerprint should change when a leaf is added")
	}
	if a.Nodes[""].Fingerprint == b.Nodes[""].Fingerprint {
		t.Fatalf("root fingerprint should change (propagation)")
	}
	if a.Nodes["internal/db"].Fingerprint != b.Nodes["internal/db"].Fingerprint {
		t.Fatalf("unrelated internal/db fingerprint must not change")
	}
}

func TestGenerateFactOutlineIncremental(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	brainDir := t.TempDir()
	calls := 0
	runner := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		calls++
		return "Rollup summary.\n", nil
	}
	opts := outlineGenOptions{agent: "command", agentCommand: []string{"fake"}, minFacts: 2, run: runner}

	// First generation: nodes with subtree >= 2 are summarized — root(5),
	// internal(3), internal/cli(2). internal/db(1) is skipped.
	outline, regen, err := generateFactOutline(context.Background(), "/repo", brainDir, "main", outlineTestFacts(), opts, now)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if calls != 3 || regen != 3 {
		t.Fatalf("first gen: calls=%d regen=%d, want 3/3", calls, regen)
	}
	if outline.Nodes["internal/cli"].Summary == "" {
		t.Fatalf("internal/cli should have a summary")
	}
	if outline.Nodes["internal/db"].Summary != "" {
		t.Fatalf("internal/db is below min-facts and should be unsummarized")
	}
	if err := writeFactOutline(brainDir, "main", outline); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Second generation, unchanged facts: every fingerprint matches → 0 agent calls.
	calls = 0
	outline2, regen2, err := generateFactOutline(context.Background(), "/repo", brainDir, "main", outlineTestFacts(), opts, now)
	if err != nil {
		t.Fatalf("regen: %v", err)
	}
	if calls != 0 || regen2 != 0 {
		t.Fatalf("incremental no-op: calls=%d regen=%d, want 0/0", calls, regen2)
	}
	if outline2.Nodes["internal/cli"].Summary != "Rollup summary." {
		t.Fatalf("summary not carried over: %q", outline2.Nodes["internal/cli"].Summary)
	}
}

func TestGenerateFactOutlineBudget(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	calls := 0
	runner := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		calls++
		return "S.", nil
	}
	opts := outlineGenOptions{agent: "command", agentCommand: []string{"fake"}, minFacts: 2, budget: 1, run: runner}
	if _, _, err := generateFactOutline(context.Background(), "/repo", t.TempDir(), "main", outlineTestFacts(), opts, now); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if calls != 1 {
		t.Fatalf("budget should cap agent calls at 1, got %d", calls)
	}
}

func TestRenderFactOutline(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	outline := buildFactOutlineTree("main", outlineTestFacts(), now)
	n := outline.Nodes["internal/cli"]
	n.Summary = "Fact storage and distillation."
	outline.Nodes["internal/cli"] = n

	var b strings.Builder
	renderFactOutline(&b, outline, "", 0)
	text := b.String()
	if !strings.Contains(text, "root (5 facts)") {
		t.Fatalf("root line missing or miscounted:\n%s", text)
	}
	if !strings.Contains(text, "cli — Fact storage and distillation. (2 facts)") {
		t.Fatalf("summary line missing:\n%s", text)
	}

	// Drill into a subtree.
	var sub strings.Builder
	renderFactOutline(&sub, outline, "internal/db", 0)
	if !strings.Contains(sub.String(), "db (1 facts)") || strings.Contains(sub.String(), "cli") {
		t.Fatalf("path drill-down wrong:\n%s", sub.String())
	}
}
