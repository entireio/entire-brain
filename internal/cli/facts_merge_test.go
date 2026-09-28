package cli

import (
	"testing"
	"time"
)

func factFor(t *testing.T, text string, paths []string, now time.Time) factRecord {
	t.Helper()
	p := normalizeFactPaths(paths)
	return factRecord{
		ID:         factRecordID(text, p),
		Paths:      p,
		Text:       text,
		Branch:     "main",
		Origin:     factOriginDistilled,
		Status:     factStatusActive,
		Provenance: []factAnchor{{SessionID: "s", Line: 1}},
		CreatedAt:  now,
		UpdatedAt:  now,
	}
}

func TestApplyFactActionsNew(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	cand := factFor(t, "The project uses Go.", []string{"project.tooling.stack"}, now)
	out, proposals := applyFactActions(nil, []factAction{{Kind: factActionNew, Candidate: cand}}, defaultFactConfidenceThreshold, now)
	if len(out) != 1 || out[0].Status != factStatusActive {
		t.Fatalf("expected one active fact, got %+v", out)
	}
	if len(proposals) != 0 {
		t.Fatalf("new should not produce proposals")
	}
}

func TestApplyFactActionsHighConfidenceMerge(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	later := now.Add(time.Hour)
	target := factFor(t, "Checkpoint analysis lives in a dedicated pipeline module.", []string{"architecture.boundaries.rationale"}, now)
	target.Provenance = []factAnchor{{SessionID: "s1", Line: 1}}

	cand := factFor(t, "Analysis generation belongs in the pipeline, not the queue.", []string{"architecture.boundaries.rationale"}, later)
	cand.Provenance = []factAnchor{{SessionID: "s2", Line: 9}}

	out, proposals := applyFactActions([]factRecord{target}, []factAction{
		{Kind: factActionMerge, TargetID: target.ID, Confidence: 0.9, Candidate: cand},
	}, defaultFactConfidenceThreshold, later)

	if len(out) != 1 {
		t.Fatalf("merge should consolidate into one fact, got %d", len(out))
	}
	if out[0].ID != target.ID {
		t.Fatalf("merge should keep the target, got %s", out[0].ID)
	}
	if len(out[0].Provenance) != 2 {
		t.Fatalf("merge should union provenance, got %d", len(out[0].Provenance))
	}
	if !out[0].UpdatedAt.Equal(later) {
		t.Fatalf("merge should bump UpdatedAt")
	}
	if len(proposals) != 0 {
		t.Fatalf("high-confidence merge should not propose")
	}
}

func TestApplyFactActionsHighConfidenceSupersede(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	later := now.Add(time.Hour)
	old := factFor(t, "Use Supabase for the database.", []string{"project.tooling.stack"}, now)
	newer := factFor(t, "Use MySQL for the database, not Supabase.", []string{"project.tooling.stack"}, later)

	out, proposals := applyFactActions([]factRecord{old}, []factAction{
		{Kind: factActionSupersede, TargetID: old.ID, Confidence: 0.95, Candidate: newer},
	}, defaultFactConfidenceThreshold, later)

	if len(out) != 2 {
		t.Fatalf("supersede should retain both facts, got %d", len(out))
	}
	var gotOld, gotNew *factRecord
	for i := range out {
		switch out[i].ID {
		case old.ID:
			gotOld = &out[i]
		case newer.ID:
			gotNew = &out[i]
		}
	}
	if gotOld == nil || gotNew == nil {
		t.Fatalf("both facts must be present")
	}
	if gotOld.Status != factStatusSuperseded || gotOld.SupersededBy != newer.ID {
		t.Fatalf("old fact not marked superseded: %+v", gotOld)
	}
	if gotNew.Status != factStatusActive {
		t.Fatalf("replacement should be active")
	}
	if gotNew.Confidence != "0.95" {
		t.Fatalf("action confidence should be stamped on the record, got %q", gotNew.Confidence)
	}
	if !contains(gotNew.RelatedIDs, old.ID) {
		t.Fatalf("replacement should relate to the superseded fact")
	}
	if len(proposals) != 0 {
		t.Fatalf("high-confidence supersede should not propose")
	}
}

func TestApplyFactActionsLowConfidenceQueuesProposal(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	target := factFor(t, "Tests run with go test.", []string{"workflow.testing.rules"}, now)
	cand := factFor(t, "Always run the full suite before pushing.", []string{"workflow.testing.rules"}, now)

	out, proposals := applyFactActions([]factRecord{target}, []factAction{
		{Kind: factActionSupersede, TargetID: target.ID, Confidence: 0.4, Candidate: cand},
	}, defaultFactConfidenceThreshold, now)

	if len(out) != 2 {
		t.Fatalf("low-confidence action must keep both facts active, got %d", len(out))
	}
	for _, r := range out {
		if r.Status != factStatusActive {
			t.Fatalf("nothing should be superseded below threshold: %+v", r)
		}
	}
	if len(proposals) != 1 {
		t.Fatalf("expected one queued proposal, got %d", len(proposals))
	}
	p := proposals[0]
	if p.Action != factActionSupersede || p.CandidateID != cand.ID || p.TargetID != target.ID {
		t.Fatalf("proposal fields wrong: %+v", p)
	}
	// Both facts must be cross-linked for review.
	if !contains(out[0].RelatedIDs, out[1].ID) || !contains(out[1].RelatedIDs, out[0].ID) {
		t.Fatalf("conflicting facts not cross-linked")
	}
}

func TestApplyFactActionsUnknownTargetDegradesToNew(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	cand := factFor(t, "A brand new fact.", []string{"project.tooling.stack"}, now)
	out, proposals := applyFactActions(nil, []factAction{
		{Kind: factActionMerge, TargetID: "fact:doesnotexist", Confidence: 0.99, Candidate: cand},
	}, defaultFactConfidenceThreshold, now)
	if len(out) != 1 || out[0].ID != cand.ID {
		t.Fatalf("unknown target should add candidate as new, got %+v", out)
	}
	if len(proposals) != 0 {
		t.Fatalf("unknown target should not propose")
	}
}

func TestApplyFactActionsChronologicalChain(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	// A supersedes nothing (new); B supersedes A; C supersedes B — a chain
	// applied in order leaves only C active.
	a := factFor(t, "v1 decision.", []string{"architecture.data.flow"}, now)
	b := factFor(t, "v2 decision.", []string{"architecture.data.flow"}, now.Add(time.Hour))
	c := factFor(t, "v3 decision.", []string{"architecture.data.flow"}, now.Add(2*time.Hour))

	out, _ := applyFactActions(nil, []factAction{
		{Kind: factActionNew, Candidate: a},
		{Kind: factActionSupersede, TargetID: a.ID, Confidence: 0.9, Candidate: b},
		{Kind: factActionSupersede, TargetID: b.ID, Confidence: 0.9, Candidate: c},
	}, defaultFactConfidenceThreshold, now)

	activeCount := 0
	for _, r := range out {
		if r.Status == factStatusActive {
			activeCount++
			if r.ID != c.ID {
				t.Fatalf("only the newest fact should be active, found %s", r.ID)
			}
		}
	}
	if activeCount != 1 {
		t.Fatalf("expected exactly one active fact at the end of the chain, got %d", activeCount)
	}
	if len(out) != 3 {
		t.Fatalf("superseded facts must be retained, got %d", len(out))
	}
}

func contains(values []string, target string) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}
