package factsync

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/entireio/entire-brain/factmerge"
)

// orphanServer models the state that made a proposal unremovable: an open proposal
// set whose fact head is gone entirely (both facts retracted and GC'd).
type orphanServer struct {
	set       []OpenProposal
	setRef    string
	resolved  []ResolveProposalRequest
	factsHead []byte // nil = no head at all
}

func (s *orphanServer) Current(context.Context, string, string) (string, []byte, bool, error) {
	if len(s.factsHead) == 0 {
		return "", nil, false, nil
	}
	return "facts-1", s.factsHead, true, nil
}

func (s *orphanServer) Advance(context.Context, string, string, string, []byte) (string, error) {
	return "", errors.New("orphanServer: Advance must not be called for a reject-prune")
}

func (s *orphanServer) ListProposals(context.Context, string, string) (ProposalSet, error) {
	return ProposalSet{Branch: "main", Ref: s.setRef, Found: true, Proposals: s.set}, nil
}

func (s *orphanServer) GetProposal(_ context.Context, _, _, id string) (OpenProposal, error) {
	for _, p := range s.set {
		if p.ID == id {
			return p, nil
		}
	}
	return OpenProposal{}, ErrProposalNotFound
}

func (s *orphanServer) PublishProposals(context.Context, string, string, string, []OpenProposal) (string, error) {
	return s.setRef, nil
}

func (s *orphanServer) ResolveProposal(_ context.Context, req ResolveProposalRequest) (ResolveProposalResponse, error) {
	s.resolved = append(s.resolved, req)
	// The prune writes the proposal set only; the fact head must stay where it was.
	return ResolveProposalResponse{FactsRef: req.FactsOldRef, ProposalsRef: "props-2", Changed: true}, nil
}

// TestResolveOpenPrunesOrphanOnReject is the regression for the unremovable-orphan
// defect. A proposal whose fact head has vanished cannot be accepted, and reject
// used to be blocked too — the empty-head guard returned before the reject path and
// pushing an empty fact set is refused on purpose — so the entry sat in every
// member's open list forever with no way out.
func TestResolveOpenPrunesOrphanOnReject(t *testing.T) {
	p := factmerge.Proposal{
		Action: factmerge.ActionSupersede, CandidateID: "fact:aaa", TargetID: "fact:bbb",
		Branch: "main", ProposedBy: "member-A",
	}
	open := OpenProposals([]factmerge.Proposal{p})
	srv := &orphanServer{set: open, setRef: "props-1"} // factsHead nil: head is gone

	res, err := ResolveOpen(context.Background(), srv, "repo-1", "main", open[0].ID, Reject, time.Now().UTC())
	if err != nil {
		t.Fatalf("reject on an absent fact head must prune, got: %v", err)
	}
	if res.Remaining != 0 {
		t.Fatalf("orphan was not removed from the set: remaining=%d", res.Remaining)
	}
	if len(srv.resolved) != 1 {
		t.Fatalf("expected exactly one resolve call, got %d", len(srv.resolved))
	}
	got := srv.resolved[0]
	if !got.FactsUnchanged {
		t.Fatal("prune must set FactsUnchanged so the server leaves the fact head alone")
	}
	if len(got.Facts) != 0 {
		t.Fatalf("prune must not push facts, got %d record(s)", len(got.Facts))
	}
	if len(got.Remaining) != 0 {
		t.Fatalf("prune must drop the entry from the remaining set, got %d", len(got.Remaining))
	}
}

// TestResolveOpenStillRefusesAcceptOnAbsentHead pins the other half: the prune path
// must not become a way to "settle" an accept with no facts, which would retire the
// proposal while silently dropping the merge result.
func TestResolveOpenStillRefusesAcceptOnAbsentHead(t *testing.T) {
	p := factmerge.Proposal{
		Action: factmerge.ActionMerge, CandidateID: "fact:aaa", TargetID: "fact:bbb",
		Branch: "main", ProposedBy: "member-A",
	}
	open := OpenProposals([]factmerge.Proposal{p})
	srv := &orphanServer{set: open, setRef: "props-1"}

	_, err := ResolveOpen(context.Background(), srv, "repo-1", "main", open[0].ID, Accept, time.Now().UTC())
	if !errors.Is(err, ErrProposalNotApplicable) {
		t.Fatalf("accept with no fact head must fail loudly, got: %v", err)
	}
	if len(srv.resolved) != 0 {
		t.Fatalf("accept must not reach the server with no facts, got %d call(s)", len(srv.resolved))
	}
}

// TestHTTPResolveRejectsFactsUnchangedOnAccept guards the wire: factsUnchanged is a
// reject-only prune signal, and must never let an accept skip its fact write.
func TestHTTPResolveRejectsFactsUnchangedOnAccept(t *testing.T) {
	h := &HTTPServer{}
	_, err := h.ResolveProposal(context.Background(), ResolveProposalRequest{
		RepoID: "repo-1", Branch: "main", ProposalID: "prop-x",
		Decision: Accept, FactsUnchanged: true,
	})
	if err == nil {
		t.Fatal("factsUnchanged with accept must be refused before any request is sent")
	}
}

// countingServer counts Current calls so a test can assert network round-trips.
type countingServer struct {
	orphanServer
	currentCalls int
}

func (s *countingServer) Current(ctx context.Context, repoID, branch string) (string, []byte, bool, error) {
	s.currentCalls++
	return s.orphanServer.Current(ctx, repoID, branch)
}

func (s *countingServer) Advance(context.Context, string, string, string, []byte) (string, error) {
	return "facts-2", nil
}

// TestSyncResultCarriesMergedFacts pins the round-trip saving: Sync already computes
// the post-sync fact set, so it returns it. Callers that need the head (to check
// which proposals are still live, or to mirror settlements into local facts) must be
// able to use it instead of issuing a second Current, which is a full network
// round-trip against the hosted backend on every single sync.
func TestSyncResultCarriesMergedFacts(t *testing.T) {
	now := time.Now().UTC()
	// Ids are content-derived, and Sync verifies the head's records against their
	// own content, so the fixture mints them the way the store does.
	existing := factmerge.Record{
		ID: factmerge.RecordID("blue-green", []string{"ops.deploy.strategy"}), Paths: []string{"ops.deploy.strategy"}, Text: "blue-green",
		Branch: "main", Status: factmerge.StatusActive, CreatedAt: now, UpdatedAt: now,
	}
	head := hostedNDJSON(t, []factmerge.Record{existing})
	srv := &countingServer{orphanServer: orphanServer{factsHead: head, setRef: "props-1"}}

	local := []factmerge.Record{{
		ID: factmerge.RecordID("nightly", []string{"ops.backup.cadence"}), Paths: []string{"ops.backup.cadence"}, Text: "nightly",
		Branch: "main", Status: factmerge.StatusActive, CreatedAt: now, UpdatedAt: now,
	}}

	res, err := Sync(context.Background(), srv, "repo-1", "main", "member-A", local, now)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(res.Facts) == 0 {
		t.Fatal("Result.Facts is empty; a caller would have to re-read the head over the network")
	}
	// The merged head must contain both sides (keep-both promote only grows).
	ids := map[string]bool{}
	for _, f := range res.Facts {
		ids[f.ID] = true
	}
	if !ids[local[0].ID] || !ids[existing.ID] {
		t.Fatalf("Result.Facts is not the merged head: %+v", res.Facts)
	}
	if srv.currentCalls != 1 {
		t.Fatalf("Sync should read the head exactly once, got %d", srv.currentCalls)
	}
}

// hostedNDJSON serializes records as a fact-set head blob.
func hostedNDJSON(t *testing.T, recs []factmerge.Record) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := factmerge.WriteNDJSON(&buf, recs); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
