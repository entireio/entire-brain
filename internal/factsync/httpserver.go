package factsync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ashtom/entire-brain/internal/apiurl"
	"github.com/ashtom/entire-brain/internal/httpx"
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
	Client  *http.Client // defaults to the bounded shared upload client when nil
}

// syncRequestTimeout bounds one fact-set sync request end to end.
//
// The sync runner is driven from the CLI and the workspace daemon with
// context.Background(), so with no caller deadline this client's own bound is the
// only thing standing between a wedged hosted endpoint — one that completes the dial
// and then never responds — and a permanently hung process; http.DefaultClient has
// no timeout at all. http.Client.Timeout covers the whole exchange, which is what a
// stall after a successful dial needs.
//
// Advance uploads the whole merged fact-set, so the bound matches the five minutes
// the hosted publish path already allows for a body-carrying request
// (cli.publishRequestTimeout) rather than a short read timeout.
//
// It is a var, not a const, so tests can shorten it.
var syncRequestTimeout = 5 * time.Minute

func (h *HTTPServer) client() *http.Client {
	if h.Client != nil {
		return apiurl.WithoutRedirects(h.Client)
	}
	// The phase bounds (dial, TLS handshake, response header) come from the shared
	// bounded transport; syncRequestTimeout is this caller's own end-to-end bound.
	// A fresh Client per call is free — the transport, and so the connection pool,
	// is shared.
	return apiurl.WithoutRedirects(httpx.UploadClient(syncRequestTimeout))
}

// syncOperationTimeout is the stated ceiling for ONE do() call, retries included.
//
// syncRequestTimeout bounds a single http.Client.Do. do() may issue several, so
// without an operation-wide deadline the two compose by MULTIPLICATION and a caller
// reading syncRequestTimeout gets a number that is wrong by the attempt count:
//
//	attempts         = 1 + transientDialRetries        = 3
//	per attempt      = syncRequestTimeout              = 5m
//	inter-attempt    = 2 x transientDialRetryDelay     = 40ms
//	naive worst case = 3 x 5m + 40ms                   = 15m0.04s
//	enforced ceiling = syncOperationTimeout            = 5m
//
// In practice retries are cheap: they fire only on a LOCAL dial failure that
// returns in microseconds (EADDRNOTAVAIL, see isTransientLocalDialError), so they
// spend ~40ms of the budget rather than a second and third five-minute wait. The
// deadline turns that from an expectation into a guarantee -- whatever the remote
// does, one fact-set request costs its caller at most syncOperationTimeout, and the
// retry budget can never widen it.
//
// It must stay >= syncRequestTimeout (a single healthy slow request has to be able
// to finish) and < the naive worst case (or it caps nothing). Both are asserted.
//
// It is a var, not a const, so tests can shorten it.
var syncOperationTimeout = 5 * time.Minute

// transientDialRetries bounds how many times do() will redial after a local
// dial failure before giving up and returning it to the caller.
const transientDialRetries = 2

// transientDialRetryDelay is the fixed pause between redial attempts. It only
// needs to outlast a momentary local resource crunch (see
// isTransientLocalDialError), not model network RTT or server load, so a flat
// short delay is enough — this is not a server-side backoff policy.
const transientDialRetryDelay = 20 * time.Millisecond

// do issues req, transparently redialing up to transientDialRetries times on
// a TRANSIENT LOCAL dial failure — one where net/http never got past
// establishing the TCP connection, so nothing reached the peer and a retry
// can never double up a mutation. client() refuses redirects, so a later
// redirect dial cannot disguise an already-applied POST as a local failure.
// POST bodies are re-armed via req.GetBody,
// which http.NewRequestWithContext populates automatically for the
// bytes.Reader bodies every factsync request uses.
//
// The failure this exists for is "dial tcp ...: connect: cannot assign
// requested address" (EADDRNOTAVAIL): the LOCAL ephemeral port range or
// connection table is briefly exhausted, which has nothing to do with the
// remote host's health and clears itself in milliseconds once some of the
// host's own recently-closed sockets leave TIME_WAIT. A single member's
// laptop can hit this under its own load; a shared CI runner running more
// than one job's test suite at once hits it far more easily, since ALL of
// those jobs draw from the same finite local port table. Before this fix,
// the very first such blip failed the whole sync/proposal round-trip outright
// — a client with zero tolerance for a purely local, self-resolving hiccup.
// Anything else (refused, no route, DNS failure, TLS failure) is a real
// connectivity problem and is returned immediately, unretried.
func (h *HTTPServer) do(req *http.Request) (*http.Response, error) {
	// One deadline for the whole operation, so the retry budget and the per-request
	// bound compose by MIN rather than by multiplication. See syncOperationTimeout
	// for the arithmetic this replaces.
	ctx, cancel := context.WithTimeout(req.Context(), syncOperationTimeout)
	req = req.WithContext(ctx)

	var lastErr error
	for attempt := 0; attempt <= transientDialRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				cancel()
				return nil, ctx.Err()
			case <-time.After(transientDialRetryDelay):
			}
			if req.GetBody != nil {
				body, err := req.GetBody()
				if err != nil {
					cancel()
					return nil, err
				}
				req.Body = body
			}
		}
		resp, err := h.client().Do(req)
		if err == nil {
			// The deadline has to outlive do(): http.Client.Timeout covers the
			// body read, so the operation budget must too, and cancelling here
			// would break every caller that reads resp.Body afterwards. Hand the
			// cancel to the body instead -- every factsync call site closes it.
			resp.Body = &cancelOnCloseBody{ReadCloser: resp.Body, cancel: cancel}
			return resp, nil
		}
		if !isTransientLocalDialError(err) {
			cancel()
			return nil, err
		}
		lastErr = err
	}
	cancel()
	return nil, lastErr
}

// cancelOnCloseBody releases the operation context when the caller closes the
// response body, which is the point the request is genuinely finished.
type cancelOnCloseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelOnCloseBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// isTransientLocalDialError reports whether err is a dial failure that never
// reached the peer and is known to self-resolve: local ephemeral-port/
// connection-table exhaustion. Matched by message rather than a syscall errno
// so this stays correct across platforms (Windows reports the same condition
// under a different errno) without a build-tagged file.
func isTransientLocalDialError(err error) bool {
	var opErr *net.OpError
	if !errors.As(err, &opErr) || opErr.Op != "dial" || opErr.Err == nil {
		return false
	}
	return strings.Contains(opErr.Err.Error(), "assign requested address")
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
	resp, err := h.do(req)
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
	resp, err := h.do(req)
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
