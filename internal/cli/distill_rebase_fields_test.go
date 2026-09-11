package cli

import (
	"slices"
	"testing"
	"time"
)

func TestDistillRebasePreservesConcurrentFields(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	for _, kind := range []string{"decision", "invariant"} {
		dir, seed, _ := distillConcurrentWriterFixture(t, now)
		baseline := cloneDistillFacts([]factRecord{seed})
		local := cloneDistillFacts(baseline)
		local[0].Provenance = append(local[0].Provenance, factAnchor{SessionID: "distill"})
		disk := seed
		disk.Kind = kind
		disk.UpdatedAt = now.Add(time.Minute)
		disk.Provenance = append(disk.Provenance, factAnchor{SessionID: "concurrent"})
		if err := writeFacts(dir, "main", []factRecord{disk}); err != nil {
			t.Fatal(err)
		}
		got, err := rebaseFactsOntoDisk(dir, "main", local, baseline, false)
		if err != nil {
			t.Fatal(err)
		}
		if got[0].Kind != kind {
			t.Fatalf("lost concurrent kind: %q", got[0].Kind)
		}
		seen := map[string]bool{}
		for _, a := range got[0].Provenance {
			seen[a.SessionID] = true
		}
		if !seen["distill"] || !seen["concurrent"] {
			t.Fatal("lost provenance")
		}
	}
}

func TestDistillRebasePreservesReactivation(t *testing.T) {
	now := time.Now().UTC()
	dir, seed, _ := distillConcurrentWriterFixture(t, now)
	seed.Status = factStatusRetracted
	baseline := cloneDistillFacts([]factRecord{seed})
	disk := seed
	disk.Status = factStatusActive
	if err := writeFacts(dir, "main", []factRecord{disk}); err != nil {
		t.Fatal(err)
	}
	got, err := rebaseFactsOntoDisk(dir, "main", cloneDistillFacts(baseline), baseline, false)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Status != factStatusActive {
		t.Fatal("lost concurrent reactivation")
	}
}

func TestDistillRebaseMergesRelatedIDs(t *testing.T) {
	now := time.Now().UTC()
	dir, seed, _ := distillConcurrentWriterFixture(t, now)
	seed.RelatedIDs = []string{"keep", "removed-on-disk", "removed-locally"}
	baseline := cloneDistillFacts([]factRecord{seed})
	local := cloneDistillFacts(baseline)
	local[0].RelatedIDs = []string{"keep", "removed-on-disk", "distill-added"}
	disk := seed
	disk.RelatedIDs = []string{"keep", "removed-locally", "concurrent-added"}
	if err := writeFacts(dir, "main", []factRecord{disk}); err != nil {
		t.Fatal(err)
	}
	got, err := rebaseFactsOntoDisk(dir, "main", local, baseline, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"keep", "concurrent-added", "distill-added"}
	if !slices.Equal(got[0].RelatedIDs, want) {
		t.Fatalf("related IDs = %v, want %v", got[0].RelatedIDs, want)
	}
}
