package cli

import "testing"

func TestSemanticCompletenessAxisToleratesFewParseErrors(t *testing.T) {
	parseFailures := make([]semanticWarning, 45)
	for i := range parseFailures {
		parseFailures[i] = semanticWarning{Code: semanticParseErrorCode, Severity: "warning", Detail: "tree-sitter syntax error nodes present"}
	}
	cases := []struct {
		name      string
		source    *semanticSourceManifest
		wantState string
	}{
		{
			name:      "few parse errors stay ok",
			source:    &semanticSourceManifest{Files: 1212, PartialFailures: parseFailures},
			wantState: "ok",
		},
		{
			name:      "too many parse errors degrade",
			source:    &semanticSourceManifest{Files: 100, PartialFailures: parseFailures},
			wantState: "degraded",
		},
		{
			name: "non-parse failure always degrades",
			source: &semanticSourceManifest{Files: 1212, PartialFailures: []semanticWarning{
				{Code: "E_SYMBOL_EXTRACTION", Severity: "warning"},
			}},
			wantState: "degraded",
		},
		{
			name:      "uncountable file set degrades",
			source:    &semanticSourceManifest{Files: 0, PartialFailures: parseFailures[:1]},
			wantState: "degraded",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := semanticCompletenessAxis(tc.source)
			if got.State != tc.wantState {
				t.Fatalf("state = %q, want %q (detail: %s)", got.State, tc.wantState, got.Detail)
			}
		})
	}
}
