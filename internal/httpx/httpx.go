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
// The split matters. The request bound has to be generous, because a fact-set
// advance or a publish bundle is large and a slow-but-progressing transfer must be
// allowed to finish; a five-minute silence detector would be useless. The phase
// bounds are what actually catch a hung endpoint quickly, and they are the same for
// everyone. So each caller picks its own request bound to suit its payload and
// inherits one shared set of phase bounds:
//
//	hostedbrain.Client   60s   bounded reads (search / get / status)
//	factsync.HTTPServer   5m   uploads the whole merged fact-set
//	cli publish           5m   uploads the whole brain artifact bundle
//
// One shared transport, not one per caller: a transport owns a connection pool, so
// minting one per call site would fragment pooling and leak file descriptors. It is
// deliberately NOT http.DefaultTransport — that is process-global state any
// dependency can mutate, and it lacks the response-header bound this package exists
// to add.
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

// DefaultConfig is the shared phase policy. Request is only a default for NewClient;
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

// Client returns a client on the shared bounded transport with the caller's own
// end-to-end request bound. This is what every hosted-API call site should use.
func Client(requestTimeout time.Duration) *http.Client {
	return &http.Client{Timeout: requestTimeout, Transport: Shared()}
}
