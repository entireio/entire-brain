package cli

import "testing"

func TestPersistFactsSyncProposalsPreservesAndDeduplicatesQueue(t *testing.T) {
	brainDir := t.TempDir()
	branch := "main"
	existing := factProposal{
		Action:      factActionMerge,
		CandidateID: "fact:existing-candidate",
		TargetID:    "fact:existing-target",
		Branch:      branch,
		ProposedBy:  "member-a",
	}
	incoming := factProposal{
		Action:      factActionSupersede,
		CandidateID: "fact:incoming-candidate",
		TargetID:    "fact:incoming-target",
		Branch:      branch,
		ProposedBy:  "member-b",
	}
	if err := writeFactProposals(brainDir, branch, []factProposal{existing}); err != nil {
		t.Fatalf("write existing proposals: %v", err)
	}

	if err := persistFactsSyncProposals(brainDir, branch, []factProposal{existing, incoming}); err != nil {
		t.Fatalf("persist sync proposals: %v", err)
	}

	got, err := loadFactProposals(brainDir, branch)
	if err != nil {
		t.Fatalf("load proposals: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("proposal count = %d, want 2: %+v", len(got), got)
	}
	byCandidate := make(map[string]factProposal, len(got))
	for _, proposal := range got {
		byCandidate[proposal.CandidateID] = proposal
	}
	if gotExisting, ok := byCandidate[existing.CandidateID]; !ok || gotExisting.ProposedBy != existing.ProposedBy {
		t.Fatalf("existing proposal was not preserved: %+v", got)
	}
	if gotIncoming, ok := byCandidate[incoming.CandidateID]; !ok || gotIncoming.ProposedBy != incoming.ProposedBy {
		t.Fatalf("incoming proposal was not persisted: %+v", got)
	}
}
