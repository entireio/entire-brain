package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// The MCP tool schemas advertise "maximum": 10000 on every limit and depth
// argument so a schema-validating client can plan a valid call. Against a real
// repository (~48k symbols, ~155k relations) a call at that advertised ceiling
// hard-failed for ten of the seventeen limit-taking tools -- brain_impact at
// 33 MB, brain_query_graph at 8 MB, brain_brief at 7 MB after 34 seconds of
// work -- and the refusal never named a limit that would have fit, so recovery
// was a binary search. Tools that squeaked under the transport's frame limit
// were no better: brain_dead_code returned 3,401,455 bytes, roughly 850k
// tokens, as an unflagged success.
//
// These tests pin the contract these fixes establish: the advertised ceiling is
// reachable, an oversized answer degrades by dropping whole rows, and it says
// in the document that it did.

// mcpRowsPayload builds a tool-shaped document: `rows` rows under `field`,
// each row padded so the document is comfortably over the response budget but
// under the transport frame limit -- exactly brain_dead_code's failure mode.
func mcpRowsPayload(t *testing.T, field string, rows int) string {
	t.Helper()
	list := make([]any, rows)
	for i := range list {
		list[i] = map[string]any{
			"id":   fmt.Sprintf("symbol:%d", i),
			"name": fmt.Sprintf("Symbol%d", i),
			"path": strings.Repeat("internal/cli/", 8) + fmt.Sprintf("file%d.go", i),
			"doc":  strings.Repeat("x", 256),
		}
	}
	data, err := json.MarshalIndent(map[string]any{field: list}, "", "  ")
	if err != nil {
		t.Fatalf("build payload: %v", err)
	}
	return string(data) + "\n"
}

func mcpDecodeToolText(t *testing.T, result map[string]any) (string, map[string]any) {
	t.Helper()
	content, ok := result["content"].([]map[string]any)
	if !ok || len(content) != 1 {
		t.Fatalf("result shape changed: %#v", result)
	}
	text, _ := content[0]["text"].(string)
	var doc map[string]any
	if err := json.Unmarshal([]byte(text), &doc); err != nil {
		t.Fatalf("result text is not a JSON object: %v", err)
	}
	return text, doc
}

// TestAnOversizedRowResultTruncatesInsteadOfFailingAtTheAdvertisedCeiling is
// the blocker. Before the fix this document came back whole and unflagged
// (under the 4 MiB frame limit) or, one order of magnitude up, as a -32000 the
// caller could not act on. Neither is a contract a client can plan against.
func TestAnOversizedRowResultTruncatesInsteadOfFailingAtTheAdvertisedCeiling(t *testing.T) {
	t.Parallel()

	payload := mcpRowsPayload(t, "symbols", 4000)
	if len(payload) <= mcpToolResponseMaxBytes {
		t.Fatalf("test payload %d bytes is not over the %d byte budget", len(payload), mcpToolResponseMaxBytes)
	}
	if len(payload) >= maxMCPFrameBytes {
		t.Fatalf("test payload %d bytes must stay under the frame limit to prove the budget, not the frame, is doing the work", len(payload))
	}

	result, err := mcpToolTextResult(context.Background(), "brain_dead_code", payload)
	if err != nil {
		t.Fatalf("a reducible result was refused instead of truncated: %v", err)
	}
	text, doc := mcpDecodeToolText(t, result)

	size, ok := mcpToolFrameSize(context.Background(), text)
	if !ok {
		t.Fatal("could not measure the returned frame")
	}
	if size > mcpToolResponseMaxBytes {
		t.Fatalf("returned frame is %d bytes, over the %d byte budget", size, mcpToolResponseMaxBytes)
	}
	if doc[mcpResponseTruncatedKey] != true {
		t.Fatalf("a truncated result is not flagged: %v", doc[mcpResponseTruncatedKey])
	}
	rows, _ := doc["symbols"].([]any)
	if len(rows) == 0 {
		t.Fatal("truncation emptied the result instead of degrading it")
	}
	if len(rows) >= 4000 {
		t.Fatalf("nothing was dropped: %d rows", len(rows))
	}
}

// TestTruncationNamesWhatItDropped is the one-step-recovery property: the
// caller must learn from the response itself how much it is missing and from
// where, without a second call to find out.
func TestTruncationNamesWhatItDropped(t *testing.T) {
	t.Parallel()

	result, err := mcpToolTextResult(context.Background(), "brain_dead_code", mcpRowsPayload(t, "symbols", 4000))
	if err != nil {
		t.Fatalf("mcpToolTextResult: %v", err)
	}
	_, doc := mcpDecodeToolText(t, result)

	report, ok := doc[mcpResponseTruncationKey].(map[string]any)
	if !ok {
		t.Fatalf("no %s report: %#v", mcpResponseTruncationKey, doc)
	}
	if budget, _ := report["budget_bytes"].(float64); int(budget) != mcpToolResponseMaxBytes {
		t.Errorf("report budget_bytes = %v, want %d", report["budget_bytes"], mcpToolResponseMaxBytes)
	}
	dropped, _ := report["dropped_from"].([]any)
	if len(dropped) != 1 {
		t.Fatalf("dropped_from = %#v, want exactly the one trimmed list", dropped)
	}
	entry, _ := dropped[0].(map[string]any)
	if entry["path"] != "symbols" {
		t.Errorf("dropped_from path = %v, want \"symbols\"", entry["path"])
	}
	returned, _ := entry["returned"].(float64)
	total, _ := entry["total"].(float64)
	if int(total) != 4000 {
		t.Errorf("dropped_from total = %v, want the 4000 rows the tool produced", entry["total"])
	}
	rows, _ := doc["symbols"].([]any)
	if int(returned) != len(rows) {
		t.Errorf("dropped_from returned = %v but the document carries %d rows", entry["returned"], len(rows))
	}
	if int(returned) >= int(total) {
		t.Errorf("returned %v of %v is not a truncation", entry["returned"], entry["total"])
	}
}

// TestTruncationDropsWholeRowsAndNeverRewritesOne. A shortened row is worse
// than a missing one: the caller cannot tell an abridged record from a real
// one. Every returned row must be byte-identical to the row the tool produced.
func TestTruncationDropsWholeRowsAndNeverRewritesOne(t *testing.T) {
	t.Parallel()

	payload := mcpRowsPayload(t, "symbols", 4000)
	var before map[string]any
	if err := json.Unmarshal([]byte(payload), &before); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	originals, _ := before["symbols"].([]any)

	result, err := mcpToolTextResult(context.Background(), "brain_dead_code", payload)
	if err != nil {
		t.Fatalf("mcpToolTextResult: %v", err)
	}
	_, doc := mcpDecodeToolText(t, result)
	rows, _ := doc["symbols"].([]any)
	if len(rows) == 0 {
		t.Fatal("no rows survived")
	}
	for i, row := range rows {
		want, _ := json.Marshal(originals[i])
		got, _ := json.Marshal(row)
		if string(got) != string(want) {
			t.Fatalf("row %d was rewritten:\n got %s\nwant %s", i, got, want)
		}
	}
}

// TestTruncationRepairsTheCountsBesideTheRowsItDropped. brain_code ships a
// pagination block whose count states how many rows are in the document.
// Trimming rows without repairing it would replace one dishonest contract with
// another.
func TestTruncationRepairsTheCountsBesideTheRowsItDropped(t *testing.T) {
	t.Parallel()

	var doc map[string]any
	if err := json.Unmarshal([]byte(mcpRowsPayload(t, "results", 4000)), &doc); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	doc["pagination"] = map[string]any{"limit": 10000, "offset": 0, "count": 4000}
	payload, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	result, resErr := mcpToolTextResult(context.Background(), "brain_code", string(payload))
	if resErr != nil {
		t.Fatalf("mcpToolTextResult: %v", resErr)
	}
	_, out := mcpDecodeToolText(t, result)
	rows, _ := out["results"].([]any)
	pagination, _ := out["pagination"].(map[string]any)
	count, _ := pagination["count"].(float64)
	if int(count) != len(rows) {
		t.Fatalf("pagination.count = %v beside %d rows", pagination["count"], len(rows))
	}
	if limit, _ := pagination["limit"].(float64); int(limit) != 10000 {
		t.Errorf("pagination.limit = %v; the requested limit is still true and must not be rewritten", pagination["limit"])
	}
}

// TestNestedRowListsAreTruncated. brain_impact, brain_context, brain_tests and
// brain_boundaries carry their rows one level down (impact.relations,
// boundary.handlers). brain_impact is the worst offender on the surface -- 33 MB
// at the advertised ceiling -- so a trimmer that only saw top-level arrays would
// miss the tool that needs it most.
func TestNestedRowListsAreTruncated(t *testing.T) {
	t.Parallel()

	var inner map[string]any
	if err := json.Unmarshal([]byte(mcpRowsPayload(t, "relations", 4000)), &inner); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	payload, err := json.MarshalIndent(map[string]any{
		"freshness": map[string]any{"state": "current"},
		"impact":    inner,
	}, "", "  ")
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	result, resErr := mcpToolTextResult(context.Background(), "brain_impact", string(payload))
	if resErr != nil {
		t.Fatalf("a nested row list was refused instead of truncated: %v", resErr)
	}
	text, doc := mcpDecodeToolText(t, result)
	if size, _ := mcpToolFrameSize(context.Background(), text); size > mcpToolResponseMaxBytes {
		t.Fatalf("returned frame is %d bytes, over the %d byte budget", size, mcpToolResponseMaxBytes)
	}
	if doc[mcpResponseTruncatedKey] != true {
		t.Fatal("nested truncation is not flagged at the envelope")
	}
	impact, _ := doc["impact"].(map[string]any)
	rows, _ := impact["relations"].([]any)
	if len(rows) == 0 || len(rows) >= 4000 {
		t.Fatalf("impact.relations has %d rows, want a non-empty proper subset", len(rows))
	}
	report, _ := doc[mcpResponseTruncationKey].(map[string]any)
	dropped, _ := report["dropped_from"].([]any)
	entry, _ := dropped[0].(map[string]any)
	if entry["path"] != "impact.relations" {
		t.Errorf("dropped_from path = %v, want the nested path", entry["path"])
	}
	if freshness, ok := doc["freshness"].(map[string]any); !ok || freshness["state"] != "current" {
		t.Errorf("non-row metadata was disturbed: %#v", doc["freshness"])
	}
}

// TestATopLevelArrayResultCarriesTheMarkerOnItsLastRow. brain_patterns returns
// a bare JSON array. There is no envelope to flag, so the marker goes where the
// retrieval surface already puts its row marker: on the last row that survived.
func TestATopLevelArrayResultCarriesTheMarkerOnItsLastRow(t *testing.T) {
	t.Parallel()

	var doc map[string]any
	if err := json.Unmarshal([]byte(mcpRowsPayload(t, "rows", 4000)), &doc); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	payload, err := json.MarshalIndent(doc["rows"], "", "  ")
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	result, resErr := mcpToolTextResult(context.Background(), "brain_patterns", string(payload))
	if resErr != nil {
		t.Fatalf("a top-level array result was refused instead of truncated: %v", resErr)
	}
	content, _ := result["content"].([]map[string]any)
	text, _ := content[0]["text"].(string)
	var rows []any
	if err := json.Unmarshal([]byte(text), &rows); err != nil {
		t.Fatalf("the array root was reshaped into something else: %v", err)
	}
	if len(rows) == 0 || len(rows) >= 4000 {
		t.Fatalf("array has %d rows, want a non-empty proper subset", len(rows))
	}
	last, _ := rows[len(rows)-1].(map[string]any)
	if last[mcpResponseTruncatedKey] != true {
		t.Fatalf("the last surviving row does not carry the marker: %#v", last)
	}
	// Exactly one row may carry the marker; a stale marker left on a row
	// dropped-past in an earlier round would lie about where the cut is.
	marked := 0
	for _, row := range rows {
		if row, ok := row.(map[string]any); ok && row[mcpResponseTruncatedKey] == true {
			marked++
		}
	}
	if marked != 1 {
		t.Fatalf("%d rows carry the truncation marker, want exactly the last one", marked)
	}
}

// TestTheMarkerIsInsideEveryMeasurement. boundedRetrievalJSONPayload's
// invariant, carried over: if the marker were stamped after the last size
// check, the marker itself could be what pushes the document back over budget.
func TestTheMarkerIsInsideEveryMeasurement(t *testing.T) {
	t.Parallel()

	for _, rows := range []int{600, 4000, 20000} {
		result, err := mcpToolTextResult(context.Background(), "brain_code", mcpRowsPayload(t, "results", rows))
		if err != nil {
			t.Fatalf("%d rows: %v", rows, err)
		}
		text, doc := mcpDecodeToolText(t, result)
		if doc[mcpResponseTruncatedKey] != true {
			t.Fatalf("%d rows: result is not flagged", rows)
		}
		size, ok := mcpToolFrameSize(context.Background(), text)
		if !ok {
			t.Fatalf("%d rows: could not measure the frame", rows)
		}
		if size > mcpToolResponseMaxBytes {
			t.Fatalf("%d rows: flagged frame is %d bytes, over the %d byte budget", rows, size, mcpToolResponseMaxBytes)
		}
	}
}

// TestAResultThatFitsIsReturnedByteForByte. The budget must be invisible to
// every ordinary call: no re-serialization, no reordered keys, no marker.
func TestAResultThatFitsIsReturnedByteForByte(t *testing.T) {
	t.Parallel()

	payload := mcpRowsPayload(t, "symbols", 20)
	if len(payload) > mcpToolResponseMaxBytes {
		t.Fatalf("fixture is %d bytes, not a small result", len(payload))
	}
	result, err := mcpToolTextResult(context.Background(), "brain_dead_code", payload)
	if err != nil {
		t.Fatalf("a small result was refused: %v", err)
	}
	content, _ := result["content"].([]map[string]any)
	if content[0]["text"] != payload {
		t.Fatal("a result within budget was rewritten")
	}
}

// TestTheResponseBudgetIsTheRetrievalBudgetNotAnArbitraryNumber pins the two
// agent-facing budgets together and both well under the transport frame, so
// neither can drift back toward "whatever the wire will carry".
func TestTheResponseBudgetIsTheRetrievalBudgetNotAnArbitraryNumber(t *testing.T) {
	t.Parallel()

	if mcpToolResponseMaxBytes != conversationConceptResponseMaxBytes {
		t.Errorf("MCP tool budget %d and retrieval budget %d disagree; one surface would truncate where the other does not",
			mcpToolResponseMaxBytes, conversationConceptResponseMaxBytes)
	}
	if mcpToolResponseMaxBytes >= maxMCPFrameBytes {
		t.Fatalf("response budget %d is not under the %d byte frame limit", mcpToolResponseMaxBytes, maxMCPFrameBytes)
	}
	// A budget anywhere near the frame limit is a budget in name only: 4 MiB of
	// JSON is roughly a million tokens, which no caller can consume.
	if mcpToolResponseMaxBytes > maxMCPFrameBytes/8 {
		t.Fatalf("response budget %d is close enough to the %d byte frame limit to be no budget at all",
			mcpToolResponseMaxBytes, maxMCPFrameBytes)
	}
}

// TestEverySizeArgumentDeclaresTheTruncationContract. The advertised maximum
// and the behaviour have to agree on the schema a client reads, not only in the
// server. A ceiling that is reachable only because the answer degrades is worth
// saying out loud.
func TestEverySizeArgumentDeclaresTheTruncationContract(t *testing.T) {
	t.Parallel()

	seen := 0
	for _, tool := range mcpToolDefinitions() {
		name, _ := tool["name"].(string)
		schema, _ := tool["inputSchema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		for _, key := range []string{"limit", "depth"} {
			arg, ok := props[key].(map[string]any)
			if !ok {
				continue
			}
			if maximum, _ := arg["maximum"].(int); maximum != mcpIntegerArgMax {
				// brain_get's outline limit has its own downstream ceiling and
				// is paginated, not truncated.
				continue
			}
			seen++
			description, _ := arg["description"].(string)
			if !strings.Contains(description, mcpResponseTruncatedKey) {
				t.Errorf("%s.%s advertises maximum %d without saying what a result at that maximum does: %q",
					name, key, mcpIntegerArgMax, description)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no size arguments found; the walk is broken, not the schema")
	}
}

// TestAnUnreducibleOversizeResultIsStillRefusedActionably. Truncation is for
// documents made of rows. A result with nothing to drop -- non-JSON, or an
// envelope that is itself over budget -- must still fail loudly rather than be
// handed to a peer that cannot frame it.
func TestAnUnreducibleOversizeResultIsStillRefusedActionably(t *testing.T) {
	t.Parallel()

	for name, text := range map[string]string{
		"non-JSON":          strings.Repeat("x", mcpToolResponseMaxBytes*2),
		"no rows":           `{"blob":"` + strings.Repeat("x", mcpToolResponseMaxBytes*2) + `"}`,
		"oversize envelope": `{"blob":"` + strings.Repeat("x", mcpToolResponseMaxBytes*2) + `","rows":[{"id":1}]}`,
	} {
		_, err := mcpToolTextResult(context.Background(), "brain_status", text)
		if err == nil {
			t.Fatalf("%s: an oversize result with nothing to drop was returned", name)
		}
		for _, want := range []string{"brain_status", "MCP response budget", "narrow the request"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("%s: refusal is not actionable, missing %q: %v", name, want, err)
			}
		}
	}
}

// TestARowTooLargeToCarryDegradesToAFlaggedEmptyList. One row bigger than the
// whole budget cannot be delivered, and the honest answer is the retrieval
// surface's: an empty list that says 0 of 1, not a silent success and not a
// frame the peer drops.
func TestARowTooLargeToCarryDegradesToAFlaggedEmptyList(t *testing.T) {
	t.Parallel()

	text := `{"rows":[{"blob":"` + strings.Repeat("x", mcpToolResponseMaxBytes*2) + `"}]}`
	result, err := mcpToolTextResult(context.Background(), "brain_code", text)
	if err != nil {
		t.Fatalf("mcpToolTextResult: %v", err)
	}
	_, doc := mcpDecodeToolText(t, result)
	if rows, _ := doc["rows"].([]any); len(rows) != 0 {
		t.Fatalf("rows = %d, want the unshippable row dropped", len(rows))
	}
	if doc[mcpResponseTruncatedKey] != true {
		t.Fatal("an emptied result is not flagged")
	}
	report, _ := doc[mcpResponseTruncationKey].(map[string]any)
	dropped, _ := report["dropped_from"].([]any)
	entry, _ := dropped[0].(map[string]any)
	if returned, _ := entry["returned"].(float64); int(returned) != 0 {
		t.Errorf("dropped_from returned = %v, want 0", entry["returned"])
	}
	if total, _ := entry["total"].(float64); int(total) != 1 {
		t.Errorf("dropped_from total = %v, want 1", entry["total"])
	}
}

// TestTruncationDoesNotStarveOneListToFillAnother.
//
// brain_impact at the advertised ceiling carries 84,022 relations beside 10,000
// symbols. Filling the widest list first left relations at zero rows -- not one
// relation fits beside ten thousand symbols -- so an impact answer came back
// with no impact in it. Every list is held to one shared row cap instead.
func TestTruncationDoesNotStarveOneListToFillAnother(t *testing.T) {
	t.Parallel()

	var wide, narrow map[string]any
	if err := json.Unmarshal([]byte(mcpRowsPayload(t, "relations", 8000)), &wide); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if err := json.Unmarshal([]byte(mcpRowsPayload(t, "symbols", 2000)), &narrow); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	payload, err := json.MarshalIndent(map[string]any{"impact": map[string]any{
		"relations": wide["relations"],
		"symbols":   narrow["symbols"],
	}}, "", "  ")
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	result, resErr := mcpToolTextResult(context.Background(), "brain_impact", string(payload))
	if resErr != nil {
		t.Fatalf("mcpToolTextResult: %v", resErr)
	}
	_, doc := mcpDecodeToolText(t, result)
	impact, _ := doc["impact"].(map[string]any)
	relations, _ := impact["relations"].([]any)
	symbols, _ := impact["symbols"].([]any)
	if len(relations) == 0 {
		t.Fatalf("the widest list was starved to zero rows while symbols kept %d", len(symbols))
	}
	if len(symbols) == 0 {
		t.Fatalf("the narrower list was starved to zero rows while relations kept %d", len(relations))
	}
	// One shared cap, so neither list may run away with the budget.
	if len(relations) != len(symbols) {
		t.Errorf("relations kept %d rows and symbols %d; a shared cap keeps them level", len(relations), len(symbols))
	}
}

// TestTheServerStatesTheResponseBudgetOnce. The full contract belongs in the
// one place MCP defines for surface-wide guidance, not repeated across
// twenty-two tool schemas where it would cost more than it tells.
func TestTheServerStatesTheResponseBudgetOnce(t *testing.T) {
	t.Parallel()

	for _, want := range []string{mcpResponseTruncatedKey, mcpResponseTruncationKey, "returned", "total"} {
		if !strings.Contains(mcpServerInstructions, want) {
			t.Errorf("server instructions never mention %q: %q", want, mcpServerInstructions)
		}
	}
	if !strings.Contains(mcpServerInstructions, fmt.Sprint(mcpToolResponseMaxBytes)) {
		t.Errorf("server instructions do not name the %d byte budget: %q", mcpToolResponseMaxBytes, mcpServerInstructions)
	}
}

// TestInitializeCarriesTheResponseBudgetInstructions proves the wiring, not
// just the constant: a client that reads the handshake learns the contract.
func TestInitializeCarriesTheResponseBudgetInstructions(t *testing.T) {
	input := frameMCP(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	var out bytes.Buffer
	if err := runMCP((&cobra.Command{}).Context(), strings.NewReader(input), &out, Options{Version: "test-version"}); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	responses := readMCPResponses(t, out.String())
	result, _ := responses[0]["result"].(map[string]any)
	instructions, _ := result["instructions"].(string)
	if !strings.Contains(instructions, mcpResponseTruncatedKey) {
		t.Fatalf("initialize does not advertise the response budget: %#v", result)
	}
}

// TestAnEmptiedArrayRootIsRefusedNotReturnedBlank. An array root carries the
// marker on its last row, so an emptied one has nowhere to say it was
// truncated. Returning "[]" there would be the silent fragment this whole
// change exists to prevent.
func TestAnEmptiedArrayRootIsRefusedNotReturnedBlank(t *testing.T) {
	t.Parallel()

	text := `[{"blob":"` + strings.Repeat("x", mcpToolResponseMaxBytes*2) + `"}]`
	result, err := mcpToolTextResult(context.Background(), "brain_patterns", text)
	if err == nil {
		content, _ := result["content"].([]map[string]any)
		t.Fatalf("an unmarkable empty array was returned instead of refused: %v", content[0]["text"])
	}
	if !strings.Contains(err.Error(), "narrow the request") {
		t.Fatalf("refusal is not actionable: %v", err)
	}
}
