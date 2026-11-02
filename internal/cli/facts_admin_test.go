package cli

import (
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
)

func TestRetractFact(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	a := factFor(t, "active fact", []string{"project.tooling.stack"}, now)
	facts := []factRecord{a}

	found, changed := retractFact(facts, a.ID, now.Add(time.Hour))
	if !found || !changed {
		t.Fatalf("expected found+changed, got found=%v changed=%v", found, changed)
	}
	if facts[0].Status != factStatusRetracted || !facts[0].UpdatedAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("fact not retracted/stamped: %+v", facts[0])
	}
	// Retracting again is a no-op (found, not changed).
	found, changed = retractFact(facts, a.ID, now.Add(2*time.Hour))
	if !found || changed {
		t.Fatalf("re-retract should be found-but-unchanged, got found=%v changed=%v", found, changed)
	}
	// Unknown id.
	if found, _ := retractFact(facts, "fact:nope", now); found {
		t.Fatalf("unknown id should not be found")
	}
	// A retracted fact drops out of active recall and gc prunes it.
	if got := rankFacts(facts, "", 10, false); len(got) != 0 {
		t.Fatalf("retracted fact should not surface in active recall")
	}
	res := gcFacts(facts, factstore.DefaultTaxonomy(now), now, defaultFactRetention)
	if len(res.Pruned) != 1 || len(res.Kept) != 0 {
		t.Fatalf("gc should prune the retracted fact: %+v", res)
	}
}

func TestGCFacts(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	active := factFor(t, "active", []string{"project.tooling.stack"}, now)
	retracted := factFor(t, "retracted", []string{"project.tooling.stack"}, now)
	retracted.Status = factStatusRetracted
	oldSuperseded := factFor(t, "old superseded", []string{"project.tooling.stack"}, now.Add(-60*24*time.Hour))
	oldSuperseded.Status = factStatusSuperseded
	recentSuperseded := factFor(t, "recent superseded", []string{"project.tooling.stack"}, now.Add(-1*24*time.Hour))
	recentSuperseded.Status = factStatusSuperseded
	orphan := factFor(t, "orphan", []string{"project.tooling.stack"}, now)
	orphan.Paths = []string{"gone.sub.type"} // top-level not in taxonomy

	tax := factstore.DefaultTaxonomy(now)
	result := gcFacts([]factRecord{active, retracted, oldSuperseded, recentSuperseded, orphan}, tax, now, defaultFactRetention)

	if len(result.Pruned) != 2 {
		t.Fatalf("expected retracted + old-superseded pruned (2), got %d", len(result.Pruned))
	}
	if len(result.Orphans) != 1 || result.Orphans[0].Text != "orphan" {
		t.Fatalf("expected 1 reported orphan, got %d", len(result.Orphans))
	}
	// Orphan is active and not old → kept (orphans are never auto-deleted).
	if indexOfFact(result.Kept, orphan.ID) < 0 {
		t.Fatalf("active orphan should be kept, not pruned")
	}
	if indexOfFact(result.Kept, recentSuperseded.ID) < 0 {
		t.Fatalf("recent superseded should be retained within the window")
	}
}
