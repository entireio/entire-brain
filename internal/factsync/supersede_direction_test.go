package factsync

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/factmerge"
)

// An earlier note on this project flagged "promote supersede direction inverted
// (corrections lose to what they correct)". On this tree it does not reproduce, and
// this test is what keeps it that way — the direction is a one-word difference in
// factmerge.Promote and factmerge.ApplyProposal that nothing else would catch.
//
// The chain under test, end to end:
//
//	factsync.Sync         Promote(local, head, "keep-both") — source = THIS member's
//	                      facts, target = the shared head, so a conflict yields
//	                      Proposal{Candidate: the member's fact, Target: the head's}
//	factsync.Resolve      Accept routes to factmerge.ApplyProposal
//	factmerge.ApplyProposal
//	                      supersede retires the TARGET and points it at the CANDIDATE
//
// Net effect: accepting retires the statement that was already in the shared head
// and keeps the member's newer one. Inverting either half would make the correction
// the superseded record and leave the stale statement active — the exact failure the
// note described, and one that reads as a successful sync.

func mustRecord(t *testing.T, text, path, branch string, anchor factmerge.Anchor, now time.Time) factmerge.Record {
	t.Helper()
	r := factmerge.Record{
		Paths:      []string{path},
		Text:       text,
		Branch:     branch,
		Origin:     "distilled",
		Status:     factmerge.StatusActive,
		Provenance: []factmerge.Anchor{anchor},
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	r.Paths = factmerge.NormalizePaths(r.Paths)
	r.ID = factmerge.RecordID(r.Text, r.Paths)
	return r
}

func TestAcceptingASyncRaisedProposalRetiresTheHeadFactNotTheCorrection(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	stale := mustRecord(t, "The service listens on port 8080.", "architecture.api.ports", "main",
		factmerge.Anchor{SessionID: "old-session"}, now.Add(-72*time.Hour))
	correction := mustRecord(t, "The service listens on port 9090 since the ingress move.", "architecture.api.ports", "main",
		factmerge.Anchor{SessionID: "new-session"}, now)

	srv := newDirectionFake(t, []factmerge.Record{stale})
	res, err := Sync(context.Background(), srv, "01HZZPROBE0000000000000000", "main", "member-a",
		[]factmerge.Record{correction}, now)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(res.Proposals) != 1 {
		t.Fatalf("keep-both raised %d proposals, want 1 (the two statements conflict)", len(res.Proposals))
	}
	p := res.Proposals[0]
	if p.Action != factmerge.ActionSupersede {
		t.Fatalf("proposal action = %q, want %q", p.Action, factmerge.ActionSupersede)
	}
	if p.CandidateID != correction.ID {
		t.Errorf("candidate is %s; the member's own (correcting) fact %s must be the candidate", p.CandidateID, correction.ID)
	}
	if p.TargetID != stale.ID {
		t.Errorf("target is %s; the fact already in the shared head %s must be the target", p.TargetID, stale.ID)
	}

	// Accept it, and check which side actually lost.
	settled, err := Resolve(res.Facts, p, Accept, now)
	if err != nil {
		t.Fatalf("Resolve(Accept): %v", err)
	}
	byID := map[string]factmerge.Record{}
	for _, r := range settled {
		byID[r.ID] = r
	}
	got, ok := byID[stale.ID]
	if !ok {
		t.Fatal("the superseded fact was DELETED; keep-both retains it with Status=superseded (ADR-P1-G)")
	}
	if got.Status != factmerge.StatusSuperseded {
		t.Errorf("the stale head fact has status %q, want %q — the correction lost to what it corrects", got.Status, factmerge.StatusSuperseded)
	}
	if got.SupersededBy != correction.ID {
		t.Errorf("superseded_by = %q, want the correction %q", got.SupersededBy, correction.ID)
	}
	if kept, ok := byID[correction.ID]; !ok {
		t.Error("the correction is gone from the settled set")
	} else if kept.Status != factmerge.StatusActive {
		t.Errorf("the correction has status %q, want it to stay %q", kept.Status, factmerge.StatusActive)
	}
}

// directionFake is a minimal in-memory Server: the head starts at the given records
// and the first Advance wins.
type directionFake struct {
	t    *testing.T
	head []byte
	ref  string
}

func newDirectionFake(t *testing.T, head []factmerge.Record) *directionFake {
	t.Helper()
	f := &directionFake{t: t, ref: "ref-0"}
	if len(head) > 0 {
		var buf bytes.Buffer
		if err := factmerge.WriteNDJSON(&buf, head); err != nil {
			t.Fatal(err)
		}
		f.head = buf.Bytes()
	}
	return f
}

func (f *directionFake) Current(context.Context, string, string) (string, []byte, bool, error) {
	if len(f.head) == 0 {
		return "", nil, false, nil
	}
	return f.ref, f.head, true, nil
}

func (f *directionFake) Advance(_ context.Context, _, _, oldRef string, plaintext []byte) (string, error) {
	if oldRef != f.ref {
		return "", ErrConflict
	}
	f.head = plaintext
	f.ref = "ref-1"
	return f.ref, nil
}
