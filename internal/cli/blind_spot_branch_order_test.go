package cli

import (
	"strings"
	"testing"
	"time"
)

// The branch note must win over the coverage notes when BOTH are true.
//
// Checked last, this feature was unreachable in the case it exists for.
// `undigested > 0` holds in every repository where a session was captured
// since the last distillation -- which is every actively worked repository,
// and you are on a feature branch BECAUSE you have been working. So the note
// that said "your facts are on branch X, retry with --branch X" was replaced
// by "coverage of older sessions is unknown", which the caller cannot act on.
//
// The existing fixtures could not catch this: declareFacts sets
// LastDistilledAt to now while the sessions it writes are hours old, so
// undigested is always 0 and the branch path is never reached. This test sets
// the timestamps the other way round on purpose.
func TestBranchNoteWinsOverTheUndigestedCoverageNote(t *testing.T) {
	t.Parallel()

	brainDir := t.TempDir()
	now := time.Now().UTC()

	// Facts on main, queried from a feature branch.
	if err := writeFacts(brainDir, "main", []factRecord{
		vitalityTestFact("retries are capped at three", "main", now),
	}); err != nil {
		t.Fatalf("write facts: %v", err)
	}
	// Sessions captured recently, last distillation 48h ago: undigested > 0,
	// which is the ordinary state of a repository someone is working in.
	writeDistillFixtureAt(t, brainDir, now)
	declareFactsAt(t, brainDir, now.Add(-48*time.Hour), now, 1, "main")

	note := emptyResultBlindSpotOnBranch(brainDir, "feature/x")
	if note == "" {
		t.Fatal("an empty result on a branch with facts elsewhere must be explained")
	}
	if strings.Contains(note, "coverage of older sessions is unknown") {
		t.Fatalf("the unactionable coverage note displaced the branch note; got: %s", note)
	}
	for _, want := range []string{"main", "--branch"} {
		if !strings.Contains(note, want) {
			t.Errorf("the note must name where the facts are and how to reach them; missing %q in: %s", want, note)
		}
	}
}

// A brain whose facts were AUTHORED with `remember` has no distillation
// timestamp at all, so the coverage branch returned "no distillation timestamp
// is recorded" and the branch note never ran. That is the same suppression
// from a different direction.
func TestBranchNoteFiresOnABrainThatWasNeverDistilled(t *testing.T) {
	t.Parallel()

	brainDir := t.TempDir()
	now := time.Now().UTC()
	if err := writeFacts(brainDir, "main", []factRecord{
		vitalityTestFact("retries are capped at three", "main", now),
	}); err != nil {
		t.Fatal(err)
	}
	// No LastDistilledAt: authored facts only.
	declareFactsAt(t, brainDir, time.Time{}, now, 1, "main")

	note := emptyResultBlindSpotOnBranch(brainDir, "feature/x")
	if strings.Contains(note, "no distillation timestamp is recorded") {
		t.Fatalf("a remember-only brain dead-ended on the distill note instead of naming the branch; got: %s", note)
	}
	if !strings.Contains(note, "main") {
		t.Errorf("the note must still name the branch holding the facts; got: %s", note)
	}
}

// declareFactsAt is declareFacts with the distill timestamp under test control.
// The existing helper hardcodes LastDistilledAt=now, which is exactly what made
// the undigested>0 path untestable.
func declareFactsAt(t *testing.T, brainDir string, lastDistilled, generated time.Time, total int, branches ...string) {
	t.Helper()
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	// A brain with no manifest yet loads as an empty struct with nil Sources;
	// a remember-only brain is exactly that case.
	if manifest.Sources == nil {
		manifest.Sources = &brainSources{}
	}
	manifest.Sources.Facts = &factSourceManifest{
		LastDistilledAt: lastDistilled,
		GeneratedAt:     generated,
		Branches:        branches,
		Facts:           total,
		Distilled:       total,
	}
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		t.Fatal(err)
	}
}
