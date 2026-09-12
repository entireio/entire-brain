package cli

import (
	"context"
	"testing"
	"time"
)

// forceDistillFixture seeds the standard distill fixture plus one retired
// distilled fact whose text/paths are exactly what the fake agent re-emits, so a
// force rebuild is guaranteed to regenerate the same content-derived id.
func forceDistillFixture(t *testing.T, now time.Time, status, supersededBy string) (string, factRecord, distillCommandOptions) {
	t.Helper()
	brainDir := writeDistillFixture(t, now)
	text := "The project uses Go."
	paths := normalizeFactPaths([]string{"project.tooling.stack"})
	retired := factRecord{
		ID: factRecordID(text, paths), Paths: paths, Text: text, Branch: "main",
		Origin: factOriginDistilled, Status: status, SupersededBy: supersededBy,
		Provenance: []factAnchor{{SessionID: "s1"}}, CreatedAt: now, UpdatedAt: now,
	}
	if err := writeFacts(brainDir, "main", []factRecord{retired}); err != nil {
		t.Fatal(err)
	}
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		return "project.tooling.stack\tThe project uses Go.\n", nil
	}
	opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: run, force: true, maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute}
	return brainDir, retired, opts
}

// `facts retract` is a human tombstone that distillation never replays. A force
// rebuild must not drop it, or the same statement is re-distilled as ACTIVE and
// a fact the user declared false comes back to life without a word.
func TestForceDistillDoesNotResurrectRetractedFact(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	brainDir, retracted, opts := forceDistillFixture(t, now, factStatusRetracted, "")

	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
		t.Fatalf("force run: %v", err)
	}

	facts, err := loadFacts(brainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	i := indexOfFact(facts, retracted.ID)
	if i < 0 {
		t.Fatalf("the retracted fact vanished from the store: %+v", facts)
	}
	if facts[i].Status != factStatusRetracted {
		t.Fatalf("distill --force resurrected a retracted fact as %q: %+v", facts[i].Status, facts[i])
	}
}

// The same holds for a fact retired by a supersede the rebuild does not replay
// (an applied review proposal, a promote, a synced settlement): the tombstone and
// its replacement pointer must survive.
func TestForceDistillDoesNotResurrectSupersededFact(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	brainDir, superseded, opts := forceDistillFixture(t, now, factStatusSuperseded, "fact:replacement")

	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
		t.Fatalf("force run: %v", err)
	}

	facts, err := loadFacts(brainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	i := indexOfFact(facts, superseded.ID)
	if i < 0 {
		t.Fatalf("the superseded fact vanished from the store: %+v", facts)
	}
	if facts[i].Status != factStatusSuperseded || facts[i].SupersededBy != "fact:replacement" {
		t.Fatalf("distill --force resurrected a superseded fact: %+v", facts[i])
	}
}

// The rebuild must still be a rebuild: an ACTIVE distilled fact the sources no
// longer support is dropped, not carried forward.
func TestForceDistillStillRebuildsActiveDistilledFacts(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	brainDir := writeDistillFixture(t, now)
	stalePaths := normalizeFactPaths([]string{"project.ci_cd.pipeline"})
	staleText := "The project deploys from a bespoke shell script."
	stale := factRecord{
		ID: factRecordID(staleText, stalePaths), Paths: stalePaths, Text: staleText, Branch: "main",
		Origin: factOriginDistilled, Status: factStatusActive,
		Provenance: []factAnchor{{SessionID: "s1"}}, CreatedAt: now, UpdatedAt: now,
	}
	if err := writeFacts(brainDir, "main", []factRecord{stale}); err != nil {
		t.Fatal(err)
	}
	run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
		return "project.tooling.stack\tThe project uses Go.\n", nil
	}
	opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: run, force: true, maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute}
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
		t.Fatalf("force run: %v", err)
	}
	facts, err := loadFacts(brainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if indexOfFact(facts, stale.ID) >= 0 {
		t.Fatalf("--force kept an active distilled fact the rebuild no longer produces: %+v", facts)
	}
}
