package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ashtom/entire-brain/internal/factmerge"
	"github.com/ashtom/entire-brain/internal/factsync"
)

// hostedProposalsFake is a minimal stand-in for entire-api's fact-set head +
// open-proposal endpoints, just enough to drive the CLI verbs end to end. The
// exhaustive wire/status coverage lives in internal/factsync.
type hostedProposalsFake struct {
	mu        sync.Mutex
	facts     []byte
	proposals []factsync.OpenProposal
	requests  int

	// failPublish, when non-zero, is the status the whole-set publish endpoint
	// returns — used to prove a real share failure fails the sync.
	failPublish int
	// failList, when non-zero, is the status the collection GET returns — an
	// entire-api that predates the queue 404s here first, which is the ONLY
	// shape a sync downgrades to a warning.
	failList int
}

func (f *hostedProposalsFake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

func (f *hostedProposalsFake) openSet() []factsync.OpenProposal {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]factsync.OpenProposal(nil), f.proposals...)
}

func (f *hostedProposalsFake) headFacts(t *testing.T) []factRecord {
	t.Helper()
	f.mu.Lock()
	blob := string(f.facts)
	f.mu.Unlock()
	recs, err := factmerge.ParseNDJSON(strings.NewReader(blob))
	if err != nil {
		t.Fatalf("parse hosted head: %v", err)
	}
	return recs
}

func (f *hostedProposalsFake) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests++
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path

		switch {
		case strings.HasSuffix(path, "/brain/facts/proposals") && r.Method == http.MethodGet:
			if f.failList != 0 {
				w.WriteHeader(f.failList)
				return
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"found": len(f.proposals) > 0, "ref": "props-1", "proposals": f.proposals})

		case strings.HasSuffix(path, "/brain/facts/proposals") && r.Method == http.MethodPost:
			if f.failPublish != 0 {
				w.WriteHeader(f.failPublish)
				return
			}
			var body struct {
				Proposals []factsync.OpenProposal `json:"proposals"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			f.proposals = body.Proposals
			_ = json.NewEncoder(w).Encode(map[string]any{"ref": "props-2", "changed": true})

		case strings.HasSuffix(path, "/brain/facts/advance") && r.Method == http.MethodPost:
			var body struct {
				Data []byte `json:"data"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Data) == 0 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			f.facts = body.Data
			_ = json.NewEncoder(w).Encode(map[string]any{"newRef": "facts-2", "changed": true})

		case strings.HasSuffix(path, "/resolve") && r.Method == http.MethodPost:
			var body struct {
				Data      []byte                  `json:"data"`
				Proposals []factsync.OpenProposal `json:"proposals"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Data) == 0 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			f.facts, f.proposals = body.Data, body.Proposals
			_ = json.NewEncoder(w).Encode(map[string]any{"factsRef": "facts-2", "proposalsRef": "props-2", "changed": true})

		case strings.Contains(path, "/brain/facts/proposals/") && r.Method == http.MethodGet:
			id := path[strings.LastIndex(path, "/")+1:]
			f.mu.Lock()
			defer f.mu.Unlock()
			for _, p := range f.proposals {
				if p.ID == id {
					_ = json.NewEncoder(w).Encode(map[string]any{"found": true, "ref": "props-1", "proposal": p})
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)

		case strings.HasSuffix(path, "/brain/facts") && r.Method == http.MethodGet:
			f.mu.Lock()
			defer f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"found": len(f.facts) > 0, "ref": "facts-1", "data": f.facts})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// newHostedProposalsFixture wires a CLI fixture to a fake hosted brain holding one
// open cross-member contradiction, with the hosted opt-in ENABLED. Tests that assert
// the gates override the env themselves.
func newHostedProposalsFixture(t *testing.T) (verifyFixture, *hostedProposalsFake, factsync.OpenProposal) {
	t.Helper()
	f := newVerifyFixture(t)
	now := f.now

	paths := normalizeFactPaths([]string{"ops.deploy.strategy"})
	target := factRecord{
		ID: factRecordID("deploys use blue-green cutover", paths), Paths: paths,
		Text: "deploys use blue-green cutover", Branch: "main", Origin: factOriginDistilled, Status: factStatusActive,
		Provenance: []factAnchor{{SessionID: "session-A"}}, CreatedAt: now, UpdatedAt: now,
	}
	candidate := factRecord{
		ID: factRecordID("deploys use in-place rolling restart", paths), Paths: paths,
		Text: "deploys use in-place rolling restart", Branch: "main", Origin: factOriginDistilled, Status: factStatusActive,
		Provenance: []factAnchor{{SessionID: "session-B"}}, CreatedAt: now, UpdatedAt: now,
	}
	var blob strings.Builder
	if err := factmerge.WriteNDJSON(&blob, []factRecord{target, candidate}); err != nil {
		t.Fatal(err)
	}
	open := factsync.OpenProposals([]factProposal{{
		Action: factActionSupersede, CandidateID: candidate.ID, TargetID: target.ID,
		Branch: "main", ProposedBy: "member-B",
	}})

	fake := &hostedProposalsFake{facts: []byte(blob.String()), proposals: open}
	ts := httptest.NewServer(fake.handler())
	t.Cleanup(ts.Close)

	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "")
	t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "")
	t.Setenv("ENTIRE_BRAIN_ALLOW_HOSTED", "1")
	t.Setenv("ENTIRE_API_URL", ts.URL)
	t.Setenv("ENTIRE_API_TOKEN", "test-token")
	t.Setenv("ENTIRE_REPO_ID", "repo-01HZZ")
	return f, fake, open[0]
}

// TestFactsProposalsRefusesWithoutGates is the no-implicit-egress table: every
// unsatisfied gate and every missing target field refuses BEFORE any HTTP request.
func TestFactsProposalsRefusesWithoutGates(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		args []string
		want string
	}{
		{name: "opt-in unset", env: map[string]string{"ENTIRE_BRAIN_ALLOW_HOSTED": ""}, want: "hosted_proposals_disabled"},
		{name: "opt-in false", env: map[string]string{"ENTIRE_BRAIN_ALLOW_HOSTED": "false"}, want: "hosted_proposals_disabled"},
		{name: "no-egress wins", env: map[string]string{"ENTIRE_BRAIN_NO_EGRESS": "1"}, want: "no_egress"},
		{name: "local-only wins", env: map[string]string{"ENTIRE_BRAIN_LOCAL_ONLY": "1"}, want: "no_egress"},
		{name: "missing repo id", env: map[string]string{"ENTIRE_REPO_ID": ""}, want: "target repo id is required"},
		{name: "missing api url", env: map[string]string{"ENTIRE_API_URL": ""}, want: "API base URL is required"},
		{name: "missing token", env: map[string]string{"ENTIRE_API_TOKEN": ""}, want: "API token is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, fake, proposal := newHostedProposalsFixture(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			for _, args := range [][]string{
				{"facts", "proposals", "list"},
				{"facts", "proposals", "show", proposal.ID},
				{"facts", "proposals", "apply", proposal.ID},
				{"facts", "proposals", "reject", proposal.ID},
			} {
				out, err := execute(t, NewRootCommand(f.opts), args...)
				if err == nil {
					t.Fatalf("%v succeeded despite %s\n%s", args, tc.name, out)
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("%v error = %v; want %q", args, err, tc.want)
				}
			}
			if n := fake.count(); n != 0 {
				t.Fatalf("refused commands still made %d HTTP request(s)", n)
			}
			// The open set is untouched: nothing was resolved.
			if len(fake.openSet()) != 1 {
				t.Fatalf("open set changed under a refusal: %+v", fake.openSet())
			}
		})
	}
}

func TestFactsProposalsList(t *testing.T) {
	f, _, proposal := newHostedProposalsFixture(t)

	out, err := execute(t, NewRootCommand(f.opts), "facts", "proposals", "list")
	if err != nil {
		t.Fatalf("facts proposals list: %v\n%s", err, out)
	}
	for _, want := range []string{"1 open cross-member proposal(s) on main", proposal.ID, "supersede", "by member-B"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list output missing %q:\n%s", want, out)
		}
	}

	out, err = execute(t, NewRootCommand(f.opts), "facts", "proposals", "list", "--json")
	if err != nil {
		t.Fatalf("facts proposals list --json: %v\n%s", err, out)
	}
	var report struct {
		Branch    string                  `json:"branch"`
		RepoID    string                  `json:"repo_id"`
		Open      int                     `json:"open"`
		Proposals []factsync.OpenProposal `json:"proposals"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse list json: %v\n%s", err, out)
	}
	if report.Branch != "main" || report.RepoID != "repo-01HZZ" || report.Open != 1 {
		t.Fatalf("list json = %+v", report)
	}
	if len(report.Proposals) != 1 || report.Proposals[0].ID != proposal.ID {
		t.Fatalf("list json proposals = %+v", report.Proposals)
	}
}

func TestFactsProposalsListEmpty(t *testing.T) {
	f, fake, _ := newHostedProposalsFixture(t)
	fake.mu.Lock()
	fake.proposals = nil
	fake.mu.Unlock()

	out, err := execute(t, NewRootCommand(f.opts), "facts", "proposals", "list")
	if err != nil {
		t.Fatalf("facts proposals list: %v\n%s", err, out)
	}
	if !strings.Contains(out, "no open cross-member proposals on main") {
		t.Fatalf("empty list output = %q", out)
	}
}

func TestFactsProposalsShow(t *testing.T) {
	f, _, proposal := newHostedProposalsFixture(t)

	for _, tc := range []struct {
		name string
		ref  string
	}{
		{name: "exact id", ref: proposal.ID},
		{name: "id prefix", ref: proposal.ID[:10]},
		{name: "candidate fact id", ref: proposal.Proposal.CandidateID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := execute(t, NewRootCommand(f.opts), "facts", "proposals", "show", tc.ref)
			if err != nil {
				t.Fatalf("show %s: %v\n%s", tc.ref, err, out)
			}
			for _, want := range []string{proposal.ID, "action:     supersede", "proposedBy: member-B", proposal.Proposal.TargetID} {
				if !strings.Contains(out, want) {
					t.Fatalf("show output missing %q:\n%s", want, out)
				}
			}
		})
	}

	t.Run("unknown reference", func(t *testing.T) {
		out, err := execute(t, NewRootCommand(f.opts), "facts", "proposals", "show", "prop-nope")
		if err == nil {
			t.Fatalf("show of an unknown proposal succeeded:\n%s", out)
		}
		if !strings.Contains(err.Error(), "no such open proposal") {
			t.Fatalf("show error = %v", err)
		}
	})

	t.Run("json", func(t *testing.T) {
		out, err := execute(t, NewRootCommand(f.opts), "facts", "proposals", "show", proposal.ID, "--json")
		if err != nil {
			t.Fatalf("show --json: %v\n%s", err, out)
		}
		var report struct {
			Proposal factsync.OpenProposal `json:"proposal"`
		}
		if err := json.Unmarshal([]byte(out), &report); err != nil {
			t.Fatalf("parse show json: %v\n%s", err, out)
		}
		if report.Proposal.ID != proposal.ID || report.Proposal.Proposal.ProposedBy != "member-B" {
			t.Fatalf("show json = %+v", report.Proposal)
		}
	})
}

// TestFactsProposalsApplyConverges drives the whole user-facing loop against the fake
// hosted brain: list → apply → the shared head converges (loser retained as
// superseded, winner active) and the queue empties.
func TestFactsProposalsApplyConverges(t *testing.T) {
	f, fake, proposal := newHostedProposalsFixture(t)

	out, err := execute(t, NewRootCommand(f.opts), "facts", "proposals", "apply", proposal.ID)
	if err != nil {
		t.Fatalf("facts proposals apply: %v\n%s", err, out)
	}
	for _, want := range []string{"applied " + proposal.ID, "facts head: facts-2", "0 open proposal(s) remain"} {
		if !strings.Contains(out, want) {
			t.Fatalf("apply output missing %q:\n%s", want, out)
		}
	}
	if len(fake.openSet()) != 0 {
		t.Fatalf("open set after apply = %+v; want empty", fake.openSet())
	}

	byID := map[string]factRecord{}
	for _, r := range fake.headFacts(t) {
		byID[r.ID] = r
	}
	loser := byID[proposal.Proposal.TargetID]
	if loser.Status != factStatusSuperseded || loser.SupersededBy != proposal.Proposal.CandidateID {
		t.Fatalf("target = %q superseded_by %q; want superseded by the candidate", loser.Status, loser.SupersededBy)
	}
	if byID[proposal.Proposal.CandidateID].Status != factStatusActive {
		t.Fatalf("candidate = %q; want active", byID[proposal.Proposal.CandidateID].Status)
	}

	// The settled proposal is gone for everyone; re-applying reports it as such.
	out, err = execute(t, NewRootCommand(f.opts), "facts", "proposals", "apply", proposal.ID)
	if err == nil {
		t.Fatalf("re-applying a settled proposal succeeded:\n%s", out)
	}
	if !strings.Contains(err.Error(), "no such open proposal") {
		t.Fatalf("re-apply error = %v", err)
	}
}

func TestFactsProposalsRejectKeepsBothActive(t *testing.T) {
	f, fake, proposal := newHostedProposalsFixture(t)

	out, err := execute(t, NewRootCommand(f.opts), "facts", "proposals", "reject", proposal.Proposal.CandidateID, "--json")
	if err != nil {
		t.Fatalf("facts proposals reject: %v\n%s", err, out)
	}
	var report struct {
		Resolved factsync.ResolveOpenResult `json:"resolved"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse reject json: %v\n%s", err, out)
	}
	if report.Resolved.Decision != "reject" || report.Resolved.Remaining != 0 || report.Resolved.Proposal.ID != proposal.ID {
		t.Fatalf("reject json = %+v", report.Resolved)
	}

	facts := fake.headFacts(t)
	if len(facts) != 2 {
		t.Fatalf("reject head = %d facts; want 2", len(facts))
	}
	for _, r := range facts {
		if r.Status != factStatusActive {
			t.Fatalf("reject must keep both active; %s = %q", r.ID, r.Status)
		}
		for _, related := range r.RelatedIDs {
			if related == proposal.Proposal.CandidateID || related == proposal.Proposal.TargetID {
				t.Fatalf("reject left the conflict cross-link on %s: %v", r.ID, r.RelatedIDs)
			}
		}
	}
}

// TestFactsProposalsBranchFlag proves --branch overrides the repo's current branch on
// the wire (the fixture repo reports "main").
func TestFactsProposalsBranchFlag(t *testing.T) {
	f, _, _ := newHostedProposalsFixture(t)
	out, err := execute(t, NewRootCommand(f.opts), "facts", "proposals", "list", "--branch", "release", "--json")
	if err != nil {
		t.Fatalf("list --branch: %v\n%s", err, out)
	}
	var report struct {
		Branch string `json:"branch"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse json: %v\n%s", err, out)
	}
	if report.Branch != "release" {
		t.Fatalf("branch = %q; want release", report.Branch)
	}
}

// TestFactsProposalsRegisteredUnderFacts pins the command surface: the verbs are
// discoverable under `facts`, so the hosted review flow is reachable from help.
func TestFactsProposalsRegisteredUnderFacts(t *testing.T) {
	out, err := execute(t, NewRootCommand(Options{Version: "test"}), "facts", "proposals", "--help")
	if err != nil {
		t.Fatalf("facts proposals --help: %v\n%s", err, out)
	}
	for _, want := range []string{"list", "show", "apply", "reject", envBrainAllowHosted} {
		if !strings.Contains(out, want) {
			t.Fatalf("help missing %q:\n%s", want, out)
		}
	}
}
