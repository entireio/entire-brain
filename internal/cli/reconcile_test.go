package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseReconcileActions(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	candidates := []factRecord{
		factFor(t, "cand one", []string{"project.tooling.stack"}, now),
		factFor(t, "cand two", []string{"project.tooling.stack"}, now),
		factFor(t, "cand three", []string{"project.tooling.stack"}, now),
	}
	existing := []factRecord{
		factFor(t, "existing one", []string{"project.tooling.stack"}, now),
		factFor(t, "existing two", []string{"project.tooling.stack"}, now),
	}
	out := strings.Join([]string{
		"1 new - 1.0",
		"2 merge 1 0.88",
		"3 supersede 2 0.93",
	}, "\n")

	actions, warnings := parseReconcileActions(out, candidates, existing)
	if len(actions) != 3 {
		t.Fatalf("expected 3 actions, got %d", len(actions))
	}
	if actions[0].Kind != factActionNew {
		t.Errorf("candidate 1 should be new: %+v", actions[0])
	}
	if actions[1].Kind != factActionMerge || actions[1].TargetID != existing[0].ID || actions[1].Confidence != 0.88 {
		t.Errorf("candidate 2 merge wrong: %+v", actions[1])
	}
	if actions[2].Kind != factActionSupersede || actions[2].TargetID != existing[1].ID {
		t.Errorf("candidate 3 supersede wrong: %+v", actions[2])
	}
	if len(warnings) != 0 {
		t.Errorf("clean output should not warn: %v", warnings)
	}
}

func TestParseReconcileActionsDegradesToNew(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	candidates := []factRecord{
		factFor(t, "c1", []string{"project.tooling.stack"}, now),
		factFor(t, "c2", []string{"project.tooling.stack"}, now),
		factFor(t, "c3", []string{"project.tooling.stack"}, now),
	}
	existing := []factRecord{factFor(t, "e1", []string{"project.tooling.stack"}, now)}

	// c1: merge with out-of-range existing -> new. c2: no line at all -> new.
	// c3: malformed -> new. None must be lost.
	out := "1 merge 5 0.9\n3 garbage line here"
	actions, warnings := parseReconcileActions(out, candidates, existing)
	if len(actions) != 3 {
		t.Fatalf("every candidate must get an action, got %d", len(actions))
	}
	for i, a := range actions {
		if a.Kind != factActionNew {
			t.Errorf("candidate %d should degrade to new, got %s", i+1, a.Kind)
		}
		if a.Candidate.ID != candidates[i].ID {
			t.Errorf("action %d bound to wrong candidate", i)
		}
	}
	if len(warnings) == 0 {
		t.Errorf("expected warnings for the malformed/missing decisions")
	}
}

func TestParseReconcileActionsTolerantFormats(t *testing.T) {
	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	candidates := []factRecord{
		factFor(t, "c1", []string{"project.tooling.stack"}, now),
		factFor(t, "c2", []string{"project.tooling.stack"}, now),
		factFor(t, "c3", []string{"project.tooling.stack"}, now),
	}
	existing := []factRecord{factFor(t, "e1", []string{"project.tooling.stack"}, now)}

	// c1: "new" missing the "-" placeholder (gpt-5.3-codex-spark form).
	// c2: "new" with neither placeholder nor confidence.
	// c3: merge with the confidence omitted -> conf 0 (queued for review).
	out := "1 new 1.0\n2 new\n3 merge 1"
	actions, warnings := parseReconcileActions(out, candidates, existing)
	if len(actions) != 3 {
		t.Fatalf("expected 3 actions, got %d (warnings=%v)", len(actions), warnings)
	}
	if actions[0].Kind != factActionNew || actions[0].Confidence != 1.0 {
		t.Errorf("c1 should be new@1.0: %+v", actions[0])
	}
	if actions[1].Kind != factActionNew || actions[1].Confidence != 1.0 {
		t.Errorf("c2 should be new@1.0: %+v", actions[1])
	}
	if actions[2].Kind != factActionMerge || actions[2].TargetID != existing[0].ID || actions[2].Confidence != 0 {
		t.Errorf("c3 should be merge->e1 @0 (queued): %+v", actions[2])
	}
	// These tolerant forms must NOT warn — they are accepted, not degraded.
	if len(warnings) != 0 {
		t.Errorf("tolerant forms should not warn: %v", warnings)
	}
}

func TestParseConfidence(t *testing.T) {
	// NaN/±Inf parse via ParseFloat but must default to 0: a NaN confidence would
	// slip past the [0,1] clamp and silently bypass the auto-apply threshold.
	cases := map[string]float64{"0.5": 0.5, "1.0": 1.0, "-2": 0, "5": 1, "abc": 0, "": 0, "NaN": 0, "Inf": 0, "-Inf": 0}
	for in, want := range cases {
		if got := parseConfidence(in); got != want {
			t.Errorf("parseConfidence(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestIsNumericTokenRejectsNonFinite(t *testing.T) {
	for _, tok := range []string{"NaN", "Inf", "+Inf", "-Inf", "infinity"} {
		if isNumericToken(tok) {
			t.Errorf("isNumericToken(%q) = true, want false (non-finite must not count as a confidence)", tok)
		}
	}
	for _, tok := range []string{"0", "0.5", "1.0", "-2"} {
		if !isNumericToken(tok) {
			t.Errorf("isNumericToken(%q) = false, want true", tok)
		}
	}
}

// A merge with NaN confidence must fall back to zero so it is queued for review.
func TestParseReconcileActionsRejectsNaNConfidence(t *testing.T) {
	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	candidates := []factRecord{factFor(t, "c1", []string{"project.tooling.stack"}, now)}
	existing := []factRecord{factFor(t, "e1", []string{"project.tooling.stack"}, now)}

	actions, _ := parseReconcileActions("1 merge 1 NaN", candidates, existing)
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if actions[0].Kind != factActionMerge || actions[0].Confidence != 0 {
		t.Errorf("NaN confidence on merge must clamp to 0 (queued for review): %+v", actions[0])
	}
}

func TestActiveFactsAtPaths(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	a := factFor(t, "a", []string{"project.tooling.stack"}, now)
	b := factFor(t, "b", []string{"architecture.data.flow"}, now)
	c := factFor(t, "c", []string{"project.tooling.stack"}, now.Add(time.Hour))
	superseded := factFor(t, "old", []string{"project.tooling.stack"}, now)
	superseded.Status = factStatusSuperseded

	matched, capped := activeFactsAtPaths([]factRecord{a, b, c, superseded}, []string{"project.tooling.stack"})
	if capped {
		t.Errorf("should not be capped")
	}
	if len(matched) != 2 {
		t.Fatalf("expected 2 active facts at path (superseded excluded, other path excluded), got %d", len(matched))
	}
	// Most-recently-updated first.
	if matched[0].ID != c.ID {
		t.Errorf("expected most-recent fact first")
	}
}

// writeSameBranchFixture puts two sessions on the same branch so the reconcile
// pass actually fires on the second session.
func writeSameBranchFixture(t *testing.T, now time.Time) string {
	t.Helper()
	brainDir := t.TempDir()
	sessions := []exportSession{
		{SessionID: "s1", Branch: "main", LatestCheckpoint: "cp1", TranscriptPath: "sessions/main/s1.jsonl", CreatedAt: now.Add(-2 * time.Hour)},
		{SessionID: "s2", Branch: "main", LatestCheckpoint: "cp2", TranscriptPath: "sessions/main/s2.jsonl", CreatedAt: now.Add(-1 * time.Hour)},
	}
	for _, s := range sessions {
		path := filepath.Join(brainDir, filepath.FromSlash(s.TranscriptPath))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("turn\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		GeneratedAt:   now,
		DefaultBranch: "main",
		Sources:       &brainSources{Sessions: &sessionSourceManifest{GeneratedAt: now, DefaultBranch: "main", Sessions: sessions}},
	}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}
	return brainDir
}

func TestRunDistillForBrainReconcilesMerge(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	brainDir := writeSameBranchFixture(t, now)

	var distillCalls, reconcileCalls int
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		if strings.HasPrefix(string(input), "CANDIDATES") {
			reconcileCalls++
			// Merge the new candidate (1) into the existing fact (1).
			return "1 merge 1 0.9\n", nil
		}
		distillCalls++
		// Two distinct statements at the same path across the two sessions.
		if distillCalls == 1 {
			return "project.tooling.stack\tThe project uses Go 1.26.\n", nil
		}
		return "project.tooling.stack\tThe stack is Go.\n", nil
	}
	opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: run, maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute, confidenceThreshold: defaultFactConfidenceThreshold}

	source, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now)
	if err != nil {
		t.Fatalf("runDistillForBrain: %v", err)
	}
	if reconcileCalls == 0 {
		t.Fatalf("reconcile pass never fired despite same-path facts")
	}
	// Session 1 created fact A; session 2's candidate merged into A, so only one
	// fact remains, with provenance from both sessions.
	facts, err := loadFacts(brainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected merge to leave 1 fact, got %d", len(facts))
	}
	if len(facts[0].Provenance) != 2 {
		t.Fatalf("merge should union provenance from both sessions, got %d", len(facts[0].Provenance))
	}
	if source.Facts != 1 {
		t.Errorf("manifest should report 1 fact, got %d", source.Facts)
	}
}

func TestRunDistillForBrainReconcileQueuesProposal(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	brainDir := writeSameBranchFixture(t, now)

	calls := 0
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		if strings.HasPrefix(string(input), "CANDIDATES") {
			// Low-confidence supersede: must NOT auto-apply; queued instead.
			return "1 supersede 1 0.3\n", nil
		}
		calls++
		if calls == 1 {
			return "workflow.testing.rules\tRun unit tests before pushing.\n", nil
		}
		return "workflow.testing.rules\tRun the full suite before merging.\n", nil
	}
	opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: run, maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute, confidenceThreshold: defaultFactConfidenceThreshold}

	source, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now)
	if err != nil {
		t.Fatalf("runDistillForBrain: %v", err)
	}
	facts, err := loadFacts(brainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 2 {
		t.Fatalf("low-confidence supersede should keep both facts, got %d", len(facts))
	}
	for _, f := range facts {
		if f.Status != factStatusActive {
			t.Errorf("no fact should be superseded below threshold: %+v", f)
		}
	}
	if source.Proposals != 1 {
		t.Errorf("expected 1 queued proposal, got %d", source.Proposals)
	}
	// Proposal must be persisted for `facts review`.
	proposals, err := loadFactProposals(brainDir, "main")
	if err != nil || len(proposals) != 1 {
		t.Fatalf("expected 1 persisted proposal, got %d (%v)", len(proposals), err)
	}
	if proposals[0].Action != factActionSupersede {
		t.Errorf("wrong proposal action: %+v", proposals[0])
	}
}
