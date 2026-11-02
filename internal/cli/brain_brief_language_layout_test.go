package cli

import (
	"slices"
	"testing"
)

// These fixtures are path strings. No Python source or interpreter is required.
func TestBrainBriefLanguageLayoutCandidates(t *testing.T) {
	for _, tc := range []struct {
		name, source   string
		direct, nested []string
	}{
		{"python src", "src/auth/token.py", []string{"src/auth/token_test.py"}, []string{"tests/auth/test_token.py"}},
		{"python root src", "src/token.py", []string{"src/token_test.py"}, []string{"tests/test_token.py"}},
		{"python intended create", "src/auth/new_token.py", []string{"src/auth/new_token_test.py"}, []string{"tests/auth/test_new_token.py"}},
		{"python outside src", "lib/auth/token.py", []string{"lib/auth/token_test.py"}, nil},
		{"python lookalike src", "src-other/auth/token.py", []string{"src-other/auth/token_test.py"}, nil},
		{"python traversal", "src/../../token.py", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Traversal rejection belongs to the nested layout helper; the direct
			// candidate helper relies on the caller's repository path validation.
			if tc.direct != nil {
				if got := brainBriefSiblingTestCandidates(tc.source); !slices.Equal(got, tc.direct) {
					t.Fatalf("direct candidates = %v, want %v", got, tc.direct)
				}
			}
			if got := brainBriefNestedTestCandidates(tc.source); !slices.Equal(got, tc.nested) {
				t.Fatalf("nested candidates = %v, want %v", got, tc.nested)
			}
		})
	}
}
