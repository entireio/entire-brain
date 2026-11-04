package cli

import (
	"encoding/json"
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
			payload, err := boundedRetrievalJSONPayload(query, "main", rows, retrievalTransportExtras{})
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

func TestBriefRecognizesTestPathSegments(t *testing.T) {
	for _, path := range []string{"tests/query_test.go", "test/query_test.go", "src/__tests__/query.ts", "query_test.go", `src\tests\query.go`, "widget_test.go", "query.spec.ts", "query.test.js"} {
		if !brainBriefLikelyTestFile(path) {
			t.Errorf("test classified as implementation: %s", path)
		}
	}
	for _, path := range []string{"src/contest/query.go", "testimonials/query.ts", "src/query.go"} {
		if brainBriefLikelyTestFile(path) {
			t.Errorf("implementation classified as test: %s", path)
		}
	}
}
