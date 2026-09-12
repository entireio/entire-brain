// Package httpx is the single source of the outbound HTTP policy for every client
// that talks to the hosted Entire API.
//
// http.DefaultClient has NO timeout of any kind, and http.DefaultTransport bounds
// only the dial and the TLS handshake — it leaves ResponseHeaderTimeout at zero. A
// server that accepts the connection and then never answers therefore parks the
// caller forever on http.DefaultClient, and for the full request budget on a client
// that sets only http.Client.Timeout. That is not theoretical here: the hosted-brain
// client, the fact-set sync client and `brain publish` are driven from the CLI and
// from the long-lived workspace daemon with context.Background(), so there is no
// caller deadline to fall back on.
//
// The policy is two layers, and callers get both:
//
//	PHASE bounds (shared transport, this package's Config)
//	  Dial            the TCP connect — a blackholed address
//	  TLSHandshake    a peer that connects and then stalls mid-handshake
//	  ResponseHeader  a peer that takes the request and never replies
//
//	REQUEST bound (per caller, http.Client.Timeout)
//	  the whole exchange including the body read
//
// Queries use a 30s response-header bound within their 60s request budget.
// Fact sync and artifact publish may spend substantial time processing a completed
// upload before returning headers, so UploadClient gives those callers a separate
// pooled transport with a 5m header bound. Their 5m request timeout still bounds
// the entire upload, processing and response read together. Both pools retain
// the 10s dial/TLS phase bounds and avoid mutable http.DefaultTransport policy.
//
// Not covered here, on purpose: the two loopback-pinned ollama clients
// (cli/embed_ollama.go, cli/distill_cmd.go). They already carry their own request
// bound and deliberately depart from this policy — proxies disabled and the dialer
// pinned to loopback IPs — so that query and document text cannot leave the box.
// Routing them through the shared transport would re-enable ProxyFromEnvironment and
// undo that.
package httpx

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// Config holds the phase bounds of a transport plus the request bound of the client
// built from it. The zero value is not useful; start from DefaultConfig.
type Config struct {
	Dial           time.Duration
	TLSHandshake   time.Duration
	ResponseHeader time.Duration
	Request        time.Duration
}

// DefaultConfig is the query phase policy. Request is only a default for NewClient;
// Client() callers state their own.
func DefaultConfig() Config {
	return Config{
		Dial:           10 * time.Second,
		TLSHandshake:   10 * time.Second,
		ResponseHeader: 30 * time.Second,
		Request:        5 * time.Minute,
	}
}

// NewTransport builds an independent bounded transport. Prefer Shared unless you
// need a separate connection pool (tests tightening the bounds, mainly).
func NewTransport(cfg Config) *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: cfg.Dial, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   http.DefaultMaxIdleConnsPerHost,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   cfg.TLSHandshake,
		ResponseHeaderTimeout: cfg.ResponseHeader,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// NewClient builds a client on its own bounded transport.
func NewClient(cfg Config) *http.Client {
	return &http.Client{Timeout: cfg.Request, Transport: NewTransport(cfg)}
}

var shared = sync.OnceValue(func() *http.Transport { return NewTransport(DefaultConfig()) })

// Shared returns the process-wide bounded transport. Safe for concurrent use; it
// pools connections across callers exactly as http.DefaultTransport would, with the
// response-header bound http.DefaultTransport lacks.
func Shared() *http.Transport { return shared() }

// Client returns a query client with the shorter response-header bound and the
// caller's end-to-end request bound. Upload callers use UploadClient instead.
func Client(requestTimeout time.Duration) *http.Client {
	return &http.Client{Timeout: requestTimeout, Transport: Shared()}
}

// uploadConfig preserves the full upload-processing budget. Unlike queries, an
// upload endpoint may legitimately remain silent while processing the body.
func uploadConfig() Config {
	cfg := DefaultConfig()
	cfg.ResponseHeader = cfg.Request
	return cfg
}

var uploadShared = sync.OnceValue(func() *http.Transport { return NewTransport(uploadConfig()) })

// UploadClient uses a shared pool for fact-set and artifact uploads, allowing
// up to five minutes for response headers. The caller's overall deadline
// continues to bound upload, processing and response reading together.
func UploadClient(requestTimeout time.Duration) *http.Client {
	return &http.Client{Timeout: requestTimeout, Transport: uploadShared()}
}
