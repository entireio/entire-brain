package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// oversizedWorkspaceGroups returns a healthy member whose rows alone blow the
// 128 KiB response budget, plus a member whose retrieval failed and therefore
// carries an error with zero rows.
func oversizedWorkspaceGroups() []workspaceRetrieveResult {
	const failure = "load history index: parse history/index.json: unexpected end of JSON input"
	body := strings.Repeat("ingest rollout evidence ", 20) // ~480 B per row
	rows := make([]unifiedResult, 0, 600)
	for i := 0; i < 600; i++ {
		rows = append(rows, unifiedResult{
			Source: "history",
			ID:     fmt.Sprintf("history:%08d", i),
			Path:   "sessions/main/20260801T000000Z_session.jsonl",
			Text:   body,
		})
	}
	return []workspaceRetrieveResult{
		{RepoKey: "acme/healthy", Name: "healthy", Results: rows},
		{RepoKey: "acme/broken", Name: "broken", Results: []unifiedResult{}, Error: failure},
	}
}

const workspaceMemberFailure = "load history index: parse history/index.json: unexpected end of JSON input"

// TestBoundedWorkspaceRetrievalJSONKeepsMemberErrors pins the truncation
// honesty contract for workspace retrieval.
//
// A member whose retrieval failed is reported as {"results": [], "error": ...}.
// When the response budget binds, the trimmer must never resolve the overage by
// deleting that error: the member then serializes as an empty result list, which
// is byte-identical to a healthy member that simply had no matches. The
// top-level response_truncated marker says rows were dropped, not that a
// failure was suppressed, so the caller reads a failed repo as "searched, no
// hits" — a wrong answer with no error. Ranked rows are the cheap thing to
// drop; a member's failure is not.
func TestBoundedWorkspaceRetrievalJSONKeepsMemberErrors(t *testing.T) {
	t.Parallel()
	payload, err := boundedWorkspaceRetrievalJSONPayload("acme", "ingest rollout", oversizedWorkspaceGroups())
	if err != nil {
		t.Fatalf("bounded workspace payload: %v", err)
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > conversationConceptResponseMaxBytes {
		t.Fatalf("payload is %d bytes, over the %d budget", len(data), conversationConceptResponseMaxBytes)
	}
	if payload["response_truncated"] != true {
		t.Fatalf("expected response_truncated on a trimmed payload, got %v", payload["response_truncated"])
	}
	groups, ok := payload["results"].([]workspaceRetrieveResult)
	if !ok {
		t.Fatalf("results has unexpected type %T", payload["results"])
	}
	for _, group := range groups {
		if group.RepoKey != "acme/broken" {
			continue
		}
		if group.Error != workspaceMemberFailure {
			t.Fatalf("member error was dropped to fit the budget: error=%q results=%d; "+
				"the failed member is now indistinguishable from one with no matches",
				group.Error, len(group.Results))
		}
		return
	}
	t.Fatal("the failed member group is missing from the bounded payload")
}

// TestBoundedWorkspaceRetrievalTextKeepsMemberErrors is the same contract on
// the text surface, which renders "<repo> error <detail>" per member.
func TestBoundedWorkspaceRetrievalTextKeepsMemberErrors(t *testing.T) {
	t.Parallel()
	data, err := boundedWorkspaceRetrievalText("acme", oversizedWorkspaceGroups())
	if err != nil {
		t.Fatalf("bounded workspace text: %v", err)
	}
	if len(data) > conversationConceptResponseMaxBytes {
		t.Fatalf("text is %d bytes, over the %d budget", len(data), conversationConceptResponseMaxBytes)
	}
	if !strings.Contains(string(data), "acme/broken error "+workspaceMemberFailure) {
		t.Fatalf("member error was dropped to fit the budget; rendered text:\n%s", truncateString(string(data), 400))
	}
}
