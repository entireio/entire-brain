package factsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ashtom/entire-brain/internal/factmerge"
)

// compile-time: HTTPServer satisfies the full hosted seam ResolveOpen is written
// against (the fact-set head Server plus the open-proposal transport).
var (
	_ ProposalTransport = (*HTTPServer)(nil)
	_ ProposalServer    = (*HTTPServer)(nil)
)

// hostedFake mirrors what entire-api must store for the cross-member review model:
// the content-addressed fact-set head (reusing fakeServer's CAS semantics) plus the
// branch's open proposal set under its own CAS ref. It is the in-memory backing for
// hostedContractServer, so the REAL HTTPServer adapter is exercised over real HTTP.
type hostedFake struct {
	facts *fakeServer

	mu        sync.Mutex
	ref       string
	proposals []OpenProposal
	found     bool

	// Test hooks. forceStatus, when non-zero, is returned by every proposal
	// endpoint (error-path coverage). interpose runs once immediately before a
	// resolve is evaluated, letting a test stale the caller's refs deterministically.
	forceStatus int
	interpose   func()

	// resolvedFacts records the last facts blob the server received, so egress
	// redaction can be asserted on the bytes that actually crossed the wire.
	resolvedFacts []byte
}

func newHostedFake() *hostedFake { return &hostedFake{facts: &fakeServer{}} }

func proposalsRef(list []OpenProposal) string {
	data, _ := json.Marshal(list)
	sum := sha256.Sum256(data)
	return "props-" + hex.EncodeToString(sum[:])
}

func (f *hostedFake) seed(list []OpenProposal) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.proposals, f.ref, f.found = list, proposalsRef(list), true
}

func (f *hostedFake) snapshot() (string, []OpenProposal, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ref, append([]OpenProposal(nil), f.proposals...), f.found
}

// hostedContractServer speaks the documented fact-set + open-proposal wire contract
// (see httpserver.go / httpproposals.go) against a hostedFake.
func hostedContractServer(t *testing.T, fake *hostedFake) *httptest.Server {
	t.Helper()
	ctx := context.Background()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		branch := r.URL.Query().Get("branch")

		switch {
		// --- open proposal set ------------------------------------------------
		case strings.Contains(path, "/brain/facts/proposals"):
			if fake.forceStatus != 0 {
				w.WriteHeader(fake.forceStatus)
				return
			}
			rest := strings.TrimPrefix(path[strings.Index(path, "/brain/facts/proposals"):], "/brain/facts/proposals")
			rest = strings.Trim(rest, "/")

			switch {
			case r.Method == http.MethodGet && rest == "": // list
				ref, list, found := fake.snapshot()
				_ = json.NewEncoder(w).Encode(map[string]any{"found": found, "ref": ref, "version": 1, "proposals": list})

			case r.Method == http.MethodGet: // fetch one
				_, list, _ := fake.snapshot()
				for _, p := range list {
					if p.ID == rest {
						_ = json.NewEncoder(w).Encode(map[string]any{"found": true, "ref": proposalsRef(list), "version": 1, "proposal": p})
						return
					}
				}
				w.WriteHeader(http.StatusNotFound)

			case r.Method == http.MethodPost && rest == "": // whole-set publish (CAS)
				var body publishProposalsBody
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				fake.mu.Lock()
				defer fake.mu.Unlock()
				cur := ""
				if fake.found {
					cur = fake.ref
				}
				if body.OldRef != cur {
					w.WriteHeader(http.StatusPreconditionFailed)
					return
				}
				newRef := proposalsRef(body.Proposals)
				if newRef == cur {
					_ = json.NewEncoder(w).Encode(map[string]any{"ref": cur, "version": 1, "changed": false})
					return
				}
				fake.proposals, fake.ref, fake.found = body.Proposals, newRef, true
				_ = json.NewEncoder(w).Encode(map[string]any{"ref": newRef, "version": 1, "changed": true})

			case r.Method == http.MethodPost && strings.HasSuffix(rest, "/resolve"):
				id := strings.TrimSuffix(rest, "/resolve")
				var body resolveRequestBody
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if len(body.Data) == 0 {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if fake.interpose != nil {
					hook := fake.interpose
					fake.interpose = nil
					hook()
				}
				fake.mu.Lock()
				cur, list, found := fake.ref, append([]OpenProposal(nil), fake.proposals...), fake.found
				fake.mu.Unlock()
				open := false
				for _, p := range list {
					if p.ID == id {
						open = true
					}
				}
				if !open {
					w.WriteHeader(http.StatusNotFound) // already settled by someone else
					return
				}
				if found && body.ProposalsOldRef != cur {
					w.WriteHeader(http.StatusConflict)
					return
				}
				newFactsRef, err := fake.facts.Advance(ctx, "repo", body.Branch, body.FactsOldRef, body.Data)
				switch {
				case errors.Is(err, ErrConflict):
					w.WriteHeader(http.StatusPreconditionFailed)
					return
				case errors.Is(err, ErrNoChange):
					newFactsRef, _, _, _ = fake.facts.Current(ctx, "repo", body.Branch)
				case err != nil:
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				fake.mu.Lock()
				fake.resolvedFacts = append([]byte(nil), body.Data...)
				fake.proposals, fake.ref, fake.found = body.Proposals, proposalsRef(body.Proposals), true
				proposalsNewRef := fake.ref
				fake.mu.Unlock()
				_ = json.NewEncoder(w).Encode(map[string]any{"factsRef": newFactsRef, "proposalsRef": proposalsNewRef, "changed": true})

			default:
				w.WriteHeader(http.StatusNotFound)
			}

		// --- fact-set head (same contract as contractServer) --------------------
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/brain/facts"):
			ref, blob, found, _ := fake.facts.Current(ctx, "repo", branch)
			_ = json.NewEncoder(w).Encode(map[string]any{"found": found, "ref": ref, "version": 1, "data": blob})

		case r.Method == http.MethodPost && strings.HasSuffix(path, "/brain/facts/advance"):
			var body struct {
				Branch string `json:"branch"`
				OldRef string `json:"oldRef"`
				Data   []byte `json:"data"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Data) == 0 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			newRef, err := fake.facts.Advance(ctx, "repo", body.Branch, body.OldRef, body.Data)
			switch {
			case errors.Is(err, ErrNoChange):
				cur, _, _, _ := fake.facts.Current(ctx, "repo", body.Branch)
				_ = json.NewEncoder(w).Encode(map[string]any{"newRef": cur, "version": 1, "changed": false})
			case errors.Is(err, ErrConflict):
				w.WriteHeader(http.StatusPreconditionFailed)
			case err != nil:
				w.WriteHeader(http.StatusBadRequest)
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{"newRef": newRef, "version": 1, "changed": true})
			}

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func testProposal(candidate, target, member string) factmerge.Proposal {
	return factmerge.Proposal{Action: factmerge.ActionSupersede, CandidateID: candidate, TargetID: target, Branch: "main", ProposedBy: member}
}

// TestHTTPProposalsListAndGet covers the read half of the transport: an empty branch
// (found=false → empty set, NOT an error), a populated set, fetching one proposal, and
// the two "already settled" shapes (404 and 200/found=false) both mapping to
// ErrProposalNotFound.
func TestHTTPProposalsListAndGet(t *testing.T) {
	ctx := context.Background()
	fake := newHostedFake()
	ts := hostedContractServer(t, fake)
	defer ts.Close()
	h := &HTTPServer{BaseURL: ts.URL, Token: "tok"}

	set, err := h.ListProposals(ctx, "repo", "main")
	if err != nil || set.Found || len(set.Proposals) != 0 {
		t.Fatalf("ListProposals(empty) = %+v, %v; want empty non-error set", set, err)
	}

	open := OpenProposals([]factmerge.Proposal{
		testProposal("fact:cand-a", "fact:target-a", "member-B"),
		testProposal("fact:cand-b", "fact:target-b", "member-C"),
	})
	fake.seed(open)

	set, err = h.ListProposals(ctx, "repo", "main")
	if err != nil || !set.Found || len(set.Proposals) != 2 {
		t.Fatalf("ListProposals = %+v, %v; want 2 open proposals", set, err)
	}
	if set.Ref == "" {
		t.Fatal("ListProposals returned no CAS ref")
	}
	if set.Proposals[0].ID != open[0].ID || set.Proposals[0].Proposal.ProposedBy != "member-B" {
		t.Fatalf("proposal[0] = %+v; want %+v", set.Proposals[0], open[0])
	}

	got, err := h.GetProposal(ctx, "repo", "main", open[1].ID)
	if err != nil || got.ID != open[1].ID || got.Proposal.CandidateID != "fact:cand-b" {
		t.Fatalf("GetProposal = %+v, %v", got, err)
	}

	if _, err := h.GetProposal(ctx, "repo", "main", "prop-deadbeefdeadbeef"); !errors.Is(err, ErrProposalNotFound) {
		t.Fatalf("GetProposal(unknown) = %v; want ErrProposalNotFound", err)
	}

	// A 200 that reports found=false is the same outcome as a 404.
	softMiss := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"found": false})
	}))
	defer softMiss.Close()
	soft := &HTTPServer{BaseURL: softMiss.URL}
	if _, err := soft.GetProposal(ctx, "repo", "main", "prop-deadbeefdeadbeef"); !errors.Is(err, ErrProposalNotFound) {
		t.Fatalf("GetProposal(found=false) = %v; want ErrProposalNotFound", err)
	}
}

// TestHTTPProposalsErrorStatuses pins every non-success status the read/publish/resolve
// endpoints can return onto its sentinel or a hard error.
func TestHTTPProposalsErrorStatuses(t *testing.T) {
	ctx := context.Background()
	facts := []factmerge.Record{fact("a fact", []string{"a.b.c"}, "s", time.Now().UTC())}

	for _, tc := range []struct {
		name   string
		status int
		check  func(t *testing.T, h *HTTPServer)
	}{
		{"list 500", http.StatusInternalServerError, func(t *testing.T, h *HTTPServer) {
			if _, err := h.ListProposals(ctx, "repo", "main"); err == nil {
				t.Fatal("ListProposals(500) must error")
			}
		}},
		{"get 503", http.StatusServiceUnavailable, func(t *testing.T, h *HTTPServer) {
			if _, err := h.GetProposal(ctx, "repo", "main", "prop-deadbeefdeadbeef"); err == nil || errors.Is(err, ErrProposalNotFound) {
				t.Fatalf("GetProposal(503) = %v; want a hard error", err)
			}
		}},
		{"publish 412", http.StatusPreconditionFailed, func(t *testing.T, h *HTTPServer) {
			if _, err := h.PublishProposals(ctx, "repo", "main", "stale", nil); !errors.Is(err, ErrConflict) {
				t.Fatalf("PublishProposals(412) = %v; want ErrConflict", err)
			}
		}},
		{"publish 409", http.StatusConflict, func(t *testing.T, h *HTTPServer) {
			if _, err := h.PublishProposals(ctx, "repo", "main", "stale", nil); !errors.Is(err, ErrConflict) {
				t.Fatalf("PublishProposals(409) = %v; want ErrConflict", err)
			}
		}},
		{"publish 400", http.StatusBadRequest, func(t *testing.T, h *HTTPServer) {
			if _, err := h.PublishProposals(ctx, "repo", "main", "", nil); err == nil || errors.Is(err, ErrConflict) {
				t.Fatalf("PublishProposals(400) = %v; want a hard error", err)
			}
		}},
		{"resolve 412", http.StatusPreconditionFailed, func(t *testing.T, h *HTTPServer) {
			_, err := h.ResolveProposal(ctx, ResolveProposalRequest{RepoID: "repo", Branch: "main", ProposalID: "prop-deadbeefdeadbeef", Facts: facts})
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("ResolveProposal(412) = %v; want ErrConflict", err)
			}
		}},
		{"resolve 409", http.StatusConflict, func(t *testing.T, h *HTTPServer) {
			_, err := h.ResolveProposal(ctx, ResolveProposalRequest{RepoID: "repo", Branch: "main", ProposalID: "prop-deadbeefdeadbeef", Facts: facts})
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("ResolveProposal(409) = %v; want ErrConflict", err)
			}
		}},
		{"resolve 404", http.StatusNotFound, func(t *testing.T, h *HTTPServer) {
			_, err := h.ResolveProposal(ctx, ResolveProposalRequest{RepoID: "repo", Branch: "main", ProposalID: "prop-deadbeefdeadbeef", Facts: facts})
			if !errors.Is(err, ErrProposalNotFound) {
				t.Fatalf("ResolveProposal(404) = %v; want ErrProposalNotFound", err)
			}
		}},
		{"resolve 500", http.StatusInternalServerError, func(t *testing.T, h *HTTPServer) {
			_, err := h.ResolveProposal(ctx, ResolveProposalRequest{RepoID: "repo", Branch: "main", ProposalID: "prop-deadbeefdeadbeef", Facts: facts})
			if err == nil || errors.Is(err, ErrConflict) || errors.Is(err, ErrProposalNotFound) {
				t.Fatalf("ResolveProposal(500) = %v; want a hard error", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newHostedFake()
			fake.forceStatus = tc.status
			ts := hostedContractServer(t, fake)
			defer ts.Close()
			tc.check(t, &HTTPServer{BaseURL: ts.URL, Token: "tok"})
		})
	}
}

// TestResolveProposalRedactsLocalProvenance proves the egress guarantee holds on the
// resolution path too: the facts that cross the wire carry no member's local
// transcript path or line offset, while the opaque cross-member ids survive.
func TestResolveProposalRedactsLocalProvenance(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)
	fake := newHostedFake()
	ts := hostedContractServer(t, fake)
	defer ts.Close()
	h := &HTTPServer{BaseURL: ts.URL, Token: "tok"}

	leaky := fact("resolved fact", []string{"ops.deploy.strategy"}, "session-A", now)
	leaky.Provenance[0].Transcript = "sessions/2026/private-path.jsonl"
	leaky.Provenance[0].Line = 42

	open := OpenProposals([]factmerge.Proposal{testProposal("fact:cand", "fact:target", "member-B")})
	fake.seed(open)

	if _, err := h.ResolveProposal(ctx, ResolveProposalRequest{
		RepoID: "repo", Branch: "main", ProposalID: open[0].ID, Decision: Accept,
		Facts: []factmerge.Record{leaky}, ProposalsOldRef: proposalsRef(open),
	}); err != nil {
		t.Fatalf("ResolveProposal: %v", err)
	}

	fake.mu.Lock()
	pushed := string(fake.resolvedFacts)
	fake.mu.Unlock()
	if strings.Contains(pushed, "private-path.jsonl") || strings.Contains(pushed, `"line"`) {
		t.Fatalf("resolution leaked local provenance coordinates: %s", pushed)
	}
	if !strings.Contains(pushed, "session-A") {
		t.Fatalf("resolution dropped the opaque cross-member session id: %s", pushed)
	}
	// The caller's own records are never mutated by the egress copy.
	if leaky.Provenance[0].Transcript == "" || leaky.Provenance[0].Line != 42 {
		t.Fatalf("caller's local provenance was mutated: %+v", leaky.Provenance[0])
	}
}

// TestResolveProposalRefusesEmptyFactSet proves the client refuses to push an empty
// head (which the server 400s) rather than emitting a request that cannot succeed.
func TestResolveProposalRefusesEmptyFactSet(t *testing.T) {
	ctx := context.Background()
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	h := &HTTPServer{BaseURL: ts.URL}
	if _, err := h.ResolveProposal(ctx, ResolveProposalRequest{RepoID: "repo", Branch: "main", ProposalID: "prop-deadbeefdeadbeef"}); err == nil {
		t.Fatal("ResolveProposal with no facts must error")
	}
	if calls != 0 {
		t.Fatalf("empty resolution made %d HTTP call(s); want 0", calls)
	}
}

// TestProposalRoundTripProposeListApplyConverges is the load-bearing proof for the
// open-proposal transport: two members raise a genuine cross-member contradiction
// through Sync, the raised proposal is PUBLISHED to the hosted open set, a reviewer
// LISTS it over HTTP, and APPLYING it converges the shared head (loser retained as
// superseded, winner active) while emptying the open set.
func TestProposalRoundTripProposeListApplyConverges(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)
	fake := newHostedFake()
	ts := hostedContractServer(t, fake)
	defer ts.Close()
	h := &HTTPServer{BaseURL: ts.URL, Token: "tok"}

	factX := fact("deploys use blue-green cutover", []string{"ops.deploy.strategy"}, "session-A", now)
	factY := fact("deploys use in-place rolling restart", []string{"ops.deploy.strategy"}, "session-B", now)

	if _, err := Sync(ctx, h, "repo", "main", "member-A", []factmerge.Record{factX}, now); err != nil {
		t.Fatalf("member-A sync: %v", err)
	}
	resB, err := Sync(ctx, h, "repo", "main", "member-B", []factmerge.Record{factY}, now)
	if err != nil {
		t.Fatalf("member-B sync: %v", err)
	}
	if len(resB.Proposals) != 1 {
		t.Fatalf("member-B raised %d proposals; want 1", len(resB.Proposals))
	}

	// PROPOSE: publish the raised conflict so every member can see it.
	pub, err := PublishRaised(ctx, h, "repo", "main", resB.Proposals)
	if err != nil || !pub.Published || pub.Open != 1 {
		t.Fatalf("PublishRaised = %+v, %v", pub, err)
	}
	// Re-publishing the same conflict is idempotent (derived ids dedupe).
	again, err := PublishRaised(ctx, h, "repo", "main", resB.Proposals)
	if err != nil || again.Published || again.Open != 1 {
		t.Fatalf("PublishRaised(idempotent) = %+v, %v", again, err)
	}

	// LIST: a reviewer sees the routed proposal over the wire.
	set, err := h.ListProposals(ctx, "repo", "main")
	if err != nil || len(set.Proposals) != 1 {
		t.Fatalf("ListProposals = %+v, %v", set, err)
	}
	if set.Proposals[0].Proposal.ProposedBy != "member-B" {
		t.Fatalf("listed proposal lost routing: %+v", set.Proposals[0])
	}

	// APPLY: accepting the supersede converges the shared head.
	res, err := ResolveOpen(ctx, h, "repo", "main", set.Proposals[0].ID, Accept, now)
	if err != nil {
		t.Fatalf("ResolveOpen(Accept): %v", err)
	}
	if res.Remaining != 0 || res.Decision != "accept" || res.FactsRef == "" {
		t.Fatalf("ResolveOpen result = %+v", res)
	}

	_, final := headRecords(ctx, t, h)
	byID := map[string]factmerge.Record{}
	for _, r := range final {
		byID[r.ID] = r
	}
	if x := byID[factX.ID]; x.Status != factmerge.StatusSuperseded || x.SupersededBy != factY.ID {
		t.Fatalf("after apply, X = %q superseded_by %q; want superseded by %s", x.Status, x.SupersededBy, factY.ID)
	}
	if byID[factY.ID].Status != factmerge.StatusActive {
		t.Fatalf("after apply, Y = %q; want active", byID[factY.ID].Status)
	}

	// The open set is now empty, and the settled proposal is gone for everyone.
	after, err := h.ListProposals(ctx, "repo", "main")
	if err != nil || len(after.Proposals) != 0 {
		t.Fatalf("open set after apply = %+v, %v; want empty", after, err)
	}
	if _, err := ResolveOpen(ctx, h, "repo", "main", set.Proposals[0].ID, Accept, now); !errors.Is(err, ErrProposalNotFound) {
		t.Fatalf("re-resolving a settled proposal = %v; want ErrProposalNotFound", err)
	}
}

// TestResolveOpenRejectKeepsBothActive proves the reject verb over the transport: both
// facts stay active and the proposal leaves the open set.
func TestResolveOpenRejectKeepsBothActive(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)
	fake := newHostedFake()
	ts := hostedContractServer(t, fake)
	defer ts.Close()
	h := &HTTPServer{BaseURL: ts.URL, Token: "tok"}

	factX := fact("staging mirrors prod region", []string{"infra.staging.topology"}, "session-A", now)
	factY := fact("staging runs a single-node cluster", []string{"infra.staging.topology"}, "session-B", now)
	if _, err := Sync(ctx, h, "repo", "main", "member-A", []factmerge.Record{factX}, now); err != nil {
		t.Fatal(err)
	}
	resB, err := Sync(ctx, h, "repo", "main", "member-B", []factmerge.Record{factY}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PublishRaised(ctx, h, "repo", "main", resB.Proposals); err != nil {
		t.Fatalf("PublishRaised: %v", err)
	}

	// A candidate fact id is a valid proposal reference (the `facts review` key).
	res, err := ResolveOpen(ctx, h, "repo", "main", factY.ID, Reject, now)
	if err != nil {
		t.Fatalf("ResolveOpen(Reject): %v", err)
	}
	if res.Decision != "reject" || res.Remaining != 0 {
		t.Fatalf("reject result = %+v", res)
	}
	_, final := headRecords(ctx, t, h)
	if len(final) != 2 {
		t.Fatalf("reject head = %d facts; want 2", len(final))
	}
	for _, r := range final {
		if r.Status != factmerge.StatusActive {
			t.Fatalf("reject must keep all active; %s = %q", r.ID, r.Status)
		}
	}
}

// TestResolveOpenRetriesOnConflict deterministically stales the reviewer's refs
// between its read and its push, proving the resolve loop re-reads and re-resolves
// instead of failing or clobbering.
func TestResolveOpenRetriesOnConflict(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)
	fake := newHostedFake()
	ts := hostedContractServer(t, fake)
	defer ts.Close()
	h := &HTTPServer{BaseURL: ts.URL, Token: "tok"}

	factX := fact("cache ttl is 60s", []string{"api.cache.ttl"}, "session-A", now)
	factY := fact("cache ttl is 300s", []string{"api.cache.ttl"}, "session-B", now)
	if _, err := Sync(ctx, h, "repo", "main", "member-A", []factmerge.Record{factX}, now); err != nil {
		t.Fatal(err)
	}
	resB, err := Sync(ctx, h, "repo", "main", "member-B", []factmerge.Record{factY}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PublishRaised(ctx, h, "repo", "main", resB.Proposals); err != nil {
		t.Fatal(err)
	}

	// Interpose one competing member sync between the reviewer's read and its push:
	// the fact-set head moves, so the reviewer's factsOldRef is stale (→ 412).
	fake.interpose = func() {
		competitor := fact("cache is warmed on deploy", []string{"api.cache.warmup"}, "session-C", now)
		if _, err := Sync(ctx, h, "repo", "main", "member-C", []factmerge.Record{competitor}, now); err != nil {
			t.Errorf("interposed sync: %v", err)
		}
	}

	res, err := ResolveOpen(ctx, h, "repo", "main", resB.Proposals[0].CandidateID, Accept, now)
	if err != nil {
		t.Fatalf("ResolveOpen under contention: %v", err)
	}
	if res.Attempts < 2 {
		t.Fatalf("expected a CAS retry (Attempts>=2), got %d", res.Attempts)
	}
	_, final := headRecords(ctx, t, h)
	texts := map[string]string{}
	for _, r := range final {
		texts[r.Text] = r.Status
	}
	if texts["cache is warmed on deploy"] != factmerge.StatusActive {
		t.Fatalf("retry dropped the competitor's fact: %v", texts)
	}
	if texts["cache ttl is 60s"] != factmerge.StatusSuperseded || texts["cache ttl is 300s"] != factmerge.StatusActive {
		t.Fatalf("retry did not apply the resolution: %v", texts)
	}
}

// TestResolveOpenNoFactSetHead proves a proposal with no fact-set head behind it is
// surfaced as not-applicable rather than pushing an empty head.
func TestResolveOpenNoFactSetHead(t *testing.T) {
	ctx := context.Background()
	fake := newHostedFake()
	ts := hostedContractServer(t, fake)
	defer ts.Close()
	h := &HTTPServer{BaseURL: ts.URL, Token: "tok"}

	open := OpenProposals([]factmerge.Proposal{testProposal("fact:cand", "fact:target", "member-B")})
	fake.seed(open)
	_, err := ResolveOpen(ctx, h, "repo", "main", open[0].ID, Accept, time.Now().UTC())
	if !errors.Is(err, ErrProposalNotApplicable) {
		t.Fatalf("ResolveOpen with no head = %v; want ErrProposalNotApplicable", err)
	}
}

// TestResolveOpenPinsProposalAcrossRetries proves a CAS retry cannot rebind the
// caller's ref to a different proposal that entered the set between attempts: the
// proposal is pinned by id after the first resolution, so the settled conflict is
// exactly the one the reviewer saw.
func TestResolveOpenPinsProposalAcrossRetries(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)
	fake := newHostedFake()
	ts := hostedContractServer(t, fake)
	defer ts.Close()
	h := &HTTPServer{BaseURL: ts.URL, Token: "tok"}

	factX := fact("retries use exponential backoff", []string{"api.retry.policy"}, "session-A", now)
	factY := fact("retries use fixed 1s intervals", []string{"api.retry.policy"}, "session-B", now)
	if _, err := Sync(ctx, h, "repo", "main", "member-A", []factmerge.Record{factX}, now); err != nil {
		t.Fatal(err)
	}
	resB, err := Sync(ctx, h, "repo", "main", "member-B", []factmerge.Record{factY}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(resB.Proposals) != 1 {
		t.Fatalf("member-B raised %d proposals; want 1", len(resB.Proposals))
	}
	if _, err := PublishRaised(ctx, h, "repo", "main", resB.Proposals); err != nil {
		t.Fatal(err)
	}
	pinned := ProposalID(resB.Proposals[0])

	// Interpose between the reviewer's read and its push: the head moves (412 on
	// the reviewer's push) AND a second proposal sharing the same candidate fact
	// id enters the open set from another member. An unpinned retry re-resolving
	// the raw candidate-id ref would now find two matches.
	fake.interpose = func() {
		fake.interpose = nil
		competitor := fact("retry metrics are exported", []string{"api.retry.metrics"}, "session-C", now)
		if _, err := Sync(ctx, h, "repo", "main", "member-C", []factmerge.Record{competitor}, now); err != nil {
			t.Errorf("interposed sync: %v", err)
		}
		rival := resB.Proposals[0]
		rival.ProposedBy = "member-D"
		if _, err := PublishRaised(ctx, h, "repo", "main", []factmerge.Proposal{rival}); err != nil {
			t.Errorf("interposed publish: %v", err)
		}
	}

	res, err := ResolveOpen(ctx, h, "repo", "main", resB.Proposals[0].CandidateID, Accept, now)
	if err != nil {
		t.Fatalf("ResolveOpen under rebinding contention: %v", err)
	}
	if res.Attempts < 2 {
		t.Fatalf("expected a CAS retry (Attempts>=2), got %d", res.Attempts)
	}
	if res.Proposal.ID != pinned {
		t.Fatalf("retry settled %s; want the pinned %s", res.Proposal.ID, pinned)
	}
	if res.Remaining != 1 {
		t.Fatalf("rival proposal should remain open; Remaining = %d", res.Remaining)
	}
}

// TestResolveOpenRejectPrunesInapplicableProposal proves reject is also the prune
// path: a proposal whose facts are no longer both in the head cannot be applied,
// but rejecting it removes the otherwise-permanent entry without touching facts.
func TestResolveOpenRejectPrunesInapplicableProposal(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)
	fake := newHostedFake()
	ts := hostedContractServer(t, fake)
	defer ts.Close()
	h := &HTTPServer{BaseURL: ts.URL, Token: "tok"}

	anchor := fact("deploys are gated on green CI", []string{"ops.deploy.gate"}, "session-A", now)
	if _, err := Sync(ctx, h, "repo", "main", "member-A", []factmerge.Record{anchor}, now); err != nil {
		t.Fatal(err)
	}
	stuck := factmerge.Proposal{
		Action:      "merge",
		CandidateID: "fact:vanished-candidate",
		TargetID:    "fact:vanished-target",
		Branch:      "main",
		ProposedBy:  "member-B",
	}
	if _, err := PublishRaised(ctx, h, "repo", "main", []factmerge.Proposal{stuck}); err != nil {
		t.Fatal(err)
	}
	id := ProposalID(stuck)

	if _, err := ResolveOpen(ctx, h, "repo", "main", id, Accept, now); !errors.Is(err, ErrProposalNotApplicable) {
		t.Fatalf("accepting a proposal with vanished facts = %v; want ErrProposalNotApplicable", err)
	}
	headBefore, before := headRecords(ctx, t, h)
	res, err := ResolveOpen(ctx, h, "repo", "main", id, Reject, now)
	if err != nil {
		t.Fatalf("rejecting a proposal with vanished facts must prune it: %v", err)
	}
	if res.Remaining != 0 {
		t.Fatalf("proposal not pruned; Remaining = %d", res.Remaining)
	}
	headAfter, after := headRecords(ctx, t, h)
	if len(before) != len(after) || headBefore == "" || headAfter == "" {
		t.Fatalf("prune-only reject changed the fact set: before=%d after=%d", len(before), len(after))
	}
	set, err := h.ListProposals(ctx, "repo", "main")
	if err != nil || len(set.Proposals) != 0 {
		t.Fatalf("open set not emptied: %+v, %v", set, err)
	}
}
