package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ashtom/entire-brain/internal/factsync"
)

// localOnlyServer implements ONLY the fact-set head seam — what internal/factgitmeta's
// local store is. It stands in for "a backend with no shared proposal queue".
type localOnlyServer struct{}

func (localOnlyServer) Current(context.Context, string, string) (string, []byte, bool, error) {
	return "", nil, false, nil
}

func (localOnlyServer) Advance(context.Context, string, string, string, []byte) (string, error) {
	return "ref-1", nil
}

// TestPublishRaisedProposalsSkipsQueuelessBackend proves the default (local) sync path
// is untouched: a backend that only serves the fact-set head shares nothing and makes
// no call, so `facts sync` stays fully offline by default.
func TestPublishRaisedProposalsSkipsQueuelessBackend(t *testing.T) {
	raised := []factProposal{{Action: factActionSupersede, CandidateID: "fact:c", TargetID: "fact:t", Branch: "main", ProposedBy: "member-B"}}
	res, err := publishRaisedProposals(context.Background(), localOnlyServer{}, "repo", "main", raised)
	if err != nil || res.Published || res.Open != 0 {
		t.Fatalf("publishRaisedProposals(local backend) = %+v, %v; want a silent no-op", res, err)
	}
	// No proposals raised is a no-op even on a queue-capable backend.
	res, err = publishRaisedProposals(context.Background(), &factsync.HTTPServer{BaseURL: "http://127.0.0.1:0"}, "repo", "main", nil)
	if err != nil || res.Published {
		t.Fatalf("publishRaisedProposals(no proposals) = %+v, %v", res, err)
	}
}

// TestFactsSyncSharesRaisedProposals proves the sync verb feeds the review queue: a
// local fact contradicting the hosted head raises a routed proposal, and that proposal
// lands in the shared open set where `facts proposals` can see it.
func TestFactsSyncSharesRaisedProposals(t *testing.T) {
	f, fake, existing := newHostedProposalsFixture(t)
	// Start from an empty queue so the proposal the sync raises is unambiguous.
	fake.mu.Lock()
	fake.proposals = nil
	fake.mu.Unlock()

	paths := normalizeFactPaths([]string{"ops.deploy.strategy"})
	local := factRecord{
		ID: factRecordID("deploys use a canary rollout", paths), Paths: paths,
		Text: "deploys use a canary rollout", Branch: "main", Origin: factOriginDistilled, Status: factStatusActive,
		Provenance: []factAnchor{{SessionID: "session-C", Transcript: "sessions/local.jsonl", Line: 3}},
		CreatedAt:  f.now, UpdatedAt: f.now,
	}
	f.writeFacts(t, "main", []factRecord{local})

	out, err := execute(t, NewRootCommand(f.opts), "facts", "sync", "--facts-backend", "http", "--member", "member-C", "--json")
	if err != nil {
		t.Fatalf("facts sync: %v\n%s", err, out)
	}
	var report struct {
		Backend         string `json:"backend"`
		Published       bool   `json:"published"`
		ProposalsShared bool   `json:"proposals_shared"`
		ProposalsOpen   int    `json:"proposals_open"`
		ShareError      string `json:"proposals_share_error"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse sync json: %v\n%s", err, out)
	}
	if !strings.HasPrefix(report.Backend, "http (") || !report.Published {
		t.Fatalf("sync report = %+v", report)
	}
	if !report.ProposalsShared || report.ProposalsOpen != 2 || report.ShareError != "" {
		t.Fatalf("sync did not share its raised proposals: %+v", report)
	}

	open := fake.openSet()
	if len(open) != 2 {
		t.Fatalf("shared queue = %d proposal(s); want 2 (one per contradicted fact)", len(open))
	}
	for _, p := range open {
		if p.ID == existing.ID {
			t.Fatalf("sync republished a pre-existing proposal: %+v", p)
		}
		if p.Proposal.ProposedBy != "member-C" || p.Proposal.CandidateID != local.ID {
			t.Fatalf("shared proposal lost routing: %+v", p.Proposal)
		}
	}

	// The pushed head carries no local transcript path (egress redaction).
	fake.mu.Lock()
	pushed := string(fake.facts)
	fake.mu.Unlock()
	if strings.Contains(pushed, "sessions/local.jsonl") {
		t.Fatalf("sync leaked a local transcript path: %s", pushed)
	}
}

// TestFactsSyncShareFailureOnlyWarns proves that ONLY a backend with no review
// queue at all (an entire-api predating the endpoint: 404) is downgraded to a
// warning — the facts already landed and the queue is a hosted add-on there.
func TestFactsSyncShareFailureOnlyWarns(t *testing.T) {
	f, fake, _ := newHostedProposalsFixture(t)
	fake.mu.Lock()
	fake.proposals = nil
	fake.failPublish = 404
	fake.mu.Unlock()

	paths := normalizeFactPaths([]string{"ops.deploy.strategy"})
	local := factRecord{
		ID: factRecordID("deploys use a canary rollout", paths), Paths: paths,
		Text: "deploys use a canary rollout", Branch: "main", Origin: factOriginDistilled, Status: factStatusActive,
		Provenance: []factAnchor{{SessionID: "session-C"}}, CreatedAt: f.now, UpdatedAt: f.now,
	}
	f.writeFacts(t, "main", []factRecord{local})

	out, err := execute(t, NewRootCommand(f.opts), "facts", "sync", "--facts-backend", "http", "--member", "member-C")
	if err != nil {
		t.Fatalf("facts sync must survive a missing review queue: %v\n%s", err, out)
	}
	if !strings.Contains(out, "warning: backend has no shared review queue") {
		t.Fatalf("missing queue was not warned about:\n%s", out)
	}
	if !strings.Contains(out, "head advanced to") {
		t.Fatalf("sync did not report its successful advance:\n%s", out)
	}
}

// TestFactsSyncShareHardFailureFailsTheCommand proves every OTHER publish failure
// (here a 500) fails the sync command: the facts already advanced the shared
// head, so a swallowed error would leave raised conflicts that only this member
// can see — and the identical-content short-circuit means a later sync would
// never re-raise them.
func TestFactsSyncShareHardFailureFailsTheCommand(t *testing.T) {
	f, fake, _ := newHostedProposalsFixture(t)
	fake.mu.Lock()
	fake.proposals = nil
	fake.failPublish = 500
	fake.mu.Unlock()

	paths := normalizeFactPaths([]string{"ops.deploy.strategy"})
	local := factRecord{
		ID: factRecordID("deploys use a canary rollout", paths), Paths: paths,
		Text: "deploys use a canary rollout", Branch: "main", Origin: factOriginDistilled, Status: factStatusActive,
		Provenance: []factAnchor{{SessionID: "session-C"}}, CreatedAt: f.now, UpdatedAt: f.now,
	}
	f.writeFacts(t, "main", []factRecord{local})

	out, err := execute(t, NewRootCommand(f.opts), "facts", "sync", "--facts-backend", "http", "--member", "member-C")
	if err == nil {
		t.Fatalf("a real publish failure must fail the command:\n%s", out)
	}
	if !strings.Contains(err.Error(), "sharing") || !strings.Contains(err.Error(), "facts review") {
		t.Fatalf("failure must say the head advanced and point at local review: %v", err)
	}
	queued, loadErr := loadFactProposals(f.storage.BrainDir, "main")
	if loadErr != nil || len(queued) == 0 {
		t.Fatalf("raised conflicts must stay in the local queue for recovery: %v, %d", loadErr, len(queued))
	}
}

// TestFactsSyncReconcilesLocalQueueWithHostedResolutions proves the local review
// queue converges on the hosted open set after a publish: a conflict a teammate
// settled hosted-side stops being listed (and re-appliable) locally, while local
// entries the hosted set still carries survive.
func TestFactsSyncReconcilesLocalQueueWithHostedResolutions(t *testing.T) {
	f, fake, _ := newHostedProposalsFixture(t)
	fake.mu.Lock()
	fake.proposals = nil
	fake.mu.Unlock()

	paths := normalizeFactPaths([]string{"ops.deploy.strategy"})
	local := factRecord{
		ID: factRecordID("deploys use a canary rollout", paths), Paths: paths,
		Text: "deploys use a canary rollout", Branch: "main", Origin: factOriginDistilled, Status: factStatusActive,
		Provenance: []factAnchor{{SessionID: "session-C"}}, CreatedAt: f.now, UpdatedAt: f.now,
	}
	f.writeFacts(t, "main", []factRecord{local})

	if out, err := execute(t, NewRootCommand(f.opts), "facts", "sync", "--facts-backend", "http", "--member", "member-C"); err != nil {
		t.Fatalf("first sync: %v\n%s", err, out)
	}
	queued, err := loadFactProposals(f.storage.BrainDir, "main")
	if err != nil || len(queued) == 0 {
		t.Fatalf("first sync queued nothing locally: %v, %d", err, len(queued))
	}

	// A teammate settles every open proposal on the hosted queue.
	fake.mu.Lock()
	fake.proposals = nil
	fake.mu.Unlock()

	if out, err := execute(t, NewRootCommand(f.opts), "facts", "sync", "--facts-backend", "http", "--member", "member-C"); err != nil {
		t.Fatalf("second sync: %v\n%s", err, out)
	}
	after, err := loadFactProposals(f.storage.BrainDir, "main")
	if err != nil {
		t.Fatalf("load queue after reconcile: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("hosted-settled proposals still in the local queue: %d", len(after))
	}
}
