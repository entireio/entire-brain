package factsync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/entireio/entire-brain/internal/apiurl"
	"github.com/entireio/entire-brain/internal/httpx"
	"github.com/entireio/entire-brain/internal/repoid"
)

// HTTPServer is the hosted-sync HTTP client: it drives the per-fact wire
// (perfact.go — brainstore's batch upsert and repo-wide delta feed) under the
// member's bearer token. The name survives from when it also drove the retired
// branch-head CAS wire against entire-api (COR-2235/COR-2236).
type HTTPServer struct {
	BaseURL string       // entire-api origin, e.g. https://api.entire.io (no trailing slash needed); must clear apiurl.Validate — https, or http only to loopback
	Token   string       // bearer token; sent as Authorization: Bearer <token> when non-empty
	Version string       // this build's version; sent as the User-Agent when non-empty
	Client  *http.Client // defaults to the bounded shared upload client when nil
}

// userAgentPrefix names the product in the User-Agent this client sends. The
// server logs the header, which is the only way to tell whether a fleet has
// picked up a client-side fix: a stuck client is fixed by its own upgrade, and
// nothing the server deploys can hurry that along or observe it.
const userAgentPrefix = "entire-brain/"

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
// target in this package. Every per-fact endpoint (perfact.go) is built on top
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

// detail renders the server's objection with this client's own bearer token removed.
// Every non-2xx render on the per-fact surface goes through here, so the
// redaction is applied once rather than at each call site. See
// httpx.Redact for why an echoed token has to be stripped even though the peer that
// echoed it already had it.
func (h *HTTPServer) detail(resp *http.Response) string {
	return httpx.Redact(httpx.ErrorSuffix(resp), h.Token)
}

func (h *HTTPServer) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	// Scheme floor (apiurl.Validate) at the single request chokepoint of the
	// per-fact surface: the bearer token below and the fact batch above it must
	// never be built into a plaintext request to a non-loopback host. Every
	// construction path lands here, so a caller that bypassed the CLI's own
	// check is still covered.
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
	if h.Version != "" {
		req.Header.Set("User-Agent", userAgentPrefix+h.Version)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}
