// Package httpx holds the brain's outbound HTTP client policy.
//
// http.DefaultClient has NO timeout of any kind: a server that accepts the
// connection and then never answers parks the caller forever. That is not a
// theoretical risk for this repo — the hosted-brain client and the fact-set
// sync client are driven from the CLI and from the long-lived `watch` daemon,
// neither of which sets a request deadline, so a single unresponsive endpoint
// wedges the process with no error and no recovery.
//
// Every outbound client therefore comes from here, bounded at four separate
// layers so no single hang mode is left open:
//
//	Dial            — the TCP connect (a blackholed address)
//	TLSHandshake    — a peer that completes the TCP handshake and then stalls
//	ResponseHeader  — a server that accepts the request and never replies
//	Request         — the whole exchange, including a body that dribbles
//
// Request is deliberately the loosest bound: fact-set payloads can be large,
// and a slow-but-progressing transfer must be allowed to finish. The narrower
// bounds are what actually catch a hung endpoint quickly.
package httpx

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// Config bounds one outbound client. The zero value is not useful; use
// DefaultConfig and adjust.
type Config struct {
	Dial           time.Duration
	TLSHandshake   time.Duration
	ResponseHeader time.Duration
	Request        time.Duration
}

// DefaultConfig is the policy every outbound brain client gets unless a caller
// injects its own *http.Client.
func DefaultConfig() Config {
	return Config{
		Dial:           10 * time.Second,
		TLSHandshake:   10 * time.Second,
		ResponseHeader: 30 * time.Second,
		Request:        5 * time.Minute,
	}
}

// NewClient builds a bounded client. It never shares a Transport with
// http.DefaultTransport, so tuning here cannot leak into unrelated callers.
func NewClient(cfg Config) *http.Client {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: cfg.Dial, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   cfg.TLSHandshake,
		ResponseHeaderTimeout: cfg.ResponseHeader,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{Timeout: cfg.Request, Transport: transport}
}

var defaultClient = sync.OnceValue(func() *http.Client { return NewClient(DefaultConfig()) })

// Default returns the shared bounded client. It is safe for concurrent use and
// pools connections across callers, exactly as http.DefaultClient would — with
// the timeouts http.DefaultClient lacks.
func Default() *http.Client { return defaultClient() }
