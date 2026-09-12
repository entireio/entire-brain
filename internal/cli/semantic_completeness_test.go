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
			name: "intentional provider skips stay recorded without degrading",
			source: &semanticSourceManifest{Files: 1212, Symbols: 1, PartialFailures: []semanticWarning{
				{Code: semanticFileTooLargeCode, Severity: "warning"},
				{Code: semanticMinifiedCode, Severity: "warning"},
			}},
			wantState: "ok",
		},
		{
			name: "intentional skips do not consume parse error tolerance",
			source: &semanticSourceManifest{Files: 20, Symbols: 1, PartialFailures: []semanticWarning{
				{Code: semanticFileTooLargeCode, Severity: "warning"},
				{Code: semanticMinifiedCode, Severity: "warning"},
				{Code: semanticParseErrorCode, Severity: "warning"},
			}},
			wantState: "ok",
		},
		{
			name: "intentional skip does not hide extraction failure",
			source: &semanticSourceManifest{Files: 1212, Symbols: 1, PartialFailures: []semanticWarning{
				{Code: semanticMinifiedCode, Severity: "warning"},
				{Code: "E_SYMBOL_EXTRACTION", Severity: "warning"},
			}},
			wantState: "degraded",
		},
		{
			name: "majority intentionally skipped remains unsafe",
			source: &semanticSourceManifest{Files: 3, Symbols: 1, PartialFailures: []semanticWarning{
				{Code: semanticFileTooLargeCode, Severity: "warning"},
				{Code: semanticMinifiedCode, Severity: "warning"},
			}},
			wantState: "unsafe",
		},
		{
			name: "parsed files with zero symbols remain degraded",
			source: &semanticSourceManifest{Files: 2, PartialFailures: []semanticWarning{
				{Code: semanticMinifiedCode, Severity: "warning"},
			}},
			wantState: "degraded",
		},
		{
			name: "intentional skip with uncountable file set degrades",
			source: &semanticSourceManifest{PartialFailures: []semanticWarning{
				{Code: semanticMinifiedCode, Severity: "warning"},
			}},
			wantState: "degraded",
		},
		{
			name: "unknown casing fails closed",
			source: &semanticSourceManifest{Files: 1212, PartialFailures: []semanticWarning{
				{Code: "e_minified", Severity: "warning"},
			}},
			wantState: "degraded",
		},
		{
			name: "empty failure code fails closed",
			source: &semanticSourceManifest{Files: 1212, PartialFailures: []semanticWarning{
				{Severity: "warning"},
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
