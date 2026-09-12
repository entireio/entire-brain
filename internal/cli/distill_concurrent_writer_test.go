package cli

import (
	"context"
	"testing"
	"time"
)

// distillConcurrentWriterFixture seeds a brain with one active fact and returns
// it alongside a second fact the test authors mid-run.
func distillConcurrentWriterFixture(t *testing.T, now time.Time) (string, factRecord, factRecord) {
	t.Helper()
	brainDir := writeDistillFixture(t, now)
	paths := normalizeFactPaths([]string{"workflow.testing.rules"})
	seeded := factRecord{
		ID: factRecordID("Always run go test before pushing.", paths), Paths: paths,
		Text: "Always run go test before pushing.", Branch: "main",
		Origin: factOriginAuthored, Status: factStatusActive,
		Provenance: []factAnchor{{SessionID: "seed"}}, CreatedAt: now, UpdatedAt: now,
	}
	if err := writeFacts(brainDir, "main", []factRecord{seeded}); err != nil {
		t.Fatal(err)
	}
	newPaths := normalizeFactPaths([]string{"preferences.coding.style"})
	authored := factRecord{
		ID: factRecordID("The user prefers tabs.", newPaths), Paths: newPaths,
		Text: "The user prefers tabs.", Branch: "main",
		Origin: factOriginAuthored, Status: factStatusActive,
		Provenance: []factAnchor{{SessionID: "mid-run"}},
		CreatedAt:  now.Add(time.Minute), UpdatedAt: now.Add(time.Minute),
	}
	return brainDir, seeded, authored
}

// A distill run loads a branch's facts once and rewrites the whole set at every
// flush, so its read-modify-write spans the entire run — hours of agent calls.
// Everything another writer commits in between (remember, facts retract, an
// applied review, a settlement mirrored by facts sync) must survive the flush.
func TestDistillFlushKeepsConcurrentWrites(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	for _, force := range []bool{false, true} {
		name := "incremental"
		if force {
			name = "force"
		}
		t.Run(name, func(t *testing.T) {
			brainDir, seeded, authored := distillConcurrentWriterFixture(t, now)
			fired := false
			run := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
				if !fired {
					fired = true
					// Exactly what `facts retract` and `remember` do: a whole
					// transaction under the brain write lock, mid-run.
					if err := withBrainWriteLock(brainDir, func() error {
						facts, err := loadFacts(brainDir, "main")
						if err != nil {
							return err
						}
						retractFact(facts, seeded.ID, now.Add(time.Minute))
						facts = upsertFact(facts, authored)
						return writeFacts(brainDir, "main", facts)
					}); err != nil {
						t.Errorf("concurrent write: %v", err)
					}
				}
				return "project.tooling.stack\tThe project uses Go.\n", nil
			}
			opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: run,
				maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute, flushEvery: 1, force: force}
			if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
				t.Fatalf("distill: %v", err)
			}

			facts, err := loadFacts(brainDir, "main")
			if err != nil {
				t.Fatal(err)
			}
			if indexOfFact(facts, authored.ID) < 0 {
				t.Errorf("a fact authored during the run was silently erased by the run's flush")
			}
			i := indexOfFact(facts, seeded.ID)
			if i < 0 {
				t.Fatalf("the seeded fact vanished entirely: %+v", facts)
			}
			if facts[i].Status != factStatusRetracted {
				t.Errorf("a retraction made during the run was silently reverted to %q", facts[i].Status)
			}
			// The run's own work still lands.
			distilled := 0
			for _, f := range facts {
				if f.Origin == factOriginDistilled {
					distilled++
				}
			}
			if distilled == 0 {
				t.Errorf("the run produced no distilled facts: %+v", facts)
			}
		})
	}
}

// The rebase must not turn a --force rebuild into an append: an ACTIVE distilled
// fact the rebuild no longer produces is still dropped.
func TestDistillForceStillDropsStaleDistilledFacts(t *testing.T) {
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
	opts := distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: run,
		maxChunkBytes: defaultDistillChunkSize, timeout: time.Minute, flushEvery: 1, force: true}
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), brainDir, opts, now); err != nil {
		t.Fatalf("distill: %v", err)
	}
	facts, err := loadFacts(brainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if indexOfFact(facts, stale.ID) >= 0 {
		t.Fatalf("--force kept an active distilled fact the rebuild no longer produces: %+v", facts)
	}
}
