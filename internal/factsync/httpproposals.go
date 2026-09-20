package factsync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/entireio/entire-brain/internal/factmerge"
	"github.com/entireio/entire-brain/internal/httpx"
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

// notFound renders a 404 on a single proposal as ErrProposalNotFound, carrying the
// server's account of why when it gave one.
//
// As with conflict(), the SENTINEL is the contract: the resolve loops treat
// errors.Is(err, ErrProposalNotFound) as "another member already settled this", a
// normal outcome they skip past rather than a failure. %w keeps that true while the
// message grows.
func (h *HTTPServer) notFound(resp *http.Response, proposalID string) error {
	if detail := httpx.Redact(httpx.ErrorDetail(resp), h.Token); detail != "" {
		return fmt.Errorf("%w: %s: %s", ErrProposalNotFound, proposalID, detail)
	}
	return fmt.Errorf("%w: %s", ErrProposalNotFound, proposalID)
}

// proposalsPath is the collection endpoint for a repo's open proposal set.
//
// It returns an error rather than a string because the repo id is interpolated into
// the target by concatenation, and repoBasePath refuses an id that is not exactly one
// safe path segment (see its comment in httpserver.go for why escaping is not enough:
// url.PathEscape("..") == ".." and ".../proposals" under a ".." repo id normalizes to
// a different route, reached with the member's bearer token). Every proposal endpoint
// goes through here, so the refusal happens before any request is built.
// It also applies the branch cap (requestTarget), so a branch the hosted API is known
// to refuse never costs a round trip with the member's token attached.
func proposalsPath(repoID, branch string) (string, error) {
	base, err := requestTarget(repoID, branch)
	if err != nil {
		return "", err
	}
	return base + "/brain/facts/proposals", nil
}

// proposalPath is the ONE place a proposal id becomes part of a request target, and
// the reason it returns an error rather than a string: the two endpoints addressed by
// id cannot build a path without handling the refusal, so no call site has to remember
// to check. See validateProposalID for why escaping is not the rule and why a proposal
// id is held to a stricter one than a repo id.
func proposalPath(repoID, branch, proposalID string) (string, error) {
	if err := validateProposalID(proposalID); err != nil {
		return "", err
	}
	// Concatenated raw: a validated id equals its own PathEscape, so escaping here
	// would be a no-op that hides which line is actually doing the work.
	collection, err := proposalsPath(repoID, branch)
	if err != nil {
		return "", err
	}
	return collection + "/" + proposalID, nil
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
	collection, err := proposalsPath(repoID, branch)
	if err != nil {
		return ProposalSet{}, err
	}
	path := collection + "?branch=" + url.QueryEscape(branch)
	req, err := h.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return ProposalSet{}, err
	}
	resp, err := h.do(req)
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
		return ProposalSet{}, fmt.Errorf("%w: GET proposals %s/%s: %s%s", ErrProposalQueueUnsupported, repoID, branch, resp.Status, h.detail(resp))
	default:
		return ProposalSet{}, fmt.Errorf("factsync: GET proposals %s/%s: unexpected status %s%s", repoID, branch, resp.Status, h.detail(resp))
	}
	var out wireProposalSet
	if err := httpx.DecodeJSONBody(resp, &out); err != nil {
		return ProposalSet{}, fmt.Errorf("factsync: decode proposals %s/%s: %w", repoID, branch, err)
	}
	set := ProposalSet{Branch: branch, Ref: out.Ref, Found: out.Found, Proposals: out.Proposals}
	// A server that omits ids (older/looser implementation) still yields addressable
	// proposals: the id is content-derived, so recomputing it is always correct.
	var invalid []int
	for i := range set.Proposals {
		if err := bindProposalID(&set.Proposals[i]); err != nil {
			invalid = append(invalid, i)
		}
	}
	if len(invalid) != 0 {
		return ProposalSet{}, &InvalidProposalSetError{Set: set, Invalid: invalid}
	}
	return set, nil
}

// GetProposal fetches one open proposal by id. A 404, or a 200 with found=false /
// no proposal, is ErrProposalNotFound — the normal outcome when another member
// already settled it.
func (h *HTTPServer) GetProposal(ctx context.Context, repoID, branch, proposalID string) (OpenProposal, error) {
	target, err := proposalPath(repoID, branch, proposalID)
	if err != nil {
		return OpenProposal{}, err
	}
	path := target + "?branch=" + url.QueryEscape(branch)
	req, err := h.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return OpenProposal{}, err
	}
	resp, err := h.do(req)
	if err != nil {
		return OpenProposal{}, fmt.Errorf("factsync: GET proposal %s/%s/%s: %w", repoID, branch, proposalID, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return OpenProposal{}, h.notFound(resp, proposalID)
	default:
		return OpenProposal{}, fmt.Errorf("factsync: GET proposal %s/%s/%s: unexpected status %s%s", repoID, branch, proposalID, resp.Status, h.detail(resp))
	}
	var out wireProposalSet
	if err := httpx.DecodeJSONBody(resp, &out); err != nil {
		return OpenProposal{}, fmt.Errorf("factsync: decode proposal %s/%s/%s: %w", repoID, branch, proposalID, err)
	}
	if !out.Found || out.Proposal == nil {
		return OpenProposal{}, fmt.Errorf("%w: %s", ErrProposalNotFound, proposalID)
	}
	p := *out.Proposal
	if err := bindProposalID(&p); err != nil {
		return OpenProposal{}, fmt.Errorf("factsync: proposal %s/%s/%s: %w", repoID, branch, proposalID, err)
	}
	if p.ID != proposalID {
		return OpenProposal{}, fmt.Errorf("%w: requested %s but server returned %s", ErrProposalIDMismatch, proposalID, p.ID)
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
	collection, err := proposalsPath(repoID, branch)
	if err != nil {
		return "", err
	}
	body := publishProposalsBody{Branch: branch, OldRef: oldRef, Proposals: proposals}
	if body.Proposals == nil {
		body.Proposals = []OpenProposal{}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := h.newRequest(ctx, http.MethodPost, collection, bytes.NewReader(encoded))
	if err != nil {
		return "", err
	}
	resp, err := h.do(req)
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
		if err := httpx.DecodeJSONBody(resp, &out); err != nil {
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
		return "", h.conflict(resp)
	case http.StatusNotFound, http.StatusNotImplemented, http.StatusServiceUnavailable:
		// Same reading as the list path: no queue on this deployment, so the proposals
		// stay local rather than failing a sync that already landed its facts.
		return "", fmt.Errorf("%w: POST proposals %s/%s: %s%s", ErrProposalQueueUnsupported, repoID, branch, resp.Status, h.detail(resp))
	default:
		return "", fmt.Errorf("factsync: POST proposals %s/%s: unexpected status %s%s", repoID, branch, resp.Status, h.detail(resp))
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
	// Build the target first, before the resolution is serialized at all: this body
	// carries the member's whole fact set, so the id that decides where it is POSTed
	// has to be settled before there is anything to send.
	target, err := proposalPath(req.RepoID, req.Branch, req.ProposalID)
	if err != nil {
		return ResolveProposalResponse{}, err
	}
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
	path := target + "/resolve"
	httpReq, err := h.newRequest(ctx, http.MethodPost, path, bytes.NewReader(encoded))
	if err != nil {
		return ResolveProposalResponse{}, err
	}
	resp, err := h.do(httpReq)
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
		if err := httpx.DecodeJSONBody(resp, &out); err != nil {
			return ResolveProposalResponse{}, fmt.Errorf("factsync: decode resolve %s/%s/%s: %w", req.RepoID, req.Branch, req.ProposalID, err)
		}
		changed := true
		if out.Changed != nil {
			changed = *out.Changed
		}
		return ResolveProposalResponse{FactsRef: out.FactsRef, ProposalsRef: out.ProposalsRef, Changed: changed}, nil
	case http.StatusPreconditionFailed, http.StatusConflict:
		return ResolveProposalResponse{}, h.conflict(resp)
	case http.StatusNotFound:
		return ResolveProposalResponse{}, h.notFound(resp, req.ProposalID)
	default:
		return ResolveProposalResponse{}, fmt.Errorf("factsync: POST resolve %s/%s/%s: unexpected status %s%s",
			req.RepoID, req.Branch, req.ProposalID, strings.TrimSpace(resp.Status), h.detail(resp))
	}
}
