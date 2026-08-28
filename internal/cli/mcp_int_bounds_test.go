package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// Integer tool arguments had a floor but no ceiling: mcpPositiveInt accepted
// anything up to math.MaxInt. An MCP client is not a trusted peer here — the
// agent driving it carries attacker-influenced repository text in its context,
// and a tools/call is one line of JSON.
//
// The consequence is not a bad result, it is a dead server: `limit` flows
// unclamped into make([]T, 0, limit) in the retrieval layer (history_fts.go,
// and doc_fts.go at limit*4), so limit = 1e9 asks the runtime for hundreds of
// gigabytes. That is a fatal out-of-memory, which no recover() catches, and it
// takes the whole brain MCP server with it.

func TestMCPIntegerArgumentsAreCapped(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"limit", "depth", "context_lines", "offset"} {
		for _, value := range []any{float64(1_000_000_000), float64(1 << 40), int(math.MaxInt32)} {
			if _, err := mcpPositiveInt(map[string]any{key: value}, key, 5); err == nil {
				t.Errorf("mcpPositiveInt accepted %s=%v; it sizes a slice capacity", key, value)
			}
			if _, err := mcpNonNegativeInt(map[string]any{key: value}, key, 0); err == nil {
				t.Errorf("mcpNonNegativeInt accepted %s=%v", key, value)
			}
		}
	}
}

func TestMCPIntegerArgumentsStillAcceptUsefulValues(t *testing.T) {
	t.Parallel()
	for _, value := range []any{float64(1), float64(10), float64(mcpIntegerArgMax), int(50)} {
		if got, err := mcpPositiveInt(map[string]any{"limit": value}, "limit", 5); err != nil {
			t.Errorf("mcpPositiveInt rejected an in-range limit=%v: %v", value, err)
		} else if got < 1 {
			t.Errorf("mcpPositiveInt returned %d for %v", got, value)
		}
	}
	if _, err := mcpNonNegativeInt(map[string]any{"after_turn": float64(0)}, "after_turn", 0); err != nil {
		t.Errorf("mcpNonNegativeInt rejected 0: %v", err)
	}
}

// TestMCPToolCallRejectsOversizedLimit drives the real MCP loop, which is the
// path an untrusted client actually reaches.
func TestMCPToolCallRejectsOversizedLimit(t *testing.T) {
	payload := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brain_query","arguments":{"query":"ValidateToken","limit":%d}}}`, 1_000_000_000)
	var out bytes.Buffer
	if err := runMCP((&cobra.Command{}).Context(), strings.NewReader(frameMCP(payload)), &out, Options{Version: "test-version"}); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	responses := readMCPResponses(t, out.String())
	data, _ := json.Marshal(responses[0]["error"])
	if !strings.Contains(string(data), "limit must be an integer between 1 and") {
		t.Fatalf("error = %s", data)
	}
}
