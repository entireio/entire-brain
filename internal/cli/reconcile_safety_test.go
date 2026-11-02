package cli

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
)

func TestStagedReconcilePreservesDistinctClaims(t *testing.T) {
	cases := []struct{ name, candidate, target string }{
		{"recorded_manifest", "Seed and export write to a shared brain manifest and README through a unified atomic writer (schema 2, Phase 1) to prevent last-writer-wins data loss. Seed owns `seed/*`, export owns `sessions/*`. The writer must deduplicate warnings from merged sources to avoid stale warnings accumulating on subsequent runs.", "Manifest uses a multi-source structure with separate seed and sessions sections, allowing deterministic repo analysis and real Entire session logs to coexist independently. Export cleanup removes only stale session files, never deletes seed/ or the seed manifest section. Brain README describes both sources and the time gap between oldest commit and oldest session, ensuring baseline context persists as session history grows."},
		{"overlap_is_not_entailment", "All writes use atomic rename; retry failed writes with exponential backoff.", "All writes use atomic rename."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
			target := stagedTestFact(tc.target, []string{"architecture.boundaries.rationale"}, factStatusActive, now.Add(-time.Hour))
			candidate := stagedTestFact(tc.candidate, target.Paths, factStatusCandidate, now)
			run := func(context.Context, string, []string, []byte, time.Duration) (string, error) {
				return "1 merge 1 1.0", nil
			}
			got, _, _ := reconcileStagedCandidates(context.Background(), run, nil, []factRecord{target, candidate}, ".", time.Minute, 4, now)
			ci, ti := indexOfFact(got, candidate.ID), indexOfFact(got, target.ID)
			if ci < 0 || ti < 0 || got[ci].Text != tc.candidate || got[ti].Text != tc.target {
				t.Fatalf("neither side's text may be rewritten: facts=%+v", got)
			}
			// Keep-both: the merge decision publishes the candidate and
			// cross-links the pair; nothing is superseded or dropped. The
			// compaction loop settles the pair later (COR-2273).
			if got[ci].Status != factStatusActive || got[ti].Status != factStatusActive {
				t.Fatalf("both sides must stay active: %+v", got)
			}
			if !contains(got[ci].RelatedIDs, target.ID) || !contains(got[ti].RelatedIDs, candidate.ID) {
				t.Fatalf("conflicting pair must be cross-linked: %+v", got)
			}
			noCall := func(context.Context, string, []string, []byte, time.Duration) (string, error) {
				t.Error("published facts were re-adjudicated")
				return "1 new", nil
			}
			resumed, stats, _ := reconcileStagedCandidates(context.Background(), noCall, nil, got, ".", time.Minute, 4, now)
			if stats.Candidates != 0 || resumed[indexOfFact(resumed, candidate.ID)].Status != factStatusActive {
				t.Fatalf("re-run re-staged a published fact: %+v", resumed)
			}
		})
	}
}

func TestStagedReconcileMalformedStaysUnpublished(t *testing.T) {
	for _, output := range []string{"", "1 unknown", "1 merge 99 1", "1 merge", "1 new garbage", "1 new - NaN", "1 merge 1 1\n1 new", "1 new\n1 merge 1 1"} {
		t.Run(output, func(t *testing.T) {
			now := time.Now().UTC()
			target := stagedTestFact("All writes use atomic rename.", []string{"workflow.testing.rules"}, factStatusActive, now)
			candidate := stagedTestFact("All writes use atomic rename and retry failures.", target.Paths, factStatusCandidate, now)
			run := func(context.Context, string, []string, []byte, time.Duration) (string, error) { return output, nil }
			got, _, warnings := reconcileStagedCandidates(context.Background(), run, nil, []factRecord{target, candidate}, ".", time.Minute, 2, now)
			if len(got) != 2 || got[indexOfFact(got, candidate.ID)].Status != factStatusCandidate || len(warnings) == 0 {
				t.Fatalf("malformed result published candidate: %+v %v", got, warnings)
			}
		})
	}
}

func TestStagedForceResumeKeepsCacheBeforeExtraction(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	dir := writeSingleSessionFixture(t, now, "some transcript\n")
	leftover := stagedTestFact("leftover standalone fact", []string{"workflow.testing.rules"}, factStatusCandidate, now)
	if err := factstore.Write(dir, "main", []factRecord{leftover}); err != nil {
		t.Fatal(err)
	}
	if err := saveDistillCache(dir, distillCache{Version: distillCacheVersion, Sessions: map[string]string{"unchanged-session": "fingerprint"}}); err != nil {
		t.Fatal(err)
	}
	run := func(context.Context, string, []string, []byte, time.Duration) (string, error) {
		if cache := loadDistillCache(dir); cache.Sessions["unchanged-session"] != "fingerprint" {
			t.Error("resume flush erased cache before force extraction")
		}
		return "", fmt.Errorf("extraction interrupted")
	}
	if _, err := runDistillForBrain(context.Background(), t.TempDir(), dir, distillCommandOptions{agent: "command", agentCommand: []string{"fake"}, run: run, force: true, timeout: time.Minute}, now); err == nil {
		t.Fatal("expected interrupted extraction")
	}
	if cache := loadDistillCache(dir); cache.Sessions["unchanged-session"] != "fingerprint" {
		t.Fatal("interrupted resume lost unchanged session cache")
	}
}

func TestStagedExactIdentityPreservesStatusAndProvenance(t *testing.T) {
	for _, status := range []string{factStatusActive, factStatusRetracted, factStatusSuperseded} {
		t.Run(status, func(t *testing.T) {
			now := time.Now().UTC()
			original := stagedTestFact("All writes use atomic rename.", []string{"workflow.testing.rules"}, status, now)
			original.Provenance = []factAnchor{{SessionID: "original"}}
			duplicate := original
			duplicate.Status = factStatusCandidate
			duplicate.Provenance = []factAnchor{{SessionID: "duplicate"}}
			staged := upsertFact([]factRecord{original}, duplicate)
			run := func(context.Context, string, []string, []byte, time.Duration) (string, error) {
				t.Error("exact identity should not call the model")
				return "", nil
			}
			got, _, _ := reconcileStagedCandidates(context.Background(), run, nil, staged, ".", time.Minute, 2, now)
			if len(got) != 1 || got[0].Status != status || len(got[0].Provenance) != 2 {
				t.Fatalf("identity upsert changed status or lost provenance: %+v", got)
			}
		})
	}
}
