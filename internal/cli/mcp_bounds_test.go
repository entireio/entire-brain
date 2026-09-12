package cli

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Two unbounded paths on the MCP surface: one outbound, one in time.
//
// Carries #187 and #188, reimplemented against current main — both patches
// conflict on pure line drift from #118/#156/#158.

// --- #187: outbound frames were never bounded ----------------------------
//
// Inbound frames have been bounded by maxMCPFrameBytes since framing was
// written. Outbound frames were not bounded at all: every use of
// maxMCPFrameBytes in mcp.go was on the read side. The retrieval surface grew
// its own 128 KiB budget, but the ~25 graph/semantic/status/pattern tools had
// none — so a wide call could build a response larger than any frame the peer
// will accept, and the client either drops the connection or blocks. Either
// way the caller learns nothing about why.

func TestAnOversizedToolResultIsRefusedWithAnActionableError(t *testing.T) {
	huge := strings.Repeat("x", maxMCPFrameBytes+1)

	_, err := mcpToolTextResult(context.Background(), "brain_impact", huge)
	if err == nil {
		t.Fatal("a result larger than the frame limit was returned instead of refused")
	}
	// The caller has to be able to act on this: it must name the tool, the
	// limit, and what to change.
	for _, want := range []string{"brain_impact", "MCP frame limit", "narrow the request"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error is not actionable, missing %q: %v", want, err)
		}
	}
}

func TestAnOrdinaryToolResultIsUnaffected(t *testing.T) {
	result, err := mcpToolTextResult(context.Background(), "brain_status", "all good")
	if err != nil {
		t.Fatalf("an ordinary result was refused: %v", err)
	}
	content, ok := result["content"].([]map[string]any)
	if !ok || len(content) != 1 {
		t.Fatalf("result shape changed: %#v", result)
	}
	if content[0]["text"] != "all good" {
		t.Fatalf("result text = %v, want %q", content[0]["text"], "all good")
	}
	if content[0]["type"] != "text" {
		t.Fatalf("result type = %v, want \"text\"", content[0]["type"])
	}
}

// TestTheBoundIsTheFrameLimitNotAnArbitraryNumber pins the bound to the same
// constant the read side uses, so the two cannot drift apart.
func TestTheBoundIsTheFrameLimitNotAnArbitraryNumber(t *testing.T) {
	// One byte under the limit must pass; the limit is on the whole frame, so
	// leave room for the JSON-RPC envelope by measuring a text well under it.
	justUnder := strings.Repeat("x", maxMCPFrameBytes-4096)
	if _, err := mcpToolTextResult(context.Background(), "brain_status", justUnder); err != nil {
		t.Fatalf("a result comfortably under the limit was refused: %v", err)
	}
	if _, err := mcpToolTextResult(context.Background(), "brain_status", strings.Repeat("x", maxMCPFrameBytes+1)); err == nil {
		t.Fatal("a result over the limit was accepted")
	}
}

// --- #188: brain_index_repository had no deadline -------------------------
//
// brain_refresh has been capped since it was written; index is the same shape
// of work — a full provider snapshot over a repository of unknown size — on
// the same stdio transport, where a call that never returns is a client that
// never recovers. The tool descriptions already advertised the asymmetry:
// brain_refresh's says "Calls are capped at 60 seconds", index's said nothing.

func TestIndexRepositoryHasADeadlineLikeRefresh(t *testing.T) {
	if mcpIndexTimeout <= 0 {
		t.Fatal("brain_index_repository has no timeout")
	}
	// Not required to be identical to refresh's, but it is the same class of
	// work on the same transport, so a wildly different value would be a
	// mistake rather than a decision.
	if mcpIndexTimeout != mcpRefreshTimeout {
		t.Logf("note: index timeout %s differs from refresh timeout %s", mcpIndexTimeout, mcpRefreshTimeout)
	}
	if mcpIndexTimeout > 5*time.Minute {
		t.Fatalf("index timeout %s is long enough that a stuck call still looks like a hang", mcpIndexTimeout)
	}
}

// TestBothLongRunningToolsAreBounded is the property that matters: no tool on
// this surface that shells out to a full repository walk may run unbounded.
func TestBothLongRunningToolsAreBounded(t *testing.T) {
	for name, d := range map[string]time.Duration{
		"brain_refresh":          mcpRefreshTimeout,
		"brain_index_repository": mcpIndexTimeout,
	} {
		if d <= 0 {
			t.Fatalf("%s is unbounded", name)
		}
	}
}
