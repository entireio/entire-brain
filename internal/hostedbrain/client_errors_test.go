package hostedbrain

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestRPCErrorSanitizesRemoteMessage(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "")
	t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "")
	body, _ := json.Marshal(map[string]any{"error": map[string]any{"code": -32001, "message": "denied audit-token\x1b[31m"}})
	c := &Client{BaseURL: "https://fuzz.invalid", Token: "audit-token", HTTP: fuzzClient(200, body)}
	_, _, err := c.Initialize(context.Background(), "repo")
	var rpcErr *jsonrpcError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32001 {
		t.Fatalf("lost RPC code/type: %v", err)
	}
	if strings.Contains(err.Error(), "audit-token") || strings.ContainsAny(err.Error(), "\x1b\r\n") {
		t.Fatalf("unsafe RPC error: %q", err)
	}
	if !strings.Contains(err.Error(), "denied") {
		t.Fatalf("lost diagnostic: %v", err)
	}
}

func TestToolErrorNeverDecodesAsSuccess(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "")
	t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "")
	for _, text := range []string{`{"error":"audit-token denied"}`, "audit-token\x1b[31m denied", ""} {
		body, _ := json.Marshal(map[string]any{"result": map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": text}}}})
		c := &Client{BaseURL: "https://fuzz.invalid", Token: "audit-token", HTTP: fuzzClient(200, body)}
		_, err := c.Status(context.Background(), "repo", "main")
		if err == nil {
			t.Errorf("tool error became successful status: %q", text)
			continue
		}
		if strings.Contains(err.Error(), "audit-token") || strings.ContainsRune(err.Error(), '\x1b') {
			t.Errorf("unsafe tool error: %q", err)
		}
	}
}

func TestFuzzOriginReachesHostedDecoder(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "")
	t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "")
	c := &Client{BaseURL: fuzzOrigin, HTTP: fuzzClient(200, []byte(`{"result":{"serverInfo":{"name":"decoded-marker"}}}`))}
	got, _, err := c.Initialize(context.Background(), "repo")
	if err != nil || got.Name != "decoded-marker" {
		t.Fatalf("fuzz harness did not decode seed: %+v, %v", got, err)
	}
}
