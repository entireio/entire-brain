package cli

import (
	"strings"
	"testing"

	"github.com/entireio/entire-brain/internal/factmerge"
	"github.com/entireio/entire-brain/internal/factsync"
)

// hostedHead re-serializes a fact set as the hosted head blob, so a test can
// stage exactly the state a teammate's settlement would have left behind.
func hostedHead(t *testing.T, facts []factRecord) []byte {
	t.Helper()
	var blob strings.Builder
	if err := factmerge.WriteNDJSON(&blob, facts); err != nil {
		t.Fatal(err)
	}
	return []byte(blob.String())
}

// TestReconcileLocalQueuePullsSettlementIntoLocalFacts is the regression for the
// "accepted settlements don't stick" defect. A teammate settling a conflict on the
// hosted queue used to leave the LOSING member's local facts untouched, so the
// next keep-both Promote re-added the retired fact and re-raised the same conflict
// under a fresh id — a loop that member could never clear.
//
// Driven through reconcileLocalProposalQueue directly: the natural sync fixture
// makes the local member WIN its own proposal, so it cannot exercise the losing
// side at all (a test asserting on it passes with the fix reverted).
func TestReconcileLocalQueuePullsSettlementIntoLocalFacts(t *testing.T) {
	paths := normalizeFactPaths([]string{"ops.deploy.strategy"})
	mk := func(text string) factRecord {
		return factRecord{
			ID: factRecordID(text, paths), Paths: paths, Text: text, Branch: "main",
			Origin: factOriginDistilled, Status: factStatusActive,
		}
	}
	mine, theirs := mk("deploys use a canary rollout"), mk("deploys use blue-green cutover")

	for _, tc := range []struct {
		name       string
		action     string
		hostedHead []factRecord // head AFTER the teammate settled
		wantGone   bool         // my fact removed locally
		wantSuper  bool         // my fact superseded locally
	}{
		{
			// merge accept removes the candidate — my fact — from the head.
			name:   "merge accept retires my fact",
			action: factActionMerge,
			hostedHead: []factRecord{
				{ID: theirs.ID, Paths: paths, Text: theirs.Text, Branch: "main", Origin: factOriginDistilled, Status: factStatusActive},
			},
			wantGone: true,
		},
		{
			// supersede accept flips MY fact (the target here) to superseded.
			name:   "supersede accept supersedes my fact",
			action: factActionSupersede,
			hostedHead: []factRecord{
				{ID: theirs.ID, Paths: paths, Text: theirs.Text, Branch: "main", Origin: factOriginDistilled, Status: factStatusActive},
				{ID: mine.ID, Paths: paths, Text: mine.Text, Branch: "main", Origin: factOriginDistilled,
					Status: factStatusSuperseded, SupersededBy: theirs.ID},
			},
			wantSuper: true,
		},
		{
			// reject changes no status; both facts stay as they are.
			name:   "reject leaves my fact active",
			action: factActionSupersede,
			hostedHead: []factRecord{
				{ID: theirs.ID, Paths: paths, Text: theirs.Text, Branch: "main", Origin: factOriginDistilled, Status: factStatusActive},
				{ID: mine.ID, Paths: paths, Text: mine.Text, Branch: "main", Origin: factOriginDistilled, Status: factStatusActive},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newVerifyFixture(t)
			f.writeFacts(t, "main", []factRecord{theirs, mine})

			// For merge the candidate is retired; for supersede the target is.
			p := factProposal{Action: tc.action, Branch: "main", ProposedBy: "member-C"}
			if tc.action == factActionMerge {
				p.CandidateID, p.TargetID = mine.ID, theirs.ID
			} else {
				p.CandidateID, p.TargetID = theirs.ID, mine.ID
			}
			if err := writeFactProposals(f.storage.BrainDir, "main", []factProposal{p}); err != nil {
				t.Fatal(err)
			}
			// It reached the hosted set once, and is now absent from it: settled.
			if err := writeSharedProposalLedger(f.storage.BrainDir, "main", map[string]struct{}{
				factsync.ProposalID(p): {},
			}); err != nil {
				t.Fatal(err)
			}

			if err := reconcileLocalProposalQueue(f.storage.BrainDir, "main", []factsync.OpenProposal{}, tc.hostedHead, f.now); err != nil {
				t.Fatalf("reconcile: %v", err)
			}

			facts, err := loadFacts(f.storage.BrainDir, "main")
			if err != nil {
				t.Fatal(err)
			}
			idx := indexOfFact(facts, mine.ID)
			switch {
			case tc.wantGone:
				if idx >= 0 {
					t.Fatalf("merge settlement did not remove my retired fact locally: %+v", facts)
				}
			case tc.wantSuper:
				if idx < 0 {
					t.Fatalf("my fact vanished instead of being superseded: %+v", facts)
				}
				if facts[idx].Status != factStatusSuperseded {
					t.Fatalf("supersede settlement did not stick locally: status=%s", facts[idx].Status)
				}
			default:
				if idx < 0 || facts[idx].Status != factStatusActive {
					t.Fatalf("reject should not have changed my fact: %+v", facts)
				}
			}

			// Either way the settled entry must leave the local queue.
			queued, qErr := loadFactProposals(f.storage.BrainDir, "main")
			if qErr != nil {
				t.Fatal(qErr)
			}
			if len(queued) != 0 {
				t.Fatalf("settled proposal still queued locally: %+v", queued)
			}
		})
	}
}

// TestFactsReviewRefusesToSettleSharedProposalLocally is the regression for the
// missing hosted interlock: settling a published proposal with the local `facts
// review` used to leave the hosted copy open, so a teammate's later apply
// silently overrode this member's decision.
func TestFactsReviewRefusesToSettleSharedProposalLocally(t *testing.T) {
	f := newVerifyFixture(t)
	paths := normalizeFactPaths([]string{"ops.deploy.strategy"})
	target := factRecord{
		ID: factRecordID("deploys use blue-green cutover", paths), Paths: paths,
		Text: "deploys use blue-green cutover", Branch: "main", Origin: factOriginDistilled, Status: factStatusActive,
		CreatedAt: f.now, UpdatedAt: f.now,
	}
	candidate := factRecord{
		ID: factRecordID("deploys use in-place rolling restart", paths), Paths: paths,
		Text: "deploys use in-place rolling restart", Branch: "main", Origin: factOriginDistilled, Status: factStatusActive,
		CreatedAt: f.now, UpdatedAt: f.now,
	}
	f.writeFacts(t, "main", []factRecord{target, candidate})

	sharedProposal := factProposal{
		Action: factActionSupersede, CandidateID: candidate.ID, TargetID: target.ID,
		Branch: "main", ProposedBy: "member-C",
	}
	if err := writeFactProposals(f.storage.BrainDir, "main", []factProposal{sharedProposal}); err != nil {
		t.Fatal(err)
	}
	if err := writeSharedProposalLedger(f.storage.BrainDir, "main", map[string]struct{}{
		factsync.ProposalID(sharedProposal): {},
	}); err != nil {
		t.Fatal(err)
	}

	out, err := execute(t, NewRootCommand(f.opts), "facts", "review", "--apply", candidate.ID)
	if err == nil {
		t.Fatalf("expected a refusal for a shared proposal, got success:\n%s", out)
	}
	if !strings.Contains(err.Error(), "facts proposals") {
		t.Fatalf("refusal should point at 'facts proposals', got: %v", err)
	}

	// The local facts must be untouched — no half-applied settlement.
	facts, loadErr := loadFacts(f.storage.BrainDir, "main")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if ti := indexOfFact(facts, target.ID); ti < 0 || facts[ti].Status != factStatusActive {
		t.Fatalf("refused review still mutated local facts: %+v", facts)
	}
	queued, qErr := loadFactProposals(f.storage.BrainDir, "main")
	if qErr != nil || len(queued) != 1 {
		t.Fatalf("refused review dropped the queue entry: %v, %d", qErr, len(queued))
	}
}

// TestFactsReviewBulkSkipsSharedButSettlesPrivate proves the interlock does not
// break the single-user flow: distill's private backlog carries no ProposedBy,
// never reaches the hosted set, and must stay settleable in bulk.
func TestFactsReviewBulkSkipsSharedButSettlesPrivate(t *testing.T) {
	f := newVerifyFixture(t)
	paths := normalizeFactPaths([]string{"ops.deploy.strategy"})
	privPaths := normalizeFactPaths([]string{"ops.backup.cadence"})

	target := factRecord{
		ID: factRecordID("deploys use blue-green cutover", paths), Paths: paths,
		Text: "deploys use blue-green cutover", Branch: "main", Origin: factOriginDistilled, Status: factStatusActive,
		CreatedAt: f.now, UpdatedAt: f.now,
	}
	candidate := factRecord{
		ID: factRecordID("deploys use in-place rolling restart", paths), Paths: paths,
		Text: "deploys use in-place rolling restart", Branch: "main", Origin: factOriginDistilled, Status: factStatusActive,
		CreatedAt: f.now, UpdatedAt: f.now,
	}
	privTarget := factRecord{
		ID: factRecordID("backups run nightly", privPaths), Paths: privPaths,
		Text: "backups run nightly", Branch: "main", Origin: factOriginDistilled, Status: factStatusActive,
		CreatedAt: f.now, UpdatedAt: f.now,
	}
	privCandidate := factRecord{
		ID: factRecordID("backups run hourly", privPaths), Paths: privPaths,
		Text: "backups run hourly", Branch: "main", Origin: factOriginDistilled, Status: factStatusActive,
		CreatedAt: f.now, UpdatedAt: f.now,
	}
	f.writeFacts(t, "main", []factRecord{target, candidate, privTarget, privCandidate})

	shared := factProposal{
		Action: factActionSupersede, CandidateID: candidate.ID, TargetID: target.ID,
		Branch: "main", ProposedBy: "member-C",
	}
	private := factProposal{
		Action: factActionSupersede, CandidateID: privCandidate.ID, TargetID: privTarget.ID,
		Branch: "main", // no ProposedBy: distill's private review backlog
	}
	if err := writeFactProposals(f.storage.BrainDir, "main", []factProposal{shared, private}); err != nil {
		t.Fatal(err)
	}
	if err := writeSharedProposalLedger(f.storage.BrainDir, "main", map[string]struct{}{
		factsync.ProposalID(shared): {},
	}); err != nil {
		t.Fatal(err)
	}

	out, err := execute(t, NewRootCommand(f.opts), "facts", "review", "--apply-all")
	if err != nil {
		t.Fatalf("bulk review should succeed for the private backlog: %v\n%s", err, out)
	}

	queued, qErr := loadFactProposals(f.storage.BrainDir, "main")
	if qErr != nil {
		t.Fatal(qErr)
	}
	if len(queued) != 1 || factsync.ProposalID(queued[0]) != factsync.ProposalID(shared) {
		t.Fatalf("bulk review should keep exactly the shared proposal queued, got %d: %+v", len(queued), queued)
	}

	facts, loadErr := loadFacts(f.storage.BrainDir, "main")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	// Private settled...
	if ti := indexOfFact(facts, privTarget.ID); ti < 0 || facts[ti].Status != factStatusSuperseded {
		t.Fatalf("private proposal was not settled locally: %+v", facts)
	}
	// ...shared untouched.
	if ti := indexOfFact(facts, target.ID); ti < 0 || facts[ti].Status != factStatusActive {
		t.Fatalf("shared proposal was settled locally despite the interlock: %+v", facts)
	}
}

// TestProposalStillOpenAgainstDetectsSettlements is the cross-clone guard. The
// shared-proposal ledger lives in one checkout, so a SECOND clone of the same member
// starts with an empty ledger and would republish conflicts the team already settled,
// resurrecting them for everyone. The shared head is identical for every clone, so a
// proposal that is no longer a live conflict against it is treated as settled
// regardless of local ledger state.
func TestProposalStillOpenAgainstDetectsSettlements(t *testing.T) {
	paths := normalizeFactPaths([]string{"ops.deploy.strategy"})
	a := factRecord{ID: "fact:aaa", Paths: paths, Text: "canary", Branch: "main", Status: factStatusActive}
	b := factRecord{ID: "fact:bbb", Paths: paths, Text: "blue-green", Branch: "main", Status: factStatusActive}
	p := factProposal{Action: factActionSupersede, CandidateID: a.ID, TargetID: b.ID, Branch: "main", ProposedBy: "member-A"}

	for _, tc := range []struct {
		name string
		head []factRecord
		want bool
	}{
		{"both active: still a live conflict", []factRecord{a, b}, true},
		{"candidate merged away", []factRecord{b}, false},
		{"target gone", []factRecord{a}, false},
		{"head empty", nil, false},
		{
			"target superseded by the candidate: settled as asked",
			[]factRecord{a, {ID: b.ID, Paths: paths, Branch: "main", Status: factStatusSuperseded, SupersededBy: a.ID}},
			false,
		},
		{
			"candidate superseded by the target: settled the other way",
			[]factRecord{{ID: a.ID, Paths: paths, Branch: "main", Status: factStatusSuperseded, SupersededBy: b.ID}, b},
			false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := proposalStillOpenAgainst(tc.head, p); got != tc.want {
				t.Fatalf("proposalStillOpenAgainst = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestReconcileKeepsSettledEntryWhenHeadUnavailable guards the ordering. Pruning a
// settled entry is only safe once its settlement has been mirrored into local facts.
// With no readable shared head there is nothing to mirror FROM, so dropping the entry
// would leave the retired fact active locally with no record left to reconcile it
// against — silently reintroducing the very defect the mirroring closes.
func TestReconcileKeepsSettledEntryWhenHeadUnavailable(t *testing.T) {
	f := newVerifyFixture(t)
	paths := normalizeFactPaths([]string{"ops.deploy.strategy"})
	mine := factRecord{ID: "fact:aaa", Paths: paths, Text: "canary", Branch: "main", Status: factStatusActive}
	theirs := factRecord{ID: "fact:bbb", Paths: paths, Text: "blue-green", Branch: "main", Status: factStatusActive}
	f.writeFacts(t, "main", []factRecord{mine, theirs})

	p := factProposal{Action: factActionMerge, CandidateID: mine.ID, TargetID: theirs.ID, Branch: "main", ProposedBy: "member-C"}
	if err := writeFactProposals(f.storage.BrainDir, "main", []factProposal{p}); err != nil {
		t.Fatal(err)
	}
	if err := writeSharedProposalLedger(f.storage.BrainDir, "main", map[string]struct{}{factsync.ProposalID(p): {}}); err != nil {
		t.Fatal(err)
	}

	// Settled hosted-side (absent from the open set) but the head is unavailable.
	if err := reconcileLocalProposalQueue(f.storage.BrainDir, "main", []factsync.OpenProposal{}, nil, f.now); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	queued, err := loadFactProposals(f.storage.BrainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 {
		t.Fatalf("settled entry was pruned with no head to mirror from; it can never be reconciled now: %+v", queued)
	}
	// And local facts must be untouched — no half-applied settlement.
	facts, err := loadFacts(f.storage.BrainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if indexOfFact(facts, mine.ID) < 0 {
		t.Fatal("local fact was retired without a head to justify it")
	}
}

// TestFactsProposalsApplyMirrorsIntoLocalFactsImmediately pins the member's own view.
// `facts sync` reconciles settlements eventually, but the member who just ran
// `facts proposals apply` must not keep seeing the pre-settlement state in their own
// facts until their next sync — a local view contradicting the decision they just made.
func TestFactsProposalsApplyMirrorsIntoLocalFactsImmediately(t *testing.T) {
	f, _, open := newHostedProposalsFixture(t)

	// The fixture's hosted head holds the pair; give this member the same two facts
	// locally, which is the state a member who had synced would be in.
	paths := normalizeFactPaths([]string{"ops.deploy.strategy"})
	target := factRecord{
		ID: open.Proposal.TargetID, Paths: paths, Text: "deploys use blue-green cutover",
		Branch: "main", Origin: factOriginDistilled, Status: factStatusActive, CreatedAt: f.now, UpdatedAt: f.now,
	}
	candidate := factRecord{
		ID: open.Proposal.CandidateID, Paths: paths, Text: "deploys use in-place rolling restart",
		Branch: "main", Origin: factOriginDistilled, Status: factStatusActive, CreatedAt: f.now, UpdatedAt: f.now,
	}
	f.writeFacts(t, "main", []factRecord{target, candidate})
	if _, err := execute(t, NewRootCommand(f.opts), "facts", "sync", "--facts-backend", "http"); err != nil {
		t.Fatalf("establish hosted binding: %v", err)
	}

	out, err := execute(t, NewRootCommand(f.opts), "facts", "proposals", "apply", open.ID,
		"--repo-id", "repo-01HZZ", "--branch", "main")
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}

	facts, err := loadFacts(f.storage.BrainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	// The fixture's proposal is a supersede: accepting it retires the TARGET.
	ti := indexOfFact(facts, target.ID)
	if ti < 0 {
		t.Fatalf("target vanished from local facts entirely: %+v", facts)
	}
	if facts[ti].Status != factStatusSuperseded {
		t.Fatalf("local facts still show the pre-settlement state after apply: status=%s", facts[ti].Status)
	}
	if facts[ti].SupersededBy != candidate.ID {
		t.Fatalf("supersede did not point at the winning candidate: %q", facts[ti].SupersededBy)
	}
}
