package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// `brain mcp` spoke only stdio, so nothing off the local machine could reach a
// brain. Closing that is the most dangerous gap on the list: a brain holds
// repository source, agent transcripts and prompts, and an HTTP endpoint in
// front of it is a door.
//
// These tests are weighted accordingly. Most of them are about who is refused.

func httpTestOptions(t *testing.T) Options {
	t.Helper()
	repoDir := seedFixtureRepo(t)
	return Options{
		Version: "test-version",
		Env: EntireEnv{
			RepoRoot:        repoDir,
			PluginConfigDir: filepath.Join(t.TempDir(), "config"),
			PluginDataDir:   filepath.Join(t.TempDir(), "data"),
			PluginStateDir:  filepath.Join(t.TempDir(), "state"),
			PluginCacheDir:  filepath.Join(t.TempDir(), "cache"),
		},
		Runner: seedFixtureRunner(repoDir),
		Now:    func() time.Time { return time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC) },
	}
}

func TestMCPHTTPBindsLoopbackWhenTheHostIsUnspecified(t *testing.T) {
	// Go's default for ":7777" is every interface. For a server in front of a
	// brain that is the wrong default, so a bare port must resolve to loopback
	// explicitly — the difference between "my other terminal" and "everyone on
	// this network" should never be something you get by omission.
	for _, addr := range []string{"7777", ":7777"} {
		resolved, loopback, err := resolveMCPHTTPAddr(addr)
		if err != nil {
			t.Fatalf("%q: %v", addr, err)
		}
		if !loopback || !strings.HasPrefix(resolved, "127.0.0.1:") {
			t.Fatalf("%q resolved to %q (loopback=%v); want explicit loopback", addr, resolved, loopback)
		}
	}
	for _, addr := range []string{"0.0.0.0:7777", "192.168.1.10:7777", "example.internal:7777"} {
		_, loopback, err := resolveMCPHTTPAddr(addr)
		if err != nil {
			t.Fatalf("%q: %v", addr, err)
		}
		if loopback {
			t.Fatalf("%q was classified loopback; it is reachable beyond this machine", addr)
		}
	}
	// A hostname we cannot prove is loopback must not be assumed safe: deciding
	// by resolving it would make the safety of the default depend on DNS.
	if _, loopback, _ := resolveMCPHTTPAddr("not-localhost.example:1234"); loopback {
		t.Fatal("an unresolvable hostname must not be treated as loopback")
	}
}

func TestMCPHTTPRefusesToServeBeyondTheMachineWithoutASecondFlag(t *testing.T) {
	t.Setenv(mcpHTTPTokenEnv, "token-for-test")
	if _, err := buildMCPHTTPConfig("0.0.0.0:7777", false); err == nil {
		t.Fatal("binding every interface was allowed without --http-allow-remote")
	} else if !strings.Contains(err.Error(), "http-allow-remote") {
		t.Fatalf("the refusal must name the flag that permits it: %v", err)
	}
	cfg, err := buildMCPHTTPConfig("0.0.0.0:7777", true)
	if err != nil {
		t.Fatalf("explicit opt-in was still refused: %v", err)
	}
	if cfg.Loopback {
		t.Fatal("0.0.0.0 reported as loopback")
	}
}

func TestMCPHTTPAlwaysHasAToken(t *testing.T) {
	// Including on loopback. Every process on the machine shares loopback, and
	// "localhost is safe" is how a brain gets read by a browser tab.
	t.Setenv(mcpHTTPTokenEnv, "")
	cfg, err := buildMCPHTTPConfig("127.0.0.1:7777", false)
	if err != nil {
		t.Fatalf("loopback config: %v", err)
	}
	if cfg.Token == "" {
		t.Fatal("no token was required on loopback")
	}
	if cfg.TokenSource != "generated" || len(cfg.Token) < 32 {
		t.Fatalf("generated token looks weak: source=%s len=%d", cfg.TokenSource, len(cfg.Token))
	}
	// Two runs must not produce the same token.
	other, err := buildMCPHTTPConfig("127.0.0.1:7777", false)
	if err != nil {
		t.Fatalf("second config: %v", err)
	}
	if other.Token == cfg.Token {
		t.Fatal("generated tokens repeat between runs")
	}

	t.Setenv(mcpHTTPTokenEnv, "supplied-by-the-operator")
	supplied, err := buildMCPHTTPConfig("127.0.0.1:7777", false)
	if err != nil {
		t.Fatalf("env config: %v", err)
	}
	if supplied.Token != "supplied-by-the-operator" || supplied.TokenSource != "environment" {
		t.Fatalf("env token ignored: %+v", supplied)
	}
}

func TestMCPHTTPRefusesEveryRequestWithoutTheRightToken(t *testing.T) {
	opts := httpTestOptions(t)
	cfg := mcpHTTPConfig{Addr: "127.0.0.1:0", Token: "right-token", Loopback: true}
	server := httptest.NewServer(newMCPHTTPHandler(opts, cfg))
	defer server.Close()

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	for _, header := range []string{"", "Bearer", "Bearer ", "Bearer wrong", "Basic right-token", "right-token"} {
		req, _ := http.NewRequest(http.MethodPost, server.URL, strings.NewReader(body))
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("Authorization %q got %d, want 401", header, resp.StatusCode)
		}
	}

	// Authorisation is checked before method and path, so an unauthenticated
	// caller cannot map the surface by probing for what answers differently.
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/some/path", nil)
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated GET revealed %d instead of 401", resp.StatusCode)
	}
}

func TestMCPHTTPServesTheSameToolsAsStdio(t *testing.T) {
	opts := httpTestOptions(t)
	cfg := mcpHTTPConfig{Addr: "127.0.0.1:0", Token: "right-token", Loopback: true}
	server := httptest.NewServer(newMCPHTTPHandler(opts, cfg))
	defer server.Close()

	req, _ := http.NewRequest(http.MethodPost, server.URL,
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	req.Header.Set("Authorization", "Bearer right-token")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var overHTTP struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&overHTTP); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// The transports share handleMCPMessage precisely so they cannot drift.
	// Comparing the surfaces is what keeps that true.
	viaHTTP := make([]string, 0, len(overHTTP.Result.Tools))
	for _, tool := range overHTTP.Result.Tools {
		viaHTTP = append(viaHTTP, tool.Name)
	}
	viaStdio := make([]string, 0)
	for _, def := range mcpToolDefinitions() {
		viaStdio = append(viaStdio, def["name"].(string))
	}
	if len(viaHTTP) != len(viaStdio) {
		t.Fatalf("HTTP exposes %d tools, stdio %d — the transports have drifted", len(viaHTTP), len(viaStdio))
	}
}

func TestMCPHTTPRejectsMalformedAndOversizeBodies(t *testing.T) {
	opts := httpTestOptions(t)
	cfg := mcpHTTPConfig{Addr: "127.0.0.1:0", Token: "right-token", Loopback: true}
	server := httptest.NewServer(newMCPHTTPHandler(opts, cfg))
	defer server.Close()

	post := func(body string) int {
		req, _ := http.NewRequest(http.MethodPost, server.URL, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer right-token")
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := post("not json at all"); got != http.StatusBadRequest {
		t.Fatalf("malformed body got %d, want 400", got)
	}
	// An MCP message is small; an unbounded body on a local daemon is a
	// trivial way to exhaust memory.
	if got := post(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"x":"` +
		strings.Repeat("A", mcpHTTPMaxBodyBytes+1024) + `"}}`); got == http.StatusOK {
		t.Fatal("an oversize body was accepted")
	}
}

func TestMCPHTTPTokenComparisonIsConstantTime(t *testing.T) {
	// A length-dependent comparison leaks the token a byte at a time to anyone
	// who can time the endpoint.
	if mcpHTTPAuthorized("Bearer short", "a-much-longer-token-value") {
		t.Fatal("a short wrong token was accepted")
	}
	// The case that actually distinguishes a constant-time compare from the
	// obvious wrong implementations. strings.HasPrefix(token, presented)
	// passes every other assertion here and accepts any PREFIX of the real
	// token, which is a guessing oracle one byte at a time.
	if mcpHTTPAuthorized("Bearer a-much-longer", "a-much-longer-token-value") {
		t.Fatal("a prefix of the token was accepted; the comparison is not an equality check")
	}
	if mcpHTTPAuthorized("Bearer a", "a-much-longer-token-value") {
		t.Fatal("a single-character prefix was accepted")
	}
	if mcpHTTPAuthorized("Bearer a-much-longer-token-value-x", "a-much-longer-token-value") {
		t.Fatal("a token with a trailing byte was accepted")
	}
	if !mcpHTTPAuthorized("Bearer exact", "exact") {
		t.Fatal("the correct token was rejected")
	}
}

// The retrieval privacy lock must be held until the response has actually been
// delivered. A retrieval command checks its output against the exclusion state;
// a tombstone landing between that check and the client receiving the bytes
// would deliver content that should already have been excluded.
//
// The stdio loop had this ordering and the HTTP handler did not, so the two
// transports disagreed about whether the window was closed. They now share one
// helper, and these tests hold both halves of what it guarantees.
func TestDeliveryHoldsThePrivacyLockUntilTheResponseIsOut(t *testing.T) {
	opts := httpTestOptions(t)
	msg := mcpMessage{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/list"}

	var (
		created            *mcpResponsePrivacyState
		releasedAtDelivery bool
		released           bool
	)
	original := newMCPResponsePrivacyState
	newMCPResponsePrivacyState = func() *mcpResponsePrivacyState {
		created = &mcpResponsePrivacyState{unlock: func() { released = true }}
		return created
	}
	t.Cleanup(func() { newMCPResponsePrivacyState = original })

	var seededDuringDelivery *mcpResponsePrivacyState
	if _, err := deliverMCPResponse(context.Background(), opts, msg, func(ctx context.Context, _ mcpMessage) error {
		releasedAtDelivery = released
		// The state has to be ON the context handling ran under. The tool
		// output buffer reads it by key and silently takes the
		// immediate-unlock path when it is absent, so a state that exists but
		// never reached the context closes nothing.
		seededDuringDelivery, _ = ctx.Value(mcpResponsePrivacyContextKey{}).(*mcpResponsePrivacyState)
		return nil
	}); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	if created == nil {
		t.Fatal("no privacy state was created; the lock-until-delivered window is open")
	}
	if seededDuringDelivery != created {
		t.Fatalf("the privacy state was not seeded on the request context (got %p, want %p); the tool buffer would never see it",
			seededDuringDelivery, created)
	}
	if releasedAtDelivery {
		t.Fatal("the privacy lock was released before the response was delivered")
	}
	if !released {
		t.Fatal("the privacy lock was never released; it would be held for the life of the process")
	}
}

// Delivery failing must still release the lock, or a client that hangs up
// mid-response leaves the brain write lock held until the process exits.
func TestDeliveryReleasesThePrivacyLockWhenDeliveryFails(t *testing.T) {
	opts := httpTestOptions(t)
	msg := mcpMessage{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/list"}

	released := false
	original := newMCPResponsePrivacyState
	newMCPResponsePrivacyState = func() *mcpResponsePrivacyState {
		return &mcpResponsePrivacyState{unlock: func() { released = true }}
	}
	t.Cleanup(func() { newMCPResponsePrivacyState = original })

	_, err := deliverMCPResponse(context.Background(), opts, msg, func(context.Context, mcpMessage) error {
		return errDeliveryFailed
	})
	if err == nil {
		t.Fatal("a failed delivery was reported as successful")
	}
	if !released {
		t.Fatal("a failed delivery left the privacy lock held")
	}
}

var errDeliveryFailed = errors.New("client hung up")

// Both transports must obtain the ordering from the shared helper rather than
// reimplementing it. This is a source check on purpose: the defect it guards
// was a call site that looked correct in isolation and disagreed with the other
// transport, which no behavioural test of either one alone would surface.
func TestBothTransportsDispatchThroughTheSharedHelper(t *testing.T) {
	for _, file := range []string{"mcp.go", "mcp_http.go"} {
		data, err := os.ReadFile(filepath.Join(".", file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		source := string(data)
		if !strings.Contains(source, "deliverMCPResponse(") {
			t.Fatalf("%s does not dispatch through deliverMCPResponse; the transports can disagree about the privacy window again", file)
		}
		for _, line := range strings.Split(source, "\n") {
			if !strings.Contains(line, "handleMCPMessage(") {
				continue
			}
			trimmed := strings.TrimSpace(line)
			// The declaration, and the one call inside the helper itself.
			if strings.HasPrefix(trimmed, "func handleMCPMessage") ||
				strings.HasPrefix(trimmed, "response := handleMCPMessage(ctx, opts, msg)") {
				continue
			}
			t.Fatalf("%s calls handleMCPMessage outside the helper:\n  %s\nuse deliverMCPResponse so the privacy lock spans delivery", file, trimmed)
		}
	}
}

// A response that fails partway leaves the client reading a truncated body
// under a 200, because the status line is long gone. Nothing can be done about
// that at this point — but swallowing the error left no trace anywhere, so a
// client-side parse failure could not be accounted for from this side.
func TestFailedHTTPDeliveryIsReported(t *testing.T) {
	opts := httpTestOptions(t)
	msg := mcpMessage{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/list"}

	_, err := deliverMCPResponse(context.Background(), opts, msg, func(context.Context, mcpMessage) error {
		return errDeliveryFailed
	})
	if err == nil {
		t.Fatal("deliverMCPResponse discarded the delivery error; the handler has nothing to report")
	}
	if !errors.Is(err, errDeliveryFailed) {
		t.Fatalf("the delivery error was replaced rather than returned: %v", err)
	}
}

// The handler must actually consult that error rather than discarding it with
// `_, _ =`, which is what it did.
func TestHTTPHandlerDoesNotDiscardTheDeliveryError(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(".", "mcp_http.go"))
	if err != nil {
		t.Fatalf("read mcp_http.go: %v", err)
	}
	source := string(data)
	if strings.Contains(source, "_, _ = deliverMCPResponse(") {
		t.Fatal("mcp_http.go discards the delivery error; a truncated response would leave no trace")
	}
	if !strings.Contains(source, "not fully written") {
		t.Fatal("mcp_http.go does not report a failed delivery")
	}
}
