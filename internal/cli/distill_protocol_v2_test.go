package cli

import (
	"fmt"
	"strings"
	"testing"
)

func TestParseDistillCandidateMemberResultsV2(t *testing.T) {
	const (
		candidateA = "candidate-a"
		candidateB = "candidate-b"
	)
	validFactA := candidateA + "\tdecision\tarchitecture.boundaries.rationale\tKeep the protocol parser separate from durable fact construction."
	validFactB := candidateA + "\tgotcha\tconstraints.invariants.general\tEvery packed member must receive a completion."
	validNoFactsB := candidateB + "\tNO_FACTS"

	tests := []struct {
		name     string
		expected []string
		output   string
		wantErr  string
		want     []distillCandidateMemberResultV2
	}{
		{
			name:     "facts and sentinel return expected order",
			expected: []string{candidateB, candidateA},
			output:   validFactA + "\n" + validFactB + "\n" + validNoFactsB + "\n",
			want: []distillCandidateMemberResultV2{
				{CandidateID: candidateB, NoFacts: true},
				{CandidateID: candidateA, Facts: []distillCandidateMemberFactV2{
					{Kind: "decision", Path: "architecture.boundaries.rationale", Text: "Keep the protocol parser separate from durable fact construction."},
					{Kind: "gotcha", Path: "constraints.invariants.general", Text: "Every packed member must receive a completion."},
				}},
			},
		},
		{
			name:     "CRLF final newline accepted",
			expected: []string{candidateA},
			output:   validFactA + "\r\n",
			want: []distillCandidateMemberResultV2{{CandidateID: candidateA, Facts: []distillCandidateMemberFactV2{
				{Kind: "decision", Path: "architecture.boundaries.rationale", Text: "Keep the protocol parser separate from durable fact construction."},
			}}},
		},
		{
			name:     "local model may replace final tab with one space",
			expected: []string{candidateA},
			output:   candidateA + "\tconvention\tworkflow.testing.rules Always run race tests before merging.",
			want: []distillCandidateMemberResultV2{{CandidateID: candidateA, Facts: []distillCandidateMemberFactV2{
				{Kind: "convention", Path: "workflow.testing.rules", Text: "Always run race tests before merging."},
			}}},
		},
		{
			name:     "blank output",
			expected: []string{candidateA},
			output:   "",
			wantErr:  "blank output",
		},
		{
			name:     "blank record",
			expected: []string{candidateA},
			output:   validFactA + "\n\n",
			wantErr:  "blank line",
		},
		{
			name:     "prose",
			expected: []string{candidateA},
			output:   "Here are the extracted facts:\n" + validFactA,
			wantErr:  "expected candidate_id",
		},
		{
			name:     "unknown ID",
			expected: []string{candidateA},
			output:   "unknown\tNO_FACTS",
			wantErr:  "unknown candidate ID",
		},
		{
			name:     "missing completion",
			expected: []string{candidateA, candidateB},
			output:   validFactA,
			wantErr:  "missing completion",
		},
		{
			name:     "duplicate expected ID",
			expected: []string{candidateA, candidateA},
			output:   validFactA,
			wantErr:  "duplicate expected candidate ID",
		},
		{
			name:     "duplicate sentinel",
			expected: []string{candidateA},
			output:   candidateA + "\tNO_FACTS\n" + candidateA + "\tNO_FACTS",
			wantErr:  "duplicate or conflicting completion",
		},
		{
			name:     "sentinel and facts conflict",
			expected: []string{candidateA},
			output:   candidateA + "\tNO_FACTS\n" + validFactA,
			wantErr:  "NO_FACTS conflicts with facts",
		},
		{
			name:     "literal tab escapes are prose",
			expected: []string{candidateA},
			output:   strings.ReplaceAll(validFactA, "\t", `\t`),
			wantErr:  "expected candidate_id",
		},
		{
			name:     "invalid kind",
			expected: []string{candidateA},
			output:   candidateA + "\trule\tarchitecture.boundaries.rationale\tA fact.",
			wantErr:  "invalid fact kind",
		},
		{
			name:     "kind must be canonical lowercase",
			expected: []string{candidateA},
			output:   candidateA + "\tDECISION\tarchitecture.boundaries.rationale\tA fact.",
			wantErr:  "invalid fact kind",
		},
		{
			name:     "invalid path",
			expected: []string{candidateA},
			output:   candidateA + "\tdecision\tnot a path\tA fact.",
			wantErr:  "invalid fact path",
		},
		{
			name:     "empty fact",
			expected: []string{candidateA},
			output:   candidateA + "\tdecision\tarchitecture.boundaries.rationale\t  ",
			wantErr:  "blank fact",
		},
		{
			name:     "NO_FACTS must be a sentinel",
			expected: []string{candidateA},
			output:   candidateA + "\tnot facts",
			wantErr:  "two-column response must use NO_FACTS",
		},
		{
			name:     "three columns without fact separator",
			expected: []string{candidateA},
			output:   candidateA + "\tconvention\tworkflow.testing.rules",
			wantErr:  "three-column response must contain path",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDistillCandidateMemberResultsV2(tc.expected, tc.output)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprintf("%#v", got) != fmt.Sprintf("%#v", tc.want) {
				t.Fatalf("results = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestParseDistillCandidateMemberResultsV2CapsFactsPerCandidate(t *testing.T) {
	const candidateID = "candidate-a"
	lines := make([]string, 0, factsMaxPerChunk+1)
	for i := 0; i < factsMaxPerChunk+1; i++ {
		lines = append(lines, fmt.Sprintf("%s\tdecision\tarchitecture.boundaries.rationale\tfact %d", candidateID, i))
	}
	_, err := parseDistillCandidateMemberResultsV2([]string{candidateID}, strings.Join(lines, "\n"))
	if err == nil || !strings.Contains(err.Error(), "produced more than") {
		t.Fatalf("error = %v, want per-candidate cap failure", err)
	}
}

func TestParseDistillCandidateMemberResultsV2DoesNotApplyPackWideFactCap(t *testing.T) {
	const candidates = 3
	ids := make([]string, candidates)
	lines := make([]string, 0, candidates*factsMaxPerChunk)
	for candidate := 0; candidate < candidates; candidate++ {
		ids[candidate] = fmt.Sprintf("candidate-%d", candidate)
		for fact := 0; fact < factsMaxPerChunk; fact++ {
			lines = append(lines, fmt.Sprintf("%s\tdecision\tarchitecture.boundaries.rationale\tfact %d for candidate %d", ids[candidate], fact, candidate))
		}
	}
	got, err := parseDistillCandidateMemberResultsV2(ids, strings.Join(lines, "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != candidates {
		t.Fatalf("results = %d, want %d", len(got), candidates)
	}
	for _, result := range got {
		if len(result.Facts) != factsMaxPerChunk {
			t.Fatalf("candidate %q facts = %d, want %d", result.CandidateID, len(result.Facts), factsMaxPerChunk)
		}
	}
}

func TestParseDistillCandidateMemberResultsV2ToleratesOnlyTrailingRedundantNoFacts(t *testing.T) {
	id := "candidate-a"
	got, err := parseDistillCandidateMemberResultsV2([]string{id}, id+"\tconvention\tworkflow.testing.rules\tRun race tests.\n"+id+"\tNO_FACTS")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].NoFacts || len(got[0].Facts) != 1 {
		t.Fatalf("trailing redundant sentinel changed fact result: %+v", got)
	}
	if _, err := parseDistillCandidateMemberResultsV2([]string{id}, id+"\tNO_FACTS\n"+id+"\tconvention\tworkflow.testing.rules\tRun race tests."); err == nil {
		t.Fatal("leading NO_FACTS followed by a fact must remain a protocol failure")
	}
	if _, err := parseDistillCandidateMemberResultsV2([]string{id}, id+"\tconvention\tworkflow.testing.rules\tRun race tests.\n"+id+"\tNO_FACTS\n"+id+"\tgotcha\tworkflow.testing.rules\tA later fact."); err == nil {
		t.Fatal("a fact after the tolerated trailing sentinel must fail")
	}
}

func TestParseDistillCandidateMemberResultsOllamaV2TreatsOmissionAsNoFacts(t *testing.T) {
	const (
		candidateA = "candidate-a"
		candidateB = "candidate-b"
	)
	line := candidateB + "\tconvention\tworkflow.testing.rules\tAlways run race tests before merging."
	got, err := parseDistillCandidateMemberResultsOllamaV2([]string{candidateA, candidateB}, line)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].CandidateID != candidateA || !got[0].NoFacts || len(got[0].Facts) != 0 || got[1].CandidateID != candidateB || len(got[1].Facts) != 1 {
		t.Fatalf("Ollama omission results = %+v", got)
	}
	empty, err := parseDistillCandidateMemberResultsOllamaV2([]string{candidateA, candidateB}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 2 || !empty[0].NoFacts || !empty[1].NoFacts {
		t.Fatalf("blank Ollama completion was not conservative empty: %+v", empty)
	}
	if _, err := parseDistillCandidateMemberResultsOllamaV2([]string{candidateA}, "unknown\tNO_FACTS"); err == nil {
		t.Fatal("Ollama compatibility accepted an unknown candidate ID")
	}
	separated := candidateB + "\tgotcha\tconstraints.invariants.general\tA supported fact.\n\n" +
		candidateB + "\tconvention\tworkflow.testing.rules\tA second supported fact."
	got, err = parseDistillCandidateMemberResultsOllamaV2([]string{candidateA, candidateB}, separated)
	if err != nil {
		t.Fatal(err)
	}
	if !got[0].NoFacts || len(got[1].Facts) != 2 {
		t.Fatalf("Ollama blank-line compatibility changed attribution: %+v", got)
	}
	malformedExpected := strings.Join([]string{
		candidateA + "\tacceptance\tconstraints.invariants.general\tUnsupported kind.",
		candidateA + "\tdecision\tinvalid path\tUnsupported path.",
		candidateA + "\tnot a framed fact",
		candidateB + "\tconvention\tworkflow.testing.rules\tKeep the valid attributed fact.",
	}, "\n")
	got, err = parseDistillCandidateMemberResultsOllamaV2([]string{candidateA, candidateB}, malformedExpected)
	if err != nil {
		t.Fatal(err)
	}
	if !got[0].NoFacts || len(got[1].Facts) != 1 {
		t.Fatalf("Ollama malformed-line discard changed valid neighbor: %+v", got)
	}
	if _, err := parseDistillCandidateMemberResultsOllamaV2([]string{candidateA}, "unframed prose"); err == nil {
		t.Fatal("Ollama compatibility accepted unframed prose")
	}
}
