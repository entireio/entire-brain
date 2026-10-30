package cli

import (
	"bytes"
	"fmt"
	"github.com/entireio/entire-brain/internal/cli/internal/cmdutil"
	"github.com/entireio/entire-brain/internal/cli/internal/retrieval"
	"strings"
	"testing"
)

// The bounded transport functions replaced a one-step-at-a-time trim loop
// with a monotone search over the same trim sequence. These references are
// verbatim copies of the replaced loops; the tests below hold the shipped
// functions to byte-identical output so the search can never drift from the
// stepwise semantics.

func referenceBoundedRetrievalJSONPayload(query, branch string, results []unifiedResult, extras retrievalTransportExtras) (map[string]any, error) {
	allRows := retrieval.CompactResults(results, query)
	extras = cloneRetrievalTransportExtras(extras)
	count := len(allRows)
	anythingTruncated := false
	for {
		rows := append([]compactUnifiedResult{}, allRows[:count]...)
		truncated := anythingTruncated || count != len(allRows)
		payload := buildRetrievalJSONPayload(query, branch, rows, extras, truncated)
		data, err := cmdutil.JSONOutputBytes(payload)
		if err != nil {
			return nil, err
		}
		size := len(data)
		if size <= conversationConceptResponseMaxBytes {
			return payload, nil
		}
		if trimRetrievalExtrasOnce(&extras) {
			anythingTruncated = true
			continue
		}
		if count > 0 {
			count--
			anythingTruncated = true
			continue
		}
		break
	}
	return nil, fmt.Errorf("retrieval response envelope exceeds %d bytes without any result rows", conversationConceptResponseMaxBytes)
}

func referenceBoundedSingleRepoRetrievalText(query string, results []unifiedResult, extras retrievalTransportExtras) ([]byte, error) {
	extras = cloneRetrievalTransportExtras(extras)
	count := len(results)
	anythingTruncated := false
	for {
		truncated := anythingTruncated || count != len(results)
		data := renderSingleRepoRetrievalText(query, results[:count], extras, truncated)
		if len(data) <= conversationConceptResponseMaxBytes {
			return data, nil
		}
		if trimRetrievalExtrasOnce(&extras) {
			anythingTruncated = true
			continue
		}
		if count > 0 {
			count--
			anythingTruncated = true
			continue
		}
		break
	}
	return nil, fmt.Errorf("retrieval text envelope exceeds %d bytes without any result rows", conversationConceptResponseMaxBytes)
}

func referenceBoundedWorkspaceRetrievalJSONPayload(workspaceName, query string, groups []workspaceRetrieveResult) (map[string]any, error) {
	qualified := cloneWorkspaceRetrieveGroups(qualifyWorkspaceRetrieveGroups(workspaceName, groups))
	total := 0
	for _, group := range qualified {
		total += len(group.Results)
	}
	keep := total
	anythingTruncated := false
	for {
		truncated := anythingTruncated || keep != total
		payload := map[string]any{
			"workspace": workspaceName,
			"query":     query,
			"results":   trimWorkspaceGroupsToCount(qualified, keep, truncated),
		}
		if truncated {
			payload["response_truncated"] = true
		}
		data, err := cmdutil.JSONOutputBytes(payload)
		if err != nil {
			return nil, err
		}
		if len(data) <= conversationConceptResponseMaxBytes {
			return payload, nil
		}
		if trimWorkspaceExtrasOnce(qualified) {
			anythingTruncated = true
			continue
		}
		if keep > 0 {
			keep--
			anythingTruncated = true
			continue
		}
		if len(qualified) > 0 {
			qualified = qualified[:len(qualified)-1]
			anythingTruncated = true
			continue
		}
		break
	}
	return nil, fmt.Errorf("workspace retrieval response envelope exceeds %d bytes without any result rows", conversationConceptResponseMaxBytes)
}

func referenceBoundedWorkspaceRetrievalText(workspaceName string, groups []workspaceRetrieveResult) ([]byte, error) {
	groups = cloneWorkspaceRetrieveGroups(groups)
	total := 0
	for _, group := range groups {
		total += len(group.Results)
	}
	keep := total
	anythingTruncated := false
	for {
		data := renderWorkspaceRetrievalText(workspaceName, groups, keep, anythingTruncated || keep != total)
		if len(data) <= conversationConceptResponseMaxBytes {
			return data, nil
		}
		if trimWorkspaceExtrasOnce(groups) {
			anythingTruncated = true
			continue
		}
		if keep > 0 {
			keep--
			anythingTruncated = true
			continue
		}
		if len(groups) > 0 {
			groups = groups[:len(groups)-1]
			anythingTruncated = true
			continue
		}
		break
	}
	return nil, fmt.Errorf("workspace retrieval text envelope exceeds %d bytes without any result rows", conversationConceptResponseMaxBytes)
}

func jsonPayloadBytes(t *testing.T, payload map[string]any) []byte {
	t.Helper()
	data, err := cmdutil.JSONOutputBytes(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return data
}

func TestBoundedTruncationMatchesStepwiseReference(t *testing.T) {
	t.Parallel()
	// Three stop regimes: fits untrimmed, stops inside the extras phase, and
	// stops inside the row phase. largeCoverageResults rows are a few tens of
	// KB each, so 12 of them force deep row trimming against the 128 KiB
	// budget without a multi-megabyte fixture.
	small := largeCoverageResults(1)
	big := largeCoverageResults(12)
	for _, tc := range []struct {
		name    string
		results []unifiedResult
		extras  retrievalTransportExtras
	}{
		{"fits untrimmed", small[:1], retrievalTransportExtras{}},
		{"stops in extras", small[:1], largeRetrievalExtras(big)},
		{"stops in rows", big, largeRetrievalExtras(big)},
		{"zero rows fit", big, retrievalTransportExtras{BlindSpot: "nothing indexed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want, wantErr := referenceBoundedRetrievalJSONPayload("query", "main", tc.results, tc.extras)
			got, gotErr := boundedRetrievalJSONPayload("query", "main", tc.results, tc.extras)
			if (wantErr != nil) != (gotErr != nil) {
				t.Fatalf("json: err = %v, reference err = %v", gotErr, wantErr)
			}
			if wantErr == nil && !bytes.Equal(jsonPayloadBytes(t, want), jsonPayloadBytes(t, got)) {
				t.Fatalf("JSON payload diverged from stepwise reference")
			}

			wantText, wantTextErr := referenceBoundedSingleRepoRetrievalText("query", tc.results, tc.extras)
			gotText, gotTextErr := boundedSingleRepoRetrievalText("query", tc.results, tc.extras)
			if (wantTextErr != nil) != (gotTextErr != nil) {
				t.Fatalf("text: err = %v, reference err = %v", gotTextErr, wantTextErr)
			}
			if wantTextErr == nil && !bytes.Equal(wantText, gotText) {
				t.Fatalf("text output diverged from stepwise reference")
			}
		})
	}
}

func TestBoundedWorkspaceTruncationMatchesStepwiseReference(t *testing.T) {
	t.Parallel()
	rows := largeCoverageResults(6)
	// Untrimmable member errors big enough that dropping every row still
	// exceeds the budget, forcing the whole-group-drop phase the single-repo
	// surfaces do not have.
	hugeErr := strings.Repeat("provider exploded. ", 4_000)
	for _, tc := range []struct {
		name   string
		groups []workspaceRetrieveResult
	}{
		{"fits untrimmed", []workspaceRetrieveResult{{RepoKey: "gh/acme/one", Name: "one", Results: largeCoverageResults(1)}}},
		{"stops in rows", []workspaceRetrieveResult{
			{RepoKey: "gh/acme/one", Name: "one", Results: rows},
			{RepoKey: "gh/acme/two", Name: "two", Results: rows},
		}},
		{"stops in group drops", []workspaceRetrieveResult{
			{RepoKey: "gh/acme/one", Name: "one", Results: rows, Error: hugeErr},
			{RepoKey: "gh/acme/two", Name: "two", Results: rows, Error: hugeErr},
			{RepoKey: "gh/acme/three", Name: "three", Error: hugeErr},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want, wantErr := referenceBoundedWorkspaceRetrievalJSONPayload("product", "query", tc.groups)
			got, gotErr := boundedWorkspaceRetrievalJSONPayload("product", "query", tc.groups)
			if (wantErr != nil) != (gotErr != nil) {
				t.Fatalf("json: err = %v, reference err = %v", gotErr, wantErr)
			}
			if wantErr == nil && !bytes.Equal(jsonPayloadBytes(t, want), jsonPayloadBytes(t, got)) {
				t.Fatalf("workspace JSON payload diverged from stepwise reference")
			}

			wantText, wantErr := referenceBoundedWorkspaceRetrievalText("product", tc.groups)
			gotText, gotErr := boundedWorkspaceRetrievalText("product", tc.groups)
			if (wantErr != nil) != (gotErr != nil) {
				t.Fatalf("text: err = %v, reference err = %v", gotErr, wantErr)
			}
			if wantErr == nil && !bytes.Equal(wantText, gotText) {
				t.Fatalf("workspace text output diverged from stepwise reference")
			}
		})
	}
}

func TestSmallestTrimStepsThatFitMatchesLinearScan(t *testing.T) {
	t.Parallel()
	budget := conversationConceptResponseMaxBytes
	for _, tc := range []struct {
		name  string
		sizes []int // sizes[k-1] is the rendering size after k steps
	}{
		{"first step fits", []int{budget}},
		{"middle step fits", []int{budget + 3, budget + 2, budget, budget - 1}},
		{"plateau around the boundary", []int{budget + 1, budget + 1, budget, budget, budget}},
		{"only the last step fits", []int{budget + 2, budget + 1, budget}},
		{"nothing fits", []int{budget + 2, budget + 1}},
		{"no steps available", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sizeAt := func(steps int) (int, error) { return tc.sizes[steps-1], nil }
			wantSteps, wantFound := 0, false
			for k := 1; k <= len(tc.sizes); k++ {
				if tc.sizes[k-1] <= budget {
					wantSteps, wantFound = k, true
					break
				}
			}
			steps, found, err := smallestTrimStepsThatFit(len(tc.sizes), sizeAt)
			if err != nil {
				t.Fatal(err)
			}
			if found != wantFound || steps != wantSteps {
				t.Fatalf("search = (%d, %t), linear scan = (%d, %t)", steps, found, wantSteps, wantFound)
			}
		})
	}
}
