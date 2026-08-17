package cli

import (
	"strings"
	"testing"

	"github.com/ashtom/entire-brain/internal/factmerge"
	"github.com/ashtom/entire-brain/internal/factsync"
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
	if ti := indexOfFact(facts, privTarget.ID); ti >= 0 && facts[ti].Status != factStatusSuperseded {
		t.Fatalf("private proposal was not settled locally: %+v", facts[ti])
	}
	// ...shared untouched.
	if ti := indexOfFact(facts, target.ID); ti < 0 || facts[ti].Status != factStatusActive {
		t.Fatalf("shared proposal was settled locally despite the interlock: %+v", facts)
	}
}
