package factsync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/ashtom/entire-brain/internal/apiurl"
)

// HTTPServer is the real Server adapter: it drives entire-api's fact-set sync
// endpoints (internal/httpapi/brain_facts.go) over HTTP so the sync runner can talk to
// the hosted head. It is the production implementor of the Server seam that Sync/Resolve
// are written against (the tests use an in-memory fake with identical semantics).
//
// Wire contract (must stay in lockstep with brain_facts.go):
//
//	GET  {BaseURL}/api/v1/repos/{repoID}/brain/facts?branch=… → 200
//	     {found, ref, version, data(base64)}   (pull-gated)
//	POST {BaseURL}/api/v1/repos/{repoID}/brain/facts/advance  → 200
//	     {newRef, version, changed}           (push-gated)
//	     body {branch, oldRef, old_ref, data(base64)}
//	     412/409 → the head advanced concurrently (→ ErrConflict, re-read + re-merge)
//	     changed=false or unchanged:true → the merge changed nothing (→ ErrNoChange, converged)
//	     changed omitted with newRef==oldRef preserves legacy no-op compatibility
//
// Data crosses the wire base64-encoded: huma serializes a Go []byte as a base64 JSON
// string, and encoding/json here does the same on both sides, so the []byte fields match
// byte-for-byte. Authorization is a bearer token (the member's session/runner token).
type HTTPServer struct {
	BaseURL string       // entire-api origin, e.g. https://api.entire.io (no trailing slash needed); must clear apiurl.Validate — https, or http only to loopback
	Token   string       // bearer token; sent as Authorization: Bearer <token> when non-empty
	Client  *http.Client // defaults to http.DefaultClient when nil
}

func (h *HTTPServer) client() *http.Client {
	if h.Client != nil {
		return apiurl.WithoutRedirects(h.Client)
	}
	return apiurl.WithoutRedirects(http.DefaultClient)
}

func (h *HTTPServer) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	// Scheme floor (apiurl.Validate) at the single request chokepoint of both the
	// fact-set and the proposal-queue surfaces: the bearer token below and the
	// fact-set blob above it must never be built into a plaintext request to a
	// non-loopback host. Every construction path lands here, so a caller that
	// bypassed the CLI's own check is still covered.
	base, err := apiurl.Validate(h.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("factsync: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return nil, err
	}
	if h.Token != "" {
		req.Header.Set("Authorization", "Bearer "+h.Token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// Current reads the branch's fact-set head. A 200 with found=false (no head yet) returns
// found=false and nil plaintext; the data field is base64-decoded by encoding/json into
// the []byte. Any non-200 is an error (the runner cannot merge against an unknown state).
func (h *HTTPServer) Current(ctx context.Context, repoID, branch string) (string, []byte, bool, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/brain/facts?branch=%s", url.PathEscape(repoID), url.QueryEscape(branch))
	req, err := h.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", nil, false, err
	}
	resp, err := h.client().Do(req)
	if err != nil {
		return "", nil, false, fmt.Errorf("factsync: GET fact-set head %s/%s: %w", repoID, branch, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", nil, false, fmt.Errorf("factsync: GET fact-set head %s/%s: unexpected status %s", repoID, branch, resp.Status)
	}
	var out struct {
		Found   bool   `json:"found"`
		Ref     string `json:"ref"`
		Version int64  `json:"version"`
		Data    []byte `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", nil, false, fmt.Errorf("factsync: decode fact-set head %s/%s: %w", repoID, branch, err)
	}
	if !out.Found {
		return "", nil, false, nil
	}
	return out.Ref, out.Data, true, nil
}

// Advance compare-and-swaps the head onto plaintext, mapping the endpoint's outcomes to
// the Server contract: 200 with changed → the new ref; 200 with changed=false
// or unchanged:true → ErrNoChange; 412/409 → ErrConflict. Any other status is
// an error (a 400 empty-data, 404 unknown repo, 503 unconfigured, or 5xx —
// none of which the runner should paper over). The duplicated ref spellings
// keep mixed-version entire-api / entire-brain rollouts compatible.
func (h *HTTPServer) Advance(ctx context.Context, repoID, branch, oldRef string, plaintext []byte) (string, error) {
	reqBody := struct {
		Branch       string `json:"branch"`
		OldRef       string `json:"oldRef"`
		OldRefLegacy string `json:"old_ref"`
		Data         []byte `json:"data"`
	}{Branch: branch, OldRef: oldRef, OldRefLegacy: oldRef, Data: plaintext}
	buf, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}
	path := fmt.Sprintf("/api/v1/repos/%s/brain/facts/advance", url.PathEscape(repoID))
	req, err := h.newRequest(ctx, http.MethodPost, path, bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	resp, err := h.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("factsync: POST advance %s/%s: %w", repoID, branch, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var out struct {
			NewRef          string `json:"newRef"`
			NewRefLegacy    string `json:"new_ref"`
			Version         int64  `json:"version"`
			Changed         *bool  `json:"changed"`
			UnchangedLegacy *bool  `json:"unchanged"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return "", fmt.Errorf("factsync: decode advance %s/%s: %w", repoID, branch, err)
		}
		if out.Changed != nil && !*out.Changed {
			return "", ErrNoChange
		}
		if out.UnchangedLegacy != nil && *out.UnchangedLegacy {
			return "", ErrNoChange
		}
		newRef := out.NewRef
		if newRef == "" {
			newRef = out.NewRefLegacy
		}
		if out.Changed == nil && out.UnchangedLegacy == nil && oldRef != "" && newRef == oldRef {
			return "", ErrNoChange
		}
		if newRef == "" {
			return "", fmt.Errorf("factsync: decode advance %s/%s: missing new ref", repoID, branch)
		}
		return newRef, nil
	case http.StatusPreconditionFailed, http.StatusConflict:
		return "", ErrConflict
	default:
		return "", fmt.Errorf("factsync: POST advance %s/%s: unexpected status %s", repoID, branch, resp.Status)
	}
}
