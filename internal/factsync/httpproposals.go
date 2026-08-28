package factsync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/ashtom/entire-brain/internal/factmerge"
)

// HTTPServer also drives the OPEN-PROPOSAL SET endpoints, symmetric with the fact-set
// head endpoints in httpserver.go:
//
//	GET  {BaseURL}/api/v1/repos/{repoID}/brain/facts/proposals?branch=…       → 200
//	     {found, ref, version, proposals:[{id, proposal}]}      (pull-gated)
//	GET  {BaseURL}/api/v1/repos/{repoID}/brain/facts/proposals/{id}?branch=…  → 200
//	     {found, ref, version, proposal:{id, proposal}}         (pull-gated)
//	     404 or found=false → ErrProposalNotFound (already settled / never existed)
//	POST {BaseURL}/api/v1/repos/{repoID}/brain/facts/proposals                → 200
//	     {ref, version, changed}                                (push-gated)
//	     body {branch, oldRef, proposals}                       (whole-set CAS)
//	     412/409 → the set advanced concurrently (→ ErrConflict, re-read + re-union)
//	POST {BaseURL}/api/v1/repos/{repoID}/brain/facts/proposals/{id}/resolve   → 200
//	     {factsRef, proposalsRef, changed}                      (push-gated)
//	     body {branch, decision, factsOldRef, data(base64), proposalsOldRef, proposals}
//	     412/409 → a head advanced concurrently (→ ErrConflict, re-read + re-resolve)
//	     404     → the proposal is no longer open (→ ErrProposalNotFound)
//
// The resolve body carries the runner-computed result of the merge, never an
// instruction to merge: `data` is the full resolved fact set (NDJSON, base64 as
// huma/encoding/json encode a []byte) and `proposals` is the full open set minus the
// resolved one. The server compare-and-swaps both from the supplied old refs. The
// `decision` field is informational (audit/telemetry) — the server does not act on it.
//
// Egress redaction is applied HERE, at the transport chokepoint, exactly as Sync
// applies it before pushing merged facts: SanitizeForEgress strips every anchor's
// local transcript path and line offset, so a resolution can never leak a member's
// filesystem layout even if a caller hands the client unsanitized records.

// proposalsPath is the collection endpoint for a repo's open proposal set.
func proposalsPath(repoID string) string {
	return fmt.Sprintf("/api/v1/repos/%s/brain/facts/proposals", url.PathEscape(repoID))
}

// wireProposalSet is the list/get response envelope.
type wireProposalSet struct {
	Found     bool           `json:"found"`
	Ref       string         `json:"ref"`
	Version   int64          `json:"version"`
	Proposals []OpenProposal `json:"proposals"`
	Proposal  *OpenProposal  `json:"proposal"`
}

// ListProposals reads the branch's open proposal set. found=false (no set stored yet)
// is an EMPTY set, not an error — a branch with no cross-member conflict has nothing
// to review. Any non-200 is an error: the caller must not resolve against an unknown
// open set.
func (h *HTTPServer) ListProposals(ctx context.Context, repoID, branch string) (ProposalSet, error) {
	path := proposalsPath(repoID) + "?branch=" + url.QueryEscape(branch)
	req, err := h.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return ProposalSet{}, err
	}
	resp, err := h.client().Do(req)
	if err != nil {
		return ProposalSet{}, fmt.Errorf("factsync: GET proposals %s/%s: %w", repoID, branch, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusNotImplemented, http.StatusServiceUnavailable:
		// 503 is "this deployment has no queue", not a transient outage to retry:
		// entire-api answers it whenever the hosted brain is disabled, the same way its
		// fact-set endpoints do. Treating it as unsupported degrades to the local review
		// queue with a warning, which is right for an optional sub-feature — and the
		// next sync re-checks, so a genuinely transient 503 costs one cycle, never a
		// failed sync whose head-advance already succeeded.
		return ProposalSet{}, fmt.Errorf("%w: GET proposals %s/%s: %s", ErrProposalQueueUnsupported, repoID, branch, resp.Status)
	default:
		return ProposalSet{}, fmt.Errorf("factsync: GET proposals %s/%s: unexpected status %s", repoID, branch, resp.Status)
	}
	var out wireProposalSet
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ProposalSet{}, fmt.Errorf("factsync: decode proposals %s/%s: %w", repoID, branch, err)
	}
	set := ProposalSet{Branch: branch, Ref: out.Ref, Found: out.Found, Proposals: out.Proposals}
	// A server that omits ids (older/looser implementation) still yields addressable
	// proposals: the id is content-derived, so recomputing it is always correct.
	for i := range set.Proposals {
		if err := bindProposalID(&set.Proposals[i]); err != nil {
			return ProposalSet{}, fmt.Errorf("factsync: proposals %s/%s: %w", repoID, branch, err)
		}
	}
	return set, nil
}

// GetProposal fetches one open proposal by id. A 404, or a 200 with found=false /
// no proposal, is ErrProposalNotFound — the normal outcome when another member
// already settled it.
func (h *HTTPServer) GetProposal(ctx context.Context, repoID, branch, proposalID string) (OpenProposal, error) {
	path := proposalsPath(repoID) + "/" + url.PathEscape(proposalID) + "?branch=" + url.QueryEscape(branch)
	req, err := h.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return OpenProposal{}, err
	}
	resp, err := h.client().Do(req)
	if err != nil {
		return OpenProposal{}, fmt.Errorf("factsync: GET proposal %s/%s/%s: %w", repoID, branch, proposalID, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return OpenProposal{}, fmt.Errorf("%w: %s", ErrProposalNotFound, proposalID)
	default:
		return OpenProposal{}, fmt.Errorf("factsync: GET proposal %s/%s/%s: unexpected status %s", repoID, branch, proposalID, resp.Status)
	}
	var out wireProposalSet
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return OpenProposal{}, fmt.Errorf("factsync: decode proposal %s/%s/%s: %w", repoID, branch, proposalID, err)
	}
	if !out.Found || out.Proposal == nil {
		return OpenProposal{}, fmt.Errorf("%w: %s", ErrProposalNotFound, proposalID)
	}
	p := *out.Proposal
	if err := bindProposalID(&p); err != nil {
		return OpenProposal{}, fmt.Errorf("factsync: proposal %s/%s/%s: %w", repoID, branch, proposalID, err)
	}
	return p, nil
}

// publishRequestBody is the POST body for a whole-set publish.
type publishProposalsBody struct {
	Branch    string         `json:"branch"`
	OldRef    string         `json:"oldRef"`
	Proposals []OpenProposal `json:"proposals"`
}

// PublishProposals compare-and-swaps the branch's whole open proposal set from oldRef
// (empty when the branch has no set yet) onto the supplied list, returning the new ref.
// 412/409 → ErrConflict (another member changed the set first); a 200 reporting
// changed=false → ErrNoChange, mirroring Advance's contract on the fact-set head.
func (h *HTTPServer) PublishProposals(ctx context.Context, repoID, branch, oldRef string, proposals []OpenProposal) (string, error) {
	body := publishProposalsBody{Branch: branch, OldRef: oldRef, Proposals: proposals}
	if body.Proposals == nil {
		body.Proposals = []OpenProposal{}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := h.newRequest(ctx, http.MethodPost, proposalsPath(repoID), bytes.NewReader(encoded))
	if err != nil {
		return "", err
	}
	resp, err := h.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("factsync: POST proposals %s/%s: %w", repoID, branch, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var out struct {
			Ref     string `json:"ref"`
			Version int64  `json:"version"`
			Changed *bool  `json:"changed"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return "", fmt.Errorf("factsync: decode publish proposals %s/%s: %w", repoID, branch, err)
		}
		if out.Changed != nil && !*out.Changed {
			return "", ErrNoChange
		}
		if out.Ref == "" {
			return "", fmt.Errorf("factsync: decode publish proposals %s/%s: missing ref", repoID, branch)
		}
		return out.Ref, nil
	case http.StatusPreconditionFailed, http.StatusConflict:
		return "", ErrConflict
	case http.StatusNotFound, http.StatusNotImplemented, http.StatusServiceUnavailable:
		// Same reading as the list path: no queue on this deployment, so the proposals
		// stay local rather than failing a sync that already landed its facts.
		return "", fmt.Errorf("%w: POST proposals %s/%s: %s", ErrProposalQueueUnsupported, repoID, branch, resp.Status)
	default:
		return "", fmt.Errorf("factsync: POST proposals %s/%s: unexpected status %s", repoID, branch, resp.Status)
	}
}

// resolveRequestBody is the POST body for a resolution.
type resolveRequestBody struct {
	Branch      string `json:"branch"`
	Decision    string `json:"decision"`
	FactsOldRef string `json:"factsOldRef"`
	Data        []byte `json:"data"`
	// FactsUnchanged tells the server to CAS the proposal set only and leave the
	// fact head at factsOldRef. Sent only with decision=reject, to prune a proposal
	// whose fact head is gone; data is then empty and must NOT be read as "empty the
	// fact set". Omitted on every normal resolution, so an older server that ignores
	// the field still sees exactly the previous request shape.
	FactsUnchanged  bool           `json:"factsUnchanged,omitempty"`
	ProposalsOldRef string         `json:"proposalsOldRef"`
	Proposals       []OpenProposal `json:"proposals"`
}

// ResolveProposal pushes a runner-computed resolution: the resolved fact set and the
// remaining open proposals, each compare-and-swapped from the ref the runner read.
// Status mapping mirrors Advance — 200 → the new refs, 412/409 → ErrConflict, 404 →
// ErrProposalNotFound — and any other status is an error the runner must not paper
// over. changed=false is reported as-is (the caller keeps its refs); it is not an
// error, because a re-pushed identical resolution is idempotent.
func (h *HTTPServer) ResolveProposal(ctx context.Context, req ResolveProposalRequest) (ResolveProposalResponse, error) {
	// Redact local-only provenance coordinates before the facts leave this member —
	// the same guarantee Sync makes on its merged blob (see egress.go).
	var buf bytes.Buffer
	if !req.FactsUnchanged {
		if err := factmerge.WriteNDJSON(&buf, SanitizeForEgress(req.Facts)); err != nil {
			return ResolveProposalResponse{}, err
		}
		if buf.Len() == 0 {
			// A resolution never empties the fact set (supersede retains the loser, reject
			// keeps both, merge unions into the target), so an empty blob means the caller
			// computed something wrong — and the head endpoint would 400 it anyway.
			return ResolveProposalResponse{}, fmt.Errorf("factsync: resolve %s: refusing to push an empty fact set", req.ProposalID)
		}
	} else if req.Decision != Reject {
		// factsUnchanged exists to prune an orphan; only reject leaves facts alone.
		// Guard it here too so a future caller cannot quietly skip a fact write on an
		// accept, which would drop the merge result while still retiring the proposal.
		return ResolveProposalResponse{}, fmt.Errorf("factsync: resolve %s: factsUnchanged is only valid with reject", req.ProposalID)
	}
	body := resolveRequestBody{
		Branch:          req.Branch,
		Decision:        req.Decision.String(),
		FactsOldRef:     req.FactsOldRef,
		Data:            buf.Bytes(),
		FactsUnchanged:  req.FactsUnchanged,
		ProposalsOldRef: req.ProposalsOldRef,
		Proposals:       req.Remaining,
	}
	if body.Proposals == nil {
		// An explicit empty list, never JSON null: the last resolution CLEARS the set.
		body.Proposals = []OpenProposal{}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return ResolveProposalResponse{}, err
	}
	path := proposalsPath(req.RepoID) + "/" + url.PathEscape(req.ProposalID) + "/resolve"
	httpReq, err := h.newRequest(ctx, http.MethodPost, path, bytes.NewReader(encoded))
	if err != nil {
		return ResolveProposalResponse{}, err
	}
	resp, err := h.client().Do(httpReq)
	if err != nil {
		return ResolveProposalResponse{}, fmt.Errorf("factsync: POST resolve %s/%s/%s: %w", req.RepoID, req.Branch, req.ProposalID, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var out struct {
			FactsRef     string `json:"factsRef"`
			ProposalsRef string `json:"proposalsRef"`
			Changed      *bool  `json:"changed"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return ResolveProposalResponse{}, fmt.Errorf("factsync: decode resolve %s/%s/%s: %w", req.RepoID, req.Branch, req.ProposalID, err)
		}
		changed := true
		if out.Changed != nil {
			changed = *out.Changed
		}
		return ResolveProposalResponse{FactsRef: out.FactsRef, ProposalsRef: out.ProposalsRef, Changed: changed}, nil
	case http.StatusPreconditionFailed, http.StatusConflict:
		return ResolveProposalResponse{}, ErrConflict
	case http.StatusNotFound:
		return ResolveProposalResponse{}, fmt.Errorf("%w: %s", ErrProposalNotFound, req.ProposalID)
	default:
		return ResolveProposalResponse{}, fmt.Errorf("factsync: POST resolve %s/%s/%s: unexpected status %s",
			req.RepoID, req.Branch, req.ProposalID, strings.TrimSpace(resp.Status))
	}
}
