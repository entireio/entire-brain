package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/spf13/cobra"
	"strings"
	"testing"
)

func TestMultiConceptEmptyResultsRemainJSONArray(t *testing.T) {
	for _, trimmed := range []bool{false, true} {
		t.Run(map[bool]string{false: "no matches", true: "all rows trimmed"}[trimmed], func(t *testing.T) {
			query := "nothing"
			var rows []unifiedResult
			if trimmed {
				query = strings.Repeat("q", conversationConceptResponseMaxBytes-150)
				rows = []unifiedResult{{ID: "fact:a", Text: strings.Repeat("detail", 200)}}
			}
			payload, err := boundedRetrievalJSONPayload(context.Background(), query, "main", rows, retrievalTransportExtras{}, "query")
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(data, &raw); err != nil {
				t.Fatal(err)
			}
			if string(raw["results"]) != "[]" {
				t.Fatalf("empty results must be [], got %s", raw["results"])
			}
			if trimmed && string(raw["response_truncated"]) != "true" {
				t.Fatal("missing truncation marker")
			}
		})
	}
}

func TestCompactBriefPreservesPendingFactReview(t *testing.T) {
	report := brainBriefReport{Task: "review fact", Facts: []factRecord{{ID: "fact:a", Text: "Use old API", Status: "active"}}, FactsPendingReview: map[string]factReviewNotice{"fact:a": {ReviewID: "review:a", Action: "supersede", Message: "Check the newer API", RelatedIDs: []string{"fact:b"}, Confidence: 0.75}}}
	for _, format := range []brainBriefPacketFormat{brainBriefPacketText, brainBriefPacketLegacyJSON, brainBriefPacketCompactV1, brainBriefPacketCompactV2, brainBriefPacketCompactV3} {
		t.Run(string(format), func(t *testing.T) {
			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&out)
			if err := emitBrainBriefPacket(cmd, report, format); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"Use old API", "review:a", "supersede", "fact:b"} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("missing %q", want)
				}
			}
		})
	}
}

func TestBriefRecognizesTestPathSegments(t *testing.T) {
	for _, path := range []string{"tests/test_query.py", "test/test_query.py", "src/__tests__/query.ts", "test_query.py", `src\tests\query.py`, "widget_test.go", "query.spec.ts", "query.test.js"} {
		if !brainBriefLikelyTestFile(path) {
			t.Errorf("test classified as implementation: %s", path)
		}
	}
	for _, path := range []string{"src/contest/query.py", "testimonials/query.ts", "src/query.py"} {
		if brainBriefLikelyTestFile(path) {
			t.Errorf("implementation classified as test: %s", path)
		}
	}
}

func TestMCPBudgetInstructionsDescribeUnreducibleRefusal(t *testing.T) {
	_, err := mcpToolTextResult(context.Background(), "brain_status", `{"blob":"`+strings.Repeat("x", mcpToolResponseMaxBytes*2)+`"}`)
	if err == nil {
		t.Fatal("unreducible response should fail")
	}
	for _, promise := range []string{"always returns an answer", "rather than failing", "Raise the limit to learn the real count"} {
		if strings.Contains(mcpServerInstructions, promise) {
			t.Errorf("instructions promise unsupported behavior: %q", promise)
		}
	}
	if !strings.Contains(mcpServerInstructions, "cannot be reduced") {
		t.Error("instructions omit unreducible-result refusal")
	}
}
