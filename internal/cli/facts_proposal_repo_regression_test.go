package cli

import "testing"

func TestHostedOtherRepoDoesNotMutateLocalFacts(t *testing.T) {
	f, fake, proposal := newHostedProposalsFixture(t)
	f.writeFacts(t, "main", fake.headFacts(t))
	out, err := execute(t, NewRootCommand(f.opts), "facts", "proposals", "apply", proposal.ID, "--repo-id", "a-different-repository")
	if err != nil {
		t.Fatalf("resolve: %v %s", err, out)
	}
	facts, err := loadFacts(f.storage.BrainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, fact := range facts {
		if fact.Status != factStatusActive {
			t.Errorf("another repo changed local fact: %s status=%s", fact.ID, fact.Status)
		}
	}
}

func TestHostedSettlementRequiresMatchingLocalBinding(t *testing.T) {
	for _, tc := range []struct{ name, repo, host string }{
		{"missing", "", ""},
		{"different repo", "other-repo", "same"},
		{"different API", "repo-01HZZ", "https://other.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, fake, proposal := newHostedProposalsFixture(t)
			f.writeFacts(t, "main", fake.headFacts(t))
			if err := writeFactProposals(f.storage.BrainDir, "main", []factProposal{proposal.Proposal}); err != nil {
				t.Fatal(err)
			}
			if tc.repo != "" {
				host := tc.host
				if host == "same" {
					host = publishFlagOrEnv("", envAPIBaseURL)
				}
				if err := writeHostedFactsBinding(f.storage.BrainDir, "main", tc.repo, host); err != nil {
					t.Fatal(err)
				}
			}
			out, err := execute(t, NewRootCommand(f.opts), "facts", "proposals", "apply", proposal.ID)
			if err != nil {
				t.Fatalf("hosted settlement: %v %s", err, out)
			}
			facts, err := loadFacts(f.storage.BrainDir, "main")
			if err != nil {
				t.Fatal(err)
			}
			for _, fact := range facts {
				if fact.Status != factStatusActive {
					t.Errorf("unbound settlement changed local fact %s", fact.ID)
				}
			}
			queue, err := loadFactProposals(f.storage.BrainDir, "main")
			if err != nil || len(queue) != 1 {
				t.Fatalf("unbound settlement pruned local queue: %+v %v", queue, err)
			}
		})
	}
}
