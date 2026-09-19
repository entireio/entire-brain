package factsync

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/entireio/entire-brain/internal/factmerge"
)

func TestProposalRepairPreservesValidEntriesAndRequiresReviewedRef(t *testing.T) {
	t.Parallel()
	fake := newHostedFake()
	srv := hostedContractServer(t, fake)
	defer srv.Close()
	client := &HTTPServer{BaseURL: srv.URL}
	ctx := context.Background()
	good := OpenProposals([]factmerge.Proposal{{Action: factmerge.ActionMerge, CandidateID: "fact:a", TargetID: "fact:b", Branch: "main"}})[0]
	bad := good
	bad.ID = "prop-forged"
	fake.seed([]OpenProposal{good, bad})
	if _, err := client.ListProposals(ctx, "repo", "main"); !errors.Is(err, ErrProposalIDMismatch) {
		t.Fatalf("normal list accepted bad entry: %v", err)
	}
	preview, err := client.RepairProposalIDs(ctx, "repo", "main", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Invalid) != 1 || preview.Invalid[0].ID != bad.ID || preview.Applied {
		t.Fatalf("bad preview: %+v", preview)
	}
	_, current, _ := fake.snapshot()
	if len(current) != 2 {
		t.Fatal("preview changed the queue")
	}
	if _, err := client.RepairProposalIDs(ctx, "repo", "main", true, ""); err == nil {
		t.Fatal("apply without preview ref accepted")
	}
	// Concurrent additions invalidate the preview. They must not be removed by
	// an automatic retry of the operator's old authorization.
	next := good
	next.Proposal.TargetID = "fact:c"
	next.ID = ProposalID(next.Proposal)
	fake.seed([]OpenProposal{good, bad, next})
	if _, err := client.RepairProposalIDs(ctx, "repo", "main", true, preview.Ref); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale preview accepted: %v", err)
	}
	_, current, _ = fake.snapshot()
	if len(current) != 3 {
		t.Fatal("stale repair changed the queue")
	}
	preview, err = client.RepairProposalIDs(ctx, "repo", "main", false, "")
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.RepairProposalIDs(ctx, "repo", "main", true, preview.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Applied || result.Removed != 1 {
		t.Fatalf("repair: %+v", result)
	}
	set, err := client.ListProposals(ctx, "repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Proposals) != 2 || set.Proposals[0].ID != good.ID || set.Proposals[1].ID != next.ID {
		t.Fatalf("valid proposals changed: %+v", set)
	}
	if len(fake.resolvedFacts) != 0 {
		t.Fatal("repair wrote facts")
	}
}

type repairRoundTripper func(*http.Request) (*http.Response, error)

func (f repairRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestProposalRepairDoesNotRetryPastCASConflict(t *testing.T) {
	t.Parallel()
	fake := newHostedFake()
	srv := hostedContractServer(t, fake)
	defer srv.Close()
	good := OpenProposals([]factmerge.Proposal{{Action: factmerge.ActionMerge, CandidateID: "fact:a", TargetID: "fact:b", Branch: "main"}})[0]
	bad := good
	bad.ID = "forged"
	fake.seed([]OpenProposal{bad})
	client := &HTTPServer{BaseURL: srv.URL}
	preview, err := client.RepairProposalIDs(context.Background(), "repo", "main", false, "")
	if err != nil {
		t.Fatal(err)
	}
	posts := 0
	base := srv.Client().Transport
	client.Client = &http.Client{Transport: repairRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			posts++
			fake.seed([]OpenProposal{bad, good})
		}
		return base.RoundTrip(r)
	})}
	if _, err := client.RepairProposalIDs(context.Background(), "repo", "main", true, preview.Ref); !errors.Is(err, ErrConflict) {
		t.Fatalf("CAS race was accepted: %v", err)
	}
	if posts != 1 {
		t.Fatalf("repair retried %d writes against unreviewed state", posts)
	}
	_, set, _ := fake.snapshot()
	if len(set) != 2 || set[1].ID != good.ID {
		t.Fatalf("concurrent proposal lost: %+v", set)
	}
}
