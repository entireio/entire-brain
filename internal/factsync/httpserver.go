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

	"github.com/entireio/entire-brain/internal/apiurl"
	"github.com/entireio/entire-brain/internal/httpx"
	"github.com/entireio/entire-brain/internal/repoid"
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

// syncRequestTimeout bounds one HTTP exchange, including a full fact-set upload.
// Tests may shorten it; the default matches the hosted publish upload budget.
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

// syncOperationTimeout bounds the complete operation, including retries and
// response-body consumption. It must allow one syncRequestTimeout exchange.
// Tests may shorten it.
var syncOperationTimeout = 5 * time.Minute

// transientDialRetries bounds how many times do() will redial after a local
// dial failure before giving up and returning it to the caller.
const transientDialRetries = 2

// transientDialRetryDelay is the fixed pause between redial attempts. It only
// needs to outlast a momentary local resource crunch (see
// isTransientLocalDialError), not model network RTT or server load, so a flat
// short delay is enough — this is not a server-side backoff policy.
const transientDialRetryDelay = 20 * time.Millisecond

// do retries transient local dial failures that occur before any request reaches
// the peer. Redirects are disabled, and GetBody rearms upload bodies. Other
// connectivity errors return immediately; retries share one operation deadline.
func (h *HTTPServer) do(req *http.Request) (*http.Response, error) {
	// Retries and body consumption share the caller-bounded deadline.
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

// repoBasePath is the single point at which a repo id becomes part of a request
// target in this package. Every fact-set and proposal endpoint below is built on top
// of it, so the segment rule is applied once rather than once per endpoint.
//
// The id is interpolated by concatenation, so it has to be exactly one safe path
// segment: a "/", a "?" or a dot segment would otherwise re-address the request while
// newRequest attaches the member's bearer token to it. Escaping does not achieve
// that — "." and ".." are RFC 3986 unreserved, so url.PathEscape("..") == "..", and
//
//	/api/v1/repos/../brain/facts
//
// collapses at any normalizing hop to /api/v1/brain/facts, a route the caller never
// asked for. repoid.Validate states the rule instead; because it also requires the id
// to equal its own escaped form, the id is concatenated raw here — PathEscape would
// be a no-op and hiding the check behind it is what made this look safe before.
//
// Returning an error (rather than a string) is deliberate: it forces every call site
// to refuse BEFORE http.NewRequest exists, so a rejected id costs zero egress.
func repoBasePath(repoID string) (string, error) {
	if err := repoid.Validate(repoID); err != nil {
		return "", fmt.Errorf("factsync: %w", err)
	}
	return "/api/v1/repos/" + repoID, nil
}

// ErrBranchTooLong is returned when the caller's branch name exceeds the hosted
// fact-set API's cap. It is a distinct sentinel so a caller can tell a client-side
// refusal (nothing was sent; fix the branch) from a server rejection.
var ErrBranchTooLong = errors.New("factsync: branch name is too long for the hosted fact-set API")

// maxBranchBytes is the hosted fact-set API's cap on a branch name, in BYTES as the
// server counts them (not runes — a multi-byte name is longer on the wire than it
// looks on screen).
//
// This is the ONE server cap this client mirrors, and it is worth saying why, because
// mirroring a server-owned number is normally a bug waiting to happen: if the hosted
// cap is raised, a client that hardcoded the old one refuses branches the server would
// now accept, and no amount of error-body surfacing helps because the request is never
// made. Two things make this cap the exception:
//
//   - The round trip it saves is expensive and pointless. The branch is known before
//     a socket is opened, and the request that would learn the limit carries the
//     member's bearer token and, on Advance/Resolve, the member's whole fact set — a
//     multi-megabyte upload posted only to be told the branch name was too long.
//   - The number is not close to anything real. Git states no length limit of its own
//     on a ref name — the effective one is the filesystem's — and a branch anywhere
//     near 512 bytes is pathological rather than merely long, so nothing a member
//     would plausibly name a branch sits near this value.
//
// The residual risk is stated rather than hidden: if the hosted cap is RAISED, this
// client refuses a branch the server would now accept, and it cannot detect that on
// its own, because the request it would have learned from is the one it declined to
// make. Raising this constant is then the fix, and it is one line. That risk is
// accepted only because the value is far outside the range of real branch names; it is
// why no other server-owned cap is mirrored here.
//
// The other caps the hosted API grew at the same time are deliberately NOT mirrored:
// the artifact count and the manifest reference count on the publish path, and
// whatever the hosted MCP query surface applies to its own branch argument. The client
// has no independent basis for those numbers (unlike the publish body-size ceiling,
// which it must project anyway to avoid building a multi-GB buffer), and the hosted
// MCP branch is server-defaulted, so a guessed constant there would refuse requests
// the server accepts. Those are surfaced from the server's own error body instead —
// see internal/cli/publish.go and internal/hostedbrain/client.go.
const maxBranchBytes = 512

// validateBranch refuses a branch the hosted API is known to reject, before the
// request exists.
func validateBranch(branch string) error {
	if len(branch) > maxBranchBytes {
		return fmt.Errorf("%w: branch is %d bytes, limit is %d", ErrBranchTooLong, len(branch), maxBranchBytes)
	}
	return nil
}

// detail renders the server's objection with this client's own bearer token removed.
// Every non-2xx render on the fact-set and proposal surfaces goes through here, so
// the redaction is applied once rather than at each of the ten call sites. See
// httpx.Redact for why an echoed token has to be stripped even though the peer that
// echoed it already had it.
func (h *HTTPServer) detail(resp *http.Response) string {
	return httpx.Redact(httpx.ErrorSuffix(resp), h.Token)
}

// conflict renders a 412/409 as ErrConflict, carrying whatever the server said about
// the head that moved.
//
// The SENTINEL is the contract: Sync and the proposal loops re-read and re-merge on
// errors.Is(err, ErrConflict), so a CAS loss must keep matching it no matter how the
// message grows. fmt.Errorf with %w preserves that; a plain formatted error would
// silently turn a converging retry into a failed sync. When the server said nothing
// the bare sentinel is returned unchanged, so a quiet 412 reads exactly as it did
// before this file learned to read bodies.
func (h *HTTPServer) conflict(resp *http.Response) error {
	if detail := httpx.Redact(httpx.ErrorDetail(resp), h.Token); detail != "" {
		return fmt.Errorf("%w: %s", ErrConflict, detail)
	}
	return ErrConflict
}

// requestTarget is the single chokepoint for the two caller-supplied values that
// decide whether a fact-set request can possibly succeed: the repo id, which must be
// exactly one safe path segment (repoBasePath), and the branch, which must be within
// the hosted cap. Both are checked before http.NewRequest exists, so a request that
// cannot succeed costs zero egress.
func requestTarget(repoID, branch string) (string, error) {
	// Repo id first: it is the check that keeps the request on the route the caller
	// asked for, so when both values are bad that is the one worth reporting.
	base, err := repoBasePath(repoID)
	if err != nil {
		return "", err
	}
	if err := validateBranch(branch); err != nil {
		return "", err
	}
	return base, nil
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
	base, err := requestTarget(repoID, branch)
	if err != nil {
		return "", nil, false, err
	}
	path := base + "/brain/facts?branch=" + url.QueryEscape(branch)
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
		return "", nil, false, fmt.Errorf("factsync: GET fact-set head %s/%s: unexpected status %s%s", repoID, branch, resp.Status, h.detail(resp))
	}
	var out struct {
		Found   bool   `json:"found"`
		Ref     string `json:"ref"`
		Version int64  `json:"version"`
		Data    []byte `json:"data"`
	}
	if err := httpx.DecodeJSONBody(resp, &out); err != nil {
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
	base, err := requestTarget(repoID, branch)
	if err != nil {
		return "", err
	}
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

	path := base + "/brain/facts/advance"
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
		if err := httpx.DecodeJSONBody(resp, &out); err != nil {
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
		// The sentinel is the contract (sync.go re-reads and re-merges on
		// errors.Is(err, ErrConflict)); the server's account of WHICH head moved is
		// additive, and only when it said something.
		return "", h.conflict(resp)
	default:
		return "", fmt.Errorf("factsync: POST advance %s/%s: unexpected status %s%s", repoID, branch, resp.Status, h.detail(resp))
	}
}
