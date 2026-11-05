package cli

import (
	"testing"
)

func TestSampleAcrossStrataRoundRobinsStableRepresentatives(t *testing.T) {
	var tasks []evalTask
	for _, stratum := range []string{queryTypeCode, queryTypeConcept, queryTypeHowto, queryTypeConvention} {
		for i := 0; i < 2; i++ {
			tasks = append(tasks, evalTask{ID: stratum + "-" + string(rune('a'+i)), Task: "task " + stratum, QueryType: stratum})
		}
	}
	one := sampleAcrossStrata(tasks, 5)
	two := sampleAcrossStrata(tasks, 5)
	if len(one) != 5 || len(two) != 5 {
		t.Fatalf("sample lengths = %d, %d; want 5", len(one), len(two))
	}
	seen := map[string]bool{}
	strata := map[string]int{}
	for i, task := range one {
		if task.ID == "" || seen[task.ID] {
			t.Fatalf("sample contains duplicate or empty task at %d: %+v", i, one)
		}
		seen[task.ID] = true
		strata[task.QueryType]++
		if i > 0 && one[i-1].ID >= task.ID {
			t.Fatalf("sample IDs are not stable sorted: %+v", one)
		}
		if task.Task == "" {
			t.Fatalf("sample lost task label: %+v", task)
		}
	}
	if len(strata) != 4 || strata[queryTypeCode] != 2 || strata[queryTypeConcept] != 1 || strata[queryTypeHowto] != 1 || strata[queryTypeConvention] != 1 {
		t.Fatalf("sample did not cover strata round-robin: %+v", strata)
	}
	for i := range one {
		if one[i].ID != two[i].ID {
			t.Fatalf("sampling is not deterministic: first=%+v second=%+v", one, two)
		}
	}
}
