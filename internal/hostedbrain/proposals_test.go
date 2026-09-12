package hostedbrain

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ashtom/entire-brain/internal/factmerge"
	"github.com/ashtom/entire-brain/internal/factsync"
)

// hostedFactsServer stands up an httptest server speaking entire-api's fact-set head
// + open-proposal contract, backed by an in-memory head and open set. It is
// deliberately minimal (the exhaustive status/wire coverage lives in
// internal/factsync); here it exists to prove the hosted brain CLIENT drives the
// surface end to end and honours the egress gate.
type hostedFactsServer struct {
	mu        sync.Mutex
	factsRef  string
	facts     []byte
	proposals []factsync.OpenProposal
	propsRef  string
	requests  int
}

func (s *hostedFactsServer) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests++
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path

		switch {
		case strings.HasSuffix(path, "/brain/facts/proposals") && r.Method == http.MethodGet:
			s.mu.Lock()
			defer s.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"found": len(s.proposals) > 0, "ref": s.propsRef, "proposals": s.proposals})

		case strings.HasSuffix(path, "/brain/facts/proposals") && r.Method == http.MethodPost:
			var body struct {
				Proposals []factsync.OpenProposal `json:"proposals"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.mu.Lock()
			defer s.mu.Unlock()
			s.proposals, s.propsRef = body.Proposals, "props-2"
			_ = json.NewEncoder(w).Encode(map[string]any{"ref": s.propsRef, "changed": true})

		case strings.HasSuffix(path, "/resolve") && r.Method == http.MethodPost:
			var body struct {
				Data      []byte                  `json:"data"`
				Proposals []factsync.OpenProposal `json:"proposals"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.mu.Lock()
			defer s.mu.Unlock()
			s.facts, s.factsRef = body.Data, "facts-2"
			s.proposals, s.propsRef = body.Proposals, "props-3"
			_ = json.NewEncoder(w).Encode(map[string]any{"factsRef": s.factsRef, "proposalsRef": s.propsRef, "changed": true})

		case strings.Contains(path, "/brain/facts/proposals/") && r.Method == http.MethodGet:
			id := path[strings.LastIndex(path, "/")+1:]
			s.mu.Lock()
			defer s.mu.Unlock()
			for _, p := range s.proposals {
				if p.ID == id {
					_ = json.NewEncoder(w).Encode(map[string]any{"found": true, "ref": s.propsRef, "proposal": p})
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)

		case strings.HasSuffix(path, "/brain/facts") && r.Method == http.MethodGet:
			s.mu.Lock()
			defer s.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"found": len(s.facts) > 0, "ref": s.factsRef, "data": s.facts})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func hostedFactsFixture(t *testing.T) (*hostedFactsServer, *Client, []factsync.OpenProposal, time.Time) {
	t.Helper()
	now := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)
	paths := factmerge.NormalizePaths([]string{"ops.deploy.strategy"})
	target := factmerge.Record{
		ID: factmerge.RecordID("deploys use blue-green cutover", paths), Paths: paths,
		Text: "deploys use blue-green cutover", Branch: "main", Origin: "distilled", Status: factmerge.StatusActive,
		Provenance: []factmerge.Anchor{{SessionID: "session-A", Transcript: "sessions/local.jsonl", Line: 7}},
		CreatedAt:  now, UpdatedAt: now,
	}
	candidate := factmerge.Record{
		ID: factmerge.RecordID("deploys use in-place rolling restart", paths), Paths: paths,
		Text: "deploys use in-place rolling restart", Branch: "main", Origin: "distilled", Status: factmerge.StatusActive,
		Provenance: []factmerge.Anchor{{SessionID: "session-B"}},
		CreatedAt:  now, UpdatedAt: now,
	}
	var blob strings.Builder
	if err := factmerge.WriteNDJSON(&blob, []factmerge.Record{target, candidate}); err != nil {
		t.Fatal(err)
	}
	open := factsync.OpenProposals([]factmerge.Proposal{{
		Action: factmerge.ActionSupersede, CandidateID: candidate.ID, TargetID: target.ID,
		Branch: "main", ProposedBy: "member-B",
	}})

	srv := &hostedFactsServer{factsRef: "facts-1", facts: []byte(blob.String()), proposals: open, propsRef: "props-1"}
	ts := httptest.NewServer(srv.handler(t))
	t.Cleanup(ts.Close)
	return srv, &Client{BaseURL: ts.URL, Token: "tok"}, open, now
}

func TestClientListAndGetProposals(t *testing.T) {
	ctx := context.Background()
	_, c, open, _ := hostedFactsFixture(t)

	got, err := c.ListProposals(ctx, "repo1", "main")
	if err != nil || len(got) != 1 || got[0].ID != open[0].ID {
		t.Fatalf("ListProposals = %+v, %v", got, err)
	}
	if got[0].Proposal.ProposedBy != "member-B" {
		t.Fatalf("listed proposal lost routing: %+v", got[0])
	}

	one, err := c.GetProposal(ctx, "repo1", "main", open[0].ID)
	if err != nil || one.ID != open[0].ID {
		t.Fatalf("GetProposal = %+v, %v", one, err)
	}
	if _, err := c.GetProposal(ctx, "repo1", "main", "prop-deadbeefdeadbeef"); !errors.Is(err, factsync.ErrProposalNotFound) {
		t.Fatalf("GetProposal(missing) = %v; want ErrProposalNotFound", err)
	}
}

func TestClientApplyProposalConverges(t *testing.T) {
	ctx := context.Background()
	srv, c, open, now := hostedFactsFixture(t)

	res, err := c.ApplyProposal(ctx, "repo1", "main", open[0].ID, now)
	if err != nil {
		t.Fatalf("ApplyProposal: %v", err)
	}
	if res.Decision != "accept" || res.Remaining != 0 || res.FactsRef != "facts-2" {
		t.Fatalf("ApplyProposal result = %+v", res)
	}

	srv.mu.Lock()
	pushed := string(srv.facts)
	remaining := len(srv.proposals)
	srv.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("open set after apply = %d; want 0", remaining)
	}
	recs, err := factmerge.ParseNDJSON(strings.NewReader(pushed))
	if err != nil {
		t.Fatal(err)
	}
	var superseded, active int
	for _, r := range recs {
		switch r.Status {
		case factmerge.StatusSuperseded:
			superseded++
			if r.SupersededBy != open[0].Proposal.CandidateID {
				t.Fatalf("loser superseded by %q; want %s", r.SupersededBy, open[0].Proposal.CandidateID)
			}
		case factmerge.StatusActive:
			active++
		}
	}
	if superseded != 1 || active != 1 {
		t.Fatalf("converged head = %d superseded / %d active; want 1/1", superseded, active)
	}
	// The write path redacts local provenance exactly like sync does.
	if strings.Contains(pushed, "sessions/local.jsonl") {
		t.Fatalf("apply leaked a local transcript path: %s", pushed)
	}
}

func TestClientRejectProposalKeepsBothActive(t *testing.T) {
	ctx := context.Background()
	srv, c, open, now := hostedFactsFixture(t)

	res, err := c.RejectProposal(ctx, "repo1", "main", open[0].Proposal.CandidateID, now)
	if err != nil {
		t.Fatalf("RejectProposal: %v", err)
	}
	if res.Decision != "reject" || res.Remaining != 0 {
		t.Fatalf("RejectProposal result = %+v", res)
	}
	srv.mu.Lock()
	pushed := string(srv.facts)
	srv.mu.Unlock()
	recs, err := factmerge.ParseNDJSON(strings.NewReader(pushed))
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("reject head = %d facts; want 2", len(recs))
	}
	for _, r := range recs {
		if r.Status != factmerge.StatusActive {
			t.Fatalf("reject must keep both active; %s = %q", r.ID, r.Status)
		}
	}
}

func TestClientPublishProposals(t *testing.T) {
	ctx := context.Background()
	srv, c, open, _ := hostedFactsFixture(t)

	// Already open → idempotent no-op, no write.
	res, err := c.PublishProposals(ctx, "repo1", "main", open)
	if err != nil || res.Published || res.Open != 1 {
		t.Fatalf("PublishProposals(idempotent) = %+v, %v", res, err)
	}

	fresh := factsync.OpenProposals([]factmerge.Proposal{{
		Action: factmerge.ActionMerge, CandidateID: "fact:new-cand", TargetID: "fact:new-target",
		Branch: "main", ProposedBy: "member-C",
	}})
	res, err = c.PublishProposals(ctx, "repo1", "main", fresh)
	if err != nil || !res.Published || res.Open != 2 {
		t.Fatalf("PublishProposals(new) = %+v, %v", res, err)
	}
	srv.mu.Lock()
	stored := len(srv.proposals)
	srv.mu.Unlock()
	if stored != 2 {
		t.Fatalf("hosted open set = %d; want 2", stored)
	}
}

// TestClientProposalsRespectEgressGate proves every proposal verb refuses BEFORE any
// request when the local-only gate is set — the same guarantee the MCP surface makes.
func TestClientProposalsRespectEgressGate(t *testing.T) {
	ctx := context.Background()
	srv, c, open, now := hostedFactsFixture(t)
	srv.mu.Lock()
	before := srv.requests
	srv.mu.Unlock()

	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
	for name, call := range map[string]func() error{
		"repair":  func() error { _, err := c.RepairProposalIDs(ctx, "repo1", "main", false, ""); return err },
		"list":    func() error { _, err := c.ListProposals(ctx, "repo1", "main"); return err },
		"get":     func() error { _, err := c.GetProposal(ctx, "repo1", "main", open[0].ID); return err },
		"apply":   func() error { _, err := c.ApplyProposal(ctx, "repo1", "main", open[0].ID, now); return err },
		"reject":  func() error { _, err := c.RejectProposal(ctx, "repo1", "main", open[0].ID, now); return err },
		"publish": func() error { _, err := c.PublishProposals(ctx, "repo1", "main", open); return err },
	} {
		if err := call(); !errors.Is(err, ErrNoEgress) {
			t.Fatalf("%s under no-egress = %v; want ErrNoEgress", name, err)
		}
	}

	srv.mu.Lock()
	after := srv.requests
	srv.mu.Unlock()
	if after != before {
		t.Fatalf("no-egress calls still made %d request(s)", after-before)
	}
}
