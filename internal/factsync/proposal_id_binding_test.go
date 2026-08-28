package factsync

import (
	"context"
	"testing"

	"github.com/ashtom/entire-brain/internal/factmerge"
)

// A proposal id is content-derived: ProposalID hashes action, candidate id,
// target id, branch and proposer. That is what makes an id safe to show a human
// and safe to accept back from them — "apply prop-abc123" is supposed to name a
// specific (action, candidate, target) triple and nothing else.
//
// The open set, though, comes from the hosted server, and a server-supplied
// non-empty id was taken verbatim: only an EMPTY id was recomputed. So the id a
// member reads out of `facts proposals list`, and types into
// `facts proposals apply`, was not bound to the decision that id would execute.
// Settlement is not read-only — accept supersedes the target fact, and a merge
// removes the candidate outright — and the settled proposal is mirrored into
// LOCAL facts by applySettlementLocally. Confirming an id therefore has to mean
// confirming its content.

// TestListProposalsRejectsIDNotDerivedFromContent proves the binding on the list
// path, which is what populates the review surface.
func TestListProposalsRejectsIDNotDerivedFromContent(t *testing.T) {
	t.Parallel()
	fake := newHostedFake()
	srv := hostedContractServer(t, fake)
	defer srv.Close()

	// What the server displays under a benign-looking id...
	shown := factmerge.Proposal{Action: factmerge.ActionMerge, CandidateID: "fact:aaa", TargetID: "fact:bbb", Branch: "main"}
	// ...is not what the id commits to: the server keeps the id of the shown
	// proposal but swaps the decision underneath it for a supersede against a
	// different target.
	swapped := factmerge.Proposal{Action: factmerge.ActionSupersede, CandidateID: "fact:attacker", TargetID: "fact:victim", Branch: "main"}
	fake.seed([]OpenProposal{{ID: ProposalID(shown), Proposal: swapped}})

	client := &HTTPServer{BaseURL: srv.URL}
	set, err := client.ListProposals(context.Background(), "repo-1", "main")
	if err == nil {
		for _, p := range set.Proposals {
			if p.ID != ProposalID(p.Proposal) {
				t.Fatalf("proposal id %s does not derive from the decision it names (%+v)", p.ID, p.Proposal)
			}
		}
		t.Fatalf("ListProposals accepted a proposal whose id is not derived from its content")
	}
}

// TestGetProposalRejectsIDNotDerivedFromContent covers the single-fetch path,
// which is what `facts proposals show <id>` and the resolve loop use.
func TestGetProposalRejectsIDNotDerivedFromContent(t *testing.T) {
	t.Parallel()
	fake := newHostedFake()
	srv := hostedContractServer(t, fake)
	defer srv.Close()

	shown := factmerge.Proposal{Action: factmerge.ActionMerge, CandidateID: "fact:aaa", TargetID: "fact:bbb", Branch: "main"}
	swapped := factmerge.Proposal{Action: factmerge.ActionSupersede, CandidateID: "fact:attacker", TargetID: "fact:victim", Branch: "main"}
	id := ProposalID(shown)
	fake.seed([]OpenProposal{{ID: id, Proposal: swapped}})

	client := &HTTPServer{BaseURL: srv.URL}
	got, err := client.GetProposal(context.Background(), "repo-1", "main", id)
	if err == nil {
		t.Fatalf("GetProposal returned %+v under an id that does not derive from it", got)
	}
}

// TestProposalsStillAcceptDerivedAndOmittedIDs pins both honest shapes: a server
// that stamps the derived id, and an older/looser one that omits it entirely.
func TestProposalsStillAcceptDerivedAndOmittedIDs(t *testing.T) {
	t.Parallel()
	honest := factmerge.Proposal{Action: factmerge.ActionSupersede, CandidateID: "fact:aaa", TargetID: "fact:bbb", Branch: "main"}

	for name, seeded := range map[string]OpenProposal{
		"derived": {ID: ProposalID(honest), Proposal: honest},
		"omitted": {Proposal: honest},
	} {
		t.Run(name, func(t *testing.T) {
			fake := newHostedFake()
			srv := hostedContractServer(t, fake)
			defer srv.Close()
			fake.seed([]OpenProposal{seeded})

			client := &HTTPServer{BaseURL: srv.URL}
			set, err := client.ListProposals(context.Background(), "repo-1", "main")
			if err != nil {
				t.Fatalf("honest proposal set rejected: %v", err)
			}
			if len(set.Proposals) != 1 {
				t.Fatalf("got %d proposals, want 1", len(set.Proposals))
			}
			if set.Proposals[0].ID != ProposalID(honest) {
				t.Fatalf("id = %s, want the derived %s", set.Proposals[0].ID, ProposalID(honest))
			}
		})
	}
}
