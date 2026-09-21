package cli

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// MCP over HTTP.
//
// `brain mcp` speaks stdio, which means the only thing that can reach a brain
// is a process the user launched on the same machine. That is a real limit: a
// teammate, a second machine, or an agent running anywhere else simply cannot
// query it. mem0, cognee and supermemory all expose remote MCP.
//
// It is also the most dangerous gap on the list to close, and the design says
// so throughout. A brain holds repository source, agent transcripts, prompts
// and commit history. An HTTP endpoint in front of that is a door, and the
// whole question is who gets to walk through it. So:
//
//   - It does not exist unless asked for. No flag, no listener. `brain mcp`
//     keeps speaking stdio and keeps making no network calls, which is what
//     SECURITY.md promises.
//   - It binds loopback unless explicitly told otherwise. A bare port means
//     127.0.0.1, never 0.0.0.0 — the difference between "my other terminal"
//     and "everyone on the coffee shop wifi" should not be a default.
//   - It always requires a bearer token. There is no unauthenticated mode, not
//     even on loopback: every process on the machine shares loopback, and
//     "localhost is safe" is how a brain gets read by a browser tab.
//   - Reaching a non-loopback interface needs a second, separate flag. Two
//     deliberate acts, because that one is genuinely different in kind.
//
// The handler reuses handleMCPMessage, the same dispatch stdio uses, so the two
// transports cannot drift apart in what they expose.

const (
	mcpHTTPTokenEnv = "ENTIRE_BRAIN_MCP_TOKEN"
	// Requests are bounded: an MCP message is small, and an unbounded body on a
	// local daemon is a trivial way to exhaust memory.
	mcpHTTPMaxBodyBytes = 4 << 20
	mcpHTTPReadTimeout  = 30 * time.Second
	mcpHTTPWriteTimeout = 120 * time.Second
	mcpHTTPIdleTimeout  = 90 * time.Second
)

// mcpHTTPConfig is the resolved, validated listener configuration. Building it
// is separated from serving so the safety rules are testable without opening a
// socket.
type mcpHTTPConfig struct {
	Addr        string
	Token       string
	TokenSource string // "environment" | "generated"
	Loopback    bool
}

// resolveMCPHTTPAddr normalises the address and reports whether it is loopback.
// A bare port ("7777" or ":7777") resolves to loopback explicitly rather than
// to the wildcard, because Go's default for ":7777" is every interface and that
// is the wrong default for this.
func resolveMCPHTTPAddr(addr string) (string, bool, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", false, errors.New("an address is required, for example 127.0.0.1:7777")
	}
	if !strings.Contains(addr, ":") {
		addr = ":" + addr
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", false, fmt.Errorf("address %q is not host:port: %w", addr, err)
	}
	if strings.TrimSpace(port) == "" {
		return "", false, fmt.Errorf("address %q has no port", addr)
	}
	if host == "" {
		// The key decision in this file. An empty host binds every interface;
		// making it loopback means the easy thing is also the safe thing.
		return net.JoinHostPort("127.0.0.1", port), true, nil
	}
	if ip := net.ParseIP(host); ip != nil {
		return net.JoinHostPort(host, port), ip.IsLoopback(), nil
	}
	if strings.EqualFold(host, "localhost") {
		return net.JoinHostPort(host, port), true, nil
	}
	// A hostname we cannot prove is loopback is treated as remote. Resolving it
	// to decide would make the safety of the default depend on DNS.
	return net.JoinHostPort(host, port), false, nil
}

// newMCPHTTPToken returns the bearer token and where it came from. A token is
// mandatory: supplying one through the environment keeps it out of the process
// table, and generating one is a convenience that still cannot be skipped.
func newMCPHTTPToken() (string, string, error) {
	if fromEnv := strings.TrimSpace(os.Getenv(mcpHTTPTokenEnv)); fromEnv != "" {
		return fromEnv, "environment", nil
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(raw), "generated", nil
}

// buildMCPHTTPConfig applies every safety rule and returns a config or the
// reason it refused.
func buildMCPHTTPConfig(addr string, allowRemote bool) (mcpHTTPConfig, error) {
	resolved, loopback, err := resolveMCPHTTPAddr(addr)
	if err != nil {
		return mcpHTTPConfig{}, err
	}
	if !loopback && !allowRemote {
		return mcpHTTPConfig{}, fmt.Errorf(
			"refusing to serve a brain on %s: it is not a loopback address.\n"+
				"  A brain holds repository source, agent transcripts and prompts.\n"+
				"  Pass --http-allow-remote to serve it beyond this machine, and put it behind TLS.", resolved)
	}
	token, source, err := newMCPHTTPToken()
	if err != nil {
		return mcpHTTPConfig{}, err
	}
	return mcpHTTPConfig{Addr: resolved, Token: token, TokenSource: source, Loopback: loopback}, nil
}

// mcpHTTPAuthorized compares the presented credential in constant time. A
// length-independent comparison would leak the token a byte at a time to anyone
// who can time the endpoint.
func mcpHTTPAuthorized(header, token string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	presented := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	return subtle.ConstantTimeCompare([]byte(presented), []byte(token)) == 1
}

// newMCPHTTPHandler serves one JSON-RPC message per request. It deliberately
// does not keep session state: MCP over HTTP is request/response here, and a
// server that remembered a peer would need to decide what happens when two
// peers present the same token.
func newMCPHTTPHandler(opts Options, cfg mcpHTTPConfig) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Authorise before anything else, including method and path checks, so
		// an unauthenticated caller cannot map the surface by probing.
		if !mcpHTTPAuthorized(r.Header.Get("Authorization"), cfg.Token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="entire-brain"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, mcpHTTPMaxBodyBytes))
		if err != nil {
			http.Error(w, "request body too large or unreadable", http.StatusRequestEntityTooLarge)
			return
		}
		var msg mcpMessage
		if err := json.Unmarshal(body, &msg); err != nil {
			writeMCPHTTPParseError(w)
			return
		}
		// Same dispatch as stdio, through the same helper. The transports must
		// not be able to disagree about which tools exist, what they do, or —
		// as they once did — whether the retrieval privacy lock is held until
		// the response has actually been delivered.
		if msg.ID == nil {
			// A JSON-RPC notification. The stdio loop skips dispatch entirely
			// for these, and the two transports must not disagree about what
			// they execute: dispatching here meant an authenticated HTTP client
			// could run a state-mutating tools/call by omitting the id, and
			// have the only evidence discarded as a 204.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, deliverErr := deliverMCPResponse(r.Context(), opts, msg, func(_ context.Context, response mcpMessage) error {
			w.Header().Set("Content-Type", "application/json")
			return json.NewEncoder(w).Encode(response)
		})
		if deliverErr != nil {
			// The status line and headers are already sent, so the client will
			// read a truncated body under a 200. Nothing can be done about that
			// here — but swallowing the error entirely left no trace anywhere
			// that the response was malformed, which makes a client-side parse
			// failure impossible to account for from this side.
			fmt.Fprintf(os.Stderr, "warning: mcp http response for %s not fully written: %v\n", msg.Method, deliverErr)
		}
	})
	return mux
}

func writeMCPHTTPParseError(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"parse error"}}`))
}

// runMCPHTTP starts the listener and serves until the context is cancelled.
func runMCPHTTP(ctx context.Context, out io.Writer, opts Options, addr string, allowRemote bool) error {
	cfg, err := buildMCPHTTPConfig(addr, allowRemote)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Addr, err)
	}
	server := &http.Server{
		Handler:           newMCPHTTPHandler(opts, cfg),
		ReadHeaderTimeout: mcpHTTPReadTimeout,
		ReadTimeout:       mcpHTTPReadTimeout,
		WriteTimeout:      mcpHTTPWriteTimeout,
		IdleTimeout:       mcpHTTPIdleTimeout,
	}

	fmt.Fprintf(out, "brain MCP listening on http://%s\n", listener.Addr().String())
	if cfg.TokenSource == "generated" {
		fmt.Fprintf(out, "bearer token: %s\n", cfg.Token)
		fmt.Fprintf(out, "  (set %s to supply your own and keep it out of this output)\n", mcpHTTPTokenEnv)
	} else {
		fmt.Fprintf(out, "bearer token: taken from %s\n", mcpHTTPTokenEnv)
	}
	if !cfg.Loopback {
		fmt.Fprintf(out, "WARNING: %s is reachable beyond this machine and the connection is not encrypted.\n"+
			"  This brain holds repository source, agent transcripts and prompts. Put TLS in front of it.\n", cfg.Addr)
	}

	errc := make(chan error, 1)
	go func() { errc <- server.Serve(listener) }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
