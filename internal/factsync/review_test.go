package factsync

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/factmerge"
)

// advanceResolved pushes a resolved fact set to the head under CAS (what a reviewer's
// client does after Resolve): serialize, then Advance from the ref it read.
func advanceResolved(ctx context.Context, srv Server, oldRef string, facts []factmerge.Record) (string, error) {
	var buf bytes.Buffer
	if err := factmerge.WriteNDJSON(&buf, facts); err != nil {
		return "", err
	}
	return srv.Advance(ctx, "repo", "main", oldRef, buf.Bytes())
}

func headRecords(ctx context.Context, t *testing.T, srv Server) (string, []factmerge.Record) {
	t.Helper()
	ref, blob, found, err := srv.Current(ctx, "repo", "main")
	if err != nil || !found {
		t.Fatalf("Current: found=%v err=%v", found, err)
	}
	recs, err := factmerge.ParseNDJSON(bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("parse head: %v", err)
	}
	return ref, recs
}

// TestCrossMemberConflictRoutedThenResolvedConverges is the M2.4 proof. Two members
// hold CONTRADICTORY facts on the same topic (same path, different text → different id).
// The second member's sync must NOT silently overwrite the first: it keeps both facts
// active and raises a review Proposal ROUTED to the member who raised it (ProposedBy).
// A reviewer then Accepts the supersede; advancing the resolved set converges every
// member's view (the losing fact retained as superseded, never deleted).
func TestCrossMemberConflictRoutedThenResolvedConverges(t *testing.T) {
	ctx := context.Background()
	srv := &fakeServer{}
	now := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)

	factX := fact("deploys use blue-green cutover", []string{"ops.deploy.strategy"}, "session-A", now)
	factY := fact("deploys use in-place rolling restart", []string{"ops.deploy.strategy"}, "session-B", now)
	if factX.ID == factY.ID {
		t.Fatal("test setup: contradictory facts must have different ids")
	}

	// Member A establishes the head — no conflict yet.
	resA, err := Sync(ctx, srv, "repo", "main", "member-A", []factmerge.Record{factX}, now)
	if err != nil {
		t.Fatalf("member-A sync: %v", err)
	}
	if len(resA.Proposals) != 0 {
		t.Fatalf("member-A raised %d proposals; want 0 (empty head, no conflict)", len(resA.Proposals))
	}

	// Member B syncs a CONTRADICTORY fact → keep-both + a routed proposal, no overwrite.
	resB, err := Sync(ctx, srv, "repo", "main", "member-B", []factmerge.Record{factY}, now)
	if err != nil {
		t.Fatalf("member-B sync: %v", err)
	}
	if len(resB.Proposals) != 1 {
		t.Fatalf("member-B raised %d proposals; want 1 (the contradiction)", len(resB.Proposals))
	}
	p := resB.Proposals[0]
	if p.ProposedBy != "member-B" {
		t.Fatalf("proposal ProposedBy = %q; want member-B (routing)", p.ProposedBy)
	}
	if p.Action != factmerge.ActionSupersede || p.CandidateID != factY.ID || p.TargetID != factX.ID {
		t.Fatalf("proposal = %+v; want supersede cand=%s target=%s", p, factY.ID, factX.ID)
	}

	// No silent overwrite: both facts are present and active in the head.
	_, recs := headRecords(ctx, t, srv)
	byID := map[string]factmerge.Record{}
	for _, r := range recs {
		byID[r.ID] = r
	}
	if byID[factX.ID].Status != factmerge.StatusActive || byID[factY.ID].Status != factmerge.StatusActive {
		t.Fatalf("post-conflict head must keep BOTH active; got X=%q Y=%q", byID[factX.ID].Status, byID[factY.ID].Status)
	}

	// A reviewer Accepts the supersede; advance the resolved set to converge everyone.
	ref, head := headRecords(ctx, t, srv)
	resolved, err := Resolve(head, p, Accept, now)
	if err != nil {
		t.Fatalf("Resolve(Accept): %v", err)
	}
	if _, err := advanceResolved(ctx, srv, ref, resolved); err != nil {
		t.Fatalf("advance resolved: %v", err)
	}

	// Converged: X retired (superseded by Y), Y active. Nothing deleted.
	_, final := headRecords(ctx, t, srv)
	fByID := map[string]factmerge.Record{}
	for _, r := range final {
		fByID[r.ID] = r
	}
	x, y := fByID[factX.ID], fByID[factY.ID]
	if x.Status != factmerge.StatusSuperseded || x.SupersededBy != factY.ID {
		t.Fatalf("after resolve, X = status %q supersededBy %q; want superseded by %s", x.Status, x.SupersededBy, factY.ID)
	}
	if y.Status != factmerge.StatusActive {
		t.Fatalf("after resolve, Y = status %q; want active", y.Status)
	}
}

// TestResolveRejectKeepsBothActive proves the Reject decision: a reviewer who judges the
// two contradictory facts as genuinely coexisting keeps both active and clears the
// cross-link — nothing dropped or superseded.
func TestResolveRejectKeepsBothActive(t *testing.T) {
	ctx := context.Background()
	srv := &fakeServer{}
	now := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)

	factX := fact("staging mirrors prod region", []string{"infra.staging.topology"}, "session-A", now)
	factY := fact("staging runs a single-node cluster", []string{"infra.staging.topology"}, "session-B", now)

	if _, err := Sync(ctx, srv, "repo", "main", "member-A", []factmerge.Record{factX}, now); err != nil {
		t.Fatal(err)
	}
	resB, err := Sync(ctx, srv, "repo", "main", "member-B", []factmerge.Record{factY}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(resB.Proposals) != 1 {
		t.Fatalf("want 1 proposal, got %d", len(resB.Proposals))
	}

	ref, head := headRecords(ctx, t, srv)
	resolved, err := Resolve(head, resB.Proposals[0], Reject, now)
	if err != nil {
		t.Fatalf("Resolve(Reject): %v", err)
	}
	if _, err := advanceResolved(ctx, srv, ref, resolved); err != nil {
		t.Fatalf("advance resolved: %v", err)
	}

	_, final := headRecords(ctx, t, srv)
	for _, r := range final {
		if r.Status != factmerge.StatusActive {
			t.Fatalf("reject must keep all active; %s = %q", r.ID, r.Status)
		}
	}
	if len(final) != 2 {
		t.Fatalf("reject head has %d facts; want 2 (both kept)", len(final))
	}
}

// TestSyncRequiresMemberID proves a blank memberID is rejected — otherwise a raised
// proposal would be stamped ProposedBy="" (dropped by omitempty) and be
// indistinguishable from a local one, losing cross-member routing.
func TestSyncRequiresMemberID(t *testing.T) {
	ctx := context.Background()
	srv := &fakeServer{}
	now := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)
	local := []factmerge.Record{fact("some fact", []string{"a.b.c"}, "s", now)}
	if _, err := Sync(ctx, srv, "repo", "main", "", local, now); err == nil {
		t.Fatal("Sync with empty memberID must error (routing requires it)")
	}
}

// TestResolveStaleProposalSurfaced proves Resolve does not silently no-op a proposal
// whose target/candidate has vanished (a concurrent resolution) — it returns
// ErrProposalNotApplicable so the caller re-reads and reconciles.
func TestResolveStaleProposalSurfaced(t *testing.T) {
	now := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)
	facts := []factmerge.Record{fact("only remaining fact", []string{"misc.scratch.note"}, "s", now)}
	stale := factmerge.Proposal{Action: factmerge.ActionSupersede, CandidateID: "fact:gone", TargetID: "fact:also-gone"}
	if _, err := Resolve(facts, stale, Accept, now); err != ErrProposalNotApplicable {
		t.Fatalf("Resolve(stale, Accept) = %v; want ErrProposalNotApplicable", err)
	}
	if _, err := Resolve(facts, stale, Reject, now); err != ErrProposalNotApplicable {
		t.Fatalf("Resolve(stale, Reject) = %v; want ErrProposalNotApplicable", err)
	}
}
