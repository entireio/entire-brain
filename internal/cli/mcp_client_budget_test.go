package cli

import (
	"strings"
	"testing"
)

// The sizes a real client refused, from a recorded Claude Code session against
// a repo with a built brain. Every one of these was UNDER the old 128 KiB
// budget, so the trimming in mcp_response_budget.go worked exactly as designed
// and the result was rejected anyway. brain_impact is the tight bound.
const (
	observedRefusedBrainStatusChars = 124295
	observedRefusedBrainBriefChars  = 125822
	observedRefusedBrainImpactChars = 61580
)

// The budget is spent in an agent's context window, so it has to sit below the
// smallest result a client was observed to refuse — not below the transport
// frame, which is 4 MiB and bounds nothing an agent cares about.
func TestBudgetIsUnderTheSmallestResultAClientRefused(t *testing.T) {
	t.Parallel()

	smallest := observedRefusedBrainImpactChars
	if mcpToolResponseMaxBytes >= smallest {
		t.Fatalf("budget %d is not below %d, the smallest result a client actually refused (brain_impact); "+
			"results at that size are produced and then thrown away",
			mcpToolResponseMaxBytes, smallest)
	}
	// JSON of this shape runs about 2.5 chars/token, so a byte budget has to be
	// read as a token budget to mean anything.
	if tokens := float64(mcpToolResponseMaxBytes) / 2.5; tokens > 20000 {
		t.Fatalf("budget %d bytes is ~%.0f tokens of JSON, over what a client reserves for one tool result",
			mcpToolResponseMaxBytes, tokens)
	}
}

// The three recorded sizes must all be reducible rather than refused. An error
// carries no rows; a trimmed document carries rows plus a marker saying what
// went missing and how to narrow.
func TestRecordedOversizeResultsAreTrimmedNotRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		tool  string
		chars int
	}{
		{"brain_status", observedRefusedBrainStatusChars},
		{"brain_brief", observedRefusedBrainBriefChars},
		{"brain_impact", observedRefusedBrainImpactChars},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			doc := oversizeRowDocument(tc.chars)
			if len(doc) < tc.chars {
				t.Fatalf("fixture %d bytes, want at least %d", len(doc), tc.chars)
			}
			out, err := mcpBoundedToolText(t.Context(), tc.tool, doc)
			if err != nil {
				t.Fatalf("a %d byte result with removable rows must trim, not error: %v", tc.chars, err)
			}
			if len(out) > mcpToolResponseMaxBytes {
				t.Fatalf("trimmed to %d bytes, over the %d budget", len(out), mcpToolResponseMaxBytes)
			}
			if !strings.Contains(out, mcpResponseTruncatedKey) {
				t.Fatalf("a trimmed result must say so; %q missing from the document", mcpResponseTruncatedKey)
			}
		})
	}
}

// A client with a different window can move the budget, and a bad value must
// fall back rather than take the server down or run unbounded.
func TestBudgetOverrideParsing(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		raw  string
		want int
	}{
		{"unset", "", mcpToolResponseDefaultMaxBytes},
		{"blank", "   ", mcpToolResponseDefaultMaxBytes},
		{"valid", "65536", 65536},
		{"padded", " 65536 ", 65536},
		{"garbage", "lots", mcpToolResponseDefaultMaxBytes},
		{"zero", "0", mcpToolResponseDefaultMaxBytes},
		{"negative", "-1", mcpToolResponseDefaultMaxBytes},
		{"over the frame limit is clamped", "999999999", maxMCPFrameBytes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveMCPToolResponseMaxBytes(tc.raw); got != tc.want {
				t.Fatalf("resolveMCPToolResponseMaxBytes(%q) = %d, want %d", tc.raw, got, tc.want)
			}
		})
	}
}

// The advertised contract has to carry the budget actually in force, or a
// client plans against a number the server will not honour.
func TestServerInstructionsStateTheActiveBudget(t *testing.T) {
	t.Parallel()

	if !strings.Contains(mcpServerInstructions, itoaForTest(mcpToolResponseMaxBytes)) {
		t.Fatalf("server instructions do not state the active budget %d:\n%s",
			mcpToolResponseMaxBytes, mcpServerInstructions)
	}
}

// oversizeRowDocument builds a JSON document of at least n bytes whose weight
// is a trimmable row list, which is the shape every oversize tool result has.
func oversizeRowDocument(n int) string {
	var b strings.Builder
	b.WriteString(`{"query":"x","results":[`)
	for i := 0; b.Len() < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"id":"fact:`)
		b.WriteString(itoaForTest(i))
		b.WriteString(`","path":"architecture.data.flow","text":"`)
		b.WriteString(strings.Repeat("a durable recorded decision with enough prose to weigh something. ", 4))
		b.WriteString(`"}`)
	}
	b.WriteString(`]}`)
	return b.String()
}

func itoaForTest(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	if neg {
		return "-" + string(d)
	}
	return string(d)
}
