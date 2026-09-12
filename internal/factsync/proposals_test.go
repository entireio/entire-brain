package factsync

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ashtom/entire-brain/internal/factmerge"
)

// TestProposalIDIsStableAndDiscriminating pins the derived id: the same conflict
// always yields the same id (so two members address it identically and re-publishing
// is idempotent), and every identifying field changes it (so two conflicts can never
// collide into one reviewable entry).
func TestProposalIDIsStableAndDiscriminating(t *testing.T) {
	base := factmerge.Proposal{Action: factmerge.ActionSupersede, CandidateID: "fact:c", TargetID: "fact:t", Branch: "main", ProposedBy: "member-B"}
	id := ProposalID(base)
	// A literal, not a second call. The per-field table below proves every field
	// participates, but it cannot see a change to how the fields are COMBINED: a
	// swap of CandidateID and TargetID in the hash order leaves every subtest
	// passing while giving the same conflict a different id at a member running
	// the other version — exactly the cross-member disagreement this id exists to
	// prevent. Verified: that swap left all 2,450 tests in factsync and cli green.
	if id != "prop-08bee2872733f11a" {
		t.Fatalf("ProposalID = %q, want prop-08bee2872733f11a", id)
	}
	if !strings.HasPrefix(id, "prop-") || len(id) != len("prop-")+16 {
		t.Fatalf("ProposalID = %q; want prop-<16 hex>", id)
	}

	for name, mutate := range map[string]func(p *factmerge.Proposal){
		"action":      func(p *factmerge.Proposal) { p.Action = factmerge.ActionMerge },
		"candidate":   func(p *factmerge.Proposal) { p.CandidateID = "fact:other" },
		"target":      func(p *factmerge.Proposal) { p.TargetID = "fact:other" },
		"branch":      func(p *factmerge.Proposal) { p.Branch = "release" },
		"proposed_by": func(p *factmerge.Proposal) { p.ProposedBy = "member-C" },
	} {
		t.Run(name, func(t *testing.T) {
			other := base
			mutate(&other)
			if ProposalID(other) == id {
				t.Fatalf("%s change did not change the proposal id", name)
			}
		})
	}

	// Field boundaries are separated, so no two field splits can alias.
	a := factmerge.Proposal{Action: "merge", CandidateID: "ab", TargetID: "c"}
	b := factmerge.Proposal{Action: "merge", CandidateID: "a", TargetID: "bc"}
	if ProposalID(a) == ProposalID(b) {
		t.Fatal("adjacent-field concatenation collides")
	}
}

func TestOpenProposalsStampsIDs(t *testing.T) {
	if got := OpenProposals(nil); got != nil {
		t.Fatalf("OpenProposals(nil) = %+v; want nil", got)
	}
	raised := []factmerge.Proposal{
		{Action: factmerge.ActionSupersede, CandidateID: "fact:c1", TargetID: "fact:t1", Branch: "main", ProposedBy: "member-B"},
		{Action: factmerge.ActionMerge, CandidateID: "fact:c2", TargetID: "fact:t2", Branch: "main", ProposedBy: "member-B"},
	}
	open := OpenProposals(raised)
	if len(open) != 2 || open[0].ID == open[1].ID {
		t.Fatalf("OpenProposals = %+v; want 2 distinctly-identified entries", open)
	}
	for i, p := range open {
		if p.ID != ProposalID(raised[i]) || p.Proposal != raised[i] {
			t.Fatalf("entry %d = %+v; want id+proposal of %+v", i, p, raised[i])
		}
	}
}

// TestFindProposal is the reference-resolution table: exact id, candidate fact id,
// unambiguous prefix, too-short prefix, ambiguous prefix, and misses.
func TestFindProposal(t *testing.T) {
	first := OpenProposal{ID: "prop-aaaa111122223333", Proposal: factmerge.Proposal{CandidateID: "fact:c1", TargetID: "fact:t1"}}
	second := OpenProposal{ID: "prop-aaaa444455556666", Proposal: factmerge.Proposal{CandidateID: "fact:c2", TargetID: "fact:t2"}}
	third := OpenProposal{ID: "prop-bbbb777788889999", Proposal: factmerge.Proposal{CandidateID: "fact:c3", TargetID: "fact:t3"}}
	set := []OpenProposal{first, second, third}

	for _, tc := range []struct {
		name    string
		ref     string
		want    string
		wantErr error
	}{
		{name: "exact id", ref: first.ID, want: first.ID},
		{name: "candidate fact id", ref: "fact:c2", want: second.ID},
		{name: "unambiguous prefix", ref: "prop-bbbb", want: third.ID},
		{name: "ambiguous prefix", ref: "prop-aaaa", wantErr: ErrAmbiguousProposal},
		{name: "too-short prefix", ref: "pro", wantErr: ErrProposalNotFound},
		// The fixed "prop-" lead-in carries zero discriminating characters: a
		// ref that is only the shared prefix (or barely more) must never
		// prefix-match, or a truncated paste would settle whichever single
		// conflict happens to be open.
		{name: "bare shared prefix", ref: "prop", wantErr: ErrProposalNotFound},
		{name: "shared prefix with dash", ref: "prop-", wantErr: ErrProposalNotFound},
		{name: "under-discriminating prefix", ref: "prop-bb", wantErr: ErrProposalNotFound},
		{name: "unknown", ref: "prop-zzzz", wantErr: ErrProposalNotFound},
		{name: "empty", ref: "  ", wantErr: ErrProposalNotFound},
		{name: "whitespace trimmed", ref: "  fact:c3  ", want: third.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FindProposal(set, tc.ref)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("FindProposal(%q) err = %v; want %v", tc.ref, err, tc.wantErr)
				}
				return
			}
			if err != nil || got.ID != tc.want {
				t.Fatalf("FindProposal(%q) = %+v, %v; want %s", tc.ref, got, err, tc.want)
			}
		})
	}

	if _, err := FindProposal(nil, "prop-aaaa111122223333"); !errors.Is(err, ErrProposalNotFound) {
		t.Fatalf("FindProposal(empty set) = %v; want ErrProposalNotFound", err)
	}
}

func TestRemoveProposal(t *testing.T) {
	set := OpenProposals([]factmerge.Proposal{
		{Action: factmerge.ActionSupersede, CandidateID: "fact:c1", TargetID: "fact:t1"},
		{Action: factmerge.ActionSupersede, CandidateID: "fact:c2", TargetID: "fact:t2"},
	})
	out, found := removeProposal(set, set[0].ID)
	if !found || len(out) != 1 || out[0].ID != set[1].ID {
		t.Fatalf("removeProposal = %+v, %v", out, found)
	}
	if len(set) != 2 {
		t.Fatal("removeProposal mutated its input")
	}
	if _, found := removeProposal(set, "prop-missing"); found {
		t.Fatal("removeProposal reported a missing id as found")
	}
}

func TestDecisionStringAndParse(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    Decision
		wantErr bool
	}{
		{in: "accept", want: Accept},
		{in: "APPLY", want: Accept},
		{in: " reject ", want: Reject},
		{in: "maybe", wantErr: true},
		{in: "", wantErr: true},
	} {
		got, err := ParseDecision(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("ParseDecision(%q) = %v; want an error", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Fatalf("ParseDecision(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
	if Accept.String() != "accept" || Reject.String() != "reject" || Decision(42).String() != "unknown" {
		t.Fatal("Decision.String mapping is wrong")
	}
}

// TestResolveOpenRejectsUnknownDecision guards the driver's entry condition: an
// out-of-range Decision must fail before any transport call.
func TestResolveOpenRejectsUnknownDecision(t *testing.T) {
	if _, err := ResolveOpen(context.Background(), nil, "repo", "main", "prop-x", Decision(42), time.Time{}); err == nil {
		t.Fatal("ResolveOpen with an unknown decision must error before dialing")
	}
}
