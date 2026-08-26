package cli

import (
	"strings"
	"testing"
)

func qualityPanelPacketForTest() distillQualityPanelPacketV1 {
	item := distillQualityPanelItemV1{ID: "item_1", Kind: "candidate", Text: "Keep write operations explicit."}
	item.Digest = distillQualityPanelItemDigestV1(item)
	prompt := distillQualityPanelPromptV1{ID: "phase2_panel", Version: "v1", Instructions: "Assess the redacted item only."}
	prompt.Digest = distillQualityPanelPromptDigestV1(prompt)
	fragment := distillQualityPanelFragmentV1{ID: "evidence_1", Text: "The user requested explicit write operations."}
	fragment.Digest = distillQualityPanelFragmentDigestV1(fragment)
	payload := distillQualityPanelPayloadV1{Fragments: []distillQualityPanelFragmentV1{fragment}}
	payload.Digest = distillQualityPanelPayloadDigestV1(payload)
	packet := distillQualityPanelPacketV1{SchemaVersion: distillQualityPanelSchemaVersion, PacketID: "packet_1", Item: item, Prompt: prompt, Payload: payload}
	packet.PacketDigest = distillQualityPanelPacketDigestV1(packet)
	return packet
}

func qualityPanelVerdictForTest(packet distillQualityPanelPacketV1, judge distillQualityJudgeV1, label distillQualityAdvisoryLabelV1) distillQualityPanelVerdictV1 {
	return distillQualityPanelVerdictV1{SchemaVersion: distillQualityPanelSchemaVersion, PacketID: packet.PacketID, PacketDigest: packet.PacketDigest, ItemDigest: packet.Item.Digest, PromptDigest: packet.Prompt.Digest, PayloadDigest: packet.Payload.Digest, Judge: judge, Model: "judge_v1", State: distillQualityVerdictCompletedV1, Scores: []distillQualityPanelScoreV1{{Dimension: distillQualityDimensionAdmissionV1, Label: label, RawScore: []byte(`{"score":"high"}`), Rationale: "The item is explicit."}}}
}

func TestDistillQualityPanelPacketAndVerdictValidation(t *testing.T) {
	packet := qualityPanelPacketForTest()
	if err := validateDistillQualityPanelPacketV1(packet); err != nil {
		t.Fatal(err)
	}
	verdict := qualityPanelVerdictForTest(packet, distillQualityJudgeCopilotV1, distillQualityAdvisoryPassV1)
	if err := validateDistillQualityPanelVerdictV1(packet, verdict); err != nil {
		t.Fatal(err)
	}
	if string(verdict.Scores[0].RawScore) != `{"score":"high"}` {
		t.Fatal("raw score was not retained")
	}

	verdict.ItemDigest = "sha256:" + strings.Repeat("0", 64)
	if err := validateDistillQualityPanelVerdictV1(packet, verdict); err == nil {
		t.Fatal("accepted mismatched digest")
	}
	packet.Item.Text = "mutated"
	if err := validateDistillQualityPanelPacketV1(packet); err == nil {
		t.Fatal("accepted mismatched item digest")
	}
}

func TestDistillQualityPanelAggregationFlagsWithoutAveraging(t *testing.T) {
	packet := qualityPanelPacketForTest()
	pass := qualityPanelVerdictForTest(packet, distillQualityJudgeCopilotV1, distillQualityAdvisoryPassV1)
	critical := qualityPanelVerdictForTest(packet, distillQualityJudgeClaudeV1, distillQualityAdvisoryCriticalV1)
	agg, err := aggregateDistillQualityPanelV1(packet, []distillQualityPanelVerdictV1{pass, critical})
	if err != nil {
		t.Fatal(err)
	}
	if !agg.Disagreement || !agg.Critical || agg.Invalid {
		t.Fatalf("aggregate flags = %+v", agg)
	}
	if len(agg.Dimensions) != 1 || len(agg.Dimensions[0].Signals) != 2 {
		t.Fatalf("aggregate = %+v", agg)
	}
	if len(agg.MissingJudges) != 1 || agg.MissingJudges[0] != distillQualityJudgeCursorV1 {
		t.Fatalf("missing = %+v", agg.MissingJudges)
	}
	if _, err := aggregateDistillQualityPanelV1(packet, []distillQualityPanelVerdictV1{pass, pass}); err == nil {
		t.Fatal("accepted duplicate judge")
	}
}

func TestDistillQualityPanelAllowsAbstainAndInvalid(t *testing.T) {
	packet := qualityPanelPacketForTest()
	for _, state := range []distillQualityVerdictStateV1{distillQualityVerdictAbstainV1, distillQualityVerdictInvalidV1} {
		verdict := qualityPanelVerdictForTest(packet, distillQualityJudgeCursorV1, distillQualityAdvisoryPassV1)
		verdict.State, verdict.Scores, verdict.Rationale = state, nil, "Provider could not assess the redacted evidence."
		if err := validateDistillQualityPanelVerdictV1(packet, verdict); err != nil {
			t.Fatalf("%s: %v", state, err)
		}
	}
}

func TestDistillQualityPanelStrictJSONAndHumanOnlyProofLabels(t *testing.T) {
	packet := qualityPanelPacketForTest()
	data := []byte(`{"schema_version":1,"packet_id":"packet_1","packet_digest":"` + packet.PacketDigest + `","item":{"id":"item_1","kind":"candidate","text":"Keep write operations explicit.","digest":"` + packet.Item.Digest + `"},"prompt":{"id":"phase2_panel","version":"v1","instructions":"Assess the redacted item only.","digest":"` + packet.Prompt.Digest + `"},"payload":{"fragments":[{"id":"evidence_1","text":"The user requested explicit write operations.","digest":"` + packet.Payload.Fragments[0].Digest + `"}],"digest":"` + packet.Payload.Digest + `"},"session_id":"must-be-rejected"}`)
	if _, err := decodeDistillQualityPanelPacketJSONV1(data); err == nil {
		t.Fatal("accepted unknown session_id")
	}

	verdict := qualityPanelVerdictForTest(packet, distillQualityJudgeClaudeV1, distillQualityAdvisoryConcernV1)
	agg, err := aggregateDistillQualityPanelV1(packet, []distillQualityPanelVerdictV1{verdict})
	if err != nil {
		t.Fatal(err)
	}
	record := distillQualityHumanAdjudicationV1{SchemaVersion: 1, RecordID: "human_1", PacketID: packet.PacketID, PacketDigest: packet.PacketDigest, ItemDigest: packet.Item.Digest, PromptDigest: packet.Prompt.Digest, PayloadDigest: packet.Payload.Digest, AdvisoryDigest: distillQualityPanelAggregateDigestV1(agg), AdjudicatorID: "reviewer_1", AdjudicatedAt: "2026-08-25T12:00:00Z", Decision: distillQualityHumanNeedsReviewV1, Dimensions: []distillQualityHumanDimensionV1{{Dimension: distillQualityDimensionAdmissionV1, ProofLabel: distillQualityProofUncertainV1, Rationale: "Human needs more evidence."}}}
	if err := validateDistillQualityHumanAdjudicationV1(packet, agg, record); err != nil {
		t.Fatal(err)
	}
}
