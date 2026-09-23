package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedFactBranch writes one active fact onto a branch's store.
func seedFactBranch(t *testing.T, brainDir, branch string, ids ...string) {
	t.Helper()
	dir := filepath.Join(brainDir, factsBranchRelDir(branch))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, id := range ids {
		b.WriteString(`{"id":"fact:` + id + `","kind":"decision","branch":"` + branch +
			`","status":"active","text":"a distilled decision"}` + "\n")
	}
	if err := os.WriteFile(filepath.Join(dir, factsFileName), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// declareFacts makes the manifest agree with what was seeded, so the
// store-integrity check ahead of the branch note does not fire first and mask
// what these tests are about.
func declareFacts(t *testing.T, brainDir string, now time.Time, total int, branches ...string) {
	t.Helper()
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Sources.Facts = &factSourceManifest{
		LastDistilledAt: now,
		GeneratedAt:     now,
		Branches:        branches,
		Facts:           total,
		Distilled:       total,
	}
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		t.Fatal(err)
	}
}

// Facts are recorded against the branch the session ran on, so a query from
// main routinely finds nothing while the answer sits on a feature branch. The
// generic coverage note is true there but tells the caller nothing they can
// act on.
func TestBlindSpotNamesOtherBranchesHoldingFacts(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	brainDir := writeDistillFixture(t, now)
	seedFactBranch(t, brainDir, "feat/stage1a", "aaa", "bbb")
	seedFactBranch(t, brainDir, "feat/scale", "ccc")
	declareFacts(t, brainDir, now, 3, "feat/stage1a", "feat/scale")

	note := emptyResultBlindSpotOnBranch(brainDir, "main")
	for _, want := range []string{"0 facts on main", "3 active fact(s)", "2 other branch(es)", "feat/scale", "feat/stage1a", "--branch"} {
		if !strings.Contains(note, want) {
			t.Fatalf("note must contain %q, got %q", want, note)
		}
	}
}

// Branch names come out of a map, and Go randomises map iteration, so an
// unsorted list would differ between identical runs.
func TestBlindSpotOtherBranchesAreDeterministic(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	brainDir := writeDistillFixture(t, now)
	branches := []string{"zeta", "alpha", "mid", "beta"}
	for _, b := range branches {
		seedFactBranch(t, brainDir, b, "id"+b)
	}
	declareFacts(t, brainDir, now, len(branches), branches...)
	first := emptyResultBlindSpotOnBranch(brainDir, "main")
	for i := 0; i < 40; i++ {
		if got := emptyResultBlindSpotOnBranch(brainDir, "main"); got != first {
			t.Fatalf("note must be stable across runs:\n  %q\n  %q", first, got)
		}
	}
	if !strings.Contains(first, "alpha, beta, mid, ...") {
		t.Fatalf("expected the first three branches in sorted order with an elision, got %q", first)
	}
}

// Nothing elsewhere, or no branch supplied, must fall through to the existing
// coverage note rather than claiming facts exist somewhere.
func TestBlindSpotFallsThroughWhenNoOtherBranchHasFacts(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	brainDir := writeDistillFixture(t, now)
	seedFactBranch(t, brainDir, "main", "aaa")
	declareFacts(t, brainDir, now, 1, "main")

	if note := emptyResultBlindSpotOnBranch(brainDir, "main"); !strings.Contains(note, "complete session coverage is unknown") {
		t.Fatalf("facts only on the queried branch must fall through, got %q", note)
	}
	if note := emptyResultBlindSpotOnBranch(brainDir, ""); !strings.Contains(note, "complete session coverage is unknown") {
		t.Fatalf("an unknown branch must fall through, got %q", note)
	}
}

// A retired fact on another branch is not something --branch would surface, so
// promising it would send the caller on a second empty search.
func TestBlindSpotIgnoresInactiveFactsOnOtherBranches(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	brainDir := writeDistillFixture(t, now)
	dir := filepath.Join(brainDir, factsBranchRelDir("feat/old"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, factsFileName),
		[]byte(`{"id":"fact:dead","kind":"decision","branch":"feat/old","status":"retired","text":"superseded"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	declareFacts(t, brainDir, now, 1, "feat/old")
	if note := emptyResultBlindSpotOnBranch(brainDir, "main"); strings.Contains(note, "feat/old") {
		t.Fatalf("a retired fact must not be advertised as reachable, got %q", note)
	}
}
